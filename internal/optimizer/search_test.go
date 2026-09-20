package optimizer

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

func TestSelectPortfolioMatchesExhaustiveSmallOracle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewPCG(17, 29))
	positiveGainRemoval, overBudgetRecovery, countLimited := 0, 0, 0
	for caseNumber := range 500 {
		cfg := oracleConfig()
		cfg.BudgetBytes = int64(50 + rng.IntN(41))
		cfg.MaxAdditions = 1 + rng.IntN(2)
		cfg.MaxRemovals = 1 + rng.IntN(2)

		candidates := make([]Candidate, 1+rng.IntN(5))
		for i := range candidates {
			candidates[i] = Candidate{
				ID:               string(rune('a' + i)),
				Name:             string(rune('a' + i)),
				Size:             int64(5 + rng.IntN(31)),
				Seeders:          1 + rng.IntN(5),
				Leechers:         1 + rng.IntN(9),
				PublishedAt:      now.Add(-time.Duration(rng.IntN(12)) * time.Hour),
				FreeUntil:        now.Add(time.Duration(1+rng.IntN(36)) * time.Hour),
				UploadMultiplier: 1 + rng.IntN(2),
			}
		}
		torrents := make([]Torrent, 1+rng.IntN(5))
		for i := range torrents {
			torrents[i] = Torrent{
				Hash:         string(rune('k' + i)),
				Name:         string(rune('k' + i)),
				Size:         int64(5 + rng.IntN(31)),
				Uploaded:     randomUploaded(rng),
				UploadRate:   0,
				Progress:     1,
				State:        "pausedUP",
				AddedAt:      now.Add(-time.Duration(2+rng.IntN(48)) * time.Hour),
				LastActivity: now.Add(-time.Duration(2+rng.IntN(48)) * time.Hour),
				Category:     cfg.Category,
				AutoTMM:      true,
			}
		}

		want := exhaustiveOracle(now, candidates, torrents, cfg)
		got, err := Build(now, candidates, torrents, cfg)
		if err != nil {
			t.Fatalf("case %d: Build() error = %v", caseNumber, err)
		}
		if !sameFloat(got.NetGain, want.gain) {
			t.Fatalf("case %d: net gain = %.12g, want %.12g; plan=%#v", caseNumber, got.NetGain, want.gain, got)
		}
		verifyPlanPrefix(t, got, torrents)
		if got.NetGain > 0 && removalCount(got) > 0 {
			positiveGainRemoval++
		}
		if totalSize(torrents) > got.LimitBytes && got.UsedBytes <= got.LimitBytes && len(got.Additions) > 0 {
			overBudgetRecovery++
		}
		if len(candidates) > cfg.MaxAdditions && len(got.Additions) == cfg.MaxAdditions {
			countLimited++
		}

		permutedCandidates := slices.Clone(candidates)
		permutedTorrents := slices.Clone(torrents)
		rng.Shuffle(len(permutedCandidates), func(i, j int) {
			permutedCandidates[i], permutedCandidates[j] = permutedCandidates[j], permutedCandidates[i]
		})
		rng.Shuffle(len(permutedTorrents), func(i, j int) { permutedTorrents[i], permutedTorrents[j] = permutedTorrents[j], permutedTorrents[i] })
		permuted, err := Build(now, permutedCandidates, permutedTorrents, cfg)
		if err != nil {
			t.Fatalf("case %d permuted: Build() error = %v", caseNumber, err)
		}
		if planSignature(got) != planSignature(permuted) || !sameFloat(got.NetGain, permuted.NetGain) {
			t.Fatalf("case %d: input permutation changed plan: %q != %q", caseNumber, planSignature(got), planSignature(permuted))
		}
	}
	if positiveGainRemoval == 0 || overBudgetRecovery == 0 || countLimited == 0 {
		t.Fatalf("coverage missing: positive-gain removals=%d over-budget recovery=%d count-limited=%d", positiveGainRemoval, overBudgetRecovery, countLimited)
	}
}

