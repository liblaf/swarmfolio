package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/liblaf/swarmfolio/internal/metainfo"
	"github.com/liblaf/swarmfolio/internal/optimizer"
)

type candidateMetainfo struct {
	hash  string
	bytes []byte
}

// candidateResolver shares verified metainfo across recovery, planning, and
// apply within one run. Display names are not torrent identities.
type candidateResolver struct {
	mteam MTeam
	byID  map[string]candidateMetainfo
}

func (r *candidateResolver) resolve(ctx context.Context, id string) (candidateMetainfo, error) {
	if info, ok := r.byID[id]; ok {
		return info, nil
	}
	data, err := r.mteam.Download(ctx, mustInt64(id))
	if err != nil {
		return candidateMetainfo{}, fmt.Errorf("download M-Team torrent %s: %w", id, err)
	}
	hash, err := metainfo.InfoHash(data)
	if err != nil {
		return candidateMetainfo{}, fmt.Errorf("inspect M-Team torrent %s: %w", id, err)
	}
	info := candidateMetainfo{hash: hash, bytes: data}
	r.byID[id] = info
	return info, nil
}

// buildPlan resolves only selected offers, excluding hashes already present in
// any category and rebuilding to fill their slots. Each retry removes at least
// one candidate; no torrent mutations occur while finding the final plan.
func (r Runner) buildPlan(ctx context.Context, now time.Time, candidates []optimizer.Candidate, state snapshot, resolved *candidateResolver) (optimizer.Plan, error) {
	candidates = slices.Clone(candidates)
	for {
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
			info, err := resolved.resolve(ctx, addition.Candidate.ID)
			if err != nil {
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
