package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

type transportTimeout struct{}

func (transportTimeout) Error() string   { return "transport read timed out" }
func (transportTimeout) Timeout() bool   { return true }
func (transportTimeout) Temporary() bool { return true }

type cancelingTransportTimeout struct {
	cancel context.CancelFunc
}

func (e cancelingTransportTimeout) Error() string { return "transport read timed out" }
func (e cancelingTransportTimeout) Timeout() bool {
	e.cancel()
	return true
}
func (cancelingTransportTimeout) Temporary() bool { return true }

type timeoutAfterDeleteQBT struct {
	*fakeQBT
	remainingTimeouts  int
	persistent         bool
	deleted            bool
	removeAfterTimeout bool
}

func (q *timeoutAfterDeleteQBT) Delete(ctx context.Context, hashes []string, deleteFiles bool) error {
	q.deleted = true
	return q.fakeQBT.Delete(ctx, hashes, deleteFiles)
}

func (q *timeoutAfterDeleteQBT) Torrents(ctx context.Context) ([]qbittorrent.Torrent, error) {
	if q.deleted && (q.persistent || q.remainingTimeouts > 0) {
		if q.remainingTimeouts > 0 {
			q.remainingTimeouts--
		}
		return nil, transportTimeout{}
	}
	if q.deleted && q.removeAfterTimeout {
		q.torrents = nil
		q.removeAfterTimeout = false
	}
	return q.fakeQBT.Torrents(ctx)
}

func deletionTestRunner(t *testing.T, base *fakeQBT, qbt QBittorrent, mt *fakeMTeam) Runner {
	t.Helper()
	runner := apiTestRunner(t, base, mt)
	runner.QBittorrent = qbt
	runner.PollInterval = time.Millisecond
	runner.PollTimeout = 100 * time.Millisecond
	return runner
}

func TestExecuteRetriesTransientDeletionReadTimeout(t *testing.T) {
	t.Parallel()
	base, mt := testServices(t)
	qbt := &timeoutAfterDeleteQBT{fakeQBT: base, remainingTimeouts: 1}
	runner := deletionTestRunner(t, base, qbt, mt)

	report, err := runner.Execute(context.Background(), true)
	if err != nil || len(report.Actions) != 1 || !report.Actions[0].Applied {
		t.Fatalf("error=%v report=%#v events=%v", err, report, qbt.events)
	}
	if eventCount(qbt.events, "delete:old") != 1 || prefixIndex(qbt.events, "start:") < 0 {
		t.Fatalf("delete was repeated or candidate was not confirmed started: %v", qbt.events)
	}
}

func TestExecuteStopsAfterPersistentDeletionReadTimeout(t *testing.T) {
	t.Parallel()
	base, mt := testServices(t)
	qbt := &timeoutAfterDeleteQBT{fakeQBT: base, persistent: true}
	runner := deletionTestRunner(t, base, qbt, mt)
	runner.PollTimeout = 10 * time.Millisecond

	report, err := runner.Execute(context.Background(), true)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "wait for deletion and disk space") {
		t.Fatalf("error=%v", err)
	}
	if len(report.Actions) != 1 || report.Actions[0].Applied || eventCount(qbt.events, "delete:old") != 1 || prefixIndex(qbt.events, "start:") >= 0 {
		t.Fatalf("unsafe mutation after persistent timeout: report=%#v events=%v", report, qbt.events)
	}
}

func TestWaitForRegistrationsRemovedRetriesTransientReadTimeout(t *testing.T) {
	t.Parallel()
	base, mt := testServices(t)
	base.torrents = []qbittorrent.Torrent{{Hash: "stale"}}
	qbt := &timeoutAfterDeleteQBT{fakeQBT: base, deleted: true, remainingTimeouts: 1, removeAfterTimeout: true}
	runner := deletionTestRunner(t, base, qbt, mt)

	if err := runner.waitForRegistrationsRemoved(context.Background(), []string{"stale"}); err != nil {
		t.Fatal(err)
	}
	if findHash(qbt.torrents, "stale") != nil || prefixIndex(qbt.events, "delete:") >= 0 || prefixIndex(qbt.events, "start:") >= 0 {
		t.Fatalf("registration wait made an unsafe mutation: %v", qbt.events)
	}
}

