package app

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/liblaf/swarmfolio/internal/config"
	"github.com/liblaf/swarmfolio/internal/disk"
	"github.com/liblaf/swarmfolio/internal/metainfo"
	"github.com/liblaf/swarmfolio/internal/mteam"
	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

var appNow = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

func TestExecutePlansWithoutMutating(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	report, err := testRunner(qbt, mt).Execute(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Actions) != 1 || len(report.Actions[0].Removals) != 1 || report.Actions[0].Applied {
		t.Fatalf("report = %#v", report)
	}
	if qbt.mutations != 0 || mt.downloads != 0 {
		t.Fatalf("read-only plan mutated qBittorrent=%d or downloaded tokens=%d", qbt.mutations, mt.downloads)
	}
	if report.Budget.MinimumFreePercent != 25 || report.Budget.LimitBytes != 75 {
		t.Fatalf("budget = %#v", report.Budget)
	}
}

func TestExecuteAppliesPausedAdditionBeforeDeletion(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	report, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Actions) != 1 || !report.Actions[0].Applied {
		t.Fatalf("report = %#v", report)
	}
	joined := strings.Join(qbt.events, ",")
	for _, sequence := range []string{"add", "delete:old", "start:"} {
		if !strings.Contains(joined, sequence) {
			t.Fatalf("events %q do not contain %q", joined, sequence)
		}
	}
	if index(qbt.events, "add") > prefixIndex(qbt.events, "delete:old") {
		t.Fatalf("candidate was not added before removal: %v", qbt.events)
	}
	if len(qbt.torrents) != 1 || qbt.torrents[0].Name != "new" || qbt.torrents[0].State != "downloading" {
		t.Fatalf("final torrents = %#v", qbt.torrents)
	}
	if qbt.addSavePath != "/downloads/swarmfolio" {
		t.Fatalf("add save path = %q, want resolved category path", qbt.addSavePath)
	}
}

func TestExecuteRemovesExpiredEmptyPendingTorrent(t *testing.T) {
	t.Parallel()
	qbt := &fakeQBT{defaultPath: "/downloads", categoryPath: "/downloads/swarmfolio", torrents: []qbittorrent.Torrent{{
		Hash: "pending", Name: "pending", Size: 30, AmountLeft: 30,
		AddedOn: appNow.Add(-time.Hour), LastActivity: appNow.Add(-time.Hour), SavePath: "/downloads/swarmfolio/pending",
		Category: "swarmfolio", AutoTMM: true, State: "stoppedDL",
	}}}
	mt := &fakeMTeam{results: []mteam.Torrent{{
		ID: 9, Name: "pending", Size: 30, Seeders: 1, Leechers: 2,
		PublishedAt: appNow.Add(-time.Hour), DiscountEndTime: appNow.Add(30 * time.Minute),
	}}}
	report, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(qbt.torrents) != 0 || len(report.Recoveries) != 1 || report.Recoveries[0].Action != "remove" {
		t.Fatalf("torrents=%#v recoveries=%#v", qbt.torrents, report.Recoveries)
	}
}

func TestExecuteRejectsPreallocationWithoutPhysicalHeadroom(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.preallocate = true
	_, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "safely preallocated") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(strings.Join(qbt.events, ","), "add") || len(qbt.torrents) != 1 || qbt.torrents[0].Hash != "old" {
		t.Fatalf("unsafe mutation: events=%v torrents=%#v", qbt.events, qbt.torrents)
	}
}

func TestExecuteStopsBeforeStartingWhenFreeBudgetChangesAfterDeletion(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	runner := testRunner(qbt, mt)
	runner.ProbeDisk = func(string) (disk.Space, error) {
		free := int64(30)
		if prefixIndex(qbt.events, "delete:old") >= 0 {
			free = 0
		}
		return disk.Space{CapacityBytes: 100, FreeBytes: free}, nil
	}
	_, err := runner.Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "after deletion") {
		t.Fatalf("error = %v", err)
	}
	if len(qbt.torrents) != 1 || qbt.torrents[0].Name != "new" || qbt.torrents[0].State != "stoppedDL" ||
		prefixIndex(qbt.events, "start:") >= 0 {
		t.Fatalf("candidate should remain stopped and recoverable: events=%v torrents=%#v", qbt.events, qbt.torrents)
	}
}

