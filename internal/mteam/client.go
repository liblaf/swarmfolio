// Package mteam provides the small M-Team API surface Swarmfolio needs.
package mteam

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/liblaf/swarmfolio/internal/metainfo"
)

const defaultBaseURL = "https://api.m-team.cc"

// ErrTorrentDownloadLimit means M-Team has exhausted this torrent's daily
// metainfo download allowance. Other torrents may still be downloaded.
var ErrTorrentDownloadLimit = errors.New("daily torrent download limit reached")

// DownloadRefusedError means M-Team's metainfo download endpoint answered with
// an API error code instead of metainfo. The upstream message is deliberately
// omitted because it may contain private URLs.
type DownloadRefusedError struct {
	Code int64
}

func (e *DownloadRefusedError) Error() string {
	return fmt.Sprintf("download API error %d", e.Code)
}

// Config controls requests to the M-Team API. Timezone is an IANA location
// name used for M-Team's zone-less discountEndTime values (for example,
// "Asia/Shanghai").
type Config struct {
	BaseURL    string
	APIKey     string
	Mode       string
	PageSize   int
	Pages      int
	Timezone   string
	HTTPClient *http.Client
}

// Torrent is an M-Team torrent offer. Search returns only download-free offers;
// Detail returns the current offer even when it is no longer download-free.
type Torrent struct {
	ID              int64
	Name            string
	Size            int64
	PublishedAt     time.Time
	Seeders         int64
	Leechers        int64
	Discount        string
	DiscountEndTime time.Time // Zero means the offer has no scheduled end time.
}

// Client is an M-Team API client. It holds no state beyond its configuration.
type Client struct {
	baseURL    *url.URL
	apiKey     string
	mode       string
	pageSize   int
	pages      int
	location   *time.Location
	httpClient *http.Client
	memberMu   sync.Mutex
	memberID   int64
}

// NewClient validates config and returns a client ready for use.
func NewClient(config Config) (*Client, error) {
	if config.APIKey == "" {
		return nil, errors.New("mteam: API key is required")
	}
	base := config.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("mteam: invalid base URL %q", base)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("mteam: base URL must not contain a query or fragment: %q", base)
	}
	pageSize := config.PageSize
	if pageSize == 0 {
		pageSize = 200
	}
	if pageSize < 1 || pageSize > 200 {
		return nil, fmt.Errorf("mteam: page size must be in [1, 200], got %d", pageSize)
	}
	pages := config.Pages
	if pages == 0 {
		pages = 1
	}
	if pages < 1 || pages > 1000 {
		return nil, fmt.Errorf("mteam: pages must be in [1, 1000], got %d", pages)
	}
	mode := config.Mode
	if mode == "" {
		mode = "normal"
	}
	if !validMode(mode) {
		return nil, fmt.Errorf("mteam: invalid mode %q", mode)
	}
	zone := config.Timezone
	if zone == "" {
		zone = "Asia/Shanghai"
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return nil, fmt.Errorf("mteam: load timezone %q: %w", zone, err)
	}
	client := config.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &Client{baseURL: u, apiKey: config.APIKey, mode: mode, pageSize: pageSize, pages: pages, location: location, httpClient: client}, nil
}

// Search returns current download-free results from the configured number of
// pages. It retries transient search failures, rejects malformed envelopes,
// and filters expired promotions.
func (c *Client) Search(ctx context.Context) ([]Torrent, error) {
	var torrents []Torrent
	seen := make(map[int64]Torrent)
	for page := 1; page <= c.pages; page++ {
		for _, discount := range []string{"FREE", "_2X_FREE"} {
			body, err := json.Marshal(searchRequest{
				PageNumber:    page,
				PageSize:      c.pageSize,
				Mode:          c.mode,
				Discount:      discount,
				SortField:     "LEECHERS",
				SortDirection: "DESC",
			})
			if err != nil {
				return nil, fmt.Errorf("mteam: marshal search request: %w", err)
			}
			payload, err := c.searchPage(ctx, body)
			if err != nil {
				return nil, fmt.Errorf("mteam: search page %d discount %s: %w", page, discount, err)
			}
			items, err := decodeSearch(payload)
			if err != nil {
				return nil, fmt.Errorf("mteam: search page %d discount %s: %w", page, discount, err)
			}
			for _, item := range items {
				torrent, include, err := c.decodeTorrent(item)
				if err != nil {
					return nil, fmt.Errorf("mteam: search page %d discount %s: %w", page, discount, err)
				}
				if !include {
					continue
				}
				if existing, ok := seen[torrent.ID]; ok {
					if !sameTorrentIdentity(existing, torrent) {
						return nil, fmt.Errorf("mteam: conflicting duplicate torrent ID %d", torrent.ID)
					}
					continue
				}
				seen[torrent.ID] = torrent
				torrents = append(torrents, torrent)
			}
		}
	}
	return torrents, nil
}

