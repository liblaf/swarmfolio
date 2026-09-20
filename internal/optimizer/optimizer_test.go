package optimizer

import (
	"math"
	"reflect"
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

func testConfig() Config {
	return Config{BudgetBytes: 100, ReserveBytes: 10, Category: "swarmfolio", CandidateMaxAge: 24 * time.Hour, MinFreeleechRemaining: time.Hour, MinLeechers: 1, MinOpportunityRatio: .5, MinResidency: time.Hour, MinIdle: time.Hour, ActiveUploadRate: 1, MaxAdditions: 2, MaxRemovals: 2, PlanningHorizon: 24 * time.Hour, ReplacementMargin: 1.25}
}

func candidate(id string, size int64, seeds, leeches int) Candidate {
	return Candidate{ID: id, Name: id, Size: size, Seeders: seeds, Leechers: leeches, PublishedAt: testNow.Add(-time.Hour), FreeUntil: testNow.Add(2 * time.Hour), UploadMultiplier: 1}
}

func torrent(hash string, size, uploaded int64) Torrent {
	return Torrent{Hash: hash, Name: hash, Size: size, Uploaded: uploaded, Progress: 1, State: "pausedUP", AddedAt: testNow.Add(-2 * time.Hour), LastActivity: testNow.Add(-2 * time.Hour), Category: "swarmfolio", AutoTMM: true}
}

func TestBuildFillsSpareBudgetInOpportunityOrder(t *testing.T) {
	cfg := testConfig()
	plan, err := Build(testNow, []Candidate{candidate("later", 20, 5, 3), candidate("first", 20, 1, 8)}, []Torrent{torrent("owned", 50, 1)}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{plan.Additions[0].Candidate.ID, plan.Additions[1].Candidate.ID}; !reflect.DeepEqual(got, []string{"first", "later"}) {
		t.Fatalf("order = %v", got)
	}
	if len(plan.Additions[0].Removals) != 0 {
		t.Fatal("spare capacity should be used first")
	}
}

func TestBuildReplacesLowestUtilityOnly(t *testing.T) {
	cfg := testConfig()
	low, high := torrent("low", 40, 1), torrent("high", 40, 1_000_000)
	plan, err := Build(testNow, []Candidate{candidate("new", 30, 1, 8)}, []Torrent{low, high}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Additions) != 1 || len(plan.Additions[0].Removals) != 1 || plan.Additions[0].Removals[0].Hash != "low" {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestBuildNeverRemovesIncompleteOrActive(t *testing.T) {
	cfg := testConfig()
	incomplete := torrent("incomplete", 40, 0)
	incomplete.Progress = .5
	active := torrent("active", 40, 0)
	active.UploadRate = 2
	plan, err := Build(testNow, []Candidate{candidate("new", 30, 1, 8)}, []Torrent{incomplete, active}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Additions) != 0 {
		t.Fatalf("unsafe plan = %#v", plan)
	}
}

func TestBuildNeverRemovesOutsideCategoryOrManualManagement(t *testing.T) {
	cfg := testConfig()
	wrongCategory := torrent("wrong-category", 40, 0)
	wrongCategory.Category = "user-managed"
	manual := torrent("manual", 40, 0)
	manual.AutoTMM = false

	plan, err := Build(testNow, []Candidate{candidate("new", 30, 1, 8)}, []Torrent{wrongCategory, manual}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Additions) != 0 {
		t.Fatalf("unsafe plan = %#v", plan)
	}
}

func TestBuildAllowsPresentAndSkipsIneligibleCandidates(t *testing.T) {
	cfg := testConfig()
	present := torrent("old", 20, 0)
	tooOld := candidate("old", 20, 1, 9)
	tooOld.PublishedAt = testNow.Add(-25 * time.Hour)
	plan, err := Build(testNow, []Candidate{candidate("present", 20, 1, 9), tooOld, candidate("ok", 20, 1, 9)}, []Torrent{present}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{plan.Additions[0].Candidate.ID, plan.Additions[1].Candidate.ID}; !reflect.DeepEqual(got, []string{"ok", "present"}) {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestBuildRejectsInvalidInputs(t *testing.T) {
	cfg := testConfig()
	cfg.BudgetBytes = 10
	cfg.ReserveBytes = 10
	if _, err := Build(testNow, nil, nil, cfg); err == nil {
		t.Fatal("expected config error")
	}
	cfg = testConfig()
	if _, err := Build(testNow, []Candidate{{ID: "bad"}}, nil, cfg); err == nil {
		t.Fatal("expected candidate error")
	}
	if _, err := Build(testNow, nil, []Torrent{{Hash: "x", Size: -1}}, cfg); err == nil {
		t.Fatal("expected torrent error")
	}
}

func TestBuildMaximizesCombinedCreditInsteadOfFirstRankedCandidate(t *testing.T) {
	cfg := testConfig()
	cs := []Candidate{candidate("large", 90, 1, 7), candidate("medium-a", 45, 1, 8), candidate("medium-b", 45, 1, 8)}
	plan, err := Build(testNow, cs, []Torrent{torrent("old-a", 45, 1), torrent("old-b", 45, 1)}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Additions) != 2 || plan.Additions[0].Candidate.ID != "medium-a" || plan.Additions[1].Candidate.ID != "medium-b" {
		t.Fatalf("joint selection = %#v", plan)
	}
	if plan.UsedBytes != 90 || plan.NetGain <= 0 {
		t.Fatalf("joint budget/gain = %#v", plan)
	}
}

func TestBuildFindsRemovalThatFitsActionCap(t *testing.T) {
	cfg := testConfig()
	plan, err := Build(testNow, []Candidate{candidate("new", 60, 1, 8)}, []Torrent{
		torrent("tiny-a", 10, 0), torrent("tiny-b", 10, 0), torrent("large", 70, 1),
	}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Additions) != 1 || len(plan.Additions[0].Removals) != 1 || plan.Additions[0].Removals[0].Hash != "large" {
		t.Fatalf("removal combination = %#v", plan)
	}
}

func TestBuildRequiresPositiveGainOverKeepingIncumbents(t *testing.T) {
	cfg := testConfig()
	plan, err := Build(testNow, []Candidate{candidate("new", 60, 1, 8)}, []Torrent{torrent("productive", 90, 100)}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Additions) != 0 || plan.UsedBytes != 90 || plan.NetGain != 0 {
		t.Fatalf("unprofitable churn = %#v", plan)
	}
}

func TestBuildUsesCreditAndPayloadInsteadOfRatioAlone(t *testing.T) {
	cfg := testConfig()
	cfg.MaxAdditions = 1
	double := candidate("double", 60, 1, 4)
	double.UploadMultiplier = 2
	double.FreeUntil = testNow.Add(cfg.PlanningHorizon)
	plan, err := Build(testNow, []Candidate{candidate("tiny-high-ratio", 10, 1, 12), candidate("plain", 60, 1, 4), double}, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Additions) != 1 || plan.Additions[0].Candidate.ID != "double" {
		t.Fatalf("credited-byte selection = %#v", plan)
	}
}

func TestPromotionBonusExpiresAndFreshnessDecays(t *testing.T) {
	c := candidate("double", 100, 1, 2)
	c.UploadMultiplier = 2
	c.PublishedAt = testNow
	horizon := 24 * time.Hour
	for _, tc := range []struct {
		remaining time.Duration
		want      float64
	}{{48 * time.Hour, 200}, {24 * time.Hour, 200}, {12 * time.Hour, 150}, {0, 100}} {
		c.FreeUntil = testNow.Add(tc.remaining)
		if got := candidateScore(testNow, c, horizon); got != tc.want {
			t.Fatalf("remaining %s score=%g, want %g", tc.remaining, got, tc.want)
		}
	}
	c.PublishedAt = testNow.Add(-horizon)
	if got := candidateScore(testNow, c, horizon); got != 50 {
		t.Fatalf("aged demand score=%g, want 50", got)
	}
}

func TestRetentionProtectsCurrentUploadAndAppliesMargin(t *testing.T) {
	cfg := testConfig()
	cfg.PlanningHorizon = time.Hour
	cfg.MinIdle = 0
	old := torrent("old", 90, 1)
	old.UploadRate = 1
	if got := retentionScore(testNow, old, cfg.PlanningHorizon); got != 7200 {
		t.Fatalf("retention current-rate floor=%g, want 7200", got)
	}
	old.UploadRate = 0
	old.Uploaded = 100              // 1h retention is 2 * (100 / 2h) * 1h = 100.
	c := candidate("new", 90, 1, 5) // age 1h, value = 90 * 2.5 / 2 = 112.5.
	plan, err := Build(testNow, []Candidate{c}, []Torrent{old}, cfg)
	if err != nil || len(plan.Additions) != 0 {
		t.Fatalf("replacement below 1.25 margin: plan=%#v err=%v", plan, err)
	}
	cfg.ReplacementMargin = 1
	plan, err = Build(testNow, []Candidate{c}, []Torrent{old}, cfg)
	if err != nil || len(plan.Additions) != 1 || plan.NetGain != 12.5 {
		t.Fatalf("replacement without safety margin: plan=%#v err=%v", plan, err)
	}
}

func TestBuildRejectsPhantomDemandAndUnavailableSwarms(t *testing.T) {
	cfg := testConfig()
	cfg.MinLeechers, cfg.MinOpportunityRatio, cfg.MinFreeleechRemaining = 0, 0, 0
	expired := candidate("expired", 10, 1, 5)
	expired.FreeUntil = testNow
	plan, err := Build(testNow, []Candidate{candidate("no-demand", 10, 1, 0), candidate("no-source", 10, 0, 5), expired}, nil, cfg)
	if err != nil || len(plan.Additions) != 0 {
		t.Fatalf("unavailable opportunity: plan=%#v err=%v", plan, err)
	}
}

func TestBuildNeverRemovesWithoutAnAddition(t *testing.T) {
	cfg := testConfig()
	plan, err := Build(testNow, nil, []Torrent{torrent("over-budget", 100, 0)}, cfg)
	if err != nil || len(plan.Additions) != 0 || plan.UsedBytes != 100 {
		t.Fatalf("removal-only plan: plan=%#v err=%v", plan, err)
	}
}

func TestBuildValidatesScoringInputs(t *testing.T) {
	for _, margin := range []float64{0, .9, math.Inf(1), math.NaN()} {
		cfg := testConfig()
		cfg.ReplacementMargin = margin
		if _, err := Build(testNow, nil, nil, cfg); err == nil {
			t.Fatalf("accepted margin %g", margin)
		}
	}
	cfg := testConfig()
	cfg.PlanningHorizon = 0
	if _, err := Build(testNow, nil, nil, cfg); err == nil {
		t.Fatal("accepted zero horizon")
	}
	for _, multiplier := range []int{0, -1, 3} {
		c := candidate("invalid-promotion", 1, 1, 1)
		c.UploadMultiplier = multiplier
		if _, err := Build(testNow, []Candidate{c}, nil, testConfig()); err == nil {
			t.Fatalf("accepted multiplier %d", multiplier)
		}
	}
}
