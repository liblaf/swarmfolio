package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/liblaf/swarmfolio/internal/metainfo"
	"github.com/liblaf/swarmfolio/internal/mteam"
)

type candidateDownloadErrorMTeam struct {
	*fakeMTeam
	errorsByID map[int64]error
	requests   map[int64]int
}

func (m *candidateDownloadErrorMTeam) Download(ctx context.Context, id int64) ([]byte, error) {
	if m.requests == nil {
		m.requests = make(map[int64]int)
	}
	m.requests[id]++
	if err := m.errorsByID[id]; err != nil {
		return nil, err
	}
	return m.fakeMTeam.Download(ctx, id)
}

func alternativeCandidate(t *testing.T, mt *fakeMTeam) (mteam.Torrent, string) {
	t.Helper()
	data := []byte("d4:infod6:lengthi20e4:name5:otheree")
	hash, err := metainfo.InfoHash(data)
	if err != nil {
		t.Fatal(err)
	}
	other := mt.results[0]
	other.ID, other.Name, other.Size, other.Leechers = 3, "other", 20, 8
	mt.results = append(mt.results, other)
	mt.metainfoByID = map[int64][]byte{3: data}
	return other, hash
}

func TestExecuteSkipsExistingHashRegardlessOfTitleCategoryOrSelectedSize(t *testing.T) {
	t.Parallel()
	for _, category := range []string{"swarmfolio", "user-managed"} {
		for _, apply := range []bool{false, true} {
			t.Run(category+"/"+map[bool]string{false: "plan", true: "apply"}[apply], func(t *testing.T) {
				t.Parallel()
				qbt, mt := testServices(t)
				qbt.torrents[0].Hash = strings.ToUpper(qbt.addHash)
				qbt.torrents[0].Name = "different display title"
				qbt.torrents[0].Size = 30
				qbt.torrents[0].Category = category
				if category == "user-managed" {
					qbt.torrents[0].AutoTMM = false
					qbt.torrents[0].SavePath = "/downloads/user"
					qbt.torrents[0].Size = 10 // Only selected files count in qBittorrent's size.
				}
				before := slices.Clone(qbt.torrents)
				report, err := testRunner(qbt, mt).Execute(context.Background(), apply)
				if err != nil {
					t.Fatal(err)
				}
				if len(report.Actions) != 0 || mt.downloads != 1 || qbt.mutations != 0 || !slices.Equal(before, qbt.torrents) {
					t.Fatalf("existing hash was not preserved: report=%#v downloads=%d events=%v", report, mt.downloads, qbt.events)
				}
			})
		}
	}
}

func TestExecuteReplansAfterExcludingExistingHash(t *testing.T) {
	t.Parallel()
	for _, apply := range []bool{false, true} {
		t.Run(map[bool]string{false: "plan", true: "apply"}[apply], func(t *testing.T) {
			t.Parallel()
			qbt, mt := testServices(t)
			qbt.torrents[0].Hash = qbt.addHash
			qbt.torrents[0].Name = "existing title"
			qbt.torrents[0].Size = 30
			before := qbt.torrents[0]
			other := mt.results[0]
			other.ID, other.Name, other.Size, other.Leechers = 3, "another offer", 20, 2
			mt.results = append(mt.results, other)
			otherMetainfo := []byte("d4:infod6:lengthi20e4:name5:otheree")
			mt.metainfoByID = map[int64][]byte{3: otherMetainfo}
			hash, err := metainfo.InfoHash(otherMetainfo)
			if err != nil {
				t.Fatal(err)
			}
			qbt.addHash, qbt.addSize = hash, 20
			report, err := testRunner(qbt, mt).Execute(context.Background(), apply)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Actions) != 1 || report.Actions[0].CandidateID != "3" || report.Actions[0].Applied != apply || mt.downloads != 2 {
				t.Fatalf("report=%#v metainfo downloads=%d", report, mt.downloads)
			}
			if qbt.torrents[0] != before || prefixIndex(qbt.events, "delete:") >= 0 || (!apply && qbt.mutations != 0) {
				t.Fatalf("existing torrent changed: events=%v torrents=%#v", qbt.events, qbt.torrents)
			}
		})
	}
}

func TestExecuteReplansAroundDailyDownloadLimit(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	indefiniteFreeleech(mt)
	_, otherHash := alternativeCandidate(t, mt)
	qbt.addHash, qbt.addSize = otherHash, 20
	limited := &candidateDownloadErrorMTeam{fakeMTeam: mt, errorsByID: map[int64]error{2: mteam.ErrTorrentDownloadLimit}}
	runner := testRunner(qbt, mt)
	runner.MTeam = limited
	report, err := runner.Execute(context.Background(), true)
	if err != nil || len(report.Actions) != 1 || report.Actions[0].CandidateID != "3" || !report.Actions[0].Applied || limited.requests[2] != 1 {
		t.Fatalf("error=%v requests=%v report=%#v", err, limited.requests, report)
	}
	if len(report.SkippedCandidates) != 1 || report.SkippedCandidates[0] != (SkippedCandidate{CandidateID: "2", Reason: "daily torrent download limit reached"}) {
		t.Fatalf("skipped candidates=%#v", report.SkippedCandidates)
	}
	if findHash(qbt.torrents, "old") != nil || findHash(qbt.torrents, otherHash) == nil {
		t.Fatalf("limited candidate blocked alternative replacement: %#v", qbt.torrents)
	}

	// The limit is per-run only: a new invocation retries the offer.
	limited.requests = nil
	_, _ = runner.Execute(context.Background(), false)
	if limited.requests[2] != 1 {
		t.Fatalf("new run did not retry limited candidate: %v", limited.requests)
	}
}

