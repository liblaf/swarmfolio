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

// Candidate is a transient freeleech offer from the configured M-Team source.
type Candidate struct {
	ID          string
	Name        string
	Size        int64
	Seeders     int
	Leechers    int
	PublishedAt time.Time
	FreeUntil   time.Time
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
}

// Removal records a torrent that the caller is authorized to remove.
type Removal struct {
	Hash string
	Name string
	Size int64
}

// Addition records a candidate and the removals required before it may run.
type Addition struct {
	Candidate Candidate
	Removals  []Removal
}

// Plan is the complete, side-effect-free result of Build.
type Plan struct {
	Additions  []Addition
	UsedBytes  int64
	LimitBytes int64
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
	slices.SortFunc(eligible, func(a, b Torrent) int {
		if au, bu := utility(now, a), utility(now, b); au != bu {
			return cmpFloat(au, bu)
		}
		return strings.Compare(a.Hash, b.Hash)
	})

	filtered := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if candidateEligible(now, c, cfg) {
			filtered = append(filtered, c)
		}
	}
	slices.SortFunc(filtered, func(a, b Candidate) int {
		if ao, bo := opportunity(a), opportunity(b); ao != bo {
			return cmpFloat(bo, ao)
		}
		if n := strings.Compare(a.ID, b.ID); n != 0 {
			return n
		}
		return strings.Compare(a.Name, b.Name)
	})

	plan := Plan{UsedBytes: used, LimitBytes: limit}
	removed := make(map[string]bool)
	removalCount := 0
	for _, candidate := range filtered {
		if len(plan.Additions) == cfg.MaxAdditions {
			break
		}
		if candidate.Size > math.MaxInt64-used {
			return Plan{}, errors.New("optimizer: planned torrent size overflows int64")
		}
		needed := used + candidate.Size - limit
		var rs []Removal
		for _, t := range eligible {
			if needed <= 0 {
				break
			}
			if removed[t.Hash] || removalCount+len(rs) >= cfg.MaxRemovals {
				continue
			}
			rs = append(rs, Removal{Hash: t.Hash, Name: t.Name, Size: t.Size})
			needed -= t.Size
		}
		if needed > 0 {
			continue
		}
		for _, r := range rs {
			removed[r.Hash] = true
			used -= r.Size
		}
		removalCount += len(rs)
		used += candidate.Size
		plan.Additions = append(plan.Additions, Addition{Candidate: candidate, Removals: rs})
	}
	plan.UsedBytes = used
	return plan, nil
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
	return nil
}

func validateTorrents(torrents []Torrent) (int64, error) {
	used := int64(0)
	hashes := make(map[string]bool, len(torrents))
	for _, t := range torrents {
		if t.Hash == "" || t.Size < 0 || t.Uploaded < 0 || t.UploadRate < 0 || t.Progress < 0 || t.Progress > 1 || t.AddedAt.IsZero() || t.LastActivity.IsZero() {
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
		if c.ID == "" || c.Size <= 0 || c.Seeders < 0 || c.Leechers < 0 || c.PublishedAt.IsZero() || c.FreeUntil.IsZero() {
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
		c.FreeUntil.Sub(now) >= cfg.MinFreeleechRemaining &&
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
	return (float64(c.Leechers) + 1) / (float64(c.Seeders) + 1)
}

func utility(now time.Time, t Torrent) float64 {
	age := now.Sub(t.AddedAt).Seconds()
	if age < 1 {
		age = 1
	}
	size := t.Size
	if size < 1 {
		size = 1
	}
	return float64(t.Uploaded) / age / float64(size)
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
