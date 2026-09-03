// Package qbittorrent implements the small qBittorrent Web API surface used by
// Swarmfolio. It deliberately treats unexpected API responses as errors.
package qbittorrent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Config configures a qBittorrent Web API client. BaseURL is the qBittorrent
// web UI URL; an optional /api/v2 suffix is accepted.
type Config struct {
	BaseURL    string
	Username   string
	Password   string
	APIKey     string
	HTTPClient *http.Client
}

// Client is a qBittorrent Web API client.
type Client struct {
	baseURL  *url.URL
	origin   string
	apiKey   string
	username string
	password string
	http     *http.Client
}

// Torrent is the portion of qBittorrent's torrent-info response relevant to
// placement and replacement decisions.
type Torrent struct {
	Hash         string
	Name         string
	Size         int64
	Uploaded     int64
	AmountLeft   int64
	Progress     float64
	Ratio        float64
	SeedingTime  time.Duration
	AddedOn      time.Time
	CompletionOn time.Time
	LastActivity time.Time
	ETA          time.Duration
	State        string
	DLRate       int64
	UPRate       int64
	SavePath     string
	Category     string
	Tags         []string
}

// AddRequest is the data qBittorrent needs to add one torrent.
type AddRequest struct {
	Metainfo     []byte
	MetainfoName string
	SavePath     string
	Category     string
	Tags         []string
	Stopped      bool
}

// New constructs a client. API-key authentication takes precedence over the
// cookie-based login flow.
func New(config Config) (*Client, error) {
	if config.BaseURL == "" {
		return nil, errors.New("qBittorrent base URL is required")
	}
	if config.APIKey == "" && (config.Username == "" || config.Password == "") {
		return nil, errors.New("qBittorrent username and password are required without an API key")
	}
	u, err := url.Parse(config.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid qBittorrent base URL %q", config.BaseURL)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/api/v2") {
		u.Path = path.Join(u.Path, "api/v2")
	}

	client := config.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	if config.APIKey == "" && client.Jar == nil {
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, fmt.Errorf("create cookie jar: %w", err)
		}
		copy := *client
		copy.Jar = jar
		client = &copy
	}

	return &Client{
		baseURL:  u,
		origin:   (&url.URL{Scheme: u.Scheme, Host: u.Host}).String(),
		apiKey:   config.APIKey,
		username: config.Username,
		password: config.Password,
		http:     client,
	}, nil
}

// Login establishes a qBittorrent session. It is a no-op when APIKey is set.
func (c *Client) Login(ctx context.Context) error {
	if c.apiKey != "" {
		return nil
	}
	form := url.Values{"username": {c.username}, "password": {c.password}}
	return c.login(ctx, form)
}

// Torrents lists torrents known to qBittorrent.
func (c *Client) Torrents(ctx context.Context) ([]Torrent, error) {
	response, err := c.request(ctx, http.MethodGet, "torrents/info", nil, "", nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if err := requireSuccess(response); err != nil {
		return nil, err
	}

	var wire []torrentResponse
	if err := json.NewDecoder(response.Body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("decode torrents response: %w", err)
	}
	torrents := make([]Torrent, 0, len(wire))
	for _, item := range wire {
		torrents = append(torrents, item.torrent())
	}
	return torrents, nil
}

// DefaultSavePath returns qBittorrent's configured default download path.
func (c *Client) DefaultSavePath(ctx context.Context) (string, error) {
	response, err := c.request(ctx, http.MethodGet, "app/defaultSavePath", nil, "", nil)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if err := requireSuccess(response); err != nil {
		return "", err
	}
	value, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("read default save path: %w", err)
	}
	path := strings.TrimSpace(string(value))
	if path == "" {
		return "", errors.New("qBittorrent returned an empty default save path")
	}
	return path, nil
}

