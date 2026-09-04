package config

import (
	"os"
	"path/filepath"
	"slices"
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

func TestParseRejectsUnknownAndMissingSecrets(t *testing.T) {
	t.Parallel()
	if _, err := Parse([]byte("version = 1\nunknown = true\n"), func(string) string { return "" }); err == nil {
		t.Fatal("Parse() accepted an unknown field")
	}
	if _, err := Parse([]byte(Example), func(string) string { return "" }); err == nil {
		t.Fatal("Parse() accepted missing credentials")
	}
}

func TestParseUsesEnvironmentSecrets(t *testing.T) {
	t.Parallel()
	environment := map[string]string{
		"SWARMFOLIO_MTEAM_API_KEY":        "mteam-secret",
		"SWARMFOLIO_QBITTORRENT_PASSWORD": "qbt-secret",
	}
	settings, err := Parse([]byte(Example), func(key string) string { return environment[key] })
	if err != nil {
		t.Fatal(err)
	}
	if settings.MTeam.APIKey != "mteam-secret" || settings.QBittorrent.Password != "qbt-secret" {
		t.Fatal("Parse() did not resolve environment credentials")
	}
	if settings.Portfolio.BudgetBytes != 0 || settings.Portfolio.MinimumFreePercent != 25 {
		t.Fatalf("unexpected portfolio settings: %#v", settings.Portfolio)
	}
}

func TestParseDefaultsOwnershipTagsAndDiskReserve(t *testing.T) {
	t.Parallel()
	minimal := `
version = 1
[portfolio]
[mteam]
base_url = "https://api.m-team.cc"
mode = "normal"
page_size = 1
pages = 1
timezone = "UTC"
[qbittorrent]
base_url = "http://localhost:8080"
username = "user"
[policy]
candidate_max_age = "1h"
minimum_freeleech_remaining = "0s"
minimum_residency = "0s"
minimum_idle = "0s"
active_upload_rate = "0 B"
max_additions = 1
max_removals = 1
[http]
timeout = "1s"
`
	environment := map[string]string{
		"SWARMFOLIO_MTEAM_API_KEY":        "mteam-secret",
		"SWARMFOLIO_QBITTORRENT_PASSWORD": "qbt-secret",
	}
	settings, err := Parse([]byte(minimal), func(key string) string { return environment[key] })
	if err != nil {
		t.Fatal(err)
	}
	if settings.QBittorrent.ManagedTag != "swarmfolio" || settings.QBittorrent.PendingTag != "swarmfolio-pending" {
		t.Fatalf("ownership tags = %q, %q", settings.QBittorrent.ManagedTag, settings.QBittorrent.PendingTag)
	}
	if settings.QBittorrent.Category != "swarmfolio" {
		t.Fatalf("category = %q, want %q", settings.QBittorrent.Category, "swarmfolio")
	}
	if settings.Portfolio.MinimumFreePercent != 25 {
		t.Fatalf("minimum free percent = %v", settings.Portfolio.MinimumFreePercent)
	}
	if !slices.Equal(settings.QBittorrent.ProtectedTags, []string{"keep", "archive"}) {
		t.Fatalf("protected tags = %v", settings.QBittorrent.ProtectedTags)
	}
}

func TestParseRejectsInvalidCategory(t *testing.T) {
	t.Parallel()
	for _, category := range []string{"", " swarmfolio", "swarmfolio ", "swarmfolio\nother"} {
		category := category
		t.Run(category, func(t *testing.T) {
			t.Parallel()
			config := strings.Replace(Example, `category = "swarmfolio"`, `category = "`+category+`"`, 1)
			_, err := Parse([]byte(config), func(key string) string {
				return map[string]string{
					"SWARMFOLIO_MTEAM_API_KEY":        "mteam-secret",
					"SWARMFOLIO_QBITTORRENT_PASSWORD": "qbt-secret",
				}[key]
			})
			if err == nil {
				t.Fatal("Parse() accepted an invalid category")
			}
		})
	}
}
