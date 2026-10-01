package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/liblaf/swarmfolio/internal/optimizer"
)

// waitForRegistrationsRemoved waits only for qBittorrent to remove torrents
// from its registry. It deliberately does not wait for disk space: callers
// that retain files must not treat a registration delete as a space reclaim.
func (r Runner) waitForRegistrationsRemoved(ctx context.Context, hashes []string) error {
	return r.poll(ctx, "wait for qBittorrent to remove registrations", func(ctx context.Context) (error, error) {
		torrents, err := r.QBittorrent.Torrents(ctx)
		if err != nil {
			return nil, fmt.Errorf("list qBittorrent torrents while waiting for registration removal: %w", err)
		}
		for _, hash := range hashes {
			if findHash(torrents, hash) != nil {
				return fmt.Errorf("registration %q is still present", hash), nil
			}
		}
		return nil, nil
	})
}

// waitForRemovals waits for both the torrent list and physical-space accounting
// to catch up with an accepted delete. Invalid responses still fail immediately.
func (r Runner) waitForRemovals(ctx context.Context, removals []optimizer.Removal) (snapshot, error) {
	return r.waitForSnapshot(ctx, "wait for deletion and disk space", func(state snapshot) error {
		for _, removal := range removals {
			if findHash(state.all, removal.Hash) != nil {
				return fmt.Errorf("deleted torrent %s is still present", removal.Hash)
			}
		}
		if err := validatePlannedUsed(state, 0, nil); err != nil {
			return err
		}
		if !preservesReserve(state) {
			return errors.New("reclaimed disk space does not yet preserve the free-space reserve")
		}
		return nil
	})
}

// waitForPendingBudget gives space reclaimed by an earlier, interrupted run
// time to appear before a valid pending addition is judged over budget.
// Removing it early would make the planner delete further replacements for
// the same offer.
func (r Runner) waitForPendingBudget(ctx context.Context) (snapshot, error) {
	return r.waitForSnapshot(ctx, "wait for pending torrents to fit the budget", func(state snapshot) error {
		return validatePlannedUsed(state, 0, nil)
	})
}

// waitForSnapshot polls qBittorrent snapshots until pending reports that the
// latest one has converged, and returns that snapshot.
func (r Runner) waitForSnapshot(ctx context.Context, what string, pending func(snapshot) error) (snapshot, error) {
	var state snapshot
	err := r.poll(ctx, what, func(ctx context.Context) (error, error) {
		var err error
		state, err = r.snapshot(ctx)
		if err != nil {
			return nil, err
		}
		return pending(state), nil
	})
	if err != nil {
		return snapshot{}, err
	}
	return state, nil
}

func preservesReserve(state snapshot) bool {
	return state.budget.FreeBytes-state.budget.OutstandingBytes >= state.budget.RequiredFreeBytes
}