func TestSelectPortfolioPrefersFewerActionsOnEqualGain(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	cfg := oracleConfig()
	cfg.BudgetBytes = 20
	candidates := []Candidate{
		{ID: "one", Name: "one", Size: 20, Seeders: 1, Leechers: 1, PublishedAt: now, FreeUntil: now.Add(24 * time.Hour), UploadMultiplier: 1},
		{ID: "two-a", Name: "two-a", Size: 10, Seeders: 1, Leechers: 1, PublishedAt: now, FreeUntil: now.Add(24 * time.Hour), UploadMultiplier: 1},
		{ID: "two-b", Name: "two-b", Size: 10, Seeders: 1, Leechers: 1, PublishedAt: now, FreeUntil: now.Add(24 * time.Hour), UploadMultiplier: 1},
	}
	plan, err := Build(now, candidates, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Additions) != 1 || plan.Additions[0].Candidate.ID != "one" {
		t.Fatalf("equal gain should use fewer additions: %#v", plan)
	}

	plan, err = Build(now, []Candidate{{
		ID: "candidate", Name: "candidate", Size: 10, Seeders: 1, Leechers: 1,
		PublishedAt: now, FreeUntil: now.Add(24 * time.Hour), UploadMultiplier: 1,
	}}, []Torrent{{
		Hash: "zero-a", Name: "zero-a", Size: 10, Progress: 1, State: "pausedUP",
		AddedAt: now.Add(-2 * time.Hour), LastActivity: now.Add(-2 * time.Hour),
		Category: cfg.Category, AutoTMM: true,
	}, {
		Hash: "zero-b", Name: "zero-b", Size: 10, Progress: 1, State: "pausedUP",
		AddedAt: now.Add(-2 * time.Hour), LastActivity: now.Add(-2 * time.Hour),
		Category: cfg.Category, AutoTMM: true,
	}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Additions) != 1 || len(plan.Additions[0].Removals) != 1 {
		t.Fatalf("equal gain should use fewer removals: %#v", plan)
	}
}

func TestSelectPortfolioRejectsExcessiveActionCapsBeforeSearch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	cfg := oracleConfig()
	cfg.MaxAdditions, cfg.MaxRemovals = 1500, 1500
	candidates := make([]Candidate, 1500)
	torrents := make([]Torrent, 1500)
	for i := range candidates {
		candidates[i] = Candidate{ID: string(rune(0x3000 + i)), Name: "candidate", Size: 1, Seeders: 1, Leechers: 1,
			PublishedAt: now, FreeUntil: now.Add(24 * time.Hour), UploadMultiplier: 1}
		torrents[i] = Torrent{Hash: string(rune(0x4000 + i)), Name: "torrent", Size: 1, Progress: 1, State: "pausedUP",
			AddedAt: now.Add(-2 * time.Hour), LastActivity: now.Add(-2 * time.Hour), Category: cfg.Category, AutoTMM: true}
	}
	if _, err := Build(now, candidates, torrents, cfg); err == nil {
		t.Fatal("Build accepted action caps that exceed the exact-search work limit")
	}
}

func BenchmarkBuildTypicalPortfolio200Candidates100Torrents(b *testing.B) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	rng := rand.New(rand.NewPCG(101, 103))
	cfg := oracleConfig()
	cfg.BudgetBytes = 3 << 40
	cfg.MaxAdditions, cfg.MaxRemovals = 2, 4
	candidates := make([]Candidate, 200)
	for i := range candidates {
		candidates[i] = Candidate{ID: string(rune(0x1000 + i)), Name: "candidate", Size: int64(1+rng.IntN(40)) << 30,
			Seeders: 1 + rng.IntN(100), Leechers: 1 + rng.IntN(300), PublishedAt: now.Add(-time.Duration(rng.IntN(24)) * time.Hour),
			FreeUntil: now.Add(time.Duration(1+rng.IntN(48)) * time.Hour), UploadMultiplier: 1 + rng.IntN(2)}
	}
	torrents := make([]Torrent, 100)
	for i := range torrents {
		torrents[i] = Torrent{Hash: string(rune(0x2000 + i)), Name: "torrent", Size: int64(1+rng.IntN(40)) << 30,
			Uploaded: int64(rng.IntN(10)) << 30, Progress: 1, State: "pausedUP", AddedAt: now.Add(-time.Duration(2+rng.IntN(720)) * time.Hour),
			LastActivity: now.Add(-time.Duration(2+rng.IntN(720)) * time.Hour), Category: cfg.Category, AutoTMM: true}
	}
	b.ResetTimer()
	for range b.N {
		if _, err := Build(now, candidates, torrents, cfg); err != nil {
			b.Fatal(err)
		}
	}
}

type oracleResult struct{ gain float64 }

