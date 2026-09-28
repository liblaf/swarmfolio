package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/liblaf/swarmfolio/internal/optimizer"
)

// waitForRegistrationsRemoved waits only for qBittorrent to remove torrents
// from its registry. It deliberately does not wait for disk space: callers
// that retain files must not treat a registration delete as a space reclaim.
func (r Runner) waitForRegistrationsRemoved(ctx context.Context, hashes []string) error {
	pollCtx, cancel := context.WithTimeout(ctx, r.PollTimeout)
	defer cancel()
	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()
	for {
		torrents, err := r.QBittorrent.Torrents(pollCtx)
		if err != nil {
			if pollCtx.Err() != nil {
				return fmt.Errorf("wait for qBittorrent to remove registrations within %s: %w", r.PollTimeout, errors.Join(pollCtx.Err(), err))
			}
			if !retryableReadTimeout(pollCtx, err) {
				return fmt.Errorf("list qBittorrent torrents while waiting for registration removal: %w", err)
			}
			select {
			case <-pollCtx.Done():
				return fmt.Errorf("wait for qBittorrent to remove registrations within %s after transient read timeouts: %w", r.PollTimeout, errors.Join(pollCtx.Err(), err))
			case <-ticker.C:
				continue
			}
		}
		pending := ""
		for _, hash := range hashes {
			if findHash(torrents, hash) != nil {
				pending = hash
				break
			}
		}
		if pending == "" {
			return nil
		}
		select {
		case <-pollCtx.Done():
			return fmt.Errorf("wait for qBittorrent to remove registration %q within %s: %w", pending, r.PollTimeout, pollCtx.Err())
		case <-ticker.C:
		}
	}
}

// waitForRemovals waits for both the torrent list and physical-space accounting
// to catch up with an accepted delete. Invalid responses still fail immediately.
func (r Runner) waitForRemovals(ctx context.Context, removals []optimizer.Removal) (snapshot, error) {
	pollCtx, cancel := context.WithTimeout(ctx, r.PollTimeout)
	defer cancel()
	ticker := time.NewTicker(r.PollInterval)
	defer ticker.Stop()
	for {
		state, err := r.snapshot(pollCtx)
		if err != nil {
			if pollCtx.Err() != nil {
				return snapshot{}, fmt.Errorf("wait for deletion and disk space within %s: %w", r.PollTimeout, errors.Join(pollCtx.Err(), err))
			}
			if !retryableReadTimeout(pollCtx, err) {
				return snapshot{}, fmt.Errorf("read qBittorrent state while waiting for deletion: %w", err)
			}
			select {
			case <-pollCtx.Done():
				return snapshot{}, fmt.Errorf("wait for deletion and disk space within %s after transient read timeouts: %w", r.PollTimeout, errors.Join(pollCtx.Err(), err))
			case <-ticker.C:
				continue
			}
		}
		var pending error
		for _, removal := range removals {
			if findHash(state.all, removal.Hash) != nil {
				pending = fmt.Errorf("deleted torrent %s is still present", removal.Hash)
				break
			}
		}
		if pending == nil {
			pending = validatePlannedUsed(state, 0, nil)
		}
		if pending == nil && state.budget.FreeBytes-state.budget.OutstandingBytes < state.budget.RequiredFreeBytes {
			pending = errors.New("reclaimed disk space does not yet preserve the free-space reserve")
		}
		if pending == nil {
			return state, nil
		}
		select {
		case <-pollCtx.Done():
			return snapshot{}, fmt.Errorf("wait for deletion and disk space within %s: %w", r.PollTimeout, errors.Join(pollCtx.Err(), pending))
		case <-ticker.C:
		}
	}
}

// retryableReadTimeout accepts only a lower-level transport deadline while the
// caller's polling budget remains live. A completed poll context is the final
// deadline, never a reason to keep polling.
func retryableReadTimeout(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}