func TestExecuteRemovesDownloadLimitedPendingCandidateWithoutFiles(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt = pendingQBT(qbt.addHash)
	limited := &candidateDownloadErrorMTeam{fakeMTeam: mt, errorsByID: map[int64]error{2: mteam.ErrTorrentDownloadLimit}}
	runner := testRunner(qbt, mt)
	runner.MTeam = limited
	runner.Config.Policy.MaxAdditions = 0
	report, err := runner.Execute(context.Background(), true)
	if err != nil || limited.requests[2] != 1 || len(report.Recoveries) != 1 || report.Recoveries[0].Action != "remove" || len(report.SkippedCandidates) != 1 {
		t.Fatalf("error=%v requests=%v report=%#v", err, limited.requests, report)
	}
	if !slices.Equal(qbt.deleteFiles, []bool{false}) || prefixIndex(qbt.events, "start:") >= 0 || len(qbt.torrents) != 0 {
		t.Fatalf("limited pending torrent was started or its files removed: events=%v deleteFiles=%v torrents=%#v", qbt.events, qbt.deleteFiles, qbt.torrents)
	}
}

func TestExecuteDoesNotSuppressUnknownCandidateDownloadError(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	limited := &candidateDownloadErrorMTeam{fakeMTeam: mt, errorsByID: map[int64]error{2: errors.New("permission denied")}}
	runner := testRunner(qbt, mt)
	runner.MTeam = limited
	report, err := runner.Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "permission denied") || qbt.mutations != 0 || len(report.SkippedCandidates) != 0 {
		t.Fatalf("error=%v report=%#v events=%v", err, report, qbt.events)
	}
}

func TestExecuteRecoversPendingHashWithDifferentTitle(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	indefiniteFreeleech(mt)
	qbt = pendingQBT(qbt.addHash)
	qbt.torrents[0].Name = "localized torrent title"
	report, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Recoveries) != 1 || report.Recoveries[0].Action != "resume" || len(report.Actions) != 0 || mt.downloads != 1 {
		t.Fatalf("report=%#v metainfo downloads=%d", report, mt.downloads)
	}
	if len(qbt.torrents) != 1 || qbt.torrents[0].State != "downloading" || qbt.mutations != 1 || prefixIndex(qbt.events, "delete:") >= 0 || index(qbt.events, "add") >= 0 {
		t.Fatalf("pending torrent was replaced instead of resumed: events=%v torrents=%#v", qbt.events, qbt.torrents)
	}
}

func TestExecuteAddsOnlyOneOfferPerHash(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	indefiniteFreeleech(mt)
	qbt.torrents = nil
	alias := mt.results[0]
	alias.ID, alias.Name = 3, "alias title"
	mt.results = append(mt.results, alias)
	runner := testRunner(qbt, mt)
	runner.Config.Policy.MaxAdditions = 2
	report, err := runner.Execute(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Actions) != 1 || !report.Actions[0].Applied || len(qbt.torrents) != 1 || mt.downloads != 2 {
		t.Fatalf("report=%#v torrents=%#v downloads=%d", report, qbt.torrents, mt.downloads)
	}
}

func TestExecuteRejectsUnresolvableIdentityBeforeMutations(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	mt.metainfo = []byte("invalid torrent")
	_, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "inspect M-Team torrent 2") || qbt.mutations != 0 {
		t.Fatalf("error=%v events=%v", err, qbt.events)
	}
}

func TestExecuteRejectsMetainfoSizeMismatchBeforeMutations(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.preallocate = true
	// The offer is 30 bytes, but the valid torrent metainfo describes 60 bytes.
	// Do not hand this to qBittorrent: preallocation happens inside its add call.
	mt.metainfo = []byte("d4:infod6:lengthi60e4:name3:newee")
	_, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "metainfo size 60 does not match its 30-byte offer") {
		t.Fatalf("error=%v", err)
	}
	if qbt.mutations != 0 || prefixIndex(qbt.events, "add") >= 0 || len(qbt.torrents) != 1 || qbt.torrents[0].Hash != "old" {
		t.Fatalf("mismatched metainfo reached qBittorrent: events=%v torrents=%#v", qbt.events, qbt.torrents)
	}
}

func TestExecuteDetectsHashAddedBetweenPlanningAndApply(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.torrents[0].Size = 30
	mt.onDownload = func(int64) { qbt.torrents[0].Hash = qbt.addHash }
	_, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "already exists") || qbt.mutations != 0 {
		t.Fatalf("error=%v events=%v", err, qbt.events)
	}
}