// Detail returns the current offer for id. Unlike Search, it deliberately does
// not filter by discount: callers use it to verify that an already-added
// torrent is still free before allowing it to download.
func (c *Client) Detail(ctx context.Context, id int64) (Torrent, error) {
	if id <= 0 {
		return Torrent{}, fmt.Errorf("mteam: torrent ID must be positive, got %d", id)
	}
	request, err := c.request(ctx, http.MethodGet, "/api/torrent/detail?id="+url.QueryEscape(strconv.FormatInt(id, 10)), nil)
	if err != nil {
		return Torrent{}, err
	}
	response, err := c.credentialedHTTPClient().Do(request)
	if err != nil {
		return Torrent{}, fmt.Errorf("mteam: torrent detail: %w", err)
	}
	payload, err := readResponse(response)
	if err != nil {
		return Torrent{}, fmt.Errorf("mteam: torrent detail: %w", err)
	}
	var raw json.RawMessage
	if err := decodeEnvelope(payload, &raw); err != nil {
		return Torrent{}, fmt.Errorf("mteam: torrent detail: %w", err)
	}
	torrent, err := c.decodeOffer(raw)
	if err != nil {
		return Torrent{}, fmt.Errorf("mteam: torrent detail: %w", err)
	}
	if torrent.ID != id {
		return Torrent{}, fmt.Errorf("mteam: torrent detail returned ID %d for requested ID %d", torrent.ID, id)
	}
	return torrent, nil
}

const (
	offerPageSize = 200
	maxOfferPages = 1000
)

