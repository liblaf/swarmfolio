package config

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const Version = 1

const Example = `# Swarmfolio is stateless. qBittorrent tags are its only persistent state.
version = 1

[portfolio]
# Leave budget empty to derive it from disk capacity. Set it to a byte size to
# impose an additional hard ceiling.
budget = ""
minimum_free_percent = 25
# For a local qBittorrent, leave both empty and its default save path is probed.
# For a container, set disk_path to the host-visible path. For a remote server,
# set disk_capacity and qBittorrent's reported free space is used.
disk_path = ""
disk_capacity = ""

[mteam]
base_url = "https://api.m-team.cc"
api_key = ""
mode = "normal"
page_size = 100
pages = 1
timezone = "Asia/Shanghai"

[qbittorrent]
base_url = "http://127.0.0.1:8080"
username = "admin"
password = ""
api_key = ""
managed_tag = "swarmfolio"
pending_tag = "swarmfolio-pending"
protected_tags = ["keep", "archive"]
category = ""
save_path = ""

[policy]
candidate_max_age = "72h"
minimum_freeleech_remaining = "2h"
minimum_leechers = 1
minimum_opportunity_ratio = 0.1
minimum_residency = "24h"
minimum_idle = "6h"
active_upload_rate = "64 KiB"
max_additions = 2
max_removals = 4

[http]
timeout = "30s"
`

type fileConfig struct {
	Version     int             `toml:"version"`
	Portfolio   filePortfolio   `toml:"portfolio"`
	MTeam       fileMTeam       `toml:"mteam"`
	QBittorrent fileQBittorrent `toml:"qbittorrent"`
	Policy      filePolicy      `toml:"policy"`
	HTTP        fileHTTP        `toml:"http"`
}

type filePortfolio struct {
	Budget             string   `toml:"budget"`
	MinimumFreePercent *float64 `toml:"minimum_free_percent"`
	DiskPath           string   `toml:"disk_path"`
	DiskCapacity       string   `toml:"disk_capacity"`
}

type fileMTeam struct {
	BaseURL  string `toml:"base_url"`
	APIKey   string `toml:"api_key"`
	Mode     string `toml:"mode"`
	PageSize int    `toml:"page_size"`
	Pages    int    `toml:"pages"`
	Timezone string `toml:"timezone"`
}

type fileQBittorrent struct {
	BaseURL       string   `toml:"base_url"`
	Username      string   `toml:"username"`
	Password      string   `toml:"password"`
	APIKey        string   `toml:"api_key"`
	ManagedTag    string   `toml:"managed_tag"`
	PendingTag    string   `toml:"pending_tag"`
	ProtectedTags []string `toml:"protected_tags"`
	Category      string   `toml:"category"`
	SavePath      string   `toml:"save_path"`
}

type filePolicy struct {
	CandidateMaxAge           string  `toml:"candidate_max_age"`
	MinimumFreeleechRemaining string  `toml:"minimum_freeleech_remaining"`
	MinimumLeechers           int     `toml:"minimum_leechers"`
	MinimumOpportunityRatio   float64 `toml:"minimum_opportunity_ratio"`
	MinimumResidency          string  `toml:"minimum_residency"`
	MinimumIdle               string  `toml:"minimum_idle"`
	ActiveUploadRate          string  `toml:"active_upload_rate"`
	MaxAdditions              int     `toml:"max_additions"`
	MaxRemovals               int     `toml:"max_removals"`
}

type fileHTTP struct {
	Timeout string `toml:"timeout"`
}

type Settings struct {
	Path        string
	Portfolio   Portfolio
	MTeam       MTeam
	QBittorrent QBittorrent
	Policy      Policy
	HTTPTimeout time.Duration
}

type Portfolio struct {
	BudgetBytes        int64
	MinimumFreePercent float64
	DiskPath           string
	DiskCapacityBytes  int64
}

type MTeam struct {
	BaseURL  string
	APIKey   string
	Mode     string
	PageSize int
	Pages    int
	Location *time.Location
}

type QBittorrent struct {
	BaseURL       string
	Username      string
	Password      string
	APIKey        string
	ManagedTag    string
	PendingTag    string
	ProtectedTags []string
	Category      string
	SavePath      string
}

type Policy struct {
	CandidateMaxAge           time.Duration
	MinimumFreeleechRemaining time.Duration
	MinimumLeechers           int
	MinimumOpportunityRatio   float64
	MinimumResidency          time.Duration
	MinimumIdle               time.Duration
	ActiveUploadRate          int64
	MaxAdditions              int
	MaxRemovals               int
}

func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve XDG config directory: %w", err)
	}
	return filepath.Join(dir, "swarmfolio", "config.toml"), nil
}

