package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/liblaf/swarmfolio/internal/mteam"
	"github.com/liblaf/swarmfolio/internal/qbittorrent"
)

const guardInterval = time.Minute

// Keep two guard intervals ahead even when the user requests no ETA buffer.
func (r Runner) freeleechBuffer() time.Duration {
	return max(r.Config.Policy.MinimumFreeleechRemaining, 2*guardInterval)
}
func guardPaused(torrent qbittorrent.Torrent) bool {
	for _, tag := range strings.Split(torrent.Tags, ",") {
		if strings.TrimSpace(tag) == qbittorrent.GuardPausedTag {
			return true
		}
	}
	return false
}
func (r Runner) guardedDownload(t qbittorrent.Torrent) bool {
	return t.Category == r.Config.QBittorrent.Category && t.AmountLeft > 0 && !stoppedIncompleteDownload(t)
}
func (r Runner) guardDefaults() Runner {
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.PollInterval <= 0 {
		r.PollInterval = defaultPollInterval
	}
	if r.PollTimeout <= 0 {
		r.PollTimeout = defaultPollTimeout
	}
	return r
}

// Guard checks every active managed download against the account's complete
// unfinished-torrent list. Missing or unverifiable promotion evidence stops
// downloads; completed seeds and unrelated categories are never stopped.
func (r Runner) Guard(ctx context.Context) error {
	if r.QBittorrent == nil || r.MTeam == nil {
		return errors.New("guard: clients are required")
	}
	r = r.guardDefaults()
	torrents, err := r.QBittorrent.Torrents(ctx)
	if err != nil {
		return fmt.Errorf("guard: read qBittorrent: %w", err)
	}
	var active []qbittorrent.Torrent
	for _, t := range torrents {
		if r.guardedDownload(t) {
			active = append(active, t)
		}
	}
	if len(active) == 0 {
		return nil
	}
	verifyCtx, cancelVerify := context.WithTimeout(ctx, 30*time.Second)
	defer cancelVerify()
	offers, err := r.MTeam.Offers(verifyCtx)
	if err != nil {
		stopErr := r.pauseGuard(ctx, active, "M-Team promotion verification failed")
		return errors.Join(fmt.Errorf("guard: verify M-Team promotions: %w", err), stopErr)
	}
	byID := make(map[int64]mteam.Torrent, len(offers))
	for _, offer := range offers {
		if offer.ID <= 0 || offer.Size <= 0 {
			return errors.Join(errors.New("guard: invalid M-Team offer"), r.pauseGuard(ctx, active, "invalid M-Team offer"))
		}
		if _, exists := byID[offer.ID]; exists {
			return errors.Join(errors.New("guard: duplicate M-Team offer"), r.pauseGuard(ctx, active, "duplicate M-Team offer"))
		}
		byID[offer.ID] = offer
	}
	var unsafe []qbittorrent.Torrent
	var evidenceErr error
	for _, t := range active {
		id, idErr := r.QBittorrent.MTeamID(verifyCtx, t.Hash)
		if idErr != nil {
			unsafe = append(unsafe, t)
			evidenceErr = errors.Join(evidenceErr, fmt.Errorf("guard: identify %s: %w", t.Hash, idErr))
			continue
		}
		offer, ok := byID[id]
		if !ok || offer.Size != t.Size || (offer.Discount != "FREE" && offer.Discount != "_2X_FREE") {
			unsafe = append(unsafe, t)
			continue
		}
		if offer.DiscountEndTime.IsZero() {
			continue
		}
		if !offer.DiscountEndTime.After(r.Now().Add(r.freeleechBuffer())) {
			unsafe = append(unsafe, t)
			continue
		}
		// A single slow swarm can be much slower than aggregate download capacity.
		// Require its own measured rate to cover its remaining bytes as well.
		rate := t.DLRate
		elapsed := int64(t.DownloadTime / time.Second)
		if elapsed > 0 && t.Downloaded > 0 {
			rate = min(rate, t.Downloaded/elapsed)
		}
		capacity, capacityErr := completionCapacity(r.Now(), []qbittorrent.Torrent{{AmountLeft: t.AmountLeft, DLRate: rate}}, r.Config.Policy.DownloadCompletionSafetyFactor)
		if capacityErr != nil {
			unsafe = append(unsafe, t)
			evidenceErr = errors.Join(evidenceErr, capacityErr)
			continue
		}
		if !completesBeforeFreeleechExpiry(r.Now(), offer.DiscountEndTime, r.freeleechBuffer(), 0, t.AmountLeft, capacity) {
			unsafe = append(unsafe, t)
		}
	}
	if len(unsafe) > 0 {
		err = r.pauseGuard(ctx, unsafe, "promotion ended, is unverifiable, or completion no longer fits the free period")
	}
	return errors.Join(evidenceErr, err)
}

func (r Runner) pauseGuard(ctx context.Context, expected []qbittorrent.Torrent, reason string) error {
	current, err := r.QBittorrent.Torrents(ctx)
	if err != nil {
		return fmt.Errorf("guard: recheck ownership before stopping: %w", err)
	}
	var hashes []string
	for _, t := range expected {
		fresh := findHash(current, t.Hash)
		if fresh != nil && r.guardedDownload(*fresh) {
			hashes = append(hashes, fresh.Hash)
		}
	}
	if len(hashes) == 0 {
		return nil
	}
	slices.Sort(hashes)
	// Stop first: tagging errors must never allow unsafe downloads to continue.
	stopErr := r.QBittorrent.Stop(ctx, hashes)
	tagErr := r.QBittorrent.SetGuardPaused(ctx, hashes)
	confirmErr := r.poll(ctx, "guard: confirm downloads stopped", func(pollCtx context.Context) (error, error) {
		all, err := r.QBittorrent.Torrents(pollCtx)
		if err != nil {
			return nil, err
		}
		for _, hash := range hashes {
			t := findHash(all, hash)
			if t != nil && r.guardedDownload(*t) {
				return fmt.Errorf("torrent %s remains in %s", hash, t.State), nil
			}
		}
		return nil, nil
	})
	if confirmErr == nil {
		log.Printf("Freeleech guard paused %d downloads: %s", len(hashes), reason)
	}
	return errors.Join(stopErr, tagErr, confirmErr)
}

// Watch runs separately from optimization, so deletion waits cannot prevent
// promotion checks. A failed pass leaves unsafe downloads stopped and causes
// the service to restart visibly.
func (r Runner) Watch(ctx context.Context) (watchErr error) {
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		watchErr = errors.Join(watchErr, r.Halt(cleanupCtx))
	}()
	r = r.guardDefaults()
	ticker := time.NewTicker(guardInterval)
	defer ticker.Stop()
	for {
		if err := r.Guard(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Halt stops active managed downloads when the guard is exiting or unavailable.
func (r Runner) Halt(ctx context.Context) error {
	if r.QBittorrent == nil {
		return errors.New("guard: qBittorrent client is required")
	}
	r = r.guardDefaults()
	all, err := r.QBittorrent.Torrents(ctx)
	if err != nil {
		return err
	}
	var active []qbittorrent.Torrent
	for _, t := range all {
		if r.guardedDownload(t) {
			active = append(active, t)
		}
	}
	return r.pauseGuard(ctx, active, "freeleech guard stopped")
}
