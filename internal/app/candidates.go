package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/liblaf/swarmfolio/internal/metainfo"
	"github.com/liblaf/swarmfolio/internal/mteam"
	"github.com/liblaf/swarmfolio/internal/optimizer"
)

type candidateMetainfo struct {
	hash  string
	size  int64
	bytes []byte
}

// A refused metainfo download usually concerns one torrent, but repeated
// refusals in one run suggest an account-wide problem that must stop the run.
const maxRefusedCandidates = 3

// candidateResolver shares verified metainfo across recovery, planning, and
// apply within one run. Display names are not torrent identities. Offers that
// M-Team refuses to serve are excluded for the rest of the run; excluded keeps
// the download error so later lookups report the same cause.
type candidateResolver struct {
	mteam    MTeam
	byID     map[string]candidateMetainfo
	excluded map[string]error
	refused  int
	skipped  []SkippedCandidate
}

func newCandidateResolver(mt MTeam) *candidateResolver {
	return &candidateResolver{mteam: mt, byID: make(map[string]candidateMetainfo), excluded: make(map[string]error)}
}

func (r *candidateResolver) resolve(ctx context.Context, id string, expectedSize int64) (candidateMetainfo, error) {
	if err := r.excluded[id]; err != nil {
		return candidateMetainfo{}, err
	}
	if info, ok := r.byID[id]; ok {
		if info.size != expectedSize {
			return candidateMetainfo{}, fmt.Errorf("M-Team torrent %s metainfo size %d does not match its %d-byte offer", id, info.size, expectedSize)
		}
		return info, nil
	}
	data, err := r.mteam.Download(ctx, mustInt64(id))
	if err != nil {
		err = fmt.Errorf("download M-Team torrent %s: %w", id, err)
		var refused *mteam.DownloadRefusedError
		switch {
		case errors.Is(err, mteam.ErrTorrentDownloadLimit):
			r.exclude(id, "daily torrent download limit reached", err)
		case errors.As(err, &refused) && r.refused < maxRefusedCandidates:
			r.refused++
			r.exclude(id, fmt.Sprintf("M-Team refused the metainfo download with API error %d", refused.Code), err)
		}
		return candidateMetainfo{}, err
	}
	inspected, err := metainfo.Inspect(data)
	if err != nil {
		return candidateMetainfo{}, fmt.Errorf("inspect M-Team torrent %s: %w", id, err)
	}
	info := candidateMetainfo{hash: inspected.Hash, size: inspected.Size, bytes: data}
	r.byID[id] = info
	if info.size != expectedSize {
		return candidateMetainfo{}, fmt.Errorf("M-Team torrent %s metainfo size %d does not match its %d-byte offer", id, info.size, expectedSize)
	}
	return info, nil
}

func (r *candidateResolver) exclude(id, reason string, cause error) {
	if r.excluded[id] != nil {
		return
	}
	r.excluded[id] = cause
	r.skipped = append(r.skipped, SkippedCandidate{CandidateID: id, Reason: reason})
}

// buildPlan resolves only selected offers, excluding hashes already present in
// any category and rebuilding to fill their slots. Each retry removes at least
// one candidate; no torrent mutations occur while finding the final plan.
func (r Runner) buildPlan(ctx context.Context, now time.Time, candidates []optimizer.Candidate, state snapshot, resolved *candidateResolver) (optimizer.Plan, error) {
	candidates = slices.Clone(candidates)
	for {
		candidates = slices.DeleteFunc(candidates, func(candidate optimizer.Candidate) bool {
			return resolved.excluded[candidate.ID] != nil
		})
		plan, err := optimizer.Build(now, candidates, state.forOptimizer, r.planConfig(state))
		if err != nil {
			return optimizer.Plan{}, err
		}
		seen := make(map[string]bool, len(state.all)+len(plan.Additions))
		for _, torrent := range state.all {
			seen[strings.ToLower(torrent.Hash)] = true
		}
		excluded := make(map[string]bool)
		for _, addition := range plan.Additions {
			info, err := resolved.resolve(ctx, addition.Candidate.ID, addition.Candidate.Size)
			if err != nil {
				if resolved.excluded[addition.Candidate.ID] != nil {
					excluded[addition.Candidate.ID] = true
					continue
				}
				return optimizer.Plan{}, err
			}
			hash := strings.ToLower(info.hash)
			if seen[hash] {
				excluded[addition.Candidate.ID] = true
			}
			seen[hash] = true
		}
		if len(excluded) == 0 {
			return plan, nil
		}
		candidates = slices.DeleteFunc(candidates, func(candidate optimizer.Candidate) bool {
			return excluded[candidate.ID]
		})
	}
}

// planConfig applies run-wide limits that depend on current qBittorrent state.
// Unfinished managed torrents compete for the same disk and bandwidth, so the
// optional cap on them also bounds how many additions this plan may start.
func (r Runner) planConfig(state snapshot) optimizer.Config {
	cfg := r.optimizerConfig(state.budget.LimitBytes)
	if limit := r.Config.Policy.MaxIncompleteDownloads; limit > 0 {
		incomplete := 0
		for _, torrent := range state.forOptimizer {
			if torrent.Progress < 1 {
				incomplete++
			}
		}
		cfg.MaxAdditions = min(cfg.MaxAdditions, max(0, limit-incomplete))
	}
	return cfg
}
