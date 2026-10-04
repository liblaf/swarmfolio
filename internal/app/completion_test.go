package app

import (
	"testing"
	"time"

	"github.com/liblaf/swarmfolio/internal/config"
	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

func TestCompletionCapacityUsesTheLowerMeasuredRateAndSafetyFactor(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	capacity, err := completionCapacity(now, []qbittorrent.Torrent{
		{AmountLeft: 100, DLRate: 200, Downloaded: 1_000, DownloadTime: 4 * time.Second},
		{AmountLeft: 100, DLRate: 300, Downloaded: 1_000, DownloadTime: 6 * time.Second},
	}, 2)
	if err != nil || capacity != 100 {
		t.Fatalf("capacity=%d error=%v, want 100 and nil", capacity, err)
	}
}

func TestCompletionCapacityAcceptsEitherCurrentOrHistoricalEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, torrents := range [][]qbittorrent.Torrent{
		{{AmountLeft: 1, DLRate: 200}},
		{{Downloaded: 1_000, DownloadTime: 5 * time.Second, CompletionOn: now.Add(-recentDownloadWindow)}},
	} {
		capacity, err := completionCapacity(now, torrents, 2)
		if err != nil || capacity != 100 {
			t.Fatalf("capacity=%d error=%v, want 100 and nil", capacity, err)
		}
	}
}

func TestCompletionCapacityRefusesUnknownOrInvalidEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, torrents := range [][]qbittorrent.Torrent{
		nil,
		{{AmountLeft: 20}},
		{{AmountLeft: 20, DLRate: -1}},
	} {
		capacity, err := completionCapacity(now, torrents, 2)
		if capacity != 0 {
			t.Fatalf("capacity=%d error=%v, want zero", capacity, err)
		}
		if torrents != nil && torrents[0].DLRate < 0 && err == nil {
			t.Fatalf("invalid evidence error=%v, want error", err)
		}
	}
}

func TestCompletionCapacityRejectsStaleCompletedDownload(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	capacity, err := completionCapacity(now, []qbittorrent.Torrent{{
		Downloaded: 1 << 40, DownloadTime: time.Second, CompletionOn: now.Add(-recentDownloadWindow - time.Nanosecond),
	}}, 1)
	if err != nil || capacity != 0 {
		t.Fatalf("capacity=%d error=%v, want zero and nil", capacity, err)
	}
}

func TestCompletionCapacityRejectsStalledDownloadHistory(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	capacity, err := completionCapacity(now, []qbittorrent.Torrent{{
		AmountLeft: 1, Downloaded: 1 << 40, DownloadTime: time.Second,
	}}, 1)
	if err != nil || capacity != 0 {
		t.Fatalf("capacity=%d error=%v, want zero and nil", capacity, err)
	}
}

func TestCompletionFitsRejectsExpiredCachedCapacity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	runner := Runner{
		Config:             config.Settings{Policy: config.Policy{DownloadCompletionSafetyFactor: 2}},
		downloadCapacity:   100,
		capacityObservedAt: now.Add(-recentDownloadWindow - time.Nanosecond),
	}
	fits, err := runner.completionFits(now, now.Add(time.Hour), 1, nil, "")
	if err != nil || fits {
		t.Fatalf("fits=%t error=%v, want false and nil", fits, err)
	}
}

func TestCompletesBeforeFreeleechExpiryIncludesExistingQueueAndMargin(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	if completesBeforeFreeleechExpiry(now, now.Add(4*time.Second), time.Second, 300, 100, 100) {
		t.Fatal("four queued seconds must not fit in the three-second test window")
	}
	if !completesBeforeFreeleechExpiry(now, now.Add(5*time.Second), time.Second, 300, 100, 100) {
		t.Fatal("four queued seconds must fit in the four-second test window")
	}
}

func TestCompletesBeforeFreeleechExpiryRejectsUnknownCapacityAndOverflow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	if completesBeforeFreeleechExpiry(now, now.Add(time.Hour), 0, 1, 1, 0) {
		t.Fatal("unknown capacity admitted timed candidate")
	}
	if completesBeforeFreeleechExpiry(now, now.Add(time.Hour), 0, int64(^uint64(0)>>1), 1, 1) {
		t.Fatal("overflowing queue admitted timed candidate")
	}
}
