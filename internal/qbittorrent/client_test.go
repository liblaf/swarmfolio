package qbittorrent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCookieLoginAndTorrents(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got, want := request.Header.Get("Origin"), serverOrigin(request); got != want {
			t.Errorf("Origin = %q, want %q", got, want)
		}
		if got, want := request.Header.Get("Referer"), serverOrigin(request)+"/"; got != want {
			t.Errorf("Referer = %q, want %q", got, want)
		}
		switch request.URL.Path {
		case "/api/v2/auth/login":
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if got := request.Form.Get("username"); got != "user" {
				t.Errorf("username = %q", got)
			}
			if got := request.Form.Get("password"); got != "pass" {
				t.Errorf("password = %q", got)
			}
			http.SetCookie(writer, &http.Cookie{Name: "SID", Value: "session", Path: "/"})
			_, _ = io.WriteString(writer, "Ok.")
		case "/api/v2/torrents/info":
			if cookie, err := request.Cookie("SID"); err != nil || cookie.Value != "session" {
				t.Errorf("missing session cookie: %v", err)
			}
			_, _ = io.WriteString(writer, `[{"hash":"abc","name":"Example","size":42,"uploaded":123,"amount_left":456,"progress":0.5,"ratio":1.2,"seeding_time":61,"added_on":100,"completion_on":200,"last_activity":300,"eta":400,"state":"uploading","dlspeed":2,"upspeed":3,"save_path":"/data","category":"freeleech","auto_tmm":true,"tags":"mteam, freeleech"}]`)
		default:
			t.Errorf("unexpected request %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, "")
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	torrents, err := client.Torrents(context.Background())
	if err != nil {
		t.Fatalf("Torrents: %v", err)
	}
	if len(torrents) != 1 {
		t.Fatalf("torrent count = %d", len(torrents))
	}
	torrent := torrents[0]
	if torrent.SeedingTime != 61*time.Second || torrent.ETA != 400*time.Second {
		t.Errorf("durations = %s, %s", torrent.SeedingTime, torrent.ETA)
	}
	if !torrent.AddedOn.Equal(time.Unix(100, 0)) || !torrent.CompletionOn.Equal(time.Unix(200, 0)) || !torrent.LastActivity.Equal(time.Unix(300, 0)) {
		t.Errorf("timestamps = %#v", torrent)
	}
	if got, want := strings.Join(torrent.Tags, ","), "mteam,freeleech"; got != want {
		t.Errorf("tags = %q, want %q", got, want)
	}
	if torrent.Uploaded != 123 || torrent.AmountLeft != 456 {
		t.Errorf("transfer amounts = uploaded %d, left %d", torrent.Uploaded, torrent.AmountLeft)
	}
	if !torrent.AutoTMM {
		t.Error("AutoTMM = false, want true")
	}
}

func TestDefaultSavePathAndFreeSpace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/app/defaultSavePath":
			_, _ = io.WriteString(writer, " /downloads \n")
		case "/api/v2/sync/maindata":
			if got, want := request.URL.Query().Get("rid"), "0"; got != want {
				t.Errorf("rid = %q, want %q", got, want)
			}
			_, _ = io.WriteString(writer, `{"server_state":{"free_space_on_disk":987654321}}`)
		case "/api/v2/app/preferences":
			_, _ = io.WriteString(writer, `{"preallocate_all":true}`)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, "key")
	path, err := client.DefaultSavePath(context.Background())
	if err != nil {
		t.Fatalf("DefaultSavePath: %v", err)
	}
	if got, want := path, "/downloads"; got != want {
		t.Errorf("save path = %q, want %q", got, want)
	}
	freeSpace, err := client.FreeSpace(context.Background())
	if err != nil {
		t.Fatalf("FreeSpace: %v", err)
	}
	if got, want := freeSpace, int64(987654321); got != want {
		t.Errorf("free space = %d, want %d", got, want)
	}
	preallocate, err := client.PreallocateAll(context.Background())
	if err != nil {
		t.Fatalf("PreallocateAll: %v", err)
	}
	if !preallocate {
		t.Fatal("PreallocateAll = false, want true")
	}
}

func TestCategorySavePath(t *testing.T) {
	for name, response := range map[string]string{
		"success":               `{"freeleech":{"savePath":"/downloads/freeleech","download_path":false}}`,
		"missing category":      `{}`,
		"empty save path":       `{"freeleech":{"savePath":"","download_path":false}}`,
		"missing download path": `{"freeleech":{"savePath":"/downloads/freeleech"}}`,
		"inherited path":        `{"freeleech":{"savePath":"/downloads/freeleech","download_path":null}}`,
		"enabled path":          `{"freeleech":{"savePath":"/downloads/freeleech","download_path":"/incomplete"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if got, want := request.URL.Path, "/api/v2/torrents/categories"; got != want {
					t.Errorf("path = %q, want %q", got, want)
				}
				_, _ = io.WriteString(writer, response)
			}))
			defer server.Close()

			client := newTestClient(t, server.URL, "key")
			path, err := client.CategorySavePath(context.Background(), "freeleech")
			if name == "success" {
				if err != nil {
					t.Fatalf("CategorySavePath: %v", err)
				}
				if got, want := path, "/downloads/freeleech"; got != want {
					t.Errorf("path = %q, want %q", got, want)
				}
				return
			}
			if err == nil {
				t.Fatal("CategorySavePath succeeded for an invalid category")
			}
		})
	}
}

func TestFreeSpaceRejectsMissingAndNegativeValues(t *testing.T) {
	for name, body := range map[string]string{
		"missing":  `{"server_state":{}}`,
		"negative": `{"server_state":{"free_space_on_disk":-1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.WriteString(writer, body)
			}))
			defer server.Close()

			client := newTestClient(t, server.URL, "key")
			if _, err := client.FreeSpace(context.Background()); err == nil {
				t.Fatal("FreeSpace succeeded for invalid server state")
			}
		})
	}
}