func TestExecuteDoesNotStartAdditionAfterItLeavesCategory(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	runner := testRunner(qbt, mt)
	mutated := false
	runner.ProbeDisk = func(string) (disk.Space, error) {
		if !mutated && prefixIndex(qbt.events, "delete:old") >= 0 {
			qbt.torrents[0].Category = "user-managed"
			mutated = true
		}
		return disk.Space{CapacityBytes: 100, FreeBytes: qbt.free()}, nil
	}
	_, err := runner.Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "no longer safe") {
		t.Fatalf("error = %v", err)
	}
	if len(qbt.torrents) != 1 || qbt.torrents[0].Category != "user-managed" || prefixIndex(qbt.events, "start:") >= 0 {
		t.Fatalf("addition that left the category was started: events=%v torrents=%#v", qbt.events, qbt.torrents)
	}
}

func TestExecuteRollsBackNewPendingWhenRemovalBecomesBusy(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.onAdd = func(q *fakeQBT) { q.torrents[0].State = "checkingUP" }
	_, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "no longer safe") {
		t.Fatalf("error = %v", err)
	}
	if len(qbt.torrents) != 1 || qbt.torrents[0].Hash != "old" {
		t.Fatalf("pending was not rolled back: %#v", qbt.torrents)
	}
}

func TestExecuteRollsBackWhenRemovalLeavesManagedCategory(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.onAdd = func(q *fakeQBT) { q.torrents[0].Category = "user-managed" }
	_, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "no longer safe") {
		t.Fatalf("error = %v", err)
	}
	if len(qbt.torrents) != 1 || qbt.torrents[0].Hash != "old" {
		t.Fatalf("pending was not rolled back: %#v", qbt.torrents)
	}
}

func TestExecuteRollsBackAdditionThatStartsUnexpectedly(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.onAdd = func(q *fakeQBT) { q.torrents[len(q.torrents)-1].State = "downloading" }
	_, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v", err)
	}
	if len(qbt.torrents) != 2 || qbt.torrents[1].State != "downloading" || prefixIndex(qbt.events, "delete:") >= 0 ||
		!strings.Contains(err.Error(), "refuse to roll back") {
		t.Fatalf("changed addition should be left untouched: error=%v events=%v torrents=%#v", err, qbt.events, qbt.torrents)
	}
}

func TestExecuteRollsBackAdditionWithoutAutomaticManagement(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.onAdd = func(q *fakeQBT) { q.torrents[len(q.torrents)-1].AutoTMM = false }
	_, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "automatically managed") {
		t.Fatalf("error = %v", err)
	}
	if len(qbt.torrents) != 2 || qbt.torrents[1].AutoTMM || prefixIndex(qbt.events, "delete:") >= 0 ||
		!strings.Contains(err.Error(), "refuse to roll back") {
		t.Fatalf("manually managed addition should be left untouched: error=%v events=%v torrents=%#v", err, qbt.events, qbt.torrents)
	}
}

func TestExecuteRefusesRollbackAfterAdditionLeavesCategory(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.onAdd = func(q *fakeQBT) { q.torrents[len(q.torrents)-1].Category = "user-managed" }
	_, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "refuse to roll back") {
		t.Fatalf("error = %v", err)
	}
	if len(qbt.torrents) != 2 || qbt.torrents[1].Category != "user-managed" || prefixIndex(qbt.events, "delete:") >= 0 {
		t.Fatalf("addition that left the category should be untouched: events=%v torrents=%#v", qbt.events, qbt.torrents)
	}
}

func TestAccountCountsOffPathOutstandingBytes(t *testing.T) {
	t.Parallel()
	r := Runner{}
	used, outstanding, err := r.account([]qbittorrent.Torrent{
		{Hash: "inside", Size: 50, AmountLeft: 0, SavePath: "/downloads/inside"},
		{Hash: "outside", Size: 20, AmountLeft: 20, SavePath: "/other/outside"},
	}, "/downloads")
	if err != nil || used != 50 || outstanding != 20 {
		t.Fatalf("used=%d outstanding=%d err=%v", used, outstanding, err)
	}
}

