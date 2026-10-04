package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/liblaf/swarmfolio/internal/mteam"
	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

func guardServices() (*fakeQBT, *fakeMTeam, Runner) {
	q := &fakeQBT{ids: map[string]int64{"active": 2}, torrents: []qbittorrent.Torrent{{Hash: "active", Name: "active", Size: 100, AmountLeft: 50, Progress: 0.5, DLRate: 10, Downloaded: 50, DownloadTime: 5 * time.Second, Category: "swarmfolio", AutoTMM: true, State: "downloading"}}}
	m := &fakeMTeam{results: []mteam.Torrent{{ID: 2, Name: "active", Size: 100, Discount: "FREE"}}}
	r := testRunner(q, m)
	return q, m, r
}

func TestGuardStopsNonFreeAndExpiredDownloads(t *testing.T) {
	t.Parallel()
	for _, discount := range []string{"PERCENT_50", "_2X", "NORMAL", "UNKNOWN", "", "free", "FREE "} {
		for _, management := range []string{"automatic", "manual"} {
			t.Run(discount+"/"+management, func(t *testing.T) {
				t.Parallel()
				q, m, r := guardServices()
				q.torrents[0].AutoTMM = management == "automatic"
				m.results[0].Discount = discount
				if err := r.Guard(context.Background()); err != nil {
					t.Fatal(err)
				}
				if q.torrents[0].State != "stoppedDL" || !guardPaused(q.torrents[0]) || q.torrents[0].DLRate != 0 {
					t.Fatalf("unsafe torrent still running: %#v", q.torrents[0])
				}
				if prefixIndex(q.events, "delete:") >= 0 {
					t.Fatal("guard deleted payload")
				}
			})
		}
	}
	t.Run("expiry", func(t *testing.T) {
		q, m, r := guardServices()
		m.results[0].DiscountEndTime = appNow.Add(time.Hour)
		if err := r.Guard(context.Background()); err != nil {
			t.Fatal(err)
		}
		if q.torrents[0].State != "stoppedDL" {
			t.Fatal("guard missed deadline buffer")
		}
	})
}

func TestGuardStopsSlowTimedDownloadButKeepsSafeSeedsAndCategories(t *testing.T) {
	t.Parallel()
	q, m, r := guardServices()
	m.results[0].DiscountEndTime = appNow.Add(2 * time.Hour)
	q.torrents[0].AmountLeft = 50000
	q.torrents[0].Size = 100000
	m.results[0].Size = 100000
	q.torrents = append(q.torrents, qbittorrent.Torrent{Hash: "seed", Category: "swarmfolio", AutoTMM: true, Progress: 1, State: "uploading"}, qbittorrent.Torrent{Hash: "user", Category: "other", AutoTMM: true, Size: 100, AmountLeft: 100, State: "downloading"})
	if err := r.Guard(context.Background()); err != nil {
		t.Fatal(err)
	}
	if q.torrents[0].State != "stoppedDL" || q.torrents[1].State != "uploading" || q.torrents[2].State != "downloading" {
		t.Fatalf("wrong guard scope: %#v", q.torrents)
	}
}

func TestGuardKeepsSafeTimedAndIndefiniteDownloads(t *testing.T) {
	t.Parallel()
	for _, until := range []time.Time{{}, appNow.Add(2 * time.Hour)} {
		q, m, r := guardServices()
		m.results[0].DiscountEndTime = until
		if err := r.Guard(context.Background()); err != nil {
			t.Fatal(err)
		}
		if q.mutations != 0 {
			t.Fatalf("safe torrent mutated: %v", q.events)
		}
	}
}

type unavailableOffersMTeam struct{ *fakeMTeam }

func (m unavailableOffersMTeam) Offers(context.Context) ([]mteam.Torrent, error) {
	return nil, errors.New("upstream unavailable")
}

func TestGuardStopsAllManagedDownloadsWhenPromotionsCannotBeVerified(t *testing.T) {
	t.Parallel()
	q, m, r := guardServices()
	r.MTeam = unavailableOffersMTeam{m}
	if err := r.Guard(context.Background()); err == nil || !strings.Contains(err.Error(), "upstream unavailable") {
		t.Fatalf("error=%v", err)
	}
	if q.torrents[0].State != "stoppedDL" {
		t.Fatal("unknown promotion continued downloading")
	}
}

