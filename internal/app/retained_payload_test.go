package app

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/liblaf/swarmfolio/internal/disk"
	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

func TestExecuteReusesRetainedStoppedPayloadForReplacement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		progress   float64
		amountLeft int64
		state      string
	}{
		{name: "partial", progress: .5, amountLeft: 15, state: "stoppedDL"},
		{name: "complete", progress: 1, amountLeft: 0, state: "stoppedUP"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			qbt, mt := testServices(t)
			qbt.onAdd = func(q *fakeQBT) {
				added := &q.torrents[len(q.torrents)-1]
				added.Progress = test.progress
				added.AmountLeft = test.amountLeft
				added.State = test.state
			}
			runner := testRunner(qbt, mt)
			runner.ProbeDisk = func(string) (disk.Space, error) {
				used := int64(30 - test.amountLeft) // Retained matching payload, even while unregistered.
				if findHash(qbt.torrents, "old") != nil {
					used += 70
				}
				return disk.Space{CapacityBytes: 100, FreeBytes: 100 - used}, nil
			}

			report, err := runner.Execute(context.Background(), true)
			if err != nil || len(report.Actions) != 1 || !report.Actions[0].Applied {
				t.Fatalf("error=%v report=%#v", err, report)
			}
			if findHash(qbt.torrents, "old") != nil || findHash(qbt.torrents, qbt.addHash) == nil {
				t.Fatalf("replacement did not finish: torrents=%#v", qbt.torrents)
			}
			if got := []string{report.Mutations[0].Operation, report.Mutations[1].Operation, report.Mutations[2].Operation}; !slices.Equal(got, []string{"add", "delete", "start"}) || !slices.Equal(qbt.deleteFiles, []bool{true}) {
				t.Fatalf("mutations=%#v deleteFiles=%v", report.Mutations, qbt.deleteFiles)
			}
		})
	}
}

func TestExecuteDoesNotReportPartialRecoveryBeforeStartTakesEffect(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt = pendingQBT(qbt.addHash)
	qbt.torrents[0].Progress = .5
	qbt.torrents[0].AmountLeft = 15
	delayed := &delayedStartQBT{fakeQBT: qbt}
	runner := testRunner(qbt, mt)
	runner.QBittorrent = delayed

	report, err := runner.Execute(context.Background(), true)
	if !errors.Is(err, context.DeadlineExceeded) || len(report.Recoveries) != 0 || len(report.Mutations) != 1 || report.Mutations[0].Operation != "start" || report.Mutations[0].Status != "accepted" {
		t.Fatalf("error=%v report=%#v", err, report)
	}
}

func TestWaitForTorrentWaitsForCompleteCheckingState(t *testing.T) {
	t.Parallel()
	checking := qbittorrent.Torrent{Hash: "new", State: "checkingUP"}
	stopped := checking
	stopped.State = "stoppedUP"
	qbt := &observedQBT{fakeQBT: &fakeQBT{}, observations: []qbittorrent.Torrent{checking, stopped}}
	runner := Runner{QBittorrent: qbt, PollInterval: time.Millisecond, PollTimeout: 50 * time.Millisecond}

	torrent, _, err := runner.waitForTorrent(context.Background(), "new")
	if err != nil || torrent.State != "stoppedUP" || qbt.calls < 2 {
		t.Fatalf("error=%v torrent=%#v calls=%d", err, torrent, qbt.calls)
	}
}
