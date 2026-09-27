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

// candidateResolver shares verified metainfo across recovery, planning, and
// apply within one run. Display names are not torrent identities.
type candidateResolver struct {
	mteam     MTeam
	byID      map[string]candidateMetainfo
	exhausted map[string]bool
	skipped   []SkippedCandidate
}

func (r *candidateResolver) resolve(ctx context.Context, id string, expectedSize int64) (candidateMetainfo, error) {
	if r.exhausted[id] {
		return candidateMetainfo{}, mteam.ErrTorrentDownloadLimit
	}
	if info, ok := r.byID[id]; ok {
		if info.size != expectedSize {
			return candidateMetainfo{}, fmt.Errorf("M-Team torrent %s metainfo size %d does not match its %d-byte offer", id, info.size, expectedSize)
		}
		return info, nil
	}
	data, err := r.mteam.Download(ctx, mustInt64(id))
	if err != nil {
		if errors.Is(err, mteam.ErrTorrentDownloadLimit) {
			r.excludeDownloadLimited(id)
		}
		return candidateMetainfo{}, fmt.Errorf("download M-Team torrent %s: %w", id, err)
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

func (r *candidateResolver) excludeDownloadLimited(id string) {
	if r.exhausted[id] {
		return
	}
	r.exhausted[id] = true
	r.skipped = append(r.skipped, SkippedCandidate{CandidateID: id, Reason: "daily torrent download limit reached"})
}

// buildPlan resolves only selected offers, excluding hashes already present in
// any category and rebuilding to fill their slots. Each retry removes at least
// one candidate; no torrent mutations occur while finding the final plan.
func (r Runner) buildPlan(ctx context.Context, now time.Time, candidates []optimizer.Candidate, state snapshot, resolved *candidateResolver) (optimizer.Plan, error) {
	candidates = slices.Clone(candidates)
	for {
		candidates = slices.DeleteFunc(candidates, func(candidate optimizer.Candidate) bool {
			return resolved.exhausted[candidate.ID]
		})
		plan, err := optimizer.Build(now, candidates, state.forOptimizer, r.optimizerConfig(state.budget.LimitBytes))
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
				if errors.Is(err, mteam.ErrTorrentDownloadLimit) {
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
