package optimizer

import (
	"reflect"
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

func testConfig() Config {
	return Config{BudgetBytes: 100, ReserveBytes: 10, Category: "swarmfolio", CandidateMaxAge: 24 * time.Hour, MinFreeleechRemaining: time.Hour, MinLeechers: 1, MinOpportunityRatio: .5, MinResidency: time.Hour, MinIdle: time.Hour, ActiveUploadRate: 1, MaxAdditions: 2, MaxRemovals: 2}
}

func candidate(id string, size int64, seeds, leeches int) Candidate {
	return Candidate{ID: id, Name: id, Size: size, Seeders: seeds, Leechers: leeches, PublishedAt: testNow.Add(-time.Hour), FreeUntil: testNow.Add(2 * time.Hour)}
}

func torrent(hash string, size, uploaded int64) Torrent {
	return Torrent{Hash: hash, Name: hash, Size: size, Uploaded: uploaded, Progress: 1, State: "pausedUP", AddedAt: testNow.Add(-2 * time.Hour), LastActivity: testNow.Add(-2 * time.Hour), Category: "swarmfolio", AutoTMM: true}
}

func TestBuildFillsSpareBudgetInOpportunityOrder(t *testing.T) {
	cfg := testConfig()
	plan, err := Build(testNow, []Candidate{candidate("later", 20, 5, 2), candidate("first", 20, 1, 8)}, []Torrent{torrent("owned", 50, 1)}, cfg)
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