func TestExecuteRejectsRemoteBudgetForDifferentCategoryFilesystem(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	runner := testRunner(qbt, mt)
	runner.Config.Portfolio.DiskPath = ""
	runner.Config.Portfolio.DiskCapacityBytes = 100
	_, err := runner.Execute(context.Background(), false)
	if err == nil || !strings.Contains(err.Error(), "paths to match") {
		t.Fatalf("error = %v", err)
	}
}

func TestExecuteRecoversSafePendingTorrent(t *testing.T) {
	t.Parallel()
	metainfoBytes := []byte("d4:infod6:lengthi30e4:name7:pendingee")
	hash, err := metainfo.InfoHash(metainfoBytes)
	if err != nil {
		t.Fatal(err)
	}
	qbt := &fakeQBT{
		defaultPath: "/downloads", categoryPath: "/downloads/swarmfolio",
		torrents: []qbittorrent.Torrent{{
			Hash: hash, Name: "pending", Size: 30, AmountLeft: 30,
			AddedOn: appNow.Add(-time.Hour), LastActivity: appNow.Add(-time.Hour),
			SavePath: "/downloads/swarmfolio/pending", State: "stoppedDL",
			Category: "swarmfolio", AutoTMM: true,
		}},
	}
	mt := &fakeMTeam{results: []mteam.Torrent{{
		ID: 9, Name: "pending", Size: 30, Seeders: 1, Leechers: 2,
		PublishedAt: appNow.Add(-time.Hour), DiscountEndTime: appNow.Add(3 * time.Hour),
	}}, metainfo: metainfoBytes}
	report, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Recoveries) != 1 || report.Recoveries[0].Action != "resume" {
		t.Fatalf("recoveries = %#v", report.Recoveries)
	}
	if qbt.torrents[0].State != "downloading" {
		t.Fatalf("pending torrent state = %q, want downloading", qbt.torrents[0].State)
	}
}

func TestExecuteRefusesRecoveryAfterPendingStateChanges(t *testing.T) {
	t.Parallel()
	pendingMetainfo := []byte("d4:infod6:lengthi30e4:name7:pendingee")
	pendingHash, err := metainfo.InfoHash(pendingMetainfo)
	if err != nil {
		t.Fatal(err)
	}
	wrongMetainfo := []byte("d4:infod6:lengthi30e4:name5:wrongee")
	tests := []struct {
		name     string
		metainfo []byte
		mutate   func(*qbittorrent.Torrent)
	}{
		{
			name: "category changes before stale removal", metainfo: wrongMetainfo,
			mutate: func(torrent *qbittorrent.Torrent) { torrent.Category = "user-managed" },
		},
		{
			name: "torrent starts before resume", metainfo: pendingMetainfo,
			mutate: func(torrent *qbittorrent.Torrent) { torrent.State = "downloading" },
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			qbt := pendingQBT(pendingHash)
			mt := &fakeMTeam{
				results: []mteam.Torrent{{
					ID: 9, Name: "pending", Size: 30, Seeders: 1, Leechers: 2,
					PublishedAt: appNow.Add(-time.Hour), DiscountEndTime: appNow.Add(3 * time.Hour),
				}},
				metainfo: test.metainfo,
				onDownload: func(int64) {
					test.mutate(&qbt.torrents[0])
				},
			}
			_, err := testRunner(qbt, mt).Execute(context.Background(), true)
			if err == nil || !strings.Contains(err.Error(), "no longer safe") {
				t.Fatalf("error = %v", err)
			}
			if len(qbt.torrents) != 1 || prefixIndex(qbt.events, "delete:") >= 0 || prefixIndex(qbt.events, "start:") >= 0 {
				t.Fatalf("changed pending torrent was mutated: events=%v torrents=%#v", qbt.events, qbt.torrents)
			}
		})
	}
}

