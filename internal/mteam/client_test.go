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
	for _, test := range []struct {
		name    string
		mutate  func(*torrentFixture, searchRequest)
		wantErr bool
	}{
		{
			name: "stable allows changing swarm counters",
			mutate: func(item *torrentFixture, query searchRequest) {
				item.seeders, item.leechers = int64(query.PageNumber), int64(query.PageNumber+1)
			},
		},
		{
			name: "name", wantErr: true,
			mutate: func(item *torrentFixture, query searchRequest) {
				if query.PageNumber == 2 {
					item.name = "changed"
				}
			},
		},
		{
			name: "size", wantErr: true,
			mutate: func(item *torrentFixture, query searchRequest) {
				if query.PageNumber == 2 {
					item.size = 2
				}
			},
		},
		{
			name: "published time", wantErr: true,
			mutate: func(item *torrentFixture, query searchRequest) {
				if query.PageNumber == 2 {
					item.publishedAt = "2099-01-02 00:00:00"
				}
			},
		},
		{
			name: "promotion", wantErr: true,
			mutate: func(item *torrentFixture, query searchRequest) {
				if query.PageNumber == 2 {
					item.discount = "_2X_FREE"
				}
			},
		},
		{
			name: "promotion expiry", wantErr: true,
			mutate: func(item *torrentFixture, query searchRequest) {
				if query.PageNumber == 2 {
					item.discountEndTime = "2099-01-02 00:00:00"
				}
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var query searchRequest
				if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
					t.Fatal(err)
				}
				// The result's offer data remains stable across FREE and _2X_FREE
				// queries. The query filter is not a substitute for its status.
				item := torrentFixture{name: "one", size: 1, publishedAt: "2099-01-01 00:00:00", discount: "FREE", discountEndTime: "2099-01-01 01:00:00", seeders: 1, leechers: 1}
				test.mutate(&item, query)
				io.WriteString(w, item.response())
			}))
			defer server.Close()

			results, err := testClient(t, server.URL, Config{Pages: 2}).Search(context.Background())
			if test.wantErr {
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

type torrentFixture struct {
	name, publishedAt, discount, discountEndTime string
	size, seeders, leechers                      int64
}

func (item torrentFixture) response() string {
	return `{"code":0,"data":{"data":[{"id":1,"name":"` + item.name + `","size":` + strconv.FormatInt(item.size, 10) + `,"createdDate":"` + item.publishedAt + `","status":{"discount":"` + item.discount + `","discountEndTime":"` + item.discountEndTime + `","seeders":` + strconv.FormatInt(item.seeders, 10) + `,"leechers":` + strconv.FormatInt(item.leechers, 10) + `}}]}}`
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

func TestFreeleechAcceptsExplicitNullOrEmptyEndTime(t *testing.T) {
	t.Parallel()
	client := testClient(t, "https://example.test", Config{Timezone: "UTC"})
	for _, discount := range []string{"FREE", "_2X_FREE"} {
		for _, endTime := range []string{`null`, `""`} {
			t.Run(discount+"/"+endTime, func(t *testing.T) {
				raw := json.RawMessage(`{"id":1,"name":"open-ended","size":1,"createdDate":"2099-01-01 00:00:00","status":{"discount":"` + discount + `","discountEndTime":` + endTime + `,"seeders":1,"leechers":1}}`)
				got, include, err := client.decodeTorrent(raw)
				if err != nil || !include || !got.DiscountEndTime.IsZero() {
					t.Fatalf("got=%#v include=%v err=%v", got, include, err)
				}
			})
		}
	}
}

func TestFreeleechRejectsMissingOrMalformedEndTime(t *testing.T) {
	t.Parallel()
	client := testClient(t, "https://example.test", Config{Timezone: "UTC"})
	for name, field := range map[string]string{
		"missing":         ``,
		"numeric zero":    `,"discountEndTime":0`,
		"string zero":     `,"discountEndTime":"0"`,
		"wrong JSON type": `,"discountEndTime":{}`,
		"bad timestamp":   `,"discountEndTime":"not-a-time"`,
	} {
		t.Run(name, func(t *testing.T) {
			raw := json.RawMessage(`{"id":1,"name":"bad-expiry","size":1,"createdDate":"2099-01-01 00:00:00","status":{"discount":"FREE"` + field + `,"seeders":1,"leechers":1}}`)
			if _, _, err := client.decodeTorrent(raw); err == nil {
				t.Fatal("decodeTorrent succeeded with invalid or missing discountEndTime")
			}
		})
	}
}

func TestSearchRejectsTimedVersusIndefiniteDuplicate(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var query searchRequest
		if err := json.NewDecoder(r.Body).Decode(&query); err != nil {
			t.Fatal(err)
		}
		endTime := `"2099-01-01 00:00:00"`
		if query.PageNumber == 2 {
			endTime = `null`
		}
		io.WriteString(w, `{"code":0,"data":{"data":[{"id":1,"name":"one","size":1,"createdDate":"2099-01-01 00:00:00","status":{"discount":"FREE","discountEndTime":`+endTime+`,"seeders":1,"leechers":1}}]}}`)
	}))
	defer server.Close()

	_, err := testClient(t, server.URL, Config{Pages: 2}).Search(context.Background())
	if err == nil || !strings.Contains(err.Error(), "conflicting duplicate") {
		t.Fatalf("Search error = %v, want conflicting duplicate", err)
	}
}
