package app

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/liblaf/swarmfolio/internal/mteam"
	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

// staleFreeSpace reports a short budget for the first calls, modelling space
// that an interrupted run reclaimed but qBittorrent has not reported yet.
func staleFreeSpace(qbt *fakeQBT, staleCalls int, stale int64) (func() (int64, error), *int) {
	calls := 0
	return func() (int64, error) {
		calls++
		if calls <= staleCalls {
			return stale, nil
		}
		return qbt.free(), nil
	}, &calls
}

func TestRecoveryWaitsForReclaimedSpaceInsteadOfReplacingAgain(t *testing.T) {
	t.Parallel()
	qbt, mt := nonTimedTestServices(t)
	qbt = pendingQBT(qbt.addHash)
	// A 40-byte estimate leaves a 15-byte limit for the 30-byte pending torrent.
	// The stale window covers every snapshot before the recovery budget check
	// (two today) with room to spare, so only the wait can outlast it.
	const staleCalls = 5
	var calls *int
	qbt.freeSpace, calls = staleFreeSpace(qbt, staleCalls, 40)
	runner := apiTestRunner(t, qbt, mt)

	report, err := runner.Execute(context.Background(), true)
	if err != nil || len(report.Recoveries) != 1 || report.Recoveries[0].Action != "resume" || *calls <= staleCalls {
		t.Fatalf("error=%v free-space calls=%d report=%#v", err, *calls, report)
	}
	if prefixIndex(qbt.events, "delete:") >= 0 || index(qbt.events, "add") >= 0 || qbt.torrents[0].State != "downloading" {
		t.Fatalf("recovery replaced the pending torrent instead of resuming it: events=%v torrents=%#v", qbt.events, qbt.torrents)
	}
}

func TestRecoveryRemovesPendingTorrentThatStaysOverBudget(t *testing.T) {
	t.Parallel()
	qbt, mt := nonTimedTestServices(t)
	qbt = pendingQBT(qbt.addHash)
	qbt.freeSpace = func() (int64, error) { return 40, nil }
	runner := apiTestRunner(t, qbt, mt)
	runner.Config.Policy.MaxAdditions = 0

	report, err := runner.Execute(context.Background(), true)
	if err != nil || len(report.Recoveries) != 1 || report.Recoveries[0].Action != "remove" || !slices.Equal(qbt.deleteFiles, []bool{false}) {
		t.Fatalf("error=%v report=%#v deleteFiles=%v", err, report, qbt.deleteFiles)
	}
}

type unreadableAfterQBT struct {
	*fakeQBT
	readable int
}

func (q *unreadableAfterQBT) Torrents(ctx context.Context) ([]qbittorrent.Torrent, error) {
	if q.readable <= 0 {
		return nil, transportTimeout{}
	}
	q.readable--
	return q.fakeQBT.Torrents(ctx)
}

func TestRecoveryDoesNotRemovePendingTorrentWhileQBittorrentIsUnreadable(t *testing.T) {
	t.Parallel()
	base, mt := nonTimedTestServices(t)
	base = pendingQBT(base.addHash)
	base.freeSpace = func() (int64, error) { return 40, nil }
	// Initial snapshot, pre-mutation verification, and the recovery snapshot.
	qbt := &unreadableAfterQBT{fakeQBT: base, readable: 3}
	runner := deletionTestRunner(t, base, qbt, mt)
	runner.PollTimeout = 10 * runner.PollInterval

	report, err := runner.Execute(context.Background(), true)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "no qBittorrent read succeeded during the wait") {
		t.Fatalf("error=%v", err)
	}
	if len(report.Mutations) != 0 || len(base.torrents) != 1 || base.torrents[0].State != "stoppedDL" {
		t.Fatalf("blind recovery mutated qBittorrent: report=%#v torrents=%#v", report, base.torrents)
	}
}

func TestDeletionTimeoutReportsLastObservedState(t *testing.T) {
	t.Parallel()
	qbt, mt := nonTimedTestServices(t)
	qbt.freeSpace = func() (int64, error) { return 30, nil }

	_, err := apiTestRunner(t, qbt, mt).Execute(context.Background(), true)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "last observed: projected") {
		t.Fatalf("error=%v", err)
	}
}

// timeoutAfterEventQBT times out a number of torrent reads right after the
// first event with the given prefix, modelling a Web API stalled by disk I/O.
type timeoutAfterEventQBT struct {
	*fakeQBT
	prefix    string
	remaining int
}

func (q *timeoutAfterEventQBT) Torrents(ctx context.Context) ([]qbittorrent.Torrent, error) {
	if q.remaining > 0 && prefixIndex(q.events, q.prefix) >= 0 {
		q.remaining--
		return nil, transportTimeout{}
	}
	return q.fakeQBT.Torrents(ctx)
}

