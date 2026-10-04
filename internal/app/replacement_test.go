package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/liblaf/swarmfolio/internal/disk"
	"github.com/liblaf/swarmfolio/internal/metainfo"
	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

func TestExecuteRevalidatesReplacementAfterPromotionRefresh(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"removed", "category", "started", "size"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			qbt, mt := nonTimedTestServices(t)
			runner := testRunner(qbt, mt)
			searches := 0
			runner.MTeam = &changingSearchMTeam{fakeMTeam: mt, change: func() {
				searches++
				if searches != 3 {
					return
				}
				switch change {
				case "removed":
					qbt.torrents = qbt.torrents[:1]
				case "category":
					qbt.torrents[1].Category = "user-managed"
				case "started":
					qbt.torrents[1].State = "downloading"
				case "size":
					qbt.torrents[1].Size--
					qbt.torrents[1].AmountLeft--
				}
			}}
			_, err := runner.Execute(context.Background(), true)
			if err == nil || !strings.Contains(err.Error(), "before replacement") {
				t.Fatalf("error = %v", err)
			}
			if findHash(qbt.torrents, "old") == nil || prefixIndex(qbt.events, "delete:old") >= 0 || prefixIndex(qbt.events, "start:") >= 0 {
				t.Fatalf("invalid replacement caused destructive action: %v", qbt.events)
			}
		})
	}
}

func TestExecuteWaitsForReportedSpaceAfterDeletion(t *testing.T) {
	t.Parallel()
	qbt, mt := nonTimedTestServices(t)
	runner := apiTestRunner(t, qbt, mt)
	probes := 0
	qbt.freeSpace = func() (int64, error) {
		if prefixIndex(qbt.events, "delete:old") >= 0 {
			probes++
			if probes <= 2 {
				if prefixIndex(qbt.events, "start:") >= 0 {
					t.Fatal("started before space was available")
				}
				return 30, nil
			}
		}
		return qbt.free(), nil
	}
	report, err := runner.Execute(context.Background(), true)
	if err != nil || probes < 3 || len(report.Actions) != 1 || !report.Actions[0].Applied {
		t.Fatalf("error=%v probes=%d report=%#v", err, probes, report)
	}
}

func TestExecuteAppliesUsingQBitFreeSpace(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"empty", "completed", "pending", "downloading"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			qbt, mt := nonTimedTestServices(t)
			wantMutations := 3
			switch state {
			case "empty":
				qbt.torrents = nil
				wantMutations = 2
			case "pending", "downloading":
				qbt = pendingQBT(qbt.addHash)
				wantMutations = 1
				if state == "downloading" {
					qbt.torrents[0].State = "downloading"
					wantMutations = 0
				}
			}
			runner := apiTestRunner(t, qbt, mt)
			queries := 0
			qbt.freeSpace = func() (int64, error) {
				queries++
				return qbt.free(), nil
			}
			report, err := runner.Execute(context.Background(), true)
			if err != nil || report.Mode != "apply" || queries == 0 || qbt.mutations != wantMutations || len(report.Mutations) != wantMutations {
				t.Fatalf("error=%v queries=%d report=%#v events=%v", err, queries, report, qbt.events)
			}
		})
	}
}

func TestExecuteLeavesPendingWhenReportedSpaceDoesNotRecover(t *testing.T) {
	t.Parallel()
	qbt, mt := nonTimedTestServices(t)
	runner := apiTestRunner(t, qbt, mt)
	qbt.freeSpace = func() (int64, error) { return 30, nil }
	report, err := runner.Execute(context.Background(), true)
	if !errors.Is(err, context.DeadlineExceeded) || prefixIndex(qbt.events, "start:") >= 0 || len(report.Mutations) != 2 || report.Actions[0].Applied {
		t.Fatalf("error=%v report=%#v events=%v", err, report, qbt.events)
	}
	if len(qbt.torrents) != 1 || qbt.torrents[0].State != "stoppedDL" {
		t.Fatalf("pending candidate was not retained stopped: %#v", qbt.torrents)
	}
}

func apiTestRunner(t *testing.T, qbt *fakeQBT, mt *fakeMTeam) Runner {
	t.Helper()
	runner := testRunner(qbt, mt)
	runner.Config.Portfolio.DiskPath = ""
	runner.ProbeDisk = func(string) (disk.Space, error) {
		t.Fatal("API budget unexpectedly probed the host filesystem")
		return disk.Space{}, nil
	}
	return runner
}

