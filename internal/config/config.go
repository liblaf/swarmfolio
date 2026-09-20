package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

const Version = 1

const Example = `# Swarmfolio is stateless. qBittorrent is its only persistent state.
# Optional settings use the documented defaults; see README for overrides.

[mteam]
api-key = ""
`

type fileConfig struct {
	Version     *int            `toml:"version"`
	Portfolio   filePortfolio   `toml:"portfolio"`
	MTeam       fileMTeam       `toml:"mteam"`
	QBittorrent fileQBittorrent `toml:"qbittorrent"`
	Policy      filePolicy      `toml:"policy"`
	HTTP        fileHTTP        `toml:"http"`
}

type filePortfolio struct {
	Budget           string  `toml:"budget"`
	MinimumFree      *string `toml:"minimum_free"`
	MinimumFreeKebab *string `toml:"minimum-free"`
	DiskPath         string  `toml:"disk_path"`
}

type fileMTeam struct {
	BaseURL     *string `toml:"base_url"`
	APIKey      *string `toml:"api_key"`
	APIKeyKebab *string `toml:"api-key"`
	Mode        *string `toml:"mode"`
	PageSize    *int    `toml:"page_size"`
	Pages       *int    `toml:"pages"`
	Timezone    *string `toml:"timezone"`
}

type fileQBittorrent struct {
	BaseURL     *string `toml:"base_url"`
	APIKey      *string `toml:"api_key"`
	APIKeyKebab *string `toml:"api-key"`
	Category    *string `toml:"category"`
}

type filePolicy struct {
	CandidateMaxAge           *string  `toml:"candidate_max_age"`
	PlanningHorizon           *string  `toml:"planning_horizon"`
	MinimumFreeleechRemaining *string  `toml:"minimum_freeleech_remaining"`
	MinimumLeechers           *int     `toml:"minimum_leechers"`
	MinimumOpportunityRatio   *float64 `toml:"minimum_opportunity_ratio"`
	MinimumResidency          *string  `toml:"minimum_residency"`
	MinimumIdle               *string  `toml:"minimum_idle"`
	ActiveUploadRate          *string  `toml:"active_upload_rate"`
	MaxAdditions              *int     `toml:"max_additions"`
	MaxRemovals               *int     `toml:"max_removals"`
	ReplacementMargin         *float64 `toml:"replacement_margin"`
}

type fileHTTP struct {
	Timeout *string `toml:"timeout"`
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
	BudgetBytes      int64
	MinimumFreeBytes int64
	DiskPath         string
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
	BaseURL  string
	APIKey   string
	Category string
}

