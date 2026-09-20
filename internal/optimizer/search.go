package optimizer

import (
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

// Exact knapsack search can grow exponentially. Refuse excessive work visibly;
// never silently switch to a different objective or an incomplete shortlist.
const maxSearchWork = 2_000_000

type selection struct {
	used      int64
	gain      float64
	additions []int
	removals  []int
}

func selectPortfolio(now time.Time, candidates []Candidate, eligible []Torrent, used, limit int64, cfg Config) (Plan, error) {
	plan := Plan{UsedBytes: used, LimitBytes: limit}
	addCap := min(cfg.MaxAdditions, len(candidates))
	removeCap := min(cfg.MaxRemovals, len(eligible))
	if addCap == 0 {
		return plan, nil
	}
	if removeCap == 0 {
		eligible = nil
	}
	if (addCap + 1) > maxSearchWork/(removeCap+1) {
		return Plan{}, errors.New("optimizer: action caps exceed exact search work limit; reduce action caps")
	}
	// Within each action-count bucket, retain only states for which no other
	// state uses fewer bytes and earns at least as much risk-adjusted credit.
	frontiers := make([][][]selection, addCap+1)
	for a := range frontiers {
		frontiers[a] = make([][]selection, removeCap+1)
	}
	frontiers[0][0] = []selection{{used: used}}
	work := 0
	spend := func(n int) error {
		work += n
		if work > maxSearchWork {
			return errors.New("optimizer: exact search work limit exceeded; reduce candidate pages or action caps")
		}
		return nil
	}
	for i, t := range eligible {
		cost := cfg.ReplacementMargin * retentionScore(now, t, cfg.PlanningHorizon)
		if math.IsInf(cost, 0) {
			return Plan{}, errors.New("optimizer: replacement score overflows float64")
		}
		for r := min(removeCap, i+1); r > 0; r-- {
			if err := spend(1 + len(frontiers[0][r]) + len(frontiers[0][r-1])); err != nil {
				return Plan{}, err
			}
			dst := slices.Clone(frontiers[0][r])
			for _, state := range frontiers[0][r-1] {
				if math.IsInf(state.gain-cost, 0) {
					return Plan{}, errors.New("optimizer: portfolio score overflows float64")
				}
				dst = append(dst, selection{
					used: state.used - t.Size, gain: state.gain - cost,
					removals: append(slices.Clone(state.removals), i),
				})
			}
			frontiers[0][r] = prune(dst)
		}
	}
	for i, c := range candidates {
		value := candidateScore(now, c, cfg.PlanningHorizon)
		for a := min(addCap, i+1); a > 0; a-- {
			for r := 0; r <= removeCap; r++ {
				if err := spend(1 + len(frontiers[a][r]) + len(frontiers[a-1][r])); err != nil {
					return Plan{}, err
				}
				dst := slices.Clone(frontiers[a][r])
				for _, state := range frontiers[a-1][r] {
					// All deletions have already been considered. A state that
					// cannot fit this addition can never regain capacity later.
					if state.used > limit-c.Size {
						continue
					}
					dst = append(dst, selection{
						used: state.used + c.Size, gain: state.gain + value,
						additions: append(slices.Clone(state.additions), i), removals: state.removals,
					})
				}
				frontiers[a][r] = prune(dst)
			}
		}
	}
	best := selection{used: used}
	for a := 1; a <= addCap; a++ {
		for r := 0; r <= removeCap; r++ {
			for _, state := range frontiers[a][r] {
				if state.gain > 0 && better(state, best) {
					best = state
				}
			}
		}
	}
	if len(best.additions) == 0 {
		return plan, nil
	}
	// Execute the strongest addition first and attach deletions only when they
	// are needed. Every prefix fits the budget, with no duplicate deletions.
	slices.SortFunc(best.additions, func(a, b int) int {
		if order := cmpFloat(candidateScore(now, candidates[b], cfg.PlanningHorizon), candidateScore(now, candidates[a], cfg.PlanningHorizon)); order != 0 {
			return order
		}
		return strings.Compare(candidates[a].ID, candidates[b].ID)
	})
	remaining := best.removals
	for _, index := range best.additions {
		c := candidates[index]
		addition := Addition{Candidate: c, UploadScore: candidateScore(now, c, cfg.PlanningHorizon)}
		for used > limit-c.Size {
			if len(remaining) == 0 {
				panic("optimizer: selected portfolio cannot fit its additions")
			}
			t := eligible[remaining[0]]
			remaining = remaining[1:]
			addition.Removals = append(addition.Removals, Removal{
				Hash: t.Hash, Name: t.Name, Size: t.Size,
				UploadScore: retentionScore(now, t, cfg.PlanningHorizon),
			})
			used -= t.Size
		}
		used += c.Size
		plan.Additions = append(plan.Additions, addition)
	}
	if len(remaining) != 0 || used != best.used {
		panic("optimizer: selected portfolio contains unnecessary removals")
	}
	plan.UsedBytes, plan.NetGain = used, best.gain
	return plan, nil
}

func prune(states []selection) []selection {
	slices.SortFunc(states, func(a, b selection) int {
		if a.used < b.used {
			return -1
		}
		if a.used > b.used {
			return 1
		}
		if a.gain != b.gain {
			return cmpFloat(b.gain, a.gain)
		}
		return compareChoices(a, b)
	})
	result := states[:0]
	bestGain := math.Inf(-1)
	for _, state := range states {
		if state.gain > bestGain {
			result = append(result, state)
			bestGain = state.gain
		}
	}
	return result
}

func better(a, b selection) bool {
	if a.gain != b.gain {
		return a.gain > b.gain
	}
	if len(a.removals) != len(b.removals) {
		return len(a.removals) < len(b.removals)
	}
	if len(a.additions) != len(b.additions) {
		return len(a.additions) < len(b.additions)
	}
	if a.used != b.used {
		return a.used < b.used
	}
	return compareChoices(a, b) < 0
}

func compareChoices(a, b selection) int {
	if order := slices.Compare(a.additions, b.additions); order != 0 {
		return order
	}
	return slices.Compare(a.removals, b.removals)
}
