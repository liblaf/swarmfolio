// Package optimizer builds deterministic, side-effect-free torrent portfolio plans.
package optimizer

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// Candidate is a freeleech offer accepted by the configured M-Team adapter.
// A zero FreeUntil means the offer has no scheduled end. A nonzero FreeUntil
// must still be in the future and meet MinFreeleechRemaining to be eligible.
type Candidate struct {
	ID               string
	Name             string
	Size             int64
	Seeders          int
	Leechers         int
	PublishedAt      time.Time
	FreeUntil        time.Time
	UploadMultiplier int
}

// Torrent is qBittorrent's current view of a torrent.
type Torrent struct {
	Hash         string
	Name         string
	Size         int64
	Uploaded     int64
	UploadRate   int64
	Progress     float64
	State        string
	AddedAt      time.Time
	LastActivity time.Time
	Category     string
	AutoTMM      bool
}

// Config controls selection and the destructive boundary of a plan.
type Config struct {
	BudgetBytes           int64
	ReserveBytes          int64
	Category              string
	CandidateMaxAge       time.Duration
	MinFreeleechRemaining time.Duration
	MinLeechers           int
	MinOpportunityRatio   float64
	MinResidency          time.Duration
	MinIdle               time.Duration
	ActiveUploadRate      int64
	MaxAdditions          int
	MaxRemovals           int
	PlanningHorizon       time.Duration
	ReplacementMargin     float64
}

// Removal records a torrent that the caller is authorized to remove.
type Removal struct {
	Hash        string
	Name        string
	Size        int64
	UploadScore float64
}

// Addition records a candidate and the removals required before it may run.
type Addition struct {
	Candidate   Candidate
	Removals    []Removal
	UploadScore float64
}

// Plan is the complete, side-effect-free result of Build.
type Plan struct {
	Additions  []Addition
	UsedBytes  int64
	LimitBytes int64
	NetGain    float64
}

// Build selects candidates in a stable order and returns only removals owned by
// the configured category and Automatic Torrent Management. It never mutates
// its inputs.
func Build(now time.Time, candidates []Candidate, torrents []Torrent, cfg Config) (Plan, error) {
	if err := validateConfig(cfg); err != nil {
		return Plan{}, err
	}
	if now.IsZero() {
		return Plan{}, errors.New("optimizer: now must be set")
	}

	used, err := validateTorrents(torrents)
	if err != nil {
		return Plan{}, err
	}
	if err := validateCandidates(candidates); err != nil {
		return Plan{}, err
	}
	for _, c := range candidates {
		if c.PublishedAt.After(now) {
			return Plan{}, fmt.Errorf("optimizer: candidate %q is published in the future", c.ID)
		}
	}

	limit := cfg.BudgetBytes - cfg.ReserveBytes
	eligible := removable(now, torrents, cfg)
	slices.SortFunc(eligible, func(a, b Torrent) int { return strings.Compare(a.Hash, b.Hash) })
	filtered := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if candidateEligible(now, c, cfg) && c.Size <= limit {
			filtered = append(filtered, c)
		}
	}
	slices.SortFunc(filtered, func(a, b Candidate) int {
		return strings.Compare(a.ID, b.ID)
	})
	return selectPortfolio(now, filtered, eligible, used, limit, cfg)
}

func validateConfig(c Config) error {
	if c.BudgetBytes <= 0 || c.ReserveBytes < 0 || c.ReserveBytes >= c.BudgetBytes {
		return errors.New("optimizer: budget must exceed a non-negative reserve")
	}
	if strings.TrimSpace(c.Category) == "" {
		return errors.New("optimizer: category must be set")
	}
	if c.CandidateMaxAge < 0 || c.MinFreeleechRemaining < 0 || c.MinResidency < 0 || c.MinIdle < 0 {
		return errors.New("optimizer: durations must not be negative")
	}
	if c.MinLeechers < 0 || c.ActiveUploadRate < 0 || c.MaxAdditions < 0 || c.MaxRemovals < 0 || c.MinOpportunityRatio < 0 {
		return errors.New("optimizer: thresholds and action caps must not be negative")
	}
	if math.IsNaN(c.MinOpportunityRatio) || math.IsInf(c.MinOpportunityRatio, 0) {
		return errors.New("optimizer: minimum opportunity ratio must be finite")
	}
	if c.PlanningHorizon <= 0 || c.ReplacementMargin < 1 || math.IsNaN(c.ReplacementMargin) || math.IsInf(c.ReplacementMargin, 0) {
		return errors.New("optimizer: planning horizon must be positive and replacement margin must be finite and at least one")
	}
	return nil
}

