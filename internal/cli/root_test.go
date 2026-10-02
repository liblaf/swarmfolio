package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/liblaf/swarmfolio/internal/app"
	"github.com/liblaf/swarmfolio/internal/budget"
	"github.com/liblaf/swarmfolio/internal/metainfo"
)

func TestPlanWithMinimalInitializedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"--config", path, "config", "init"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `api-key = ""`, `api-key = "mteam-secret"`, 1))
	if err := writeFile(path, data, 0o600, true); err != nil {
		t.Fatal(err)
	}

	// Intercept the default URLs so the exact minimal config can exercise the
	// CLI and both real API clients without contacting local or remote services.
	downloadPath := "/downloads/.swarmfolio"
	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	qbtRequests, mteamRequests := 0, 0
	http.DefaultTransport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		switch request.URL.Host {
		case "localhost:8080":
			qbtRequests++
			if request.URL.Scheme != "http" || request.Method != http.MethodGet {
				t.Fatalf("unexpected qBittorrent request: %s %s", request.Method, request.URL)
			}
			if _, ok := request.Header["Authorization"]; ok {
				t.Fatal("minimal config sent qBittorrent authentication")
			}
			switch request.URL.Path {
			case "/api/v2/torrents/info":
				_, _ = io.WriteString(response, `[]`)
			case "/api/v2/torrents/categories":
				if err := json.NewEncoder(response).Encode(map[string]any{
					"swarmfolio": map[string]any{"savePath": downloadPath, "download_path": false},
				}); err != nil {
					t.Fatal(err)
				}
			case "/api/v2/app/defaultSavePath":
				_, _ = io.WriteString(response, "/downloads")
			case "/api/v2/sync/maindata":
				_, _ = io.WriteString(response, `{"server_state":{"free_space_on_disk":2199023255552}}`)
			default:
				t.Fatalf("unexpected qBittorrent path: %s", request.URL.Path)
			}
		case "api.m-team.cc":
			mteamRequests++
			if request.URL.Scheme != "https" || request.Method != http.MethodPost || request.URL.Path != "/api/torrent/search" {
				t.Fatalf("unexpected M-Team request: %s %s", request.Method, request.URL)
			}
			if request.Header.Get("x-api-key") != "mteam-secret" {
				t.Fatal("M-Team API key was not sent")
			}
			_, _ = io.WriteString(response, `{"code":0,"data":{"data":[]}}`)
		default:
			t.Fatalf("unexpected request: %s", request.URL)
		}
		return response.Result(), nil
	})

	stdout.Reset()
	command = New(&stdout, &stderr)
	command.SetArgs([]string{"--config", path, "plan", "--json"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	var report app.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if qbtRequests != 4 || mteamRequests != 2 {
		t.Fatalf("API requests: qBittorrent=%d M-Team=%d", qbtRequests, mteamRequests)
	}
	if report.Mode != "plan" || report.DownloadPath != downloadPath || len(report.Actions) != 0 {
		t.Fatalf("unexpected plan: %#v", report)
	}
	if report.Budget.RequiredFreeBytes != 1<<40 || report.Budget.FreeBytes != 2<<40 || report.Budget.LimitBytes != 1<<40 {
		t.Fatalf("minimal config did not reserve 1 TiB using qBittorrent free space: %#v", report.Budget)
	}
}

func TestApplyReportsAcknowledgedMutationsAfterPostDeleteFailure(t *testing.T) {
	const metainfoBytes = "d4:infod6:lengthi30e4:name3:newee"
	newHash, err := metainfo.InfoHash([]byte(metainfoBytes))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	config := `[portfolio]
minimum_free = "1 B"
budget = "70 B"

[mteam]
api-key = "mteam-secret"
base_url = "https://mteam.test"

[qbittorrent]
base_url = "http://qbt.test"

[policy]
minimum_idle = "1h"
minimum_residency = "1h"
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	transport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = transport })
	added, deleted := false, false
	now := time.Now().Unix()
	createdDate := time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60)).Format("2006-01-02 15:04:05")
	http.DefaultTransport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		body := ""
		status := http.StatusOK
		switch request.URL.Host {
		case "mteam.test":
			switch request.URL.Path {
			case "/api/torrent/search":
				data, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), `"discount":"FREE"`) {
					body = fmt.Sprintf(`{"code":0,"data":{"data":[{"id":2,"name":"new","size":30,"createdDate":%q,"status":{"discount":"FREE","discountEndTime":"2099-01-01 00:00:00","seeders":1,"leechers":8}}]}}`, createdDate)
				} else {
					body = `{"code":0,"data":{"data":[]}}`
				}
			case "/api/torrent/genDlToken":
				body = `{"code":0,"data":"https://download.test/torrent"}`
			default:
				t.Fatalf("unexpected M-Team request: %s", request.URL)
			}
		case "download.test":
			body = metainfoBytes
		case "qbt.test":
			switch request.URL.Path {
			case "/api/v2/torrents/info":
				if deleted {
					status, body = http.StatusInternalServerError, "snapshot failed after delete"
					break
				}
				body = qBittorrentTorrentsJSON(added, newHash, now)
			case "/api/v2/torrents/categories":
				body = `{"swarmfolio":{"savePath":"/downloads/swarmfolio","download_path":false}}`
			case "/api/v2/app/defaultSavePath":
				body = "/downloads"
			case "/api/v2/sync/maindata":
				body = `{"server_state":{"free_space_on_disk":30}}`
			case "/api/v2/app/preferences":
				body = `{"preallocate_all":false}`
			case "/api/v2/torrents/add":
				added = true
			case "/api/v2/torrents/delete":
				deleted = true
			default:
				t.Fatalf("unexpected qBittorrent request: %s %s", request.Method, request.URL)
			}
		default:
			t.Fatalf("unexpected request: %s", request.URL)
		}
		return httpTestResponse(request, status, body), nil
	})

	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"--config", path, "run", "--apply", "--json"})
	err = command.Execute()
	if err == nil || !strings.Contains(err.Error(), "refresh qBittorrent after deleting replacements") {
		t.Fatalf("apply error = %v; output=%s", err, stdout.String())
	}
	var report app.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode partial report: %v; output=%s", err, stdout.String())
	}
	if report.Error == "" || !strings.Contains(report.Error, "refresh qBittorrent after deleting replacements") {
		t.Fatalf("partial report error = %q", report.Error)
	}
	if !containsMutation(report.Mutations, "delete", []string{"old"}, "accepted", true) {
		t.Fatalf("partial report did not acknowledge deletion: %#v", report.Mutations)
	}
	if len(report.Actions) != 1 || report.Actions[0].Applied {
		t.Fatalf("partial action outcome = %#v", report.Actions)
	}
}

func TestPlanReportsInitialQBittorrentConnectionFailureWithoutEmptyPlan(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	serverURL := server.URL
	server.Close()

	path := filepath.Join(t.TempDir(), "config.toml")
	config := `[mteam]
api-key = "mteam-secret"

[qbittorrent]
base_url = "` + serverURL + `"
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"--config", path, "plan"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "list qBittorrent torrents") {
		t.Fatalf("plan error = %v; output=%s", err, stdout.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "Outcome error: list qBittorrent torrents") {
		t.Fatalf("output = %q", output)
	}
	for _, unwanted := range []string{"Mode:", "Download disk:", "Portfolio:", "No changes selected."} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("output must not contain %q: %s", unwanted, output)
		}
	}
}

