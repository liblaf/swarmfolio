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

// searchFailureMTeam makes a failed promotion refresh observable at a specific
// search, after the initial offer was accepted.
type searchFailureMTeam struct {
	*fakeMTeam
	calls  int
	failAt int
}

func (m *searchFailureMTeam) Search(ctx context.Context) ([]mteam.Torrent, error) {
	m.calls++
	if m.calls == m.failAt {
		return nil, errors.New("promotion refresh failed")
	}
	return m.fakeMTeam.Search(ctx)
}

// promotionChangeQBT changes the offer only after an incumbent is deleted.
type promotionChangeQBT struct {
	*fakeQBT
	change func()
}

func (q *promotionChangeQBT) Delete(ctx context.Context, hashes []string, withFiles bool) error {
	if err := q.fakeQBT.Delete(ctx, hashes, withFiles); err != nil {
		return err
	}
	if slices.Contains(hashes, "old") {
		q.change()
	}
	return nil
}

func TestExecuteStartsOnlyDownloadFreeOffers(t *testing.T) {
	t.Parallel()
	for _, discount := range []string{"FREE", "_2X_FREE"} {
		t.Run(discount, func(t *testing.T) {
			t.Parallel()
			qbt, mt := testServices(t)
			mt.results[0].Discount = discount
			_, err := testRunner(qbt, mt).Execute(context.Background(), true)
			if err != nil {
				t.Fatal(err)
			}
			if prefixIndex(qbt.events, "start:") < 0 {
				t.Fatalf("free offer was not started: events=%v", qbt.events)
			}
		})
	}
}

func TestExecuteDoesNotAddUnverifiableOrExpiredInitialOffer(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		discount string
		expires  time.Time
	}{
		{name: "nonfree", discount: "PERCENT_50", expires: appNow.Add(3 * time.Hour)},
		{name: "unknown", discount: "UNKNOWN", expires: appNow.Add(3 * time.Hour)},
		{name: "no expiry", discount: "FREE"},
		{name: "expired", discount: "FREE", expires: appNow},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			qbt, mt := testServices(t)
			mt.results[0].Discount = test.discount
			mt.results[0].DiscountEndTime = test.expires
			_, _ = testRunner(qbt, mt).Execute(context.Background(), true)
			if qbt.mutations != 0 || prefixIndex(qbt.events, "start:") >= 0 {
				t.Fatalf("invalid initial offer caused mutation: events=%v", qbt.events)
			}
		})
	}
}

func TestExecutePreservesIncumbentWhenPromotionChangesBeforeReplacement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*fakeMTeam)
	}{
		{name: "nonfree", change: func(m *fakeMTeam) { m.results[0].Discount = "PERCENT_50" }},
		{name: "unknown", change: func(m *fakeMTeam) { m.results[0].Discount = "UNKNOWN" }},
		{name: "missing expiry", change: func(m *fakeMTeam) { m.results[0].DiscountEndTime = time.Time{} }},
		{name: "expired", change: func(m *fakeMTeam) { m.results[0].DiscountEndTime = appNow }},
		{name: "missing offer", change: func(m *fakeMTeam) { m.results = nil }},
		{name: "size changed", change: func(m *fakeMTeam) { m.results[0].Size++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			qbt, mt := testServices(t)
			qbt.onAdd = func(*fakeQBT) { test.change(mt) }
			_, err := testRunner(qbt, mt).Execute(context.Background(), true)
			if err == nil {
				t.Fatal("changed offer was accepted")
			}
			if len(qbt.torrents) == 0 || qbt.torrents[0].Hash != "old" ||
				prefixIndex(qbt.events, "delete:old") >= 0 || prefixIndex(qbt.events, "start:") >= 0 {
				t.Fatalf("changed offer replaced or started a torrent: events=%v torrents=%#v", qbt.events, qbt.torrents)
			}
		})
	}
}

func TestExecutePreservesIncumbentWhenPromotionRefreshFails(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	runner := testRunner(qbt, mt)
	runner.MTeam = &searchFailureMTeam{fakeMTeam: mt, failAt: 2}
	_, err := runner.Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "promotion refresh failed") {
		t.Fatalf("promotion refresh error = %v", err)
	}
	if len(qbt.torrents) == 0 || qbt.torrents[0].Hash != "old" ||
		prefixIndex(qbt.events, "delete:old") >= 0 || prefixIndex(qbt.events, "start:") >= 0 {
		t.Fatalf("failed refresh replaced or started a torrent: events=%v torrents=%#v", qbt.events, qbt.torrents)
	}
}