func TestExecuteRecoversAmbiguousPendingTorrentByExactHash(t *testing.T) {
	t.Parallel()
	pendingMetainfo := []byte("d4:infod6:lengthi30e4:name7:pendingee")
	pendingHash, err := metainfo.InfoHash(pendingMetainfo)
	if err != nil {
		t.Fatal(err)
	}
	qbt := pendingQBT(pendingHash)
	mt := &fakeMTeam{
		results: []mteam.Torrent{
			{ID: 8, Name: "pending", Size: 30, Seeders: 1, Leechers: 2, PublishedAt: appNow.Add(-time.Hour), DiscountEndTime: appNow.Add(3 * time.Hour)},
			{ID: 9, Name: "pending", Size: 30, Seeders: 1, Leechers: 2, PublishedAt: appNow.Add(-time.Hour), DiscountEndTime: appNow.Add(3 * time.Hour)},
		},
		metainfoByID: map[int64][]byte{
			8: []byte("d4:infod6:lengthi30e4:name5:wrongee"),
			9: pendingMetainfo,
		},
	}
	report, err := testRunner(qbt, mt).Execute(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if mt.downloads != 2 || len(report.Recoveries) != 1 || report.Recoveries[0].Action != "resume" || qbt.torrents[0].State != "downloading" {
		t.Fatalf("downloads=%d recoveries=%#v torrents=%#v", mt.downloads, report.Recoveries, qbt.torrents)
	}
}

func TestExecuteSkipsCandidateAlreadyPresentByCategoryNameAndSize(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.torrents[0].Name = "new"
	qbt.torrents[0].Size = 30
	report, err := testRunner(qbt, mt).Execute(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Actions) != 0 || mt.downloads != 0 {
		t.Fatalf("report=%#v downloads=%d", report, mt.downloads)
	}
}

func TestExecuteDoesNotTreatAnotherCategoryAsPresent(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt.torrents[0].Name = "new"
	qbt.torrents[0].Size = 30
	qbt.torrents[0].Category = "user-managed"
	qbt.torrents[0].SavePath = "/downloads/user/new"
	report, err := testRunner(qbt, mt).Execute(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Actions) != 1 || report.Actions[0].CandidateID != "2" || mt.downloads != 0 {
		t.Fatalf("report=%#v downloads=%d", report, mt.downloads)
	}
}

func pendingQBT(hash string) *fakeQBT {
	return &fakeQBT{
		defaultPath: "/downloads", categoryPath: "/downloads/swarmfolio",
		torrents: []qbittorrent.Torrent{{
			Hash: hash, Name: "pending", Size: 30, AmountLeft: 30,
			AddedOn: appNow.Add(-time.Hour), LastActivity: appNow.Add(-time.Hour),
			SavePath: "/downloads/swarmfolio/pending", State: "stoppedDL",
			Category: "swarmfolio", AutoTMM: true,
		}},
	}
}

func testRunner(qbt *fakeQBT, mt *fakeMTeam) Runner {
	return Runner{
		Config: config.Settings{
			Portfolio:   config.Portfolio{MinimumFreePercent: 25, DiskPath: "/host/downloads"},
			QBittorrent: config.QBittorrent{Category: "swarmfolio"},
			Policy: config.Policy{
				CandidateMaxAge: 24 * time.Hour, MinimumFreeleechRemaining: time.Hour,
				MinimumLeechers: 1, MinimumOpportunityRatio: 0.5,
				MinimumResidency: time.Hour, MinimumIdle: time.Hour,
				ActiveUploadRate: 1, MaxAdditions: 1, MaxRemovals: 1,
			},
		},
		QBittorrent: qbt, MTeam: mt, Now: func() time.Time { return appNow },
		ProbeDisk: func(string) (disk.Space, error) {
			return disk.Space{CapacityBytes: 100, FreeBytes: qbt.free()}, nil
		},
		PollInterval: time.Millisecond, PollTimeout: 50 * time.Millisecond,
	}
}

func testServices(t *testing.T) (*fakeQBT, *fakeMTeam) {
	t.Helper()
	metainfoBytes := []byte("d4:infod6:lengthi30e4:name3:newee")
	hash, err := metainfo.InfoHash(metainfoBytes)
	if err != nil {
		t.Fatal(err)
	}
	qbt := &fakeQBT{
		defaultPath: "/downloads", categoryPath: "/downloads/swarmfolio", addHash: hash, addSize: 30,
		torrents: []qbittorrent.Torrent{{
			Hash: "old", Name: "old", Size: 70, Uploaded: 1, Progress: 1,
			AddedOn: appNow.Add(-2 * time.Hour), LastActivity: appNow.Add(-2 * time.Hour),
			SavePath: "/downloads/swarmfolio/old", State: "stoppedUP",
			Category: "swarmfolio", AutoTMM: true,
		}},
	}
	mt := &fakeMTeam{
		results: []mteam.Torrent{{
			ID: 2, Name: "new", Size: 30, Seeders: 1, Leechers: 8,
			PublishedAt: appNow.Add(-time.Hour), DiscountEndTime: appNow.Add(3 * time.Hour),
		}},
		metainfo: metainfoBytes,
	}
	return qbt, mt
}

type fakeQBT struct {
	defaultPath  string
	categoryPath string
	torrents     []qbittorrent.Torrent
	addHash      string
	addSize      int64
	preallocate  bool
	addSavePath  string
	onAdd        func(*fakeQBT)
	events       []string
	mutations    int
}

func (q *fakeQBT) Login(context.Context) error { q.events = append(q.events, "login"); return nil }
func (q *fakeQBT) Torrents(context.Context) ([]qbittorrent.Torrent, error) {
	q.events = append(q.events, "torrents")
	return slices.Clone(q.torrents), nil
}
func (q *fakeQBT) CategorySavePath(context.Context, string) (string, error) {
	return q.categoryPath, nil
}
func (q *fakeQBT) DefaultSavePath(context.Context) (string, error) { return q.defaultPath, nil }
func (q *fakeQBT) FreeSpace(context.Context) (int64, error)        { return q.free(), nil }
func (q *fakeQBT) PreallocateAll(context.Context) (bool, error)    { return q.preallocate, nil }
func (q *fakeQBT) Add(_ context.Context, request qbittorrent.AddRequest) error {
	q.events = append(q.events, "add")
	q.mutations++
	q.addSavePath = request.SavePath
	q.torrents = append(q.torrents, qbittorrent.Torrent{
		Hash: q.addHash, Name: "new", Size: q.addSize, AmountLeft: q.addSize,
		AddedOn: appNow, LastActivity: appNow, SavePath: request.SavePath + "/new",
		State: "stoppedDL", Category: request.Category, AutoTMM: request.AutoTMM,
	})
	if q.onAdd != nil {
		q.onAdd(q)
	}
	return nil
}
func (q *fakeQBT) Delete(_ context.Context, hashes []string, _ bool) error {
	q.events = append(q.events, "delete:"+strings.Join(hashes, "|"))
	q.mutations++
	q.torrents = slices.DeleteFunc(q.torrents, func(torrent qbittorrent.Torrent) bool { return slices.Contains(hashes, torrent.Hash) })
	return nil
}
func (q *fakeQBT) Start(_ context.Context, hashes []string) error {
	q.events = append(q.events, "start:"+strings.Join(hashes, "|"))
	q.mutations++
	for i := range q.torrents {
		if slices.Contains(hashes, q.torrents[i].Hash) {
			q.torrents[i].State = "downloading"
		}
	}
	return nil
}
func (q *fakeQBT) free() int64 {
	// Tests model a 100-byte disk with 70 bytes initially occupied.
	used := int64(0)
	for _, torrent := range q.torrents {
		used += torrent.Size - torrent.AmountLeft
	}
	return 100 - used
}

type fakeMTeam struct {
	results      []mteam.Torrent
	metainfo     []byte
	metainfoByID map[int64][]byte
	onDownload   func(int64)
	downloads    int
}

func (m *fakeMTeam) Search(context.Context) ([]mteam.Torrent, error) {
	return slices.Clone(m.results), nil
}
func (m *fakeMTeam) Download(_ context.Context, id int64) ([]byte, error) {
	m.downloads++
	if m.onDownload != nil {
		m.onDownload(id)
	}
	if metainfoBytes, ok := m.metainfoByID[id]; ok {
		return slices.Clone(metainfoBytes), nil
	}
	return slices.Clone(m.metainfo), nil
}

func index(values []string, value string) int { return slices.Index(values, value) }

func prefixIndex(values []string, prefix string) int {
	for i, value := range values {
		if strings.HasPrefix(value, prefix) {
			return i
		}
	}
	return -1
}