// Offers returns every currently incomplete torrent associated with the API
// key's account. Its results intentionally include non-free discounts so the
// caller can stop downloads whose promotion has ended.
func (c *Client) Offers(ctx context.Context) ([]Torrent, error) {
	memberID, err := c.currentMemberID(ctx)
	if err != nil {
		return nil, err
	}
	var offers []Torrent
	seen := make(map[int64]Torrent)
	pages := 0
	for page := 1; ; page++ {
		if page > maxOfferPages {
			return nil, fmt.Errorf("mteam: incomplete torrent list exceeds %d pages", maxOfferPages)
		}
		body, err := json.Marshal(userTorrentRequest{UserID: memberID, Type: "INCOMPLETE", PageNumber: page, PageSize: offerPageSize})
		if err != nil {
			return nil, fmt.Errorf("mteam: marshal incomplete torrent list request: %w", err)
		}
		request, err := c.request(ctx, http.MethodPost, "/api/member/getUserTorrentList", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := c.credentialedHTTPClient().Do(request)
		if err != nil {
			return nil, fmt.Errorf("mteam: incomplete torrent list page %d: %w", page, err)
		}
		payload, err := readResponse(response)
		if err != nil {
			return nil, fmt.Errorf("mteam: incomplete torrent list page %d: %w", page, err)
		}
		var result userTorrentPage
		if err := decodeEnvelope(payload, &result); err != nil {
			return nil, fmt.Errorf("mteam: incomplete torrent list page %d: %w", page, err)
		}
		pageNumber, err := result.PageNumber.Int64("incomplete torrent list page number")
		if err != nil {
			return nil, fmt.Errorf("mteam: incomplete torrent list page %d: %w", page, err)
		}
		pageSize, err := result.PageSize.Int64("incomplete torrent list page size")
		if err != nil {
			return nil, fmt.Errorf("mteam: incomplete torrent list page %d: %w", page, err)
		}
		totalPages, err := result.TotalPages.Int64("incomplete torrent list total pages")
		if err != nil {
			return nil, fmt.Errorf("mteam: incomplete torrent list page %d: %w", page, err)
		}
		total, err := result.Total.Int64("incomplete torrent list total")
		if err != nil {
			return nil, fmt.Errorf("mteam: incomplete torrent list page %d: %w", page, err)
		}
		if pageNumber != int64(page) || pageSize != int64(offerPageSize) || totalPages > int64(maxOfferPages) {
			return nil, fmt.Errorf("mteam: invalid incomplete torrent list page metadata on page %d", page)
		}
		if totalPages == 0 {
			if page != 1 || total != 0 || result.Data == nil || len(result.Data) != 0 {
				return nil, errors.New("mteam: invalid empty incomplete torrent list")
			}
			return offers, nil
		}
		if totalPages < 1 || total == 0 {
			return nil, fmt.Errorf("mteam: invalid incomplete torrent list page metadata on page %d", page)
		}
		if pages == 0 {
			pages = int(totalPages)
		} else if totalPages != int64(pages) {
			return nil, fmt.Errorf("mteam: incomplete torrent list changed page count from %d to %d", pages, totalPages)
		}
		if result.Data == nil {
			return nil, fmt.Errorf("mteam: incomplete torrent list page %d has null data", page)
		}
		for _, item := range result.Data {
			// Deleted torrent-history entries remain in INCOMPLETE temporarily.
			// They carry an explicit null torrent and have no offer to guard.
			if bytes.Equal(bytes.TrimSpace(item.Torrent), []byte("null")) {
				continue
			}
			offer, err := c.decodeOffer(item.Torrent)
			if err != nil {
				return nil, fmt.Errorf("mteam: incomplete torrent list page %d: %w", page, err)
			}
			if existing, ok := seen[offer.ID]; ok {
				if !sameTorrentIdentity(existing, offer) {
					return nil, fmt.Errorf("mteam: conflicting duplicate torrent ID %d", offer.ID)
				}
				continue
			}
			seen[offer.ID] = offer
			offers = append(offers, offer)
		}
		if page == pages {
			return offers, nil
		}
	}
}

func (c *Client) currentMemberID(ctx context.Context) (int64, error) {
	c.memberMu.Lock()
	defer c.memberMu.Unlock()
	if c.memberID > 0 {
		return c.memberID, nil
	}
	request, err := c.request(ctx, http.MethodPost, "/api/member/profile", bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.credentialedHTTPClient().Do(request)
	if err != nil {
		return 0, fmt.Errorf("mteam: member profile: %w", err)
	}
	payload, err := readResponse(response)
	if err != nil {
		return 0, fmt.Errorf("mteam: member profile: %w", err)
	}
	var profile struct {
		ID stringOrNumber `json:"id"`
	}
	if err := decodeEnvelope(payload, &profile); err != nil {
		return 0, fmt.Errorf("mteam: member profile: %w", err)
	}
	id, err := profile.ID.Int64("member ID")
	if err != nil || id == 0 {
		return 0, fmt.Errorf("mteam: member profile has invalid member ID")
	}
	c.memberID = id
	return id, nil
}

// sameTorrentIdentity compares fields that identify the offer. Seeder and
// leecher counts are intentionally excluded because separate paginated search
// responses may observe the swarm at different moments.
func sameTorrentIdentity(a, b Torrent) bool {
	return a.Name == b.Name && a.Size == b.Size &&
		a.PublishedAt.Equal(b.PublishedAt) && a.Discount == b.Discount &&
		a.DiscountEndTime.Equal(b.DiscountEndTime)
}

// Download obtains an ephemeral token for id and returns verified torrent
// metainfo bytes. The token URL is deliberately not retained by the client.
func (c *Client) Download(ctx context.Context, id int64) ([]byte, error) {
	if id <= 0 {
		return nil, fmt.Errorf("mteam: torrent ID must be positive, got %d", id)
	}
	path := "/api/torrent/genDlToken?id=" + url.QueryEscape(strconv.FormatInt(id, 10))
	request, err := c.request(ctx, http.MethodPost, path, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.credentialedHTTPClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("mteam: generate download token: %w", err)
	}
	payload, err := readResponse(response)
	if err != nil {
		return nil, fmt.Errorf("mteam: generate download token: %w", err)
	}
	var token string
	if err := decodeEnvelope(payload, &token); err != nil {
		return nil, fmt.Errorf("mteam: generate download token: %w", err)
	}
	tokenURL, err := url.Parse(token)
	if err != nil || tokenURL.Scheme != "https" || tokenURL.Host == "" {
		return nil, errors.New("mteam: API returned an invalid download URL")
	}
	downloadRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("mteam: create download request: %w", redactTokenError(err))
	}
	downloadResponse, err := c.downloadHTTPClient().Do(downloadRequest)
	if err != nil {
		return nil, fmt.Errorf("mteam: download torrent: %w", redactTokenError(err))
	}
	metainfo, err := readResponse(downloadResponse)
	if err != nil {
		return nil, fmt.Errorf("mteam: download torrent: %w", redactTokenError(err))
	}
	if err := downloadAPIError(metainfo); err != nil {
		return nil, fmt.Errorf("mteam: download torrent: %w", err)
	}
	if err := validateTorrent(metainfo); err != nil {
		return nil, fmt.Errorf("mteam: download torrent: %w", err)
	}
	return metainfo, nil
}

func downloadAPIError(payload []byte) error {
	var response envelope
	if json.Unmarshal(payload, &response) != nil {
		return nil
	}
	code, err := parseCode(response.Code)
	if err != nil || code == 0 {
		return nil
	}
	// Code 1 also covers unrelated errors. Only this observed response proves
	// the per-torrent daily allowance is exhausted; never suppress other errors.
	if code == 1 && response.Message == "相同種子當天最多下載10次" {
		return ErrTorrentDownloadLimit
	}
	return &DownloadRefusedError{Code: code}
}

func (c *Client) credentialedHTTPClient() *http.Client {
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client
}

func (c *Client) downloadHTTPClient() *http.Client {
	client := *c.httpClient
	checkRedirect := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		request.Header.Del("Referer")
		if request.URL.Scheme != "https" || request.URL.Host == "" {
			return errors.New("mteam: download redirect must use HTTPS")
		}
		if checkRedirect != nil {
			return checkRedirect(request, via)
		}
		if len(via) >= 10 {
			return errors.New("mteam: too many download redirects")
		}
		return nil
	}
	return &client
}

