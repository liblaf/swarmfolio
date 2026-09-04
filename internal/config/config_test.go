package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPathUsesXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "swarmfolio", "config.toml")
	if path != want {
		t.Fatalf("DefaultPath() = %q, want %q", path, want)
	}
}

func TestLoadRequiresPrivateRegularConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.toml")
	if err := os.WriteFile(valid, []byte(strings.ReplaceAll(strings.ReplaceAll(Example, `api_key = ""`, `api_key = "mteam-secret"`), `password = ""`, `password = "qbt-secret"`)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(valid); err != nil {
		t.Fatalf("Load(private config): %v", err)
	}
	public := filepath.Join(dir, "public.toml")
	if err := os.WriteFile(public, []byte(Example), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(public, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(public); err == nil || !strings.Contains(err.Error(), "must not be accessible") {
		t.Fatalf("Load(public config) error = %v", err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("Load(directory) error = %v", err)
	}
}

func TestParseRejectsExplicitEmptyOptionalString(t *testing.T) {
	t.Parallel()
	base := `
[mteam]
api_key = "mteam-secret"
[qbittorrent]
base_url = "http://localhost:8080"
username = "user"
password = "qbt-secret"
`
	for _, setting := range []string{
		"[mteam]\nmode = \"\"\n",
		"[mteam]\ntimezone = \"\"\n",
		"[policy]\ncandidate_max_age = \"\"\n",
		"[policy]\nactive_upload_rate = \"\"\n",
		"[http]\ntimeout = \"\"\n",
	} {
		setting := setting
		t.Run(setting, func(t *testing.T) {
			t.Parallel()
			data := strings.Replace(base, "[mteam]\n", setting, 1)
			if strings.HasPrefix(setting, "[policy]") || strings.HasPrefix(setting, "[http]") {
				data = base + setting
			}
			if _, err := Parse([]byte(data)); err == nil {
				t.Fatalf("Parse() accepted explicit empty setting %q", setting)
			}
		})
	}
}

func TestParseBytes(t *testing.T) {
	t.Parallel()
	tests := map[string]int64{
		"1 B":    1,
		"1.5 KB": 1500,
		"2 MiB":  2 * 1024 * 1024,
		"4 TiB":  4 * 1024 * 1024 * 1024 * 1024,
	}
	for input, want := range tests {
		input, want := input, want
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, err := ParseBytes(input)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("ParseBytes(%q) = %d, want %d", input, got, want)
			}
		})
	}
}

func TestParseRejectsUnknownAndMissingCredentials(t *testing.T) {
	t.Parallel()
	if _, err := Parse([]byte("unknown = true\n")); err == nil {
		t.Fatal("Parse() accepted an unknown field")
	}
	if _, err := Parse([]byte(Example)); err == nil {
		t.Fatal("Parse() accepted missing credentials")
	}
}

func TestEnvironmentCannotReplaceConfigCredentials(t *testing.T) {
	t.Setenv("SWARMFOLIO_MTEAM_API_KEY", "legacy-mteam-secret")
	t.Setenv("SWARMFOLIO_QBITTORRENT_PASSWORD", "legacy-qbt-secret")
	if _, err := Parse([]byte(Example)); err == nil {
		t.Fatal("Parse() accepted credentials from the environment")
	}
}

func TestParseRequiresCredentialsInTOML(t *testing.T) {
	t.Parallel()
	minimal := strings.ReplaceAll(Example, `api_key = ""`, `api_key = "mteam-secret"`)
	minimal = strings.ReplaceAll(minimal, `password = ""`, `password = "qbt-secret"`)
	settings, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if settings.MTeam.APIKey != "mteam-secret" || settings.QBittorrent.Password != "qbt-secret" {
		t.Fatal("Parse() did not retain TOML credentials")
	}
	if strings.Contains(Example, "version =") || strings.Contains(Example, "[policy]") || strings.Contains(Example, "[portfolio]") || strings.Contains(Example, "[http]") {
		t.Fatalf("Example contains optional settings:\n%s", Example)
	}
	var assignments []string
	for _, line := range strings.Split(Example, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "=") && !strings.HasPrefix(line, "#") {
			assignments = append(assignments, line)
		}
	}
	want := []string{`api_key = ""`, `base_url = "http://127.0.0.1:8080"`, `username = "admin"`, `password = ""`}
	if strings.Join(assignments, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Example assignments = %q, want exactly %q", assignments, want)
	}
}

func TestParseAppliesDocumentedDefaults(t *testing.T) {
	t.Parallel()
	minimal := `
[mteam]
api_key = "mteam-secret"
[qbittorrent]
base_url = "http://localhost:8080"
username = "user"
password = "qbt-secret"
`
	settings, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if settings.QBittorrent.Category != "swarmfolio" {
		t.Fatalf("category = %q, want %q", settings.QBittorrent.Category, "swarmfolio")
	}
	if settings.Portfolio.MinimumFreePercent != 25 || settings.MTeam.BaseURL != "https://api.m-team.cc" ||
		settings.MTeam.Mode != "normal" || settings.MTeam.PageSize != 100 || settings.MTeam.Pages != 1 ||
		settings.MTeam.Location.String() != "Asia/Shanghai" || settings.HTTPTimeout.String() != "30s" {
		t.Fatalf("unexpected defaults: %#v", settings)
	}
	if settings.Policy.CandidateMaxAge.String() != "72h0m0s" || settings.Policy.MinimumFreeleechRemaining.String() != "2h0m0s" ||
		settings.Policy.MinimumLeechers != 1 || settings.Policy.MinimumOpportunityRatio != 0.1 ||
		settings.Policy.MinimumResidency.String() != "24h0m0s" || settings.Policy.MinimumIdle.String() != "6h0m0s" ||
		settings.Policy.ActiveUploadRate != 64*1024 || settings.Policy.MaxAdditions != 2 || settings.Policy.MaxRemovals != 4 {
		t.Fatalf("unexpected policy defaults: %#v", settings.Policy)
	}
}

func TestParseVersionIsOptionalButValidatedWhenPresent(t *testing.T) {
	t.Parallel()
	config := `
[mteam]
api_key = "mteam-secret"
[qbittorrent]
base_url = "http://localhost:8080"
username = "user"
password = "qbt-secret"
`
	if _, err := Parse([]byte(config)); err != nil {
		t.Fatalf("Parse() without version: %v", err)
	}
	if _, err := Parse([]byte("version = 0\n" + config)); err == nil {
		t.Fatal("Parse() accepted an explicit zero version")
	}
	if _, err := Parse([]byte("version = 2\n" + config)); err == nil {
		t.Fatal("Parse() accepted an unsupported version")
	}
}

func TestParseRejectsInvalidCategory(t *testing.T) {
	t.Parallel()
	for _, category := range []string{"", " swarmfolio", "swarmfolio ", "swarmfolio\nother"} {
		category := category
		t.Run(category, func(t *testing.T) {
			t.Parallel()
			config := `
[mteam]
api_key = "mteam-secret"
[qbittorrent]
base_url = "http://localhost:8080"
username = "user"
password = "qbt-secret"
category = "` + category + `"
`
			_, err := Parse([]byte(config))
			if err == nil {
				t.Fatal("Parse() accepted an invalid category")
			}
		})
	}
}
