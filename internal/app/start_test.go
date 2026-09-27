package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

type delayedStartQBT struct {
	*fakeQBT
	hashes       []string
	polls        int
	transitionAt int
	transition   func(*fakeQBT, []string)
}

func (q *delayedStartQBT) Start(_ context.Context, hashes []string) error {
	q.events = append(q.events, "start:"+strings.Join(hashes, "|"))
	q.mutations++
	q.hashes = slices.Clone(hashes)
	return nil
}

func (q *delayedStartQBT) Torrents(ctx context.Context) ([]qbittorrent.Torrent, error) {
	if len(q.hashes) > 0 {
		q.polls++
		if q.polls == q.transitionAt {
			q.transition(q.fakeQBT, q.hashes)
		}
	}
	return q.fakeQBT.Torrents(ctx)
}

func TestExecuteWaitsForAcknowledgedStartToTakeEffect(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"addition", "recovery"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			qbt, mt := testServices(t)
			if operation == "recovery" {
				qbt = pendingQBT(qbt.addHash)
			}
			delayed := &delayedStartQBT{fakeQBT: qbt, transitionAt: 3, transition: func(q *fakeQBT, hashes []string) {
				for index := range q.torrents {
					if slices.Contains(hashes, q.torrents[index].Hash) {
						q.torrents[index].State = "queuedDL"
					}
				}
			}}
			runner := apiTestRunner(t, qbt, mt)
			runner.QBittorrent = delayed
			report, err := runner.Execute(context.Background(), true)
			if err != nil || delayed.polls < 3 || qbt.torrents[0].State != "queuedDL" {
				t.Fatalf("error=%v polls=%d report=%#v torrents=%#v", err, delayed.polls, report, qbt.torrents)
			}
			if operation == "addition" && !report.Actions[0].Applied {
				t.Fatalf("confirmed addition not marked applied: %#v", report)
			}
		})
	}
}

func TestExecuteFailsWhenAcknowledgedStartNeverTakesEffect(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"addition", "recovery"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			qbt, mt := testServices(t)
			if operation == "recovery" {
				qbt = pendingQBT(qbt.addHash)
			}
			runner := apiTestRunner(t, qbt, mt)
			runner.QBittorrent = &delayedStartQBT{fakeQBT: qbt}
			report, err := runner.Execute(context.Background(), true)
			if !errors.Is(err, context.DeadlineExceeded) || report.Error == "" || qbt.torrents[0].State != "stoppedDL" {
				t.Fatalf("error=%v report=%#v torrents=%#v", err, report, qbt.torrents)
			}
			last := report.Mutations[len(report.Mutations)-1]
			if last.Operation != "start" || last.Status != "accepted" {
				t.Fatalf("lost API acknowledgement receipt: %#v", report)
			}
			if operation == "addition" && report.Actions[0].Applied {
				t.Fatalf("unstarted addition marked applied: %#v", report)
			}
			if operation == "recovery" && len(report.Recoveries) != 0 {
				t.Fatalf("unconfirmed resume marked recovered: %#v", report)
			}
		})
	}
}

func TestExecuteRejectsChangedTorrentAfterStartAcknowledgement(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"disappeared", "error", "category", "size"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			qbt, mt := testServices(t)
			delayed := &delayedStartQBT{fakeQBT: qbt, transitionAt: 1, transition: func(q *fakeQBT, hashes []string) {
				if change == "disappeared" {
					q.torrents = nil
					return
				}
				torrent := findHash(q.torrents, hashes[0])
				torrent.State = "downloading"
				switch change {
				case "error":
					torrent.State = "error"
				case "category":
					torrent.Category = "user-managed"
				case "size":
					torrent.Size++
				}
			}}
			runner := apiTestRunner(t, qbt, mt)
			runner.QBittorrent = delayed
			report, err := runner.Execute(context.Background(), true)
			if err == nil || !strings.Contains(err.Error(), "confirm candidate") || report.Actions[0].Applied {
				t.Fatalf("error=%v report=%#v", err, report)
			}
		})
	}
}
