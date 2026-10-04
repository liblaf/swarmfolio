package app

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

func TestExecuteRollsBackCollidingAdditionWithoutDeletingFiles(t *testing.T) {
	t.Parallel()
	for _, collision := range []string{"same file", "parent directory", "child file", "missing path", "other category"} {
		t.Run(collision, func(t *testing.T) {
			t.Parallel()
			qbt, mt := nonTimedTestServices(t)
			if collision == "other category" {
				qbt.torrents[0].Category = "user-managed"
				qbt.torrents[0].Size = 30
			}
			qbt.onAdd = func(q *fakeQBT) {
				switch collision {
				case "same file", "other category":
					q.torrents[1].ContentPath = q.torrents[0].ContentPath
				case "parent directory":
					q.torrents[1].ContentPath = "/downloads/swarmfolio"
				case "child file":
					q.torrents[1].ContentPath = q.torrents[0].ContentPath + "/file"
				case "missing path":
					q.torrents[1].ContentPath = ""
				}
			}
			report, err := testRunner(qbt, mt).Execute(context.Background(), true)
			if err == nil || !strings.Contains(err.Error(), "content") || len(report.Mutations) != 2 || report.Actions[0].Applied {
				t.Fatalf("error=%v report=%#v", err, report)
			}
			if !slices.Equal(qbt.deleteFiles, []bool{false}) || len(qbt.torrents) != 1 || qbt.torrents[0].Hash != "old" || prefixIndex(qbt.events, "start:") >= 0 {
				t.Fatalf("unsafe collision rollback: deleteFiles=%v events=%v torrents=%#v", qbt.deleteFiles, qbt.events, qbt.torrents)
			}
		})
	}
}

func TestExecuteRejectsSharedCompletedRemovalBeforeAdding(t *testing.T) {
	t.Parallel()
	qbt, mt := nonTimedTestServices(t)
	qbt.torrents = append(qbt.torrents, qbittorrent.Torrent{
		Hash: "user", AddedOn: appNow, Category: "user-managed", Size: 1, AmountLeft: 1,
		SavePath: "/downloads/user", ContentPath: qbt.torrents[0].ContentPath,
		State: "downloading",
	})
	report, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "overlaps") || qbt.mutations != 0 || len(report.Mutations) != 0 {
		t.Fatalf("error=%v report=%#v events=%v", err, report, qbt.events)
	}
}

func TestExecuteRefusesCollidingPendingResume(t *testing.T) {
	t.Parallel()
	qbt, mt := nonTimedTestServices(t)
	qbt = pendingQBT(qbt.addHash)
	qbt.torrents = append(qbt.torrents, qbittorrent.Torrent{
		Hash: "user", AddedOn: appNow, Category: "user-managed", Size: 30, Progress: 1,
		SavePath: "/downloads/user", ContentPath: qbt.torrents[0].ContentPath,
		State: "stoppedUP",
	})
	report, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "overlaps") || qbt.mutations != 0 || len(report.Mutations) != 0 {
		t.Fatalf("error=%v report=%#v events=%v", err, report, qbt.events)
	}
}

func TestExecuteRetainsFilesWhenRemovingSharedPendingTorrent(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"stale", "over budget"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			qbt, mt := nonTimedTestServices(t)
			qbt = pendingQBT(qbt.addHash)
			qbt.torrents = append(qbt.torrents, qbittorrent.Torrent{
				Hash: "user", AddedOn: appNow, Category: "user-managed", Size: 30, Progress: 1,
				SavePath: "/downloads/user", ContentPath: qbt.torrents[0].ContentPath,
				State: "stoppedUP",
			})
			runner := testRunner(qbt, mt)
			runner.Config.Policy.MaxAdditions = 0
			if reason == "stale" {
				mt.results = nil
			} else {
				runner.Config.Portfolio.BudgetBytes = 20
			}
			report, err := runner.Execute(context.Background(), true)
			if err != nil || !slices.Equal(qbt.deleteFiles, []bool{false}) || len(qbt.torrents) != 1 || qbt.torrents[0].Hash != "user" || len(report.Recoveries) != 1 || report.Recoveries[0].Action != "remove" {
				t.Fatalf("error=%v deleteFiles=%v report=%#v torrents=%#v", err, qbt.deleteFiles, report, qbt.torrents)
			}
		})
	}
}

func TestExecuteRechecksContentAfterPromotionRefresh(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"before deletion", "before start"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			qbt, mt := nonTimedTestServices(t)
			mt.results[0].DiscountEndTime = time.Time{}
			runner := testRunner(qbt, mt)
			searches := 0
			runner.MTeam = &changingSearchMTeam{fakeMTeam: mt, change: func() {
				searches++
				if stage == "before deletion" && searches == 3 {
					qbt.torrents[1].ContentPath = qbt.torrents[0].ContentPath
				}
				if stage == "before start" && searches == 4 {
					qbt.torrents = append(qbt.torrents, qbittorrent.Torrent{
						Hash: "user", AddedOn: appNow, Category: "user-managed", ContentPath: qbt.torrents[0].ContentPath,
					})
				}
			}}
			_, err := runner.Execute(context.Background(), true)
			if err == nil || !strings.Contains(err.Error(), "overlaps") || prefixIndex(qbt.events, "start:") >= 0 {
				t.Fatalf("error=%v events=%v", err, qbt.events)
			}
			if stage == "before deletion" && (findHash(qbt.torrents, "old") == nil || !slices.Equal(qbt.deleteFiles, []bool{false})) {
				t.Fatalf("incumbent deleted: events=%v", qbt.events)
			}
		})
	}
}
