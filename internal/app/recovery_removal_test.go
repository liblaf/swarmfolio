package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

type delayedRegistrationDeleteQBT struct {
	*fakeQBT
	hashes   []string
	polls    int
	removeAt int
}

func (q *delayedRegistrationDeleteQBT) Delete(_ context.Context, hashes []string, deleteFiles bool) error {
	q.deleteFiles = append(q.deleteFiles, deleteFiles)
	q.events = append(q.events, "delete:"+strings.Join(hashes, "|"))
	q.mutations++
	q.hashes = slices.Clone(hashes)
	return nil
}

func (q *delayedRegistrationDeleteQBT) Torrents(ctx context.Context) ([]qbittorrent.Torrent, error) {
	if len(q.hashes) > 0 {
		q.polls++
		if q.removeAt > 0 && q.polls >= q.removeAt {
			q.torrents = slices.DeleteFunc(q.torrents, func(torrent qbittorrent.Torrent) bool {
				return slices.Contains(q.hashes, torrent.Hash)
			})
			q.hashes = nil
		}
	}
	return q.fakeQBT.Torrents(ctx)
}

func TestExecuteWaitsForStalePendingRegistrationBeforeReplanning(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	candidateHash := qbt.addHash
	qbt = pendingQBT("stale")
	qbt.addHash, qbt.addSize = candidateHash, 30
	delayed := &delayedRegistrationDeleteQBT{fakeQBT: qbt, removeAt: 3}
	runner := apiTestRunner(t, qbt, mt)
	runner.QBittorrent = delayed
	report, err := runner.Execute(context.Background(), true)
	if err != nil || delayed.polls < 3 || len(report.Recoveries) != 1 || report.Recoveries[0].Action != "remove" || len(report.Actions) != 1 || !report.Actions[0].Applied {
		t.Fatalf("error=%v polls=%d report=%#v", err, delayed.polls, report)
	}
	if findHash(qbt.torrents, "stale") != nil || findHash(qbt.torrents, qbt.addHash) == nil {
		t.Fatalf("stale registration was not removed before replanning: %#v", qbt.torrents)
	}
}

func TestExecuteDoesNotReportUnconfirmedStaleRemoval(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt = pendingQBT("stale")
	delayed := &delayedRegistrationDeleteQBT{fakeQBT: qbt}
	runner := apiTestRunner(t, qbt, mt)
	runner.QBittorrent = delayed
	report, err := runner.Execute(context.Background(), true)
	if !errors.Is(err, context.DeadlineExceeded) || len(report.Recoveries) != 0 || len(report.Mutations) != 1 || report.Mutations[0].Operation != "delete" || report.Mutations[0].Status != "accepted" {
		t.Fatalf("error=%v report=%#v", err, report)
	}
	requireDeleteFiles(t, report.Mutations[0], false)
	if findHash(qbt.torrents, "stale") == nil || prefixIndex(qbt.events, "start:") >= 0 {
		t.Fatalf("unconfirmed registration removal changed torrent state: events=%v torrents=%#v", qbt.events, qbt.torrents)
	}
}
