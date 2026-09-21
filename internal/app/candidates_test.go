package app

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/liblaf/swarmfolio/internal/metainfo"
)

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

func TestExecuteRecoversPendingHashWithDifferentTitle(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
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