func TestTransientReadTimeoutsDoNotFailAddOrStartConfirmation(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"add", "start:"} {
		t.Run(prefix, func(t *testing.T) {
			t.Parallel()
			base, mt := nonTimedTestServices(t)
			qbt := &timeoutAfterEventQBT{fakeQBT: base, prefix: prefix, remaining: 2}
			report, err := deletionTestRunner(t, base, qbt, mt).Execute(context.Background(), true)
			if err != nil || len(report.Actions) != 1 || !report.Actions[0].Applied || qbt.remaining != 0 {
				t.Fatalf("error=%v remaining=%d report=%#v", err, qbt.remaining, report)
			}
			if eventCount(base.events, "add") != 1 || eventCount(base.events, "delete:old") != 1 {
				t.Fatalf("a mutation was repeated: %v", base.events)
			}
		})
	}
}

func TestExecuteReplansAroundRefusedMetainfoDownload(t *testing.T) {
	t.Parallel()
	qbt, mt := nonTimedTestServices(t)
	_, otherHash := alternativeCandidate(t, mt)
	qbt.addHash, qbt.addSize = otherHash, 20
	refused := &candidateDownloadErrorMTeam{fakeMTeam: mt, errorsByID: map[int64]error{2: &mteam.DownloadRefusedError{Code: 1}}}
	runner := testRunner(qbt, mt)
	runner.MTeam = refused

	report, err := runner.Execute(context.Background(), true)
	if err != nil || len(report.Actions) != 1 || report.Actions[0].CandidateID != "3" || !report.Actions[0].Applied || refused.requests[2] != 1 {
		t.Fatalf("error=%v requests=%v report=%#v", err, refused.requests, report)
	}
	want := SkippedCandidate{CandidateID: "2", Reason: "M-Team refused the metainfo download with API error 1"}
	if len(report.SkippedCandidates) != 1 || report.SkippedCandidates[0] != want {
		t.Fatalf("skipped candidates=%#v", report.SkippedCandidates)
	}
}

func TestRecoveryStopsWhenPendingOfferDownloadIsRefused(t *testing.T) {
	t.Parallel()
	qbt, mt := nonTimedTestServices(t)
	qbt = pendingQBT(qbt.addHash)
	refused := &candidateDownloadErrorMTeam{fakeMTeam: mt, errorsByID: map[int64]error{2: &mteam.DownloadRefusedError{Code: 1}}}
	runner := testRunner(qbt, mt)
	runner.MTeam = refused

	report, err := runner.Execute(context.Background(), true)
	var refusal *mteam.DownloadRefusedError
	if !errors.As(err, &refusal) || len(report.Mutations) != 0 || len(qbt.torrents) != 1 || qbt.torrents[0].State != "stoppedDL" {
		t.Fatalf("a possibly transient refusal removed or changed the pending torrent: error=%v report=%#v torrents=%#v", err, report, qbt.torrents)
	}
}

func TestExecuteStopsAfterRepeatedRefusedMetainfoDownloads(t *testing.T) {
	t.Parallel()
	qbt, mt := nonTimedTestServices(t)
	refused := &candidateDownloadErrorMTeam{fakeMTeam: mt, errorsByID: make(map[int64]error)}
	offer := mt.results[0]
	mt.results = nil
	for id := int64(2); id <= maxRefusedCandidates+2; id++ {
		offer.ID, offer.Name = id, "offer-"+strconv.FormatInt(id, 10)
		mt.results = append(mt.results, offer)
		refused.errorsByID[id] = &mteam.DownloadRefusedError{Code: 1}
	}
	runner := testRunner(qbt, mt)
	runner.MTeam = refused

	report, err := runner.Execute(context.Background(), true)
	var refusal *mteam.DownloadRefusedError
	if !errors.As(err, &refusal) || len(report.SkippedCandidates) != maxRefusedCandidates || qbt.mutations != 0 {
		t.Fatalf("error=%v report=%#v events=%v", err, report, qbt.events)
	}
	for id, count := range refused.requests {
		if count != 1 {
			t.Fatalf("candidate %d was downloaded %d times", id, count)
		}
	}
}

func TestExecuteCapsAdditionsByIncompleteDownloads(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		limit, additions int
		apply            bool
	}{{0, 1, false}, {1, 0, false}, {2, 1, false}, {1, 0, true}, {2, 1, true}} {
		t.Run(strconv.Itoa(test.limit)+"/"+strconv.FormatBool(test.apply), func(t *testing.T) {
			t.Parallel()
			qbt, mt := nonTimedTestServices(t)
			qbt.torrents = append(qbt.torrents, qbittorrent.Torrent{
				Hash: "busy", Name: "busy", Size: 1, AmountLeft: 1,
				AddedOn: appNow.Add(-2 * time.Hour), LastActivity: appNow,
				SavePath: "/downloads/swarmfolio/busy", ContentPath: "/downloads/swarmfolio/busy", State: "downloading",
				Category: "swarmfolio", AutoTMM: true,
			})
			runner := testRunner(qbt, mt)
			runner.Config.Policy.MaxIncompleteDownloads = test.limit

			report, err := runner.Execute(context.Background(), test.apply)
			if err != nil || len(report.Actions) != test.additions {
				t.Fatalf("error=%v report=%#v", err, report)
			}
			if test.apply && (eventCount(qbt.events, "add") != test.additions || (test.additions == 1 && !report.Actions[0].Applied)) {
				t.Fatalf("applied additions do not respect the cap: events=%v report=%#v", qbt.events, report)
			}
		})
	}
}