// FreeSpace returns free bytes on qBittorrent's download filesystem.
func (c *Client) FreeSpace(ctx context.Context) (int64, error) {
	response, err := c.request(ctx, http.MethodGet, "sync/maindata", nil, "", url.Values{"rid": {"0"}})
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if err := requireSuccess(response); err != nil {
		return 0, err
	}

	var data struct {
		ServerState *struct {
			FreeSpaceOnDisk *int64 `json:"free_space_on_disk"`
		} `json:"server_state"`
	}
	if err := json.NewDecoder(response.Body).Decode(&data); err != nil {
		return 0, fmt.Errorf("decode main data response: %w", err)
	}
	if data.ServerState == nil || data.ServerState.FreeSpaceOnDisk == nil {
		return 0, errors.New("qBittorrent main data response lacks server_state.free_space_on_disk")
	}
	if *data.ServerState.FreeSpaceOnDisk < 0 {
		return 0, fmt.Errorf("qBittorrent returned negative free space: %d", *data.ServerState.FreeSpaceOnDisk)
	}
	return *data.ServerState.FreeSpaceOnDisk, nil
}

// PreallocateAll reports whether qBittorrent preallocates the full size of
// every newly added torrent.
func (c *Client) PreallocateAll(ctx context.Context) (bool, error) {
	response, err := c.request(ctx, http.MethodGet, "app/preferences", nil, "", nil)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if err := requireSuccess(response); err != nil {
		return false, err
	}
	var preferences struct {
		PreallocateAll *bool `json:"preallocate_all"`
	}
	if err := json.NewDecoder(response.Body).Decode(&preferences); err != nil {
		return false, fmt.Errorf("decode preferences response: %w", err)
	}
	if preferences.PreallocateAll == nil {
		return false, errors.New("qBittorrent preferences response lacks preallocate_all")
	}
	return *preferences.PreallocateAll, nil
}