func Load(path string) (Settings, error) {
	if path == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return Settings{}, err
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Settings{}, fmt.Errorf("read config %q: %w", path, err)
	}
	settings, err := Parse(data, os.Getenv)
	if err != nil {
		return Settings{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	settings.Path = path
	return settings, nil
}

func Parse(data []byte, getenv func(string) string) (Settings, error) {
	var raw fileConfig
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return Settings{}, err
	}
	if raw.Version != Version {
		return Settings{}, fmt.Errorf("version must be %d, got %d", Version, raw.Version)
	}

	budget, err := optionalBytes("portfolio.budget", raw.Portfolio.Budget)
	if err != nil {
		return Settings{}, err
	}
	diskCapacity, err := optionalBytes("portfolio.disk_capacity", raw.Portfolio.DiskCapacity)
	if err != nil {
		return Settings{}, err
	}
	minimumFreePercent := 25.0
	if raw.Portfolio.MinimumFreePercent != nil {
		minimumFreePercent = *raw.Portfolio.MinimumFreePercent
	}
	candidateMaxAge, err := positiveDuration("policy.candidate_max_age", raw.Policy.CandidateMaxAge)
	if err != nil {
		return Settings{}, err
	}
	freeRemaining, err := nonnegativeDuration("policy.minimum_freeleech_remaining", raw.Policy.MinimumFreeleechRemaining)
	if err != nil {
		return Settings{}, err
	}
	residency, err := nonnegativeDuration("policy.minimum_residency", raw.Policy.MinimumResidency)
	if err != nil {
		return Settings{}, err
	}
	idle, err := nonnegativeDuration("policy.minimum_idle", raw.Policy.MinimumIdle)
	if err != nil {
		return Settings{}, err
	}
	uploadRate, err := ParseBytes(raw.Policy.ActiveUploadRate)
	if err != nil {
		return Settings{}, fmt.Errorf("policy.active_upload_rate: %w", err)
	}
	timeout, err := positiveDuration("http.timeout", raw.HTTP.Timeout)
	if err != nil {
		return Settings{}, err
	}
	location, err := time.LoadLocation(raw.MTeam.Timezone)
	if err != nil {
		return Settings{}, fmt.Errorf("mteam.timezone: %w", err)
	}

	mteamKey := firstNonempty(getenv("SWARMFOLIO_MTEAM_API_KEY"), raw.MTeam.APIKey)
	qbtPassword := firstNonempty(getenv("SWARMFOLIO_QBITTORRENT_PASSWORD"), raw.QBittorrent.Password)
	qbtAPIKey := firstNonempty(getenv("SWARMFOLIO_QBITTORRENT_API_KEY"), raw.QBittorrent.APIKey)
	managedTag := firstNonempty(raw.QBittorrent.ManagedTag, "swarmfolio")
	pendingTag := firstNonempty(raw.QBittorrent.PendingTag, managedTag+"-pending")
	protectedTags := raw.QBittorrent.ProtectedTags
	if protectedTags == nil {
		protectedTags = []string{"keep", "archive"}
	}
	settings := Settings{
		Portfolio: Portfolio{
			BudgetBytes: budget, MinimumFreePercent: minimumFreePercent,
			DiskPath: raw.Portfolio.DiskPath, DiskCapacityBytes: diskCapacity,
		},
		MTeam: MTeam{
			BaseURL: strings.TrimRight(raw.MTeam.BaseURL, "/"), APIKey: mteamKey,
			Mode: raw.MTeam.Mode, PageSize: raw.MTeam.PageSize, Pages: raw.MTeam.Pages,
			Location: location,
		},
		QBittorrent: QBittorrent{
			BaseURL:  strings.TrimRight(raw.QBittorrent.BaseURL, "/"),
			Username: raw.QBittorrent.Username, Password: qbtPassword, APIKey: qbtAPIKey,
			ManagedTag: managedTag, PendingTag: pendingTag,
			ProtectedTags: slices.Clone(protectedTags),
			Category:      raw.QBittorrent.Category, SavePath: raw.QBittorrent.SavePath,
		},
		Policy: Policy{
			CandidateMaxAge: candidateMaxAge, MinimumFreeleechRemaining: freeRemaining,
			MinimumLeechers:         raw.Policy.MinimumLeechers,
			MinimumOpportunityRatio: raw.Policy.MinimumOpportunityRatio,
			MinimumResidency:        residency, MinimumIdle: idle, ActiveUploadRate: uploadRate,
			MaxAdditions: raw.Policy.MaxAdditions, MaxRemovals: raw.Policy.MaxRemovals,
		},
		HTTPTimeout: timeout,
	}
	if err := settings.validate(); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

func (settings Settings) validate() error {
	if settings.Portfolio.MinimumFreePercent < 0 || settings.Portfolio.MinimumFreePercent >= 100 ||
		math.IsNaN(settings.Portfolio.MinimumFreePercent) || math.IsInf(settings.Portfolio.MinimumFreePercent, 0) {
		return errors.New("portfolio.minimum_free_percent must be between 0 and 100")
	}
	if settings.Portfolio.DiskPath != "" && settings.Portfolio.DiskCapacityBytes != 0 {
		return errors.New("portfolio.disk_path and portfolio.disk_capacity are mutually exclusive")
	}
	if settings.Portfolio.DiskPath != "" && !filepath.IsAbs(settings.Portfolio.DiskPath) {
		return errors.New("portfolio.disk_path must be absolute")
	}
	if err := validateURL("mteam.base_url", settings.MTeam.BaseURL); err != nil {
		return err
	}
	if settings.MTeam.APIKey == "" {
		return errors.New("mteam.api_key or SWARMFOLIO_MTEAM_API_KEY is required")
	}
	if settings.MTeam.Mode == "" {
		return errors.New("mteam.mode is required")
	}
	if settings.MTeam.PageSize < 1 || settings.MTeam.PageSize > 200 {
		return errors.New("mteam.page_size must be between 1 and 200")
	}
	if settings.MTeam.Pages < 1 {
		return errors.New("mteam.pages must be positive")
	}
	if err := validateURL("qbittorrent.base_url", settings.QBittorrent.BaseURL); err != nil {
		return err
	}
	if settings.QBittorrent.APIKey == "" && (settings.QBittorrent.Username == "" || settings.QBittorrent.Password == "") {
		return errors.New("qbittorrent API key or username and password are required")
	}
	if settings.QBittorrent.ManagedTag == "" || settings.QBittorrent.PendingTag == "" {
		return errors.New("qbittorrent managed_tag and pending_tag are required")
	}
	if settings.QBittorrent.SavePath != "" && !filepath.IsAbs(settings.QBittorrent.SavePath) {
		return errors.New("qbittorrent.save_path must be absolute")
	}
	for name, tag := range map[string]string{
		"qbittorrent.managed_tag": settings.QBittorrent.ManagedTag,
		"qbittorrent.pending_tag": settings.QBittorrent.PendingTag,
	} {
		if strings.TrimSpace(tag) != tag || strings.Contains(tag, ",") {
			return fmt.Errorf("%s must not contain surrounding whitespace or commas", name)
		}
	}
	if settings.QBittorrent.ManagedTag == settings.QBittorrent.PendingTag {
		return errors.New("qbittorrent managed_tag and pending_tag must differ")
	}
	idTagPrefix := settings.QBittorrent.ManagedTag + "-id-"
	if strings.HasPrefix(settings.QBittorrent.PendingTag, idTagPrefix) {
		return errors.New("qbittorrent.pending_tag conflicts with reserved candidate ID tags")
	}
	seenProtected := make(map[string]bool, len(settings.QBittorrent.ProtectedTags))
	for _, tag := range settings.QBittorrent.ProtectedTags {
		if tag == "" || strings.TrimSpace(tag) != tag || strings.Contains(tag, ",") || strings.HasPrefix(tag, idTagPrefix) {
			return fmt.Errorf("invalid qbittorrent protected tag %q", tag)
		}
		if seenProtected[tag] {
			return fmt.Errorf("duplicate qbittorrent protected tag %q", tag)
		}
		seenProtected[tag] = true
	}
	if slices.Contains(settings.QBittorrent.ProtectedTags, settings.QBittorrent.ManagedTag) ||
		slices.Contains(settings.QBittorrent.ProtectedTags, settings.QBittorrent.PendingTag) {
		return errors.New("protected_tags cannot contain the managed or pending tag")
	}
	if settings.Policy.MinimumLeechers < 0 || settings.Policy.MinimumOpportunityRatio < 0 ||
		math.IsNaN(settings.Policy.MinimumOpportunityRatio) || math.IsInf(settings.Policy.MinimumOpportunityRatio, 0) {
		return errors.New("policy candidate thresholds cannot be negative")
	}
	if settings.Policy.MaxAdditions < 1 || settings.Policy.MaxRemovals < 1 {
		return errors.New("policy action limits must be positive")
	}
	return nil
}

func validateURL(name, value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must be an absolute HTTP(S) URL without query or fragment", name)
	}
	return nil
}

var bytesPattern = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)\s*(b|kb|mb|gb|tb|kib|mib|gib|tib)$`)

func ParseBytes(value string) (int64, error) {
	match := bytesPattern.FindStringSubmatch(strings.TrimSpace(value))
	if match == nil {
		return 0, fmt.Errorf("invalid byte size %q", value)
	}
	number, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q: %w", value, err)
	}
	multipliers := map[string]float64{
		"b": 1, "kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12,
		"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
	}
	bytes := number * multipliers[strings.ToLower(match[2])]
	if math.IsInf(bytes, 0) || bytes > math.MaxInt64 {
		return 0, fmt.Errorf("byte size %q overflows int64", value)
	}
	return int64(math.Round(bytes)), nil
}

func optionalBytes(name, value string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	parsed, err := ParseBytes(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive when set", name)
	}
	return parsed, nil
}

func positiveDuration(name, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return duration, nil
}

func nonnegativeDuration(name, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	if duration < 0 {
		return 0, fmt.Errorf("%s cannot be negative", name)
	}
	return duration, nil
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
