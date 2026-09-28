package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/liblaf/swarmfolio/internal/metainfo"
	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

// Exercise multiple real metainfo identities and keep their payload paths
// distinct, as qBittorrent does for unrelated additions.
type replanningQBT struct {
	*fakeQBT
	onStart     func()
	beforeCheck func()
}

func (q *replanningQBT) Add(ctx context.Context, request qbittorrent.AddRequest) error {
	info, err := metainfo.Inspect(request.Metainfo)
	if err != nil {
		return err
	}
	q.addHash, q.addSize = info.Hash, info.Size
	if err := q.fakeQBT.Add(ctx, request); err != nil {
		return err
	}
	added := &q.torrents[len(q.torrents)-1]
	added.ContentPath = request.SavePath + "/" + info.Hash
	return nil
}

func (q *replanningQBT) Start(ctx context.Context, hashes []string) error {
	if err := q.fakeQBT.Start(ctx, hashes); err != nil {
		return err
	}
	if q.onStart != nil {
		q.onStart()
	}
	return nil
}

func (q *replanningQBT) PreallocateAll(ctx context.Context) (bool, error) {
	if q.beforeCheck != nil {
		q.beforeCheck()
	}
	return q.fakeQBT.PreallocateAll(ctx)
}

func addReplanOffer(mt *fakeMTeam, id, size, leechers int64) {
	offer := mt.results[0]
	offer.ID, offer.Size, offer.Leechers = id, size, leechers
	offer.Name = fmt.Sprintf("offer-%d", id)
	mt.results = append(mt.results, offer)
	if mt.metainfoByID == nil {
		mt.metainfoByID = make(map[int64][]byte)
	}
	mt.metainfoByID[id] = fmt.Appendf(nil, "d4:infod6:lengthi%de4:name%d:%see", size, len(offer.Name), offer.Name)
}

func TestExecuteReplansSecondAdditionWithinRunCaps(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.torrents = nil
	addReplanOffer(mt, 3, 20, 8)
	addReplanOffer(mt, 4, 10, 4)
	addReplanOffer(mt, 5, 5, 4)
	externalUse := int64(0)
	qbt.freeSpace = func() (int64, error) { return qbt.free() - externalUse, nil }
	live := &replanningQBT{fakeQBT: qbt, onStart: func() { externalUse = 30 }}
	runner := apiTestRunner(t, qbt, mt)
	runner.QBittorrent = live
	runner.Config.Policy.MaxAdditions = 2
	report, err := runner.Execute(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Replans != 1 || len(report.Actions) != 2 || report.Actions[0].CandidateID != "2" || report.Actions[1].CandidateID != "4" || !report.Actions[0].Applied || !report.Actions[1].Applied {
		t.Fatalf("report=%#v", report)
	}
	// The original second offer no longer fits. Replanning could fit both
	// smaller offers, but only one addition remains in this run's allowance.
	if len(qbt.torrents) != 2 || len(report.Mutations) != 4 || len(qbt.deleteFiles) != 0 || mt.downloads != 3 {
		t.Fatalf("torrents=%#v report=%#v downloads=%d", qbt.torrents, report, mt.downloads)
	}
	if report.ProjectedUsedBytes != 40 || report.NetGain != report.Actions[0].UploadScore+report.Actions[1].UploadScore {
		t.Fatalf("report includes discarded plan: %#v", report)
	}
}

func TestExecuteReplanningPreservesWholeRunRemovalCap(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.torrents[0].Size = 60
	other := qbt.torrents[0]
	other.Hash, other.Size = "other-old", 10
	other.ContentPath = "/downloads/swarmfolio/other-old"
	qbt.torrents = append(qbt.torrents, other)
	addReplanOffer(mt, 3, 20, 8)
	externalUse := int64(0)
	qbt.freeSpace = func() (int64, error) { return qbt.free() - externalUse, nil }
	runner := apiTestRunner(t, qbt, mt)
	runner.QBittorrent = &replanningQBT{fakeQBT: qbt, onStart: func() { externalUse = 25 }}
	runner.Config.Policy.MaxAdditions = 2
	runner.Config.Policy.MaxRemovals = 1
	report, err := runner.Execute(context.Background(), true)
	if err != nil || report.Replans != 1 || len(report.Actions) != 1 || !report.Actions[0].Applied {
		t.Fatalf("error=%v report=%#v", err, report)
	}
	if len(qbt.deleteFiles) != 1 || findHash(qbt.torrents, "other-old") == nil || findHash(qbt.torrents, "old") != nil {
		t.Fatalf("replanning exceeded deletion cap: events=%v torrents=%#v", qbt.events, qbt.torrents)
	}
	wantGain := report.Actions[0].UploadScore - runner.Config.Policy.ReplacementMargin*report.Actions[0].Removals[0].UploadScore
	if report.NetGain != wantGain || report.ProjectedUsedBytes != 40 {
		t.Fatalf("completed action was lost from report: %#v", report)
	}
}

func TestExecuteReplansWhenBudgetChangesDuringMetainfoDownload(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.torrents = nil
	addReplanOffer(mt, 3, 20, 8)
	externalUse := int64(0)
	mt.onDownload = func(int64) { externalUse = 50 }
	qbt.freeSpace = func() (int64, error) { return qbt.free() - externalUse, nil }
	runner := apiTestRunner(t, qbt, mt)
	runner.QBittorrent = &replanningQBT{fakeQBT: qbt}
	report, err := runner.Execute(context.Background(), true)
	if err != nil || report.Replans != 1 || len(report.Actions) != 1 || report.Actions[0].CandidateID != "3" || !report.Actions[0].Applied || len(report.Mutations) != 2 {
		t.Fatalf("error=%v report=%#v events=%v", err, report, qbt.events)
	}
}

func TestExecuteDoesNotReplanAfterMutationAttempt(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.torrents = nil
	addReplanOffer(mt, 3, 10, 8)
	externalUse := int64(0)
	qbt.onAdd = func(*fakeQBT) { externalUse = 60 }
	qbt.freeSpace = func() (int64, error) { return qbt.free() - externalUse, nil }
	report, err := apiTestRunner(t, qbt, mt).Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "no longer fits") || report.Replans != 0 || len(report.Mutations) != 2 || report.Actions[0].Applied {
		t.Fatalf("error=%v report=%#v", err, report)
	}
	requireDeleteFiles(t, report.Mutations[1], false)
	if prefixIndex(qbt.events, "start:") >= 0 {
		t.Fatalf("unsafe candidate started: %v", qbt.events)
	}
}

func TestExecuteBoundsRepeatedBudgetReplanning(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.torrents = nil
	for id, size := range []int64{50, 40, 20, 10} {
		addReplanOffer(mt, int64(id+3), size, 8)
	}
	externalUse := int64(-9)
	qbt.freeSpace = func() (int64, error) { return 75 - max(externalUse, 0), nil }
	runner := apiTestRunner(t, qbt, mt)
	runner.QBittorrent = &replanningQBT{fakeQBT: qbt, beforeCheck: func() { externalUse += 10 }}
	report, err := runner.Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "budget kept changing") || report.Replans != 3 || qbt.mutations != 0 {
		t.Fatalf("error=%v report=%#v events=%v", err, report, qbt.events)
	}
}