// Add uploads metainfo to qBittorrent.
func (c *Client) Add(ctx context.Context, request AddRequest) error {
	if len(request.Metainfo) == 0 {
		return errors.New("torrent metainfo is required")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	name := request.MetainfoName
	if name == "" {
		name = "torrent.torrent"
	}
	part, err := writer.CreateFormFile("torrents", name)
	if err != nil {
		return fmt.Errorf("create metainfo form field: %w", err)
	}
	if _, err := part.Write(request.Metainfo); err != nil {
		return fmt.Errorf("write metainfo form field: %w", err)
	}
	for key, value := range map[string]string{
		"savepath": request.SavePath,
		"category": request.Category,
		"tags":     strings.Join(request.Tags, ","),
		"stopped":  strconv.FormatBool(request.Stopped),
		"paused":   strconv.FormatBool(request.Stopped),
	} {
		if err := writer.WriteField(key, value); err != nil {
			return fmt.Errorf("write %s form field: %w", key, err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("finish multipart request: %w", err)
	}

	return c.mutate(ctx, "torrents/add", body.Bytes(), writer.FormDataContentType())
}

// Delete removes torrents, optionally deleting their downloaded files.
func (c *Client) Delete(ctx context.Context, hashes []string, deleteFiles bool) error {
	if err := validateHashes(hashes); err != nil {
		return err
	}
	return c.formMutate(ctx, "torrents/delete", url.Values{
		"hashes":      {strings.Join(hashes, "|")},
		"deleteFiles": {strconv.FormatBool(deleteFiles)},
	})
}

// AddTags adds comma-separated tags to torrents.
func (c *Client) AddTags(ctx context.Context, hashes, tags []string) error {
	return c.tags(ctx, "torrents/addTags", hashes, tags)
}

// RemoveTags removes comma-separated tags from torrents.
func (c *Client) RemoveTags(ctx context.Context, hashes, tags []string) error {
	return c.tags(ctx, "torrents/removeTags", hashes, tags)
}

// Start resumes torrents.
func (c *Client) Start(ctx context.Context, hashes []string) error {
	if err := validateHashes(hashes); err != nil {
		return err
	}
	return c.formMutate(ctx, "torrents/start", url.Values{"hashes": {strings.Join(hashes, "|")}})
}

func (c *Client) login(ctx context.Context, form url.Values) error {
	return c.formMutate(ctx, "auth/login", form)
}

func (c *Client) tags(ctx context.Context, endpoint string, hashes, tags []string) error {
	if err := validateHashes(hashes); err != nil {
		return err
	}
	if len(tags) == 0 {
		return errors.New("qBittorrent tag mutation requires at least one tag")
	}
	for _, tag := range tags {
		if tag == "" || strings.Contains(tag, ",") {
			return fmt.Errorf("invalid qBittorrent tag %q", tag)
		}
	}
	return c.formMutate(ctx, endpoint, url.Values{
		"hashes": {strings.Join(hashes, "|")},
		"tags":   {strings.Join(tags, ",")},
	})
}

func validateHashes(hashes []string) error {
	if len(hashes) == 0 {
		return errors.New("qBittorrent mutation requires at least one torrent hash")
	}
	for _, hash := range hashes {
		if hash == "" || strings.EqualFold(hash, "all") || strings.Contains(hash, "|") {
			return fmt.Errorf("invalid qBittorrent torrent hash %q", hash)
		}
	}
	return nil
}

func (c *Client) formMutate(ctx context.Context, endpoint string, form url.Values) error {
	return c.mutate(ctx, endpoint, []byte(form.Encode()), "application/x-www-form-urlencoded")
}

func (c *Client) mutate(ctx context.Context, endpoint string, body []byte, contentType string) error {
	response, err := c.request(ctx, http.MethodPost, endpoint, bytes.NewReader(body), contentType, nil)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return requireSuccess(response)
}

func (c *Client) request(ctx context.Context, method, endpoint string, body io.Reader, contentType string, query url.Values) (*http.Response, error) {
	u := *c.baseURL
	u.Path = path.Join(c.baseURL.Path, endpoint)
	u.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build qBittorrent request: %w", err)
	}
	request.Header.Set("Origin", c.origin)
	request.Header.Set("Referer", c.origin+"/")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("qBittorrent %s %s: %w", method, endpoint, err)
	}
	return response, nil
}

func requireSuccess(response *http.Response) error {
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("read qBittorrent response: %w", err)
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("qBittorrent returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	if strings.TrimSpace(string(body)) == "Fails." {
		return errors.New("qBittorrent rejected request: Fails.")
	}
	return nil
}

type torrentResponse struct {
	Hash         string  `json:"hash"`
	Name         string  `json:"name"`
	Size         int64   `json:"size"`
	Uploaded     int64   `json:"uploaded"`
	AmountLeft   int64   `json:"amount_left"`
	Progress     float64 `json:"progress"`
	Ratio        float64 `json:"ratio"`
	SeedingTime  int64   `json:"seeding_time"`
	AddedOn      int64   `json:"added_on"`
	CompletionOn int64   `json:"completion_on"`
	LastActivity int64   `json:"last_activity"`
	ETA          int64   `json:"eta"`
	State        string  `json:"state"`
	DLRate       int64   `json:"dlspeed"`
	UPRate       int64   `json:"upspeed"`
	SavePath     string  `json:"save_path"`
	Category     string  `json:"category"`
	Tags         string  `json:"tags"`
}

func (response torrentResponse) torrent() Torrent {
	return Torrent{
		Hash:         response.Hash,
		Name:         response.Name,
		Size:         response.Size,
		Uploaded:     response.Uploaded,
		AmountLeft:   response.AmountLeft,
		Progress:     response.Progress,
		Ratio:        response.Ratio,
		SeedingTime:  time.Duration(response.SeedingTime) * time.Second,
		AddedOn:      unixTime(response.AddedOn),
		CompletionOn: unixTime(response.CompletionOn),
		LastActivity: unixTime(response.LastActivity),
		ETA:          time.Duration(response.ETA) * time.Second,
		State:        response.State,
		DLRate:       response.DLRate,
		UPRate:       response.UPRate,
		SavePath:     response.SavePath,
		Category:     response.Category,
		Tags:         splitTags(response.Tags),
	}
}

func splitTags(value string) []string {
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for index := range parts {
		parts[index] = strings.TrimSpace(parts[index])
	}
	return parts
}

func unixTime(seconds int64) time.Time {
	if seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}