type tokenError struct {
	err error
}

func (e tokenError) Error() string {
	return "request failed"
}

func (e tokenError) Unwrap() error {
	return e.err
}

func redactTokenError(err error) error {
	return tokenError{err: err}
}

type searchRequest struct {
	PageNumber    int    `json:"pageNumber"`
	PageSize      int    `json:"pageSize"`
	Mode          string `json:"mode"`
	Discount      string `json:"discount"`
	SortField     string `json:"sortField"`
	SortDirection string `json:"sortDirection"`
}

type envelope struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type searchData struct {
	Data json.RawMessage `json:"data"`
}

type userTorrentRequest struct {
	UserID     int64  `json:"userid"`
	Type       string `json:"type"`
	PageNumber int    `json:"pageNumber"`
	PageSize   int    `json:"pageSize"`
}

type userTorrentPage struct {
	PageNumber stringOrNumber `json:"pageNumber"`
	PageSize   stringOrNumber `json:"pageSize"`
	Total      stringOrNumber `json:"total"`
	TotalPages stringOrNumber `json:"totalPages"`
	Data       []struct {
		Torrent json.RawMessage `json:"torrent"`
	} `json:"data"`
}

type rawTorrent struct {
	ID          stringOrNumber `json:"id"`
	Name        string         `json:"name"`
	Size        stringOrNumber `json:"size"`
	CreatedDate string         `json:"createdDate"`
	Status      struct {
		Discount        string          `json:"discount"`
		DiscountEndTime json.RawMessage `json:"discountEndTime"`
		Seeders         stringOrNumber  `json:"seeders"`
		Leechers        stringOrNumber  `json:"leechers"`
	} `json:"status"`
}

type stringOrNumber string

func (v *stringOrNumber) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return errors.New("unexpected null")
	}
	var value string
	if err := json.Unmarshal(data, &value); err == nil {
		*v = stringOrNumber(value)
		return nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return fmt.Errorf("want string or number: %w", err)
	}
	*v = stringOrNumber(number.String())
	return nil
}

func (v stringOrNumber) Int64(name string) (int64, error) {
	n, err := strconv.ParseInt(string(v), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s %q", name, string(v))
	}
	return n, nil
}

func (c *Client) request(ctx context.Context, method, endpoint string, body io.Reader) (*http.Request, error) {
	endpointURL, err := url.Parse(endpoint)
	if err != nil || endpointURL.IsAbs() {
		return nil, fmt.Errorf("mteam: invalid endpoint %q", endpoint)
	}
	u := *c.baseURL
	u.Path = strings.TrimRight(c.baseURL.Path, "/") + endpointURL.Path
	u.RawQuery = endpointURL.RawQuery
	request, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("mteam: create request: %w", err)
	}
	request.Header.Set("x-api-key", c.apiKey)
	return request, nil
}