func TestWriteOutcomePreservesOperationAndWriterErrors(t *testing.T) {
	t.Parallel()
	operationErr := errors.New("operation failed")
	writerErr := errors.New("writer failed")
	err := writeOutcome(errorWriter{err: writerErr}, app.Report{Error: operationErr.Error()}, false, operationErr)
	if !errors.Is(err, operationErr) || !errors.Is(err, writerErr) {
		t.Fatalf("joined error = %v", err)
	}
}

func TestWriteReportShowsDailyDownloadLimitExclusion(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := writeReport(&output, app.Report{
		Mode: "plan",
		SkippedCandidates: []app.SkippedCandidate{{
			CandidateID: "1259628", Reason: "daily torrent download limit reached",
		}},
	})
	if err != nil || !strings.Contains(output.String(), "Skipped M-Team 1259628: daily torrent download limit reached") {
		t.Fatalf("error=%v output=%q", err, output.String())
	}
}

func TestWriteReportDistinguishesPlannedActionsFromMutations(t *testing.T) {
	t.Parallel()
	deleteFiles := true
	keepFiles := false
	var output bytes.Buffer
	err := writeReport(&output, app.Report{
		DownloadPath: "/downloads/swarmfolio",
		Actions:      []app.Action{{CandidateID: "2", Name: "candidate"}},
		Mutations: []app.Mutation{
			{Operation: "delete", Hashes: []string{"old"}, Status: "accepted", DeleteFiles: &deleteFiles},
			{Operation: "delete", Hashes: []string{"pending"}, Status: "accepted", DeleteFiles: &keepFiles},
		},
		Error: "snapshot failed",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Mutation delete: old (accepted; delete files)", "Mutation delete: pending (accepted; keep files)", "Planned addition (not completed): candidate", "Outcome error: snapshot failed"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("report %q does not contain %q", output.String(), want)
		}
	}
}

