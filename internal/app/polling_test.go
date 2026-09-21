package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

type observedQBT struct {
	*fakeQBT
	observations []qbittorrent.Torrent
	calls        int
}

type addCheckingQBT struct {
	*fakeQBT
	permanent            bool
	checkingObserved     bool
	settled              bool
	deletedBeforeSettled bool
}

func (q *addCheckingQBT) Torrents(ctx context.Context) ([]qbittorrent.Torrent, error) {
	torrents, err := q.fakeQBT.Torrents(ctx)
	if err != nil || !strings.Contains(strings.Join(q.events, ","), "add") {
		return torrents, err
	}
	for index := range torrents {
		if torrents[index].Hash != q.addHash {
			continue
		}
		if q.permanent || !q.checkingObserved {
			torrents[index].State = "checkingDL"
			q.checkingObserved = true
			return torrents, nil
		}
		q.settled = true
		return torrents, nil
	}
	return torrents, nil
}

func (q *addCheckingQBT) Delete(ctx context.Context, hashes []string, deleteFiles bool) error {
	if !q.settled {
		q.deletedBeforeSettled = true
	}
	return q.fakeQBT.Delete(ctx, hashes, deleteFiles)
}

func (q *observedQBT) Torrents(context.Context) ([]qbittorrent.Torrent, error) {
	q.calls++
	index := q.calls - 1
	if index >= len(q.observations) {
		index = len(q.observations) - 1
	}
	return []qbittorrent.Torrent{q.observations[index]}, nil
}

func TestWaitForTorrentWaitsForInitialChecking(t *testing.T) {
	checking := qbittorrent.Torrent{Hash: "new", State: "ChEcKiNgDL"}
	stopped := checking
	stopped.State = "stoppedDL"
	qbt := &observedQBT{fakeQBT: &fakeQBT{}, observations: []qbittorrent.Torrent{checking, stopped}}
	runner := Runner{QBittorrent: qbt, PollInterval: time.Millisecond, PollTimeout: 50 * time.Millisecond}

	torrent, _, err := runner.waitForTorrent(context.Background(), "new")
	if err != nil {
		t.Fatal(err)
	}
	if torrent.State != "stoppedDL" || qbt.calls < 2 {
		t.Fatalf("torrent=%#v calls=%d", torrent, qbt.calls)
	}
}

func TestWaitForTorrentTimesOutWhileCheckingWithoutMutation(t *testing.T) {
	qbt := &observedQBT{
		fakeQBT:      &fakeQBT{},
		observations: []qbittorrent.Torrent{{Hash: "new", State: "checkingResumeData"}},
	}
	runner := Runner{QBittorrent: qbt, PollInterval: time.Millisecond, PollTimeout: 10 * time.Millisecond}

	_, _, err := runner.waitForTorrent(context.Background(), "new")
	if err == nil || !strings.Contains(err.Error(), "transient checking state \"checkingResumeData\"") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(strings.Join(qbt.events, ","), "delete:") || strings.Contains(strings.Join(qbt.events, ","), "start:") {
		t.Fatalf("polling mutated qBittorrent: events=%v", qbt.events)
	}
}

func TestWaitForTorrentReturnsUnexpectedStableState(t *testing.T) {
	qbt := &observedQBT{fakeQBT: &fakeQBT{}, observations: []qbittorrent.Torrent{{Hash: "new", State: "downloading", Category: "user-managed"}}}
	runner := Runner{QBittorrent: qbt, PollInterval: time.Millisecond, PollTimeout: 50 * time.Millisecond}

	torrent, _, err := runner.waitForTorrent(context.Background(), "new")
	if err != nil {
		t.Fatal(err)
	}
	if torrent.State != "downloading" || torrent.Category != "user-managed" || qbt.calls != 1 {
		t.Fatalf("torrent=%#v calls=%d", torrent, qbt.calls)
	}
}

func TestExecuteWaitsForInitialCheckingBeforeReplacingIncumbent(t *testing.T) {
	qbt, mt := testServices(t)
	polling := &addCheckingQBT{fakeQBT: qbt}
	runner := testRunner(qbt, mt)
	runner.QBittorrent = polling
	runner.PollInterval = time.Millisecond
	runner.PollTimeout = 50 * time.Millisecond

	report, err := runner.Execute(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Actions[0].Applied || !polling.checkingObserved || !polling.settled || polling.deletedBeforeSettled {
		t.Fatalf("report=%#v checking=%t settled=%t deletedBeforeSettled=%t events=%v", report, polling.checkingObserved, polling.settled, polling.deletedBeforeSettled, qbt.events)
	}
	if len(qbt.torrents) != 1 || qbt.torrents[0].Hash != qbt.addHash || qbt.torrents[0].State != "downloading" {
		t.Fatalf("torrents=%#v", qbt.torrents)
	}
}

func TestExecuteDoesNotReplaceIncumbentWhenCheckingNeverSettles(t *testing.T) {
	qbt, mt := testServices(t)
	polling := &addCheckingQBT{fakeQBT: qbt, permanent: true}
	runner := testRunner(qbt, mt)
	runner.QBittorrent = polling
	runner.PollInterval = time.Millisecond
	runner.PollTimeout = 10 * time.Millisecond

	_, err := runner.Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "transient checking state") {
		t.Fatalf("error=%v", err)
	}
	if polling.deletedBeforeSettled || strings.Contains(strings.Join(qbt.events, ","), "delete:old") || strings.Contains(strings.Join(qbt.events, ","), "start:") {
		t.Fatalf("incumbent or candidate mutated while checking: events=%v", qbt.events)
	}
	if len(qbt.torrents) != 2 || qbt.torrents[0].Hash != "old" || qbt.torrents[1].Hash != qbt.addHash {
		t.Fatalf("torrents=%#v", qbt.torrents)
	}
}