func readResponse(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 16<<20+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > 16<<20 {
		return nil, errors.New("response exceeds 16 MiB limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	return payload, nil
}

func decodeSearch(payload []byte) ([]json.RawMessage, error) {
	var data searchData
	if err := decodeEnvelope(payload, &data); err != nil {
		return nil, err
	}
	if len(data.Data) == 0 || string(data.Data) == "null" {
		return nil, errors.New("search response has no result data")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(data.Data, &items); err != nil {
		return nil, fmt.Errorf("invalid search result list: %w", err)
	}
	return items, nil
}

func decodeEnvelope(payload []byte, data any) error {
	var response envelope
	if err := json.Unmarshal(payload, &response); err != nil {
		return fmt.Errorf("invalid JSON response: %w", err)
	}
	code, err := parseCode(response.Code)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("API error %d: %s", code, response.Message)
	}
	if len(response.Data) == 0 || string(response.Data) == "null" {
		return errors.New("successful response has no data")
	}
	if err := json.Unmarshal(response.Data, data); err != nil {
		return fmt.Errorf("invalid response data: %w", err)
	}
	return nil
}

func parseCode(raw json.RawMessage) (int64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, errors.New("response has no code")
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		text = string(raw)
	}
	code, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid response code %q", text)
	}
	return code, nil
}

func (c *Client) decodeTorrent(raw json.RawMessage) (Torrent, bool, error) {
	var promotion struct {
		Status struct {
			Discount string `json:"discount"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &promotion); err != nil {
		return Torrent{}, false, fmt.Errorf("invalid torrent: %w", err)
	}
	if promotion.Status.Discount != "FREE" && promotion.Status.Discount != "_2X_FREE" {
		return Torrent{}, false, nil
	}
	torrent, err := c.decodeOffer(raw)
	if err != nil {
		return Torrent{}, false, err
	}
	if !torrent.DiscountEndTime.IsZero() && !torrent.DiscountEndTime.After(time.Now()) {
		return Torrent{}, false, nil
	}
	return torrent, true, nil
}

func (c *Client) decodeOffer(raw json.RawMessage) (Torrent, error) {
	var item rawTorrent
	if err := json.Unmarshal(raw, &item); err != nil {
		return Torrent{}, fmt.Errorf("invalid torrent: %w", err)
	}
	id, err := item.ID.Int64("torrent ID")
	if err != nil {
		return Torrent{}, err
	}
	if id == 0 {
		return Torrent{}, errors.New("torrent ID must be positive")
	}
	size, err := item.Size.Int64("torrent size")
	if err != nil {
		return Torrent{}, err
	}
	if size == 0 {
		return Torrent{}, errors.New("torrent size must be positive")
	}
	if item.Name == "" {
		return Torrent{}, errors.New("torrent has an empty name")
	}
	publishedAt, err := parseMTeamTime(item.CreatedDate, c.location)
	if err != nil {
		return Torrent{}, fmt.Errorf("invalid published time: %w", err)
	}
	seeders, err := item.Status.Seeders.Int64("seeders")
	if err != nil {
		return Torrent{}, err
	}
	leechers, err := item.Status.Leechers.Int64("leechers")
	if err != nil {
		return Torrent{}, err
	}
	if item.Status.Discount == "" {
		return Torrent{}, errors.New("torrent has an empty discount")
	}
	// Explicit null and empty strings mean no scheduled end. Decoding the raw
	// field also rejects omitted fields and unexpected JSON types.
	var endTimeText string
	if err := json.Unmarshal(item.Status.DiscountEndTime, &endTimeText); err != nil {
		return Torrent{}, fmt.Errorf("invalid discount end time: %w", err)
	}
	endTime := time.Time{}
	if endTimeText != "" {
		endTime, err = time.ParseInLocation("2006-01-02 15:04:05", endTimeText, c.location)
		if err != nil {
			return Torrent{}, fmt.Errorf("invalid discount end time %q: %w", endTimeText, err)
		}
	}
	return Torrent{ID: id, Name: item.Name, Size: size, PublishedAt: publishedAt, Seeders: seeders, Leechers: leechers, Discount: item.Status.Discount, DiscountEndTime: endTime}, nil
}

func parseMTeamTime(value string, location *time.Location) (time.Time, error) {
	if value == "" {
		return time.Time{}, errors.New("missing timestamp")
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, nil
	}
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, location)
	if err != nil {
		return time.Time{}, fmt.Errorf("want YYYY-MM-DD HH:MM:SS or RFC3339, got %q", value)
	}
	return parsed, nil
}

func validMode(mode string) bool {
	for _, candidate := range []string{"normal", "adult", "movie", "music", "tvshow", "anime", "waterfall", "rss", "rankings", "all"} {
		if mode == candidate {
			return true
		}
	}
	return false
}

func validateTorrent(data []byte) error {
	if _, err := metainfo.Inspect(data); err != nil {
		return fmt.Errorf("invalid torrent metainfo: %w", err)
	}
	return nil
}
