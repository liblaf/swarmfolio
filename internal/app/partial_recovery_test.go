package app

import (
	"context"
	"slices"
	"testing"
)

func TestExecuteResumesStoppedPartialPendingDownload(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"stoppedDL", "pausedDL"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			qbt, mt := nonTimedTestServices(t)
			qbt = pendingQBT(qbt.addHash)
			qbt.torrents[0].Progress = .5
			qbt.torrents[0].AmountLeft = 15
			qbt.torrents[0].State = state

			report, err := testRunner(qbt, mt).Execute(context.Background(), true)
			if err != nil || len(report.Recoveries) != 1 || report.Recoveries[0].Action != "resume" || len(report.Mutations) != 1 || report.Mutations[0].Operation != "start" {
				t.Fatalf("error=%v report=%#v", err, report)
			}
			if qbt.torrents[0].State != "downloading" || prefixIndex(qbt.events, "add") >= 0 || prefixIndex(qbt.events, "delete:") >= 0 {
				t.Fatalf("partial pending torrent was not only resumed: events=%v torrents=%#v", qbt.events, qbt.torrents)
			}
		})
	}
}

func TestExecuteRemovesStoppedPartialPendingWithoutDeletingFiles(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"stale", "over budget"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			qbt, mt := nonTimedTestServices(t)
			qbt = pendingQBT(qbt.addHash)
			qbt.torrents[0].Progress = .5
			qbt.torrents[0].AmountLeft = 15
			runner := testRunner(qbt, mt)
			if reason == "stale" {
				mt.results = nil
			} else {
				runner.Config.Portfolio.BudgetBytes = 20
			}
			report, err := runner.Execute(context.Background(), true)
			if err != nil || len(report.Recoveries) != 1 || report.Recoveries[0].Action != "remove" || !slices.Equal(qbt.deleteFiles, []bool{false}) || prefixIndex(qbt.events, "start:") >= 0 {
				t.Fatalf("error=%v report=%#v events=%v deleteFiles=%v", err, report, qbt.events, qbt.deleteFiles)
			}
		})
	}
}

func TestExecuteDoesNotRecoverCompletedOrUnmanagedStoppedTorrent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*fakeQBT)
	}{
		{name: "completed", mutate: func(qbt *fakeQBT) {
			qbt.torrents[0].Progress = 1
			qbt.torrents[0].AmountLeft = 0
			qbt.torrents[0].State = "stoppedUP"
		}},
		{name: "unmanaged", mutate: func(qbt *fakeQBT) {
			qbt.torrents[0].Progress = .5
			qbt.torrents[0].AmountLeft = 15
			qbt.torrents[0].Category = "user-managed"
			qbt.torrents[0].AutoTMM = false
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			qbt, mt := nonTimedTestServices(t)
			qbt = pendingQBT(qbt.addHash)
			test.mutate(qbt)
			report, err := testRunner(qbt, mt).Execute(context.Background(), true)
			if err != nil || len(report.Recoveries) != 0 || len(report.Mutations) != 0 || prefixIndex(qbt.events, "start:") >= 0 {
				t.Fatalf("error=%v report=%#v events=%v", err, report, qbt.events)
			}
		})
	}
}
