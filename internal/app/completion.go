package app

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

const recentDownloadWindow = 15 * time.Minute

// completionCapacity is a conservative estimate derived only from qBittorrent transfer
// evidence. A timed offer is unsafe when qBittorrent has neither a current
// positive aggregate download rate nor recorded downloaded-byte duration.
//
// The safety factor deliberately reduces, rather than invents, capacity. The
// caller supplies the configured factor after configuration validation.

func completionCapacity(now time.Time, torrents []qbittorrent.Torrent, safetyFactor float64) (int64, error) {
	if now.IsZero() {
		return 0, errors.New("download capacity measurement time is required")
	}
	if safetyFactor < 1 || math.IsNaN(safetyFactor) || math.IsInf(safetyFactor, 0) {
		return 0, errors.New("download completion safety factor must be finite and at least 1")
	}

	var current, downloaded, seconds int64
	for _, torrent := range torrents {
		if torrent.AmountLeft < 0 || torrent.DLRate < 0 || torrent.Downloaded < 0 || torrent.DownloadTime < 0 {
			return 0, errors.New("qBittorrent returned invalid download evidence")
		}
		active := torrent.AmountLeft > 0 && torrent.DLRate > 0
		if active {
			if torrent.DLRate > math.MaxInt64-current {
				return 0, errors.New("current aggregate download rate overflows int64")
			}
			current += torrent.DLRate
		}
		// A completed transfer is usable only while it remains recent. An
		// active transfer must have a positive current rate; its own history
		// can then only reduce that live measurement.
		elapsed := int64(torrent.DownloadTime / time.Second)
		recentCompletion := !torrent.CompletionOn.IsZero() && !torrent.CompletionOn.After(now) && now.Sub(torrent.CompletionOn) <= recentDownloadWindow
		if torrent.Downloaded > 0 && elapsed > 0 && (active || recentCompletion) {
			if torrent.Downloaded > math.MaxInt64-downloaded || elapsed > math.MaxInt64-seconds {
				return 0, errors.New("historical download evidence overflows int64")
			}
			downloaded += torrent.Downloaded
			seconds += elapsed
		}
	}

	measured := int64(0)
	if current > 0 {
		measured = current
	}
	if downloaded > 0 && seconds > 0 {
		historical := downloaded / seconds
		if historical > 0 && (measured == 0 || historical < measured) {
			measured = historical
		}
	}
	if measured == 0 {
		return 0, nil
	}
	reduced := math.Floor(float64(measured) / safetyFactor)
	if reduced >= float64(math.MaxInt64) {
		return math.MaxInt64, nil
	}
	return int64(reduced), nil
}

// completesBeforeFreeleechExpiry reserves the configured freeleech buffer and
// schedules every unfinished byte already committed to qBittorrent ahead of
// the candidate. It only accepts a timed candidate when all of those bytes
// can finish with the reduced measured capacity.
func completesBeforeFreeleechExpiry(now, freeUntil time.Time, minimumRemaining time.Duration, queuedBytes, candidateBytes, capacity int64) bool {
	if now.IsZero() || freeUntil.IsZero() || minimumRemaining < 0 || queuedBytes < 0 || candidateBytes <= 0 || capacity <= 0 {
		return false
	}
	available := freeUntil.Sub(now) - minimumRemaining
	if available <= 0 || queuedBytes > math.MaxInt64-candidateBytes {
		return false
	}
	// qBittorrent reports bytes per second. Rounding the available time down
	// prevents fractional seconds from making the admission optimistic.
	seconds := int64(available / time.Second)
	if seconds <= 0 {
		return false
	}
	// Divide first, avoiding an overflow in capacity*seconds. This is a
	// conservative whole-second schedule: an unfinished remainder consumes
	// one additional second.
	bytes := queuedBytes + candidateBytes
	required := bytes / capacity
	if bytes%capacity != 0 {
		required++
	}
	return required <= seconds
}

// queuedDownloadBytes budgets active downloads. Guard-paused payloads remain on
// disk but make no bandwidth commitment until separately verified for resume.
func queuedDownloadBytes(torrents []qbittorrent.Torrent, excludeHash string) (int64, error) {
	var queued int64
	for _, torrent := range torrents {
		if torrent.AmountLeft < 0 {
			return 0, errors.New("qBittorrent returned negative remaining bytes")
		}
		if torrent.Hash == excludeHash || stoppedIncompleteDownload(torrent) {
			continue
		}
		if torrent.AmountLeft > math.MaxInt64-queued {
			return 0, errors.New("queued download bytes overflow int64")
		}
		queued += torrent.AmountLeft
	}
	return queued, nil
}

type deadlineExceededError struct{ until time.Time }

func (e *deadlineExceededError) Error() string {
	return "download cannot conservatively finish before freeleech ends at " + e.until.Format(time.RFC3339)
}

func (r Runner) completionFits(now, until time.Time, candidateBytes int64, torrents []qbittorrent.Torrent, excludeHash string) (bool, error) {
	if until.IsZero() || candidateBytes == 0 {
		return true, nil
	}
	capacity, err := completionCapacity(now, torrents, r.Config.Policy.DownloadCompletionSafetyFactor)
	if err != nil {
		return false, err
	}
	// A verified completed torrent may be removed during replacement. Keep the
	// capacity measured earlier in this run; reduce it when fresh evidence is slower.
	cacheFresh := !r.capacityObservedAt.IsZero() && !now.Before(r.capacityObservedAt) && now.Sub(r.capacityObservedAt) <= recentDownloadWindow
	if cacheFresh && r.downloadCapacity > 0 && (capacity == 0 || r.downloadCapacity < capacity) {
		capacity = r.downloadCapacity
	}
	queued, err := queuedDownloadBytes(torrents, excludeHash)
	if err != nil {
		return false, err
	}
	return completesBeforeFreeleechExpiry(now, until, r.freeleechBuffer(), queued, candidateBytes, capacity), nil
}

func (r Runner) requireCompletion(ctx context.Context, until time.Time, candidateBytes int64, excludeHash string) error {
	if until.IsZero() || candidateBytes == 0 {
		return nil
	}
	torrents, err := r.QBittorrent.Torrents(ctx)
	if err != nil {
		return err
	}
	fits, err := r.completionFits(r.Now(), until, candidateBytes, torrents, excludeHash)
	if err != nil {
		return err
	}
	if !fits {
		return &deadlineExceededError{until: until}
	}
	return nil
}

func pendingBytes(torrents []qbittorrent.Torrent) int64 {
	var total int64
	for _, torrent := range torrents {
		total += torrent.AmountLeft
	}
	return total
}