func TestExecuteFailsImmediatelyForNonTimeoutDeletionReadError(t *testing.T) {
	t.Parallel()
	base, mt := testServices(t)
	qbt := &nonTimeoutAfterDeleteQBT{fakeQBT: base, err: errors.New("qBittorrent authentication failed")}
	runner := deletionTestRunner(t, base, qbt, mt)

	_, err := runner.Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "qBittorrent authentication failed") || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if eventCount(qbt.events, "delete:old") != 1 || prefixIndex(qbt.events, "start:") >= 0 {
		t.Fatalf("unsafe mutation after non-timeout error: %v", qbt.events)
	}
}

func TestWaitForRemovalsStopsWhenParentContextIsCanceled(t *testing.T) {
	t.Parallel()
	base, mt := testServices(t)
	qbt := &timeoutAfterDeleteQBT{fakeQBT: base, persistent: true, deleted: true}
	runner := deletionTestRunner(t, base, qbt, mt)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := runner.waitForRemovals(ctx, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if prefixIndex(qbt.events, "start:") >= 0 || prefixIndex(qbt.events, "delete:") >= 0 {
		t.Fatalf("canceled verification mutated qBittorrent: %v", qbt.events)
	}
}

func TestExecuteStopsWhenTimeoutCancelsParentDuringDeletionVerification(t *testing.T) {
	t.Parallel()
	base, mt := testServices(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	qbt := &cancelOnTimeoutAfterDeleteQBT{fakeQBT: base, cancel: cancel}
	runner := deletionTestRunner(t, base, qbt, mt)

	report, err := runner.Execute(ctx, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if len(report.Actions) != 1 || report.Actions[0].Applied || eventCount(qbt.events, "delete:old") != 1 || prefixIndex(qbt.events, "start:") >= 0 {
		t.Fatalf("unsafe mutation after deadline-boundary cancellation: report=%#v events=%v", report, qbt.events)
	}
	if len(qbt.torrents) != 1 || qbt.torrents[0].State != "stoppedDL" {
		t.Fatalf("pending addition was not retained stopped: %#v", qbt.torrents)
	}
}

func eventCount(events []string, want string) int {
	count := 0
	for _, event := range events {
		if event == want {
			count++
		}
	}
	return count
}

type nonTimeoutAfterDeleteQBT struct {
	*fakeQBT
	err     error
	deleted bool
}

type cancelOnTimeoutAfterDeleteQBT struct {
	*fakeQBT
	cancel  context.CancelFunc
	deleted bool
}

func (q *cancelOnTimeoutAfterDeleteQBT) Delete(ctx context.Context, hashes []string, deleteFiles bool) error {
	q.deleted = true
	return q.fakeQBT.Delete(ctx, hashes, deleteFiles)
}

func (q *cancelOnTimeoutAfterDeleteQBT) Torrents(ctx context.Context) ([]qbittorrent.Torrent, error) {
	if q.deleted {
		return nil, cancelingTransportTimeout{cancel: q.cancel}
	}
	return q.fakeQBT.Torrents(ctx)
}

func (q *nonTimeoutAfterDeleteQBT) Delete(ctx context.Context, hashes []string, deleteFiles bool) error {
	q.deleted = true
	return q.fakeQBT.Delete(ctx, hashes, deleteFiles)
}

func (q *nonTimeoutAfterDeleteQBT) Torrents(ctx context.Context) ([]qbittorrent.Torrent, error) {
	if q.deleted {
		return nil, q.err
	}
	return q.fakeQBT.Torrents(ctx)
}