type Policy struct {
	CandidateMaxAge           time.Duration
	PlanningHorizon           time.Duration
	MinimumFreeleechRemaining time.Duration
	MinimumLeechers           int
	MinimumOpportunityRatio   float64
	MinimumResidency          time.Duration
	MinimumIdle               time.Duration
	ActiveUploadRate          int64
	MaxAdditions              int
	MaxRemovals               int
	ReplacementMargin         float64
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
	file, err := os.Open(path)
	if err != nil {
		return Settings{}, fmt.Errorf("read config %q: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Settings{}, fmt.Errorf("inspect config %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return Settings{}, fmt.Errorf("config %q must be a regular file", path)
	}
	if err := checkConfigPermissions(file, info); err != nil {
		return Settings{}, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return Settings{}, fmt.Errorf("read config %q: %w", path, err)
	}
	settings, err := Parse(data)
	if err != nil {
		return Settings{}, fmt.Errorf("parse config %q: %w", path, err)
	}
	settings.Path = path
	return settings, nil
}

func Parse(data []byte) (Settings, error) {
	var raw fileConfig
	decoder := toml.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil {
		return Settings{}, err
	}
	if raw.Version != nil && *raw.Version != Version {
		return Settings{}, fmt.Errorf("version must be %d, got %d", Version, *raw.Version)
	}
	mteamAPIKey, err := compatibilityAlias("mteam.api_key", raw.MTeam.APIKey, "mteam.api-key", raw.MTeam.APIKeyKebab)
	if err != nil {
		return Settings{}, err
	}
	qbittorrentAPIKey, err := compatibilityAlias("qbittorrent.api_key", raw.QBittorrent.APIKey, "qbittorrent.api-key", raw.QBittorrent.APIKeyKebab)
	if err != nil {
		return Settings{}, err
	}

	budget, err := optionalBytes("portfolio.budget", raw.Portfolio.Budget)
	if err != nil {
		return Settings{}, err
	}
	minimumFree, err := compatibilityAlias("portfolio.minimum_free", raw.Portfolio.MinimumFree, "portfolio.minimum-free", raw.Portfolio.MinimumFreeKebab)
	if err != nil {
		return Settings{}, err
	}
	if raw.Portfolio.MinimumFree == nil && raw.Portfolio.MinimumFreeKebab == nil {
		minimumFree = "1 TiB"
	}
	minimumFreeBytes, err := optionalBytes("portfolio.minimum_free", minimumFree)
	if err != nil {
		return Settings{}, err
	}
	if minimumFreeBytes == 0 {
		return Settings{}, errors.New("portfolio.minimum_free must be positive")
	}
	candidateMaxAge, err := positiveDuration("policy.candidate_max_age", defaultString(raw.Policy.CandidateMaxAge, "72h"))
	if err != nil {
		return Settings{}, err
	}
	planningHorizon, err := positiveDuration("policy.planning_horizon", defaultString(raw.Policy.PlanningHorizon, "24h"))
	if err != nil {
		return Settings{}, err
	}
	freeRemaining, err := nonnegativeDuration("policy.minimum_freeleech_remaining", defaultString(raw.Policy.MinimumFreeleechRemaining, "2h"))
	if err != nil {
		return Settings{}, err
	}
	residency, err := nonnegativeDuration("policy.minimum_residency", defaultString(raw.Policy.MinimumResidency, "24h"))
	if err != nil {
		return Settings{}, err
	}
	idle, err := nonnegativeDuration("policy.minimum_idle", defaultString(raw.Policy.MinimumIdle, "6h"))
	if err != nil {
		return Settings{}, err
	}
	uploadRate, err := ParseBytes(defaultString(raw.Policy.ActiveUploadRate, "64 KiB"))
	if err != nil {
		return Settings{}, fmt.Errorf("policy.active_upload_rate: %w", err)
	}
	timeout, err := positiveDuration("http.timeout", defaultString(raw.HTTP.Timeout, "30s"))
	if err != nil {
		return Settings{}, err
	}
	zone := defaultString(raw.MTeam.Timezone, "Asia/Shanghai")
	if zone == "" {
		return Settings{}, errors.New("mteam.timezone is required when set")
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return Settings{}, fmt.Errorf("mteam.timezone: %w", err)
	}

	category := "swarmfolio"
	if raw.QBittorrent.Category != nil {
		category = *raw.QBittorrent.Category
	}
	settings := Settings{
		Portfolio: Portfolio{
			BudgetBytes: budget, MinimumFreeBytes: minimumFreeBytes,
			DiskPath: raw.Portfolio.DiskPath,
		},
		MTeam: MTeam{
			BaseURL: strings.TrimRight(defaultString(raw.MTeam.BaseURL, "https://api.m-team.cc"), "/"), APIKey: mteamAPIKey,
			Mode: defaultString(raw.MTeam.Mode, "normal"), PageSize: defaultInt(raw.MTeam.PageSize, 100), Pages: defaultInt(raw.MTeam.Pages, 1),
			Location: location,
		},
		QBittorrent: QBittorrent{
			BaseURL:  strings.TrimRight(defaultString(raw.QBittorrent.BaseURL, "http://localhost:8080"), "/"),
			APIKey:   qbittorrentAPIKey,
			Category: category,
		},
		Policy: Policy{
			CandidateMaxAge: candidateMaxAge, PlanningHorizon: planningHorizon, MinimumFreeleechRemaining: freeRemaining,
			MinimumLeechers:         defaultInt(raw.Policy.MinimumLeechers, 1),
			MinimumOpportunityRatio: defaultFloat64(raw.Policy.MinimumOpportunityRatio, 0.1),
			MinimumResidency:        residency, MinimumIdle: idle, ActiveUploadRate: uploadRate,
			MaxAdditions: defaultInt(raw.Policy.MaxAdditions, 2), MaxRemovals: defaultInt(raw.Policy.MaxRemovals, 4),
			ReplacementMargin: defaultFloat64(raw.Policy.ReplacementMargin, 1.25),
		},
		HTTPTimeout: timeout,
	}
	if err := settings.validate(); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

func (settings Settings) validate() error {
	if settings.Portfolio.DiskPath != "" && !filepath.IsAbs(settings.Portfolio.DiskPath) {
		return errors.New("portfolio.disk_path must be absolute")
	}
	if err := validateURL("mteam.base_url", settings.MTeam.BaseURL); err != nil {
		return err
	}
	if settings.MTeam.APIKey == "" {
		return errors.New("mteam.api_key is required")
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
	if settings.QBittorrent.Category == "" || strings.TrimSpace(settings.QBittorrent.Category) != settings.QBittorrent.Category ||
		strings.ContainsAny(settings.QBittorrent.Category, "\r\n\x00") {
		return errors.New("qbittorrent.category must be nonempty, trimmed, and contain no line breaks")
	}
	if settings.Policy.MinimumLeechers < 0 || settings.Policy.MinimumOpportunityRatio < 0 ||
		math.IsNaN(settings.Policy.MinimumOpportunityRatio) || math.IsInf(settings.Policy.MinimumOpportunityRatio, 0) {
		return errors.New("policy candidate thresholds cannot be negative")
	}
	if settings.Policy.MaxAdditions < 1 || settings.Policy.MaxRemovals < 1 {
		return errors.New("policy action limits must be positive")
	}
	if settings.Policy.ReplacementMargin < 1 || math.IsNaN(settings.Policy.ReplacementMargin) || math.IsInf(settings.Policy.ReplacementMargin, 0) {
		return errors.New("policy.replacement_margin must be finite and at least 1")
	}
	return nil
}

func compatibilityAlias(firstName string, first *string, secondName string, second *string) (string, error) {
	if first != nil && second != nil {
		return "", fmt.Errorf("%s and %s are mutually exclusive", firstName, secondName)
	}
	if first != nil {
		return *first, nil
	}
	if second != nil {
		return *second, nil
	}
	return "", nil
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

func defaultString(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

func defaultInt(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func defaultFloat64(value *float64, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	return *value
}