func validateTorrents(torrents []Torrent) (int64, error) {
	used := int64(0)
	hashes := make(map[string]bool, len(torrents))
	for _, t := range torrents {
		if t.Hash == "" || t.Size < 0 || t.Uploaded < 0 || t.UploadRate < 0 || math.IsNaN(t.Progress) || t.Progress < 0 || t.Progress > 1 || t.AddedAt.IsZero() || t.LastActivity.IsZero() {
			return 0, fmt.Errorf("optimizer: invalid torrent %q", t.Hash)
		}
		if hashes[t.Hash] {
			return 0, fmt.Errorf("optimizer: duplicate torrent hash %q", t.Hash)
		}
		hashes[t.Hash] = true
		if t.Size > math.MaxInt64-used {
			return 0, errors.New("optimizer: total torrent size overflows int64")
		}
		used += t.Size
	}
	return used, nil
}

func validateCandidates(candidates []Candidate) error {
	ids := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		if c.ID == "" || c.Size <= 0 || c.Seeders < 0 || c.Leechers < 0 || c.PublishedAt.IsZero() || (c.UploadMultiplier != 1 && c.UploadMultiplier != 2) {
			return fmt.Errorf("optimizer: invalid candidate %q", c.ID)
		}
		if ids[c.ID] {
			return fmt.Errorf("optimizer: duplicate candidate ID %q", c.ID)
		}
		ids[c.ID] = true
	}
	return nil
}

func candidateEligible(now time.Time, c Candidate, cfg Config) bool {
	age := now.Sub(c.PublishedAt)
	return age >= 0 && age <= cfg.CandidateMaxAge &&
		(c.FreeUntil.IsZero() || (c.FreeUntil.After(now) && c.FreeUntil.Sub(now) >= cfg.MinFreeleechRemaining)) &&
		c.Leechers > 0 && c.Seeders > 0 &&
		c.Leechers >= cfg.MinLeechers && opportunity(c) >= cfg.MinOpportunityRatio
}

func removable(now time.Time, torrents []Torrent, cfg Config) []Torrent {
	var out []Torrent
	for _, t := range torrents {
		if t.Category != cfg.Category || !t.AutoTMM || t.Progress != 1 {
			continue
		}
		if now.Sub(t.AddedAt) < cfg.MinResidency || now.Sub(t.LastActivity) < cfg.MinIdle || t.UploadRate > cfg.ActiveUploadRate || busyState(t.State) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func busyState(state string) bool {
	s := strings.ToLower(state)
	return strings.Contains(s, "downloading") || strings.Contains(s, "metadl") || strings.Contains(s, "checking") || strings.Contains(s, "moving") || strings.Contains(s, "allocating")
}

func opportunity(c Candidate) float64 {
	return float64(c.Leechers) / (float64(c.Seeders) + 1)
}

// candidateScore is a credited-byte opportunity proxy, not a calibrated forecast.
// Assume one generation of full-file leecher demand shared with the seeders,
// discount stale demand, and prorate the bonus over the planning horizon. Base
// upload remains valuable after a timed freeleech ends; only its extra credit
// expires. An offer with no scheduled end earns its full multiplier throughout
// the planning horizon.
func candidateScore(now time.Time, c Candidate, horizon time.Duration) float64 {
	h := horizon.Seconds()
	freshness := h / (h + now.Sub(c.PublishedAt).Seconds())
	bonusFraction := 1.0
	if !c.FreeUntil.IsZero() {
		bonusFraction = min(1.0, c.FreeUntil.Sub(now).Seconds()/h)
	}
	multiplier := 1 + float64(c.UploadMultiplier-1)*bonusFraction
	return float64(c.Size) * opportunity(c) * freshness * multiplier
}

// retentionScore values forgone upload in the same horizon. The search feed
// cannot reliably identify every incumbent's current promotion by infohash, so
// reserve 2x credit for incumbents instead of assuming they earn only 1x.
func retentionScore(now time.Time, t Torrent, horizon time.Duration) float64 {
	age := max(1.0, now.Sub(t.AddedAt).Seconds())
	rate := max(float64(t.UploadRate), float64(t.Uploaded)/age)
	return 2 * rate * horizon.Seconds()
}

func cmpFloat(a, b float64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
