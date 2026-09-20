package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/liblaf/swarmfolio/internal/app"
	"github.com/liblaf/swarmfolio/internal/budget"
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