func TestWriteReportPreservesSnapshotOnPlanningFailure(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := writeReport(&output, app.Report{
		Mode:         "apply",
		DownloadPath: "/downloads/swarmfolio",
		Budget: budget.Result{
			FreeBytes:         2 << 40,
			UsedBytes:         512 << 30,
			RequiredFreeBytes: 1 << 40,
			LimitBytes:        1 << 40,
		},
		ProjectedUsedBytes: 512 << 30,
		Error:              "search M-Team freeleech torrents: unavailable",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Download disk: 2.0 TiB free; reserve 1.0 TiB",
		"Portfolio: 512.0 GiB now; 1.0 TiB limit; 512.0 GiB projected",
		"Outcome error: search M-Team freeleech torrents: unavailable",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("report %q does not contain %q", output.String(), want)
		}
	}
	if strings.Contains(output.String(), "No changes selected.") {
		t.Fatalf("report incorrectly claimed no changes: %q", output.String())
	}
}

func TestWriteReportShowsNoChangesAfterSuccessfulSnapshot(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := writeReport(&output, app.Report{Mode: "plan", DownloadPath: "/downloads/swarmfolio"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "No changes selected.") {
		t.Fatalf("report = %q", output.String())
	}
}

type errorWriter struct{ err error }

func (writer errorWriter) Write([]byte) (int, error) { return 0, writer.err }

func containsMutation(mutations []app.Mutation, operation string, hashes []string, status string, deleteFiles bool) bool {
	for _, mutation := range mutations {
		if mutation.Operation == operation && slices.Equal(mutation.Hashes, hashes) && mutation.Status == status && mutation.DeleteFiles != nil && *mutation.DeleteFiles == deleteFiles {
			return true
		}
	}
	return false
}

func httpTestResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func qBittorrentTorrentsJSON(added bool, newHash string, now int64) string {
	torrents := []map[string]any{{
		"hash": "old", "name": "old", "size": 70, "uploaded": 1, "amount_left": 0, "progress": 1,
		"added_on": now - int64((2 * time.Hour).Seconds()), "last_activity": now - int64((2 * time.Hour).Seconds()),
		"save_path": "/downloads/swarmfolio/old", "content_path": "/downloads/swarmfolio/old/content", "state": "stoppedUP", "category": "swarmfolio", "auto_tmm": true,
	}}
	if added {
		torrents = append(torrents, map[string]any{
			"hash": newHash, "name": "new", "size": 30, "amount_left": 30, "progress": 0,
			"added_on": now, "last_activity": now, "save_path": "/downloads/swarmfolio/new", "content_path": "/downloads/swarmfolio/new/content", "state": "stoppedDL", "category": "swarmfolio", "auto_tmm": true,
		})
	}
	data, err := json.Marshal(torrents)
	if err != nil {
		panic(err)
	}
	return string(data)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestReportShowsFixedReserve(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	writeReport(&output, app.Report{Budget: budget.Result{FreeBytes: 2 << 40, RequiredFreeBytes: 1 << 40}})
	if !strings.Contains(output.String(), "Download disk: 2.0 TiB free; reserve 1.0 TiB\n") {
		t.Fatalf("unexpected reserve display: %s", output.String())
	}
}

func TestReportLabelsCreditedUploadScoresAsHeuristics(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	writeReport(&output, app.Report{
		PlanningHorizon: time.Hour.String(), ReplacementMargin: 1.25, NetGain: 42,
		Actions: []app.Action{{
			CandidateID: "2", Name: "candidate", SizeBytes: 10, Seeders: 1, Leechers: 2,
			UploadMultiplier: 2, UploadScore: 100,
			Removals: []app.Removal{{Hash: "abc", Name: "old", SizeBytes: 10, UploadScore: 50}},
		}},
	})
	got := output.String()
	for _, want := range []string{"Credited-upload heuristic horizon: 1h0m0s; replacement margin 1.25; net credit score 42 bytes", "2x credit, credit score 100 bytes", "credit score 50 bytes"} {
		if !strings.Contains(got, want) {
			t.Fatalf("report %q does not contain %q", got, want)
		}
	}
}

func TestConfigPathAndInitUseXDG(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("XDG_CONFIG_HOME is a Linux convention")
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"config", "path"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "swarmfolio", "config.toml")
	if strings.TrimSpace(stdout.String()) != want {
		t.Fatalf("path output = %q, want %q", stdout.String(), want)
	}
	stdout.Reset()
	command = New(&stdout, &stderr)
	command.SetArgs([]string{"config", "init"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stdout.Reset()
	command = New(&stdout, &stderr)
	command.SetArgs([]string{"config", "init", "--force"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("forced config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestConfigPathUsesUserConfigDirectory(t *testing.T) {
	dir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"config", "path"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "swarmfolio", "config.toml")
	if got := strings.TrimSpace(stdout.String()); got != want {
		t.Fatalf("path output = %q, want %q", got, want)
	}
}

func TestFishCompletion(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"completion", "fish"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "swarmfolio") || !strings.Contains(stdout.String(), "__swarmfolio") {
		t.Fatal("generated output does not look like Fish completion")
	}
}

func TestSystemdPrint(t *testing.T) {
	requireLinux(t)
	t.Parallel()
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "print", "timer"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "OnCalendar=hourly") {
		t.Fatalf("timer output = %q", stdout.String())
	}
}

func TestSystemdPrintServiceHasNoEnvironmentFile(t *testing.T) {
	requireLinux(t)
	t.Parallel()
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "print", "service"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	service := stdout.String()
	if !strings.Contains(service, "ExecStart=") {
		t.Fatalf("service output has no ExecStart: %q", service)
	}
	if strings.Contains(service, "EnvironmentFile=") {
		t.Fatalf("service output unexpectedly contains EnvironmentFile: %q", service)
	}
}

func TestSystemdServiceRetriesFailedRunsWithoutRateLimitLockout(t *testing.T) {
	requireLinux(t)
	t.Parallel()
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "print", "service"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	directives := systemdDirectives(stdout.String())
	if directives["Restart"] != "on-failure" {
		t.Fatalf("Restart = %q; failed runs will not be retried", directives["Restart"])
	}
	delay, err := time.ParseDuration(directives["RestartSec"])
	if err != nil || delay < time.Minute {
		t.Fatalf("RestartSec = %q; retry delay must be at least one minute: %v", directives["RestartSec"], err)
	}
	steps, err := strconv.Atoi(directives["RestartSteps"])
	maxDelay, maxErr := time.ParseDuration(directives["RestartMaxDelaySec"])
	if err != nil || steps < 1 || maxErr != nil || maxDelay <= delay {
		t.Fatalf("RestartSteps = %q, RestartMaxDelaySec = %q; persistent failures must back off", directives["RestartSteps"], directives["RestartMaxDelaySec"])
	}
	if directives["StartLimitIntervalSec"] != "0" {
		t.Fatalf("StartLimitIntervalSec = %q; systemd can permanently stop retries after a start burst", directives["StartLimitIntervalSec"])
	}
}

func systemdDirectives(unit string) map[string]string {
	directives := make(map[string]string)
	for line := range strings.Lines(unit) {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			directives[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return directives
}

func TestSystemdInstallUsesXDGConfigHome(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	log := installSystemctlStub(t)
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "install"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"swarmfolio.service", "swarmfolio.timer"} {
		path := filepath.Join(dir, "systemd", "user", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read installed %s: %v", name, err)
		}
		if !strings.Contains(string(data), "[Unit]") {
			t.Fatalf("installed %s is not a systemd unit: %q", name, data)
		}
	}
	if got, want := readSystemctlLog(t, log), []string{
		"--user daemon-reload",
		"--user enable --now swarmfolio.timer",
	}; !slices.Equal(got, want) {
		t.Fatalf("systemctl calls = %q, want %q", got, want)
	}

	stdout.Reset()
	command = New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "install"})
	if err := command.Execute(); err != nil {
		t.Fatalf("repeated install: %v", err)
	}
	if got, want := readSystemctlLog(t, log), []string{
		"--user daemon-reload",
		"--user enable --now swarmfolio.timer",
		"--user daemon-reload",
		"--user enable --now swarmfolio.timer",
	}; !slices.Equal(got, want) {
		t.Fatalf("repeated systemctl calls = %q, want %q", got, want)
	}
}

func TestSystemdInstallRejectsCustomizedUnitsUnlessForced(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	log := installSystemctlStub(t)
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "install"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	service := filepath.Join(dir, "systemd", "user", "swarmfolio.service")
	if err := os.WriteFile(service, []byte("customized\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command = New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "install"})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("customized install error = %v", err)
	}
	if got := readSystemctlLog(t, log); len(got) != 2 {
		t.Fatalf("customized install ran systemctl: %q", got)
	}
	command = New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "install", "--force"})
	if err := command.Execute(); err != nil {
		t.Fatalf("forced install: %v", err)
	}
	data, err := os.ReadFile(service)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "customized\n" {
		t.Fatal("--force did not replace customized unit")
	}
}

func TestSystemdInstallPropagatesSystemctlFailuresAndCanRetry(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	log := installSystemctlStub(t)
	t.Setenv("SYSTEMCTL_FAIL", "enable")
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "install"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "systemctl --user enable --now swarmfolio.timer") {
		t.Fatalf("enable failure = %v", err)
	}
	if !strings.Contains(stderr.String(), "stub enable failure") {
		t.Fatalf("missing systemctl stderr: %q", stderr.String())
	}
	if got, want := readSystemctlLog(t, log), []string{
		"--user daemon-reload",
		"--user enable --now swarmfolio.timer",
	}; !slices.Equal(got, want) {
		t.Fatalf("failed systemctl calls = %q, want %q", got, want)
	}
	t.Setenv("SYSTEMCTL_FAIL", "")
	stderr.Reset()
	command = New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "install"})
	if err := command.Execute(); err != nil {
		t.Fatalf("retry after enable failure: %v", err)
	}
}