func TestExecuteDoesNotStartAfterPromotionChangesFollowingReplacement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*fakeMTeam)
	}{
		{name: "nonfree", change: func(m *fakeMTeam) { m.results[0].Discount = "PERCENT_50" }},
		{name: "expired", change: func(m *fakeMTeam) { m.results[0].DiscountEndTime = appNow }},
		{name: "missing offer", change: func(m *fakeMTeam) { m.results = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			qbt, mt := testServices(t)
			runner := testRunner(qbt, mt)
			runner.QBittorrent = &promotionChangeQBT{fakeQBT: qbt, change: func() { test.change(mt) }}
			_, err := runner.Execute(context.Background(), true)
			if err == nil {
				t.Fatal("changed offer was started")
			}
			if prefixIndex(qbt.events, "delete:old") < 0 || prefixIndex(qbt.events, "start:") >= 0 ||
				len(qbt.torrents) != 1 || qbt.torrents[0].State != "stoppedDL" {
				t.Fatalf("changed offer was started or pending state was lost: events=%v torrents=%#v", qbt.events, qbt.torrents)
			}
		})
	}
}

func TestExecuteDoesNotStartAfterFinalPromotionRefreshFails(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	runner := testRunner(qbt, mt)
	runner.MTeam = &searchFailureMTeam{fakeMTeam: mt, failAt: 3}
	_, err := runner.Execute(context.Background(), true)
	if err == nil || !strings.Contains(err.Error(), "promotion refresh failed") {
		t.Fatalf("final promotion refresh error = %v", err)
	}
	if prefixIndex(qbt.events, "delete:old") < 0 || prefixIndex(qbt.events, "start:") >= 0 ||
		len(qbt.torrents) != 1 || qbt.torrents[0].State != "stoppedDL" {
		t.Fatalf("failed final refresh started torrent or lost pending state: events=%v torrents=%#v", qbt.events, qbt.torrents)
	}
}

func TestExecuteDoesNotResumePendingAfterPromotionRevocation(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	qbt = pendingQBT(qbt.addHash)
	runner := testRunner(qbt, mt)
	runner.Config.Policy.MaxAdditions = 0
	searches := 0
	runner.MTeam = &changingSearchMTeam{fakeMTeam: mt, change: func() {
		searches++
		if searches == 2 {
			mt.results[0].Discount = "PERCENT_50"
		}
	}}
	_, err := runner.Execute(context.Background(), true)
	if err == nil {
		t.Fatal("revoked pending offer was resumed")
	}
	if prefixIndex(qbt.events, "start:") >= 0 || len(qbt.torrents) != 1 || qbt.torrents[0].State != "stoppedDL" {
		t.Fatalf("revoked pending offer was started or removed: events=%v torrents=%#v", qbt.events, qbt.torrents)
	}
}

type changingSearchMTeam struct {
	*fakeMTeam
	change func()
}

func (m *changingSearchMTeam) Search(ctx context.Context) ([]mteam.Torrent, error) {
	m.change()
	return m.fakeMTeam.Search(ctx)
}

func TestExecuteRejectsPromotionExpiringExactlyNowWithZeroMinimum(t *testing.T) {
	t.Parallel()
	qbt, mt := testServices(t)
	runner := testRunner(qbt, mt)
	runner.Config.Policy.MinimumFreeleechRemaining = 0
	qbt.onAdd = func(*fakeQBT) { mt.results[0].DiscountEndTime = appNow }
	_, err := runner.Execute(context.Background(), true)
	if err == nil || prefixIndex(qbt.events, "start:") >= 0 || prefixIndex(qbt.events, "delete:old") >= 0 {
		t.Fatalf("offer expiring exactly now was accepted: error=%v events=%v", err, qbt.events)
	}
}

type expiryDuringValidationQBT struct {
	*fakeQBT
	advanceTime func()
}

func (q *expiryDuringValidationQBT) Torrents(ctx context.Context) ([]qbittorrent.Torrent, error) {
	q.advanceTime()
	return q.fakeQBT.Torrents(ctx)
}

func TestExecuteRechecksExpiryAfterQBTValidation(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"replacement", "start", "resume"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			qbt, mt := testServices(t)
			switch stage {
			case "start":
				qbt.torrents = nil
			case "resume":
				qbt = pendingQBT(qbt.addHash)
			}
			runner := testRunner(qbt, mt)
			runner.Config.Policy.MinimumFreeleechRemaining = 0
			now := appNow
			runner.Now = func() time.Time { return now }
			searches := 0
			runner.MTeam = &changingSearchMTeam{fakeMTeam: mt, change: func() { searches++ }}
			runner.QBittorrent = &expiryDuringValidationQBT{fakeQBT: qbt, advanceTime: func() {
				if searches >= 2 {
					now = mt.results[0].DiscountEndTime
				}
			}}
			_, err := runner.Execute(context.Background(), true)
			if err == nil || !strings.Contains(err.Error(), "required freeleech time") {
				t.Fatalf("expiry during qBittorrent validation error = %v", err)
			}
			if prefixIndex(qbt.events, "start:") >= 0 || prefixIndex(qbt.events, "delete:old") >= 0 {
				t.Fatalf("expired offer caused a start or replacement: events=%v", qbt.events)
			}
		})
	}
}
