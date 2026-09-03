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
	"time"
)

const defaultBaseURL = "https://api.m-team.cc"

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

// Torrent is a download-free search result returned by M-Team.
type Torrent struct {
	ID              int64
	Name            string
	Size            int64
	PublishedAt     time.Time
	Seeders         int64
	Leechers        int64
	Discount        string
	DiscountEndTime time.Time // Zero means M-Team did not provide an end time.
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
// pages. It rejects malformed envelopes and filters expired promotions.
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
			request, err := c.request(ctx, http.MethodPost, "/api/torrent/search", bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			request.Header.Set("Content-Type", "application/json")
			response, err := c.httpClient.Do(request)
			if err != nil {
				return nil, fmt.Errorf("mteam: search page %d discount %s: %w", page, discount, err)
			}
			payload, err := readResponse(response)
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
					if existing.Name != torrent.Name || existing.Size != torrent.Size {
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
	response, err := c.httpClient.Do(request)
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
		return nil, fmt.Errorf("mteam: create download request: %w", err)
	}
	downloadResponse, err := c.httpClient.Do(downloadRequest)
	if err != nil {
		return nil, fmt.Errorf("mteam: download torrent: %w", err)
	}
	metainfo, err := readResponse(downloadResponse)
	if err != nil {
		return nil, fmt.Errorf("mteam: download torrent: %w", err)
	}
	if err := validateTorrent(metainfo); err != nil {
		return nil, fmt.Errorf("mteam: download torrent: %w", err)
	}
	return metainfo, nil
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

type rawTorrent struct {
	ID          stringOrNumber `json:"id"`
	Name        string         `json:"name"`
	Size        stringOrNumber `json:"size"`
	CreatedDate string         `json:"createdDate"`
	Status      struct {
		Discount        string         `json:"discount"`
		DiscountEndTime string         `json:"discountEndTime"`
		Seeders         stringOrNumber `json:"seeders"`
		Leechers        stringOrNumber `json:"leechers"`
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
	var item rawTorrent
	if err := json.Unmarshal(raw, &item); err != nil {
		return Torrent{}, false, fmt.Errorf("invalid torrent: %w", err)
	}
	if item.Status.Discount != "FREE" && item.Status.Discount != "_2X_FREE" {
		return Torrent{}, false, nil
	}
	id, err := item.ID.Int64("torrent ID")
	if err != nil {
		return Torrent{}, false, err
	}
	if id == 0 {
		return Torrent{}, false, errors.New("torrent ID must be positive")
	}
	size, err := item.Size.Int64("torrent size")
	if err != nil {
		return Torrent{}, false, err
	}
	if size == 0 {
		return Torrent{}, false, errors.New("torrent size must be positive")
	}
	if item.Name == "" {
		return Torrent{}, false, errors.New("torrent has an empty name")
	}
	publishedAt, err := parseMTeamTime(item.CreatedDate, c.location)
	if err != nil {
		return Torrent{}, false, fmt.Errorf("invalid published time: %w", err)
	}
	seeders, err := item.Status.Seeders.Int64("seeders")
	if err != nil {
		return Torrent{}, false, err
	}
	leechers, err := item.Status.Leechers.Int64("leechers")
	if err != nil {
		return Torrent{}, false, err
	}
	endTime := time.Time{}
	if item.Status.DiscountEndTime != "" {
		endTime, err = time.ParseInLocation("2006-01-02 15:04:05", item.Status.DiscountEndTime, c.location)
		if err != nil {
			return Torrent{}, false, fmt.Errorf("invalid discount end time %q: %w", item.Status.DiscountEndTime, err)
		}
		if !endTime.After(time.Now()) {
			return Torrent{}, false, nil
		}
	}
	return Torrent{ID: id, Name: item.Name, Size: size, PublishedAt: publishedAt, Seeders: seeders, Leechers: leechers, Discount: item.Status.Discount, DiscountEndTime: endTime}, true, nil
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
	parser := bencodeParser{data: data}
	if len(data) == 0 || data[0] != 'd' {
		return errors.New("response is not a bencoded torrent dictionary")
	}
	hasInfo, err := parser.dictionary()
	if err != nil {
		return fmt.Errorf("invalid bencode: %w", err)
	}
	if parser.pos != len(data) {
		return errors.New("invalid bencode trailing data")
	}
	if !hasInfo {
		return errors.New("torrent dictionary has no info key")
	}
	return nil
}

type bencodeParser struct {
	data []byte
	pos  int
}

func (p *bencodeParser) value() error {
	if p.pos >= len(p.data) {
		return errors.New("unexpected end")
	}
	switch p.data[p.pos] {
	case 'i':
		p.pos++
		start := p.pos
		if p.pos < len(p.data) && p.data[p.pos] == '-' {
			p.pos++
		}
		for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
		if start == p.pos || p.pos >= len(p.data) || p.data[p.pos] != 'e' {
			return errors.New("invalid integer")
		}
		p.pos++
		return nil
	case 'l':
		p.pos++
		for p.pos < len(p.data) && p.data[p.pos] != 'e' {
			if err := p.value(); err != nil {
				return err
			}
		}
		if p.pos >= len(p.data) {
			return errors.New("unterminated list")
		}
		p.pos++
		return nil
	case 'd':
		_, err := p.dictionary()
		return err
	default:
		_, err := p.string()
		return err
	}
}

func (p *bencodeParser) dictionary() (bool, error) {
	if p.pos >= len(p.data) || p.data[p.pos] != 'd' {
		return false, errors.New("expected dictionary")
	}
	p.pos++
	hasInfo := false
	for p.pos < len(p.data) && p.data[p.pos] != 'e' {
		key, err := p.string()
		if err != nil {
			return false, err
		}
		if string(key) == "info" {
			hasInfo = true
		}
		if err := p.value(); err != nil {
			return false, err
		}
	}
	if p.pos >= len(p.data) {
		return false, errors.New("unterminated dictionary")
	}
	p.pos++
	return hasInfo, nil
}

func (p *bencodeParser) string() ([]byte, error) {
	start := p.pos
	for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
		p.pos++
	}
	if start == p.pos || p.pos >= len(p.data) || p.data[p.pos] != ':' {
		return nil, errors.New("invalid string length")
	}
	length, err := strconv.Atoi(string(p.data[start:p.pos]))
	if err != nil || length < 0 {
		return nil, errors.New("invalid string length")
	}
	p.pos++
	if length > len(p.data)-p.pos {
		return nil, errors.New("truncated string")
	}
	value := p.data[p.pos : p.pos+length]
	p.pos += length
	return value, nil
}