func TestSystemdInstallStopsWhenDaemonReloadFails(t *testing.T) {
	requireLinux(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	log := installSystemctlStub(t)
	t.Setenv("SYSTEMCTL_FAIL", "daemon-reload")
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetArgs([]string{"systemd", "install"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "systemctl --user daemon-reload") {
		t.Fatalf("daemon-reload failure = %v", err)
	}
	if got, want := readSystemctlLog(t, log), []string{"--user daemon-reload"}; !slices.Equal(got, want) {
		t.Fatalf("systemctl calls after daemon-reload failure = %q, want %q", got, want)
	}
}

func TestSystemdInstallHonorsCommandContext(t *testing.T) {
	requireLinux(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	log := installSystemctlStub(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	command := New(&stdout, &stderr)
	command.SetContext(ctx)
	command.SetArgs([]string{"systemd", "install"})
	err := command.Execute()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled install error = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled install invoked systemctl: %v", err)
	}
}

func installSystemctlStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "systemctl.log")
	script := filepath.Join(dir, "systemctl")
	const source = "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$SYSTEMCTL_LOG\"\nif [ \"$2\" = \"$SYSTEMCTL_FAIL\" ] && [ -n \"$SYSTEMCTL_FAIL\" ]; then\n  printf 'stub %s failure\\n' \"$2\" >&2\n  exit 17\nfi\n"
	if err := os.WriteFile(script, []byte(source), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SYSTEMCTL_LOG", log)
	return log
}

func readSystemctlLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func requireLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("systemd commands are available only on Linux")
	}
}