func exhaustiveOracle(now time.Time, candidates []Candidate, torrents []Torrent, cfg Config) oracleResult {
	used := int64(0)
	for _, torrent := range torrents {
		used += torrent.Size
	}
	limit := cfg.BudgetBytes - cfg.ReserveBytes
	best := oracleResult{}
	for candidateMask := 1; candidateMask < 1<<len(candidates); candidateMask++ {
		if bits(candidateMask) > cfg.MaxAdditions {
			continue
		}
		candidateBytes, gain := int64(0), 0.0
		for i, candidate := range candidates {
			if candidateMask&(1<<i) != 0 {
				candidateBytes += candidate.Size
				gain += oracleCandidateScore(now, candidate, cfg.PlanningHorizon)
			}
		}
		for removalMask := 0; removalMask < 1<<len(torrents); removalMask++ {
			if bits(removalMask) > cfg.MaxRemovals {
				continue
			}
			removed, score := int64(0), gain
			for i, torrent := range torrents {
				if removalMask&(1<<i) != 0 {
					removed += torrent.Size
					score -= cfg.ReplacementMargin * oracleRetentionScore(now, torrent, cfg.PlanningHorizon)
				}
			}
			if used-removed+candidateBytes <= limit && score > best.gain {
				best.gain = score
			}
		}
	}
	return best
}

func oracleConfig() Config {
	return Config{
		BudgetBytes: 90, Category: "swarmfolio", CandidateMaxAge: 24 * time.Hour,
		MinFreeleechRemaining: time.Hour, MinLeechers: 1, MinOpportunityRatio: 0,
		MinResidency: time.Hour, MinIdle: time.Hour, ActiveUploadRate: 100,
		MaxAdditions: 2, MaxRemovals: 2, PlanningHorizon: 24 * time.Hour, ReplacementMargin: 1.25,
	}
}

func oracleCandidateScore(now time.Time, candidate Candidate, horizon time.Duration) float64 {
	hours := horizon.Seconds()
	freshness := hours / (hours + now.Sub(candidate.PublishedAt).Seconds())
	bonusFraction := math.Min(1, candidate.FreeUntil.Sub(now).Seconds()/hours)
	multiplier := 1 + float64(candidate.UploadMultiplier-1)*bonusFraction
	return float64(candidate.Size) * float64(candidate.Leechers) / float64(candidate.Seeders+1) * freshness * multiplier
}

func oracleRetentionScore(now time.Time, torrent Torrent, horizon time.Duration) float64 {
	age := math.Max(1, now.Sub(torrent.AddedAt).Seconds())
	rate := math.Max(float64(torrent.UploadRate), float64(torrent.Uploaded)/age)
	return 2 * rate * horizon.Seconds()
}

func verifyPlanPrefix(t *testing.T, plan Plan, torrents []Torrent) {
	t.Helper()
	used := int64(0)
	for _, torrent := range torrents {
		used += torrent.Size
	}
	removed := make(map[string]bool)
	for _, addition := range plan.Additions {
		for _, removal := range addition.Removals {
			if removed[removal.Hash] {
				t.Fatalf("duplicate removal %q in %#v", removal.Hash, plan)
			}
			removed[removal.Hash] = true
			used -= removal.Size
		}
		used += addition.Candidate.Size
		if used > plan.LimitBytes {
			t.Fatalf("prefix exceeds budget: used=%d limit=%d plan=%#v", used, plan.LimitBytes, plan)
		}
	}
	if used != plan.UsedBytes {
		t.Fatalf("final used=%d, plan used=%d", used, plan.UsedBytes)
	}
}

func planSignature(plan Plan) string {
	result := ""
	for _, addition := range plan.Additions {
		result += addition.Candidate.ID + ":"
		for _, removal := range addition.Removals {
			result += removal.Hash + ","
		}
		result += ";"
	}
	return result
}

func bits(value int) int {
	count := 0
	for value > 0 {
		count += value & 1
		value >>= 1
	}
	return count
}

func sameFloat(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func randomUploaded(rng *rand.Rand) int64 {
	if rng.IntN(5) == 0 {
		return int64(100 + rng.IntN(900))
	}
	return int64(rng.IntN(11))
}

func removalCount(plan Plan) int {
	count := 0
	for _, addition := range plan.Additions {
		count += len(addition.Removals)
	}
	return count
}

func totalSize(torrents []Torrent) int64 {
	total := int64(0)
	for _, torrent := range torrents {
		total += torrent.Size
	}
	return total
}
