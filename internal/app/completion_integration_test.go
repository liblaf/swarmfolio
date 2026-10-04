package app

import (
	"context"
	"testing"
	"time"

	"github.com/liblaf/swarmfolio/internal/disk"
	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

func TestExecuteRejectsTimedOfferWithoutTransferEvidenceBeforeAdding(t *testing.T) {
	t.Parallel()
	q, m := testServices(t)
	q.torrents = nil
	report, err := testRunner(q, m).Execute(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Actions) != 0 || q.mutations != 0 || m.downloads != 0 {
		t.Fatalf("unmeasurable timed offer admitted: %#v", report)
	}
}

func TestExecuteRejectsTimedOfferThatCannotFinishBeforeExpiry(t *testing.T) {
	t.Parallel()
	q, m := testServices(t)
	q.torrents[0].Downloaded = 1
	q.torrents[0].DownloadTime = 2 * time.Second
	report, err := testRunner(q, m).Execute(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Actions) != 0 || q.mutations != 0 {
		t.Fatal("too-slow candidate added")
	}
}

func TestExecuteAccountsForOtherDownloadsBeforeTimedAdmission(t *testing.T) {
	t.Parallel()
	q, m := testServices(t)
	q.torrents = nil
	q.torrents = []qbittorrent.Torrent{{Hash: "other", Name: "other", Category: "user", Size: 100000, AmountLeft: 100000, State: "downloading", DLRate: 20, AddedOn: appNow, LastActivity: appNow}}
	q.freeSpace = func() (int64, error) { return 200000, nil }
	r := testRunner(q, m)
	r.Config.Portfolio.DiskPath = ""
	report, err := r.Execute(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Actions) != 0 || q.mutations != 0 {
		t.Fatalf("shared bandwidth queue ignored: %#v", report)
	}
}

func TestExecuteKeepsAFeasibleSubsetWithinSharedFreeleechBudget(t *testing.T) {
	t.Parallel()
	q, m := testServices(t)
	q.torrents[0].Size = 7000
	q.torrents[0].Downloaded = 20
	m.results[0].Size = 4000
	m.results[0].DiscountEndTime = appNow.Add(15 * time.Minute)
	second := m.results[0]
	second.ID = 3
	second.Name = "second"
	m.results = append(m.results, second)
	m.metainfo = []byte("d4:infod6:lengthi4000e4:name3:newee")
	m.metainfoByID = map[int64][]byte{3: []byte("d4:infod6:lengthi4000e4:name6:secondee")}
	r := testRunner(q, m)
	r.Config.Policy.MaxAdditions = 2
	r.Config.Policy.MinimumFreeleechRemaining = 2 * time.Minute
	r.ProbeDisk = func(string) (disk.Space, error) { return disk.Space{CapacityBytes: 10000, FreeBytes: 3000}, nil }
	report, err := r.Execute(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Actions) != 1 || report.Actions[0].CandidateID != "2" || m.downloads != 1 {
		t.Fatalf("shared bandwidth selected unsafe or empty portfolio: %#v", report)
	}
}