type delayedDeleteQBT struct {
	*fakeQBT
	deleted []string
	polls   int
}

func (q *delayedDeleteQBT) Delete(_ context.Context, hashes []string, _ bool) error {
	q.events = append(q.events, "delete:"+strings.Join(hashes, "|"))
	q.mutations++
	q.deleted = slices.Clone(hashes)
	return nil
}

func (q *delayedDeleteQBT) Torrents(ctx context.Context) ([]qbittorrent.Torrent, error) {
	if len(q.deleted) > 0 {
		q.polls++
		if q.polls >= 3 {
			q.torrents = slices.DeleteFunc(q.torrents, func(torrent qbittorrent.Torrent) bool { return slices.Contains(q.deleted, torrent.Hash) })
			q.deleted = nil
		}
	}
	return q.fakeQBT.Torrents(ctx)
}

func TestExecuteWaitsForDeletedTorrentToDisappear(t *testing.T) {
	t.Parallel()
	qbt, mt := nonTimedTestServices(t)
	delayed := &delayedDeleteQBT{fakeQBT: qbt}
	runner := testRunner(qbt, mt)
	runner.QBittorrent = delayed
	report, err := runner.Execute(context.Background(), true)
	if err != nil || delayed.polls != 3 || !report.Actions[0].Applied {
		t.Fatalf("error=%v polls=%d report=%#v", err, delayed.polls, report)
	}
}

func TestExecuteCancelsDiskSpaceWait(t *testing.T) {
	t.Parallel()
	qbt, mt := nonTimedTestServices(t)
	runner := apiTestRunner(t, qbt, mt)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	qbt.freeSpace = func() (int64, error) {
		if prefixIndex(qbt.events, "delete:old") >= 0 {
			cancel()
			return 0, nil
		}
		return qbt.free(), nil
	}
	report, err := runner.Execute(ctx, true)
	if !errors.Is(err, context.Canceled) || prefixIndex(qbt.events, "start:") >= 0 || len(report.Mutations) != 2 {
		t.Fatalf("error=%v events=%v report=%#v", err, qbt.events, report)
	}
}

func TestPreallocationReservesExistingOutstandingDownloads(t *testing.T) {
	t.Parallel()
	qbt, mt := nonTimedTestServices(t)
	qbt.preallocate = true
	qbt.torrents = append(qbt.torrents, qbittorrent.Torrent{
		Hash: "other", Size: 50, AmountLeft: 50, Category: "user-managed", SavePath: "/downloads/user",
		AddedOn: appNow.Add(-time.Hour), State: "downloading",
	})
	mt.results[0].Size = 60
	mt.metainfo = []byte("d4:infod6:lengthi60e4:name3:newee")
	var err error
	qbt.addHash, err = metainfo.InfoHash(mt.metainfo)
	if err != nil {
		t.Fatal(err)
	}
	qbt.addSize = 60
	runner := testRunner(qbt, mt)
	runner.ProbeDisk = func(string) (disk.Space, error) { return disk.Space{CapacityBytes: 170, FreeBytes: 100}, nil }
	report, err := runner.Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "safely preallocated") || qbt.mutations != 0 || len(report.Mutations) != 0 {
		t.Fatalf("error=%v events=%v report=%#v", err, qbt.events, report)
	}
}

func TestExecuteRechecksReportedSpaceAfterFinalPromotionRefresh(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"addition", "recovery"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			qbt, mt := nonTimedTestServices(t)
			finalSearch := 4
			if operation == "recovery" {
				qbt = pendingQBT(qbt.addHash)
				finalSearch = 2
			}
			runner := apiTestRunner(t, qbt, mt)
			searches := 0
			runner.MTeam = &changingSearchMTeam{fakeMTeam: mt, change: func() { searches++ }}
			qbt.freeSpace = func() (int64, error) {
				free := qbt.free()
				if searches >= finalSearch {
					free = 10 // qBittorrent updates its cached estimate during the HTTP refresh.
				}
				return free, nil
			}
			report, err := runner.Execute(context.Background(), true)
			if err == nil || !strings.Contains(err.Error(), "before start") || prefixIndex(qbt.events, "start:") >= 0 {
				t.Fatalf("error=%v events=%v report=%#v", err, qbt.events, report)
			}
		})
	}
}
