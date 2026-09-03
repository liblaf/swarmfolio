package mteam

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestSearchSendsFreeleechQueryAndDecodesFlexibleNumbers(t *testing.T) {
	t.Parallel()
	discounts := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/torrent/search" {
			t.Fatalf("request = %s %s", r.Method, r.URL)
		}
		if r.Header.Get("x-api-key") != "key" {
			t.Fatalf("API key = %q", r.Header.Get("x-api-key"))
		}
		var query searchRequest
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
			t.Fatal(err)
		}
		if query.Discount != "FREE" && query.Discount != "_2X_FREE" || query.SortField != "LEECHERS" || query.SortDirection != "DESC" || query.PageSize != 2 {
			t.Fatalf("unexpected query: %#v", query)
		}
		discounts[query.Discount] = true
		io.WriteString(w, `{"code":"0","message":"ok","data":{"data":[{"id":"12","name":"free","size":1024,"createdDate":"2099-01-02 03:04:05","status":{"discount":"FREE","discountEndTime":"2099-01-02 03:04:05","seeders":"3","leechers":4}},{"id":13,"name":"double","size":"2048","createdDate":"2099-01-02T03:04:05Z","status":{"discount":"_2X_FREE","discountEndTime":"","seeders":2,"leechers":"5"}},{"id":14,"name":"not free","size":"1","status":{"discount":"PERCENT_50","seeders":"1","leechers":"1"}}]}}`)
	}))
	defer server.Close()

	client := testClient(t, server.URL, Config{PageSize: 2, Timezone: "Asia/Shanghai"})
	got, err := client.Search(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d torrents, want 2", len(got))
	}
	if got[0].ID != 12 || got[0].Size != 1024 || got[0].Seeders != 3 || got[0].Leechers != 4 || got[0].DiscountEndTime.Location().String() != "Asia/Shanghai" || got[0].PublishedAt.Location().String() != "Asia/Shanghai" {
		t.Fatalf("first torrent = %#v", got[0])
	}
	if got[1].ID != 13 || !got[1].DiscountEndTime.IsZero() {
		t.Fatalf("second torrent = %#v", got[1])
	}
	if !discounts["FREE"] || !discounts["_2X_FREE"] {
		t.Fatalf("search discounts = %#v", discounts)
	}
}

func TestSearchDeduplicatesPagesAndRejectsConflicts(t *testing.T) {
	t.Parallel()
	for name, conflict := range map[string]bool{"stable": false, "conflict": true} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var query searchRequest
				if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
					t.Fatal(err)
				}
				name, size := "one", 1
				if query.PageNumber == 2 && query.Discount == "_2X_FREE" && conflict {
					name, size = "changed", 2
				}
				io.WriteString(w, `{"code":0,"data":{"data":[{"id":1,"name":"`+name+`","size":`+strconv.Itoa(size)+`,"createdDate":"2099-01-01 00:00:00","status":{"discount":"`+query.Discount+`","seeders":1,"leechers":1}}]}}`)
			}))
			defer server.Close()

			results, err := testClient(t, server.URL, Config{Pages: 2}).Search(context.Background())
			if conflict {
				if err == nil || !strings.Contains(err.Error(), "conflicting duplicate") {
					t.Fatalf("Search error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].ID != 1 {
				t.Fatalf("results = %#v", results)
			}
		})
	}
}

func TestSearchRejectsAPIErrorAndMalformedFreeTorrent(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"API error":              `{"code":123,"message":"denied","data":{}}`,
		"bad size":               `{"code":0,"data":{"data":[{"id":1,"name":"bad","size":"wat","createdDate":"2026-01-01 00:00:00","status":{"discount":"FREE","seeders":1,"leechers":1}}]}}`,
		"missing published time": `{"code":0,"data":{"data":[{"id":1,"name":"bad","size":1,"status":{"discount":"FREE","seeders":1,"leechers":1}}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer server.Close()
			_, err := testClient(t, server.URL, Config{}).Search(context.Background())
			if err == nil {
				t.Fatal("Search succeeded unexpectedly")
			}
		})
	}
}

func TestDownloadUsesTokenAndValidatesTorrent(t *testing.T) {
	t.Parallel()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/torrent/genDlToken":
			if r.Method != http.MethodPost || r.URL.Query().Get("id") != "42" {
				t.Fatalf("token request = %s %s", r.Method, r.URL)
			}
			// The production API issues HTTPS URLs. Rewrite the transport in this
			// test so the local test endpoint can stand in for that server.
			io.WriteString(w, `{"code":"0","data":"https://mteam.test/torrent"}`)
		case "/torrent":
			io.WriteString(w, "d4:infod4:name1:xee")
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.RegisterProtocol("https", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		copy.URL.Scheme = "http"
		copy.URL.Host = strings.TrimPrefix(server.URL, "http://")
		return http.DefaultTransport.RoundTrip(copy)
	}))
	client := testClient(t, server.URL, Config{HTTPClient: &http.Client{Transport: baseTransport}})
	got, err := client.Download(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "d4:infod4:name1:xee" {
		t.Fatalf("download = %q", got)
	}
}

func TestDownloadRejectsNonTorrent(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/torrent/genDlToken" {
			io.WriteString(w, `{"code":0,"data":"https://invalid.example/torrent"}`)
		}
	}))
	defer server.Close()
	client := testClient(t, server.URL, Config{})
	_, err := client.Download(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "download torrent") {
		t.Fatalf("error = %v, want download error", err)
	}
}

func TestNewClientRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	for _, config := range []Config{
		{},
		{APIKey: "key", PageSize: 201},
		{APIKey: "key", Pages: -1},
		{APIKey: "key", Timezone: "not/a/timezone"},
		{APIKey: "key", Mode: "nope"},
	} {
		if _, err := NewClient(config); err == nil {
			t.Fatalf("NewClient(%#v) succeeded", config)
		}
	}
}

func testClient(t *testing.T, baseURL string, config Config) *Client {
	t.Helper()
	config.BaseURL = baseURL
	config.APIKey = "key"
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestValidateTorrent(t *testing.T) {
	t.Parallel()
	for _, input := range [][]byte{
		[]byte("d4:infod4:name1:xee"),
		[]byte("d8:announce4:test4:infod4:name1:xee"),
	} {
		if err := validateTorrent(input); err != nil {
			t.Fatalf("validateTorrent(%q): %v", input, err)
		}
	}
	if err := validateTorrent([]byte("not a torrent")); err == nil {
		t.Fatal("validateTorrent accepted non-bencode")
	}
	if err := validateTorrent([]byte("d4:name1:xe")); err == nil {
		t.Fatal("validateTorrent accepted no-info dictionary")
	}
}

func TestExpiredFreeleechIsExcluded(t *testing.T) {
	t.Parallel()
	client := testClient(t, "https://example.test", Config{Timezone: "UTC"})
	_, include, err := client.decodeTorrent(json.RawMessage(`{"id":1,"name":"expired","size":1,"createdDate":"1999-01-01 00:00:00","status":{"discount":"FREE","discountEndTime":"2000-01-01 00:00:00","seeders":1,"leechers":1}}`))
	if err != nil || include {
		t.Fatalf("include=%v err=%v", include, err)
	}
}