func TestPreallocateAllRejectsMissingValue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, `{}`)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, "key")
	if _, err := client.PreallocateAll(context.Background()); err == nil {
		t.Fatal("PreallocateAll succeeded without preallocate_all")
	}
}

func TestMutations(t *testing.T) {
	requests := make(map[string]url.Values)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v2/torrents/add":
			if err := request.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			file, header, err := request.FormFile("torrents")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			contents, err := io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := header.Filename, "example.torrent"; got != want {
				t.Errorf("file name = %q, want %q", got, want)
			}
			if got, want := string(contents), "metainfo"; got != want {
				t.Errorf("contents = %q, want %q", got, want)
			}
			requests["add"] = request.MultipartForm.Value
			writer.WriteHeader(http.StatusAccepted)
		case "/api/v2/torrents/delete", "/api/v2/torrents/addTags", "/api/v2/torrents/removeTags", "/api/v2/torrents/start":
			if err := request.ParseForm(); err != nil {
				t.Fatal(err)
			}
			requests[request.URL.Path] = request.Form
			_, _ = io.WriteString(writer, "Ok.")
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, "key")
	context := context.Background()
	if err := client.Add(context, AddRequest{Metainfo: []byte("metainfo"), MetainfoName: "example.torrent", SavePath: "/data", Category: "freeleech", Tags: []string{"mteam", "freeleech"}, Stopped: true, AutoTMM: true}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := client.Delete(context, []string{"a", "b"}, true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := client.AddTags(context, []string{"a", "b"}, []string{"one", "two"}); err != nil {
		t.Fatalf("AddTags: %v", err)
	}
	if err := client.RemoveTags(context, []string{"a"}, []string{"one"}); err != nil {
		t.Fatalf("RemoveTags: %v", err)
	}
	if err := client.Start(context, []string{"a", "b"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if got, want := requests["add"].Get("tags"), "mteam,freeleech"; got != want {
		t.Errorf("add tags = %q, want %q", got, want)
	}
	for _, key := range []string{"stopped", "paused"} {
		if got := requests["add"].Get(key); got != "true" {
			t.Errorf("add %s = %q, want true", key, got)
		}
	}
	if got := requests["add"].Get("autoTMM"); got != "true" {
		t.Errorf("add autoTMM = %q, want true", got)
	}
	if got, want := requests["/api/v2/torrents/delete"].Get("hashes"), "a|b"; got != want {
		t.Errorf("delete hashes = %q, want %q", got, want)
	}
	if got, want := requests["/api/v2/torrents/delete"].Get("deleteFiles"), "true"; got != want {
		t.Errorf("deleteFiles = %q, want %q", got, want)
	}
	if got, want := requests["/api/v2/torrents/addTags"].Get("tags"), "one,two"; got != want {
		t.Errorf("added tags = %q, want %q", got, want)
	}
}

func TestRejectsFailureResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v2/torrents/start" {
			_, _ = io.WriteString(writer, "Fails.")
			return
		}
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(writer, "denied")
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, "key")
	if err := client.Start(context.Background(), []string{"a"}); err == nil || !strings.Contains(err.Error(), "Fails.") {
		t.Fatalf("Start failure = %v", err)
	}
	if _, err := client.Torrents(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("Torrents failure = %v", err)
	}
}

func TestMutationsRejectBroadOrEmptyTargets(t *testing.T) {
	t.Parallel()
	client := newTestClient(t, "https://example.test", "key")
	for _, hashes := range [][]string{nil, {}, {"all"}, {"safe", "bad|other"}} {
		if err := client.Delete(context.Background(), hashes, true); err == nil {
			t.Fatalf("Delete(%v) succeeded", hashes)
		}
		if err := client.Start(context.Background(), hashes); err == nil {
			t.Fatalf("Start(%v) succeeded", hashes)
		}
	}
}

func TestAPIKeySkipsLoginAndAuthenticates(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if got, want := request.Header.Get("Authorization"), "Bearer key"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		_, _ = io.WriteString(writer, "[]")
	}))
	defer server.Close()

	client := newTestClient(t, server.URL, "key")
	if err := client.Login(context.Background()); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if calls != 0 {
		t.Fatalf("API-key Login made %d requests", calls)
	}
	if _, err := client.Torrents(context.Background()); err != nil {
		t.Fatalf("Torrents: %v", err)
	}
}

func newTestClient(t *testing.T, baseURL, apiKey string) *Client {
	t.Helper()
	client, err := New(Config{BaseURL: baseURL, Username: "user", Password: "pass", APIKey: apiKey})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func serverOrigin(request *http.Request) string {
	return "http://" + request.Host
}