func TestGuardStopsMissingOffersAndMissingProvenance(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"offer", "identity"} {
		t.Run(missing, func(t *testing.T) {
			t.Parallel()
			q, m, r := guardServices()
			if missing == "offer" {
				m.results = nil
			} else {
				q.ids = nil
			}
			err := r.Guard(context.Background())
			if missing == "identity" && err == nil {
				t.Fatal("missing provenance was hidden")
			}
			if q.torrents[0].State != "stoppedDL" {
				t.Fatalf("unverified torrent kept running: %v", err)
			}
		})
	}
}

func TestGuardStopsEvenWhenTagWriteFails(t *testing.T) {
	t.Parallel()
	q, m, r := guardServices()
	m.results[0].Discount = "PERCENT_50"
	q.tagErr = errors.New("tag write failed")
	if err := r.Guard(context.Background()); err == nil {
		t.Fatal("tag failure hidden")
	}
	if q.torrents[0].State != "stoppedDL" {
		t.Fatal("tag failure prevented stop")
	}
}

type movingGuardQBT struct {
	*fakeQBT
	reads int
}

func (q *movingGuardQBT) Torrents(ctx context.Context) ([]qbittorrent.Torrent, error) {
	q.reads++
	if q.reads == 2 {
		q.torrents[0].Category = "user"
	}
	return q.fakeQBT.Torrents(ctx)
}
func TestGuardRechecksOwnershipBeforeStop(t *testing.T) {
	t.Parallel()
	q, m, r := guardServices()
	m.results[0].Discount = "PERCENT_50"
	r.QBittorrent = &movingGuardQBT{fakeQBT: q}
	if err := r.Guard(context.Background()); err != nil {
		t.Fatal(err)
	}
	if q.mutations != 0 || q.torrents[0].State != "downloading" {
		t.Fatal("guard stopped recategorized user torrent")
	}
}

func TestWatchHaltsDownloadsOnCancellation(t *testing.T) {
	t.Parallel()
	q, _, r := guardServices()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.Watch(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if q.torrents[0].State != "stoppedDL" {
		t.Fatal("download outlived the guard")
	}
}

func TestHaltStopsCategoryDownloadWithoutAutomaticManagement(t *testing.T) {
	t.Parallel()
	q, _, r := guardServices()
	q.torrents[0].AutoTMM = false
	if err := r.Halt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if q.torrents[0].State != "stoppedDL" || !guardPaused(q.torrents[0]) {
		t.Fatal("category-owned download outlived the guard")
	}
}

func TestExecutePreservesGuardPausedNonFreeRegistration(t *testing.T) {
	t.Parallel()
	q, m, r := guardServices()
	q.torrents[0].Size = 30
	q.torrents[0].AmountLeft = 15
	q.torrents[0].Downloaded = 15
	q.torrents[0].State = "stoppedDL"
	q.torrents[0].DLRate = 0
	q.torrents[0].Tags = qbittorrent.GuardPausedTag
	q.torrents[0].SavePath = "/downloads/swarmfolio/active"
	q.torrents[0].ContentPath = q.torrents[0].SavePath
	q.torrents[0].AddedOn = appNow.Add(-time.Hour)
	q.torrents[0].LastActivity = appNow
	q.defaultPath = "/downloads"
	q.categoryPath = "/downloads/swarmfolio"
	m.results = nil
	r.Config.Policy.MaxAdditions = 0
	for _, apply := range []bool{false, true} {
		if _, err := r.Execute(context.Background(), apply); err != nil {
			t.Fatal(err)
		}
	}
	if len(q.torrents) != 1 || q.mutations != 0 {
		t.Fatalf("guard-paused registration lost: %v", q.events)
	}
}

func TestGuardPauseTagsDoNotDiscardUserTags(t *testing.T) {
	// Production tag writes use additive qBittorrent addTags; verify tag recognition
	// works when the API returns multiple tags in either position.
	t.Parallel()
	for _, tags := range []string{"custom, " + qbittorrent.GuardPausedTag, qbittorrent.GuardPausedTag + ", custom"} {
		if !guardPaused(qbittorrent.Torrent{Tags: tags}) {
			t.Fatalf("tag not recognized: %q", tags)
		}
	}
	if slices.Contains(strings.Split("custom", ","), qbittorrent.GuardPausedTag) {
		t.Fatal("invalid fixture")
	}
}
