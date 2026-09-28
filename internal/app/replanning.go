package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/liblaf/swarmfolio/internal/optimizer"
)

// A changing disk estimate must not cause an unbounded planning loop.
const maxBudgetReplans = 3

type budgetExceededError struct {
	projected int64
	limit     int64
}

func (e *budgetExceededError) Error() string {
	return fmt.Sprintf("projected %d bytes exceeds current %d-byte limit", e.projected, e.limit)
}

func (r Runner) applyPlan(ctx context.Context, candidates []optimizer.Candidate, plan optimizer.Plan, resolved *candidateResolver, report *Report) error {
	completed, removed := 0, 0
	appliedGain := 0.0
	for len(plan.Additions) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		addition := plan.Additions[0]
		mutationsBefore := len(report.Mutations)
		if err := r.applyAddition(ctx, addition, resolved); err != nil {
			var exceeded *budgetExceededError
			// Only replan a capacity rejection before this addition has made
			// any mutation attempt. Interrupted or unconfirmed writes must use
			// the existing recovery path, with their receipts intact.
			if !errors.As(err, &exceeded) || len(report.Mutations) != mutationsBefore {
				return err
			}
			if report.Replans >= maxBudgetReplans {
				return fmt.Errorf("portfolio budget kept changing after %d replans: %w", report.Replans, err)
			}
			state, err := r.snapshot(ctx)
			if err != nil {
				return fmt.Errorf("refresh qBittorrent to replan remaining additions: %w", err)
			}
			// Action caps apply to the whole run, including completed steps.
			planner := r
			planner.Config.Policy.MaxAdditions -= completed
			planner.Config.Policy.MaxRemovals -= removed
			if state.budget.LimitBytes == 0 {
				plan = optimizer.Plan{UsedBytes: state.budget.UsedBytes}
			} else {
				plan, err = planner.buildPlan(ctx, r.Now(), candidates, state, resolved)
				if err != nil {
					return fmt.Errorf("replan remaining additions: %w", err)
				}
			}
			report.Replans++
			report.ProjectedUsedBytes = plan.UsedBytes
			report.NetGain = appliedGain + plan.NetGain
			report.Actions = append(report.Actions[:completed], reportActions(plan)...)
			if plan.UsedBytes > plan.LimitBytes {
				return fmt.Errorf("no safe remaining plan fits the %d-byte portfolio limit while preserving %d free bytes", plan.LimitBytes, state.budget.RequiredFreeBytes)
			}
			continue
		}
		report.Actions[completed].Applied = true
		completed++
		removed += len(addition.Removals)
		appliedGain += addition.UploadScore
		for _, removal := range addition.Removals {
			appliedGain -= r.Config.Policy.ReplacementMargin * removal.UploadScore
		}
		plan.Additions = plan.Additions[1:]
	}
	return nil
}
