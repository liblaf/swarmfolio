package app

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

func TestAutomaticRetryResumesAfterSpaceEstimateRecovers(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	runner := apiTestRunner(t, qbt, mt)
	qbt.freeSpace = func() (int64, error) { return 30, nil }
	first, err := runner.Execute(context.Background(), true)
	if !errors.Is(err, context.DeadlineExceeded) || len(first.Mutations) != 2 || first.Actions[0].Applied {
		t.Fatalf("first error=%v report=%#v", err, first)
	}
	if len(qbt.torrents) != 1 || qbt.torrents[0].State != "stoppedDL" {
		t.Fatalf("candidate was not retained stopped: %#v", qbt.torrents)
	}

	// Simulate the next service invocation after qBittorrent refreshes its cache.
	// Nobody changes the torrent's state between attempts.
	qbt.freeSpace = nil
	qbt.events = nil
	second, err := apiTestRunner(t, qbt, mt).Execute(context.Background(), true)
	if err != nil || len(second.Recoveries) != 1 || second.Recoveries[0].Action != "resume" || len(second.Mutations) != 1 || second.Mutations[0].Operation != "start" {
		t.Fatalf("retry error=%v report=%#v", err, second)
	}
	if qbt.torrents[0].State != "downloading" || prefixIndex(qbt.events, "add") >= 0 || prefixIndex(qbt.events, "delete:") >= 0 {
		t.Fatalf("retry repeated replacement instead of resuming: events=%v torrents=%#v", qbt.events, qbt.torrents)
	}
}

func TestAutomaticRetryReconcilesLostMutationResponses(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"add", "delete", "start"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			qbt, mt := testServices(t)
			runner := apiTestRunner(t, qbt, mt)
			runner.QBittorrent = &lostMutationResponseQBT{fakeQBT: qbt, operation: operation}
			first, err := runner.Execute(context.Background(), true)
			if !errors.Is(err, errMutationResponse) || first.Mutations[len(first.Mutations)-1].Status != "unconfirmed" {
				t.Fatalf("first error=%v report=%#v", err, first)
			}

			qbt.events = nil
			second, err := apiTestRunner(t, qbt, mt).Execute(context.Background(), true)
			if err != nil || len(qbt.torrents) != 1 || qbt.torrents[0].Hash != qbt.addHash || qbt.torrents[0].State != "downloading" {
				t.Fatalf("retry error=%v report=%#v torrents=%#v", err, second, qbt.torrents)
			}
			switch operation {
			case "add":
				// An interrupted replacement can be replanned automatically. Its
				// existing empty registration is removed while retaining files.
				if len(second.Recoveries) != 1 || second.Recoveries[0].Action != "remove" || len(second.Actions) != 1 || !second.Actions[0].Applied {
					t.Fatalf("replacement was not replanned: %#v", second)
				}
				requireDeleteFiles(t, second.Mutations[0], false)
			case "delete":
				if len(second.Mutations) != 1 || second.Mutations[0].Operation != "start" {
					t.Fatalf("retry repeated accepted deletion: %#v", second)
				}
			case "start":
				if len(second.Mutations) != 0 {
					t.Fatalf("retry mutated already-running download: %#v", second)
				}
			}
		})
	}
}

func TestAutomaticRetryDoesNotResumeExpiredPendingOffer(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt = pendingQBT(qbt.addHash)
	qbt.torrents = append(qbt.torrents, qbittorrent.Torrent{
		Hash: "user", AddedOn: appNow, Size: 10, Progress: 1,
		Category: "user-managed", ContentPath: qbt.torrents[0].ContentPath,
	})
	mt.results[0].DiscountEndTime = appNow
	report, err := apiTestRunner(t, qbt, mt).Execute(context.Background(), true)
	if err != nil || len(report.Recoveries) != 1 || report.Recoveries[0].Action != "remove" || !slices.Equal(qbt.deleteFiles, []bool{false}) || prefixIndex(qbt.events, "start:") >= 0 {
		t.Fatalf("error=%v report=%#v events=%v", err, report, qbt.events)
	}
	if len(qbt.torrents) != 1 || qbt.torrents[0].Hash != "user" {
		t.Fatalf("unrelated shared content torrent was removed: %#v", qbt.torrents)
	}
}
