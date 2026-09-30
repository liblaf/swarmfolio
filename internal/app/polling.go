package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// pollTimeoutError reports why a bounded wait did not converge. It keeps the
// last state observed from qBittorrent separately from the last failed read,
// so an unresponsive qBittorrent is distinguishable from slow convergence.
// The observed condition is descriptive only and deliberately not unwrapped:
// callers must not mistake it, for example a budget shortfall, for the cause.
type pollTimeoutError struct {
	what    string
	timeout time.Duration
	cause   error
	pending error // Nil when no read succeeded during the wait.
	readErr error
}

func (e *pollTimeoutError) Error() string {
	parts := []string{fmt.Sprintf("%s within %s: %v", e.what, e.timeout, e.cause)}
	if e.pending != nil {
		parts = append(parts, "last observed: "+e.pending.Error())
	} else {
		parts = append(parts, "no qBittorrent read succeeded during the wait")
	}
	if e.readErr != nil {
		parts = append(parts, "last read error: "+e.readErr.Error())
	}
	return strings.Join(parts, "; ")
}

func (e *pollTimeoutError) Unwrap() []error {
	if e.readErr == nil {
		return []error{e.cause}
	}
	return []error{e.cause, e.readErr}
}

// Polls back off to this interval, so a qBittorrent stalled by disk I/O is
// not asked for fresh state every second for the whole wait.
const maxPollInterval = 15 * time.Second

// poll calls attempt until it reports no pending condition. A non-nil pending
// value means the observation succeeded but has not converged. Read timeouts
// are retried within the polling window; any other error fails at once. The
// delay between attempts starts at PollInterval and doubles up to
// maxPollInterval.
func (r Runner) poll(ctx context.Context, what string, attempt func(context.Context) (pending, err error)) error {
	pollCtx, cancel := context.WithTimeout(ctx, r.PollTimeout)
	defer cancel()
	delay := r.PollInterval
	timer := time.NewTimer(delay)
	defer timer.Stop()
	var lastPending, lastReadErr error
	for {
		pending, err := attempt(pollCtx)
		switch {
		case err != nil:
			if !retryableReadTimeout(err) {
				return err
			}
			lastReadErr = err
		case pending == nil:
			return nil
		default:
			lastPending, lastReadErr = pending, nil
		}
		if pollCtx.Err() == nil {
			select {
			case <-pollCtx.Done():
			case <-timer.C:
				delay = min(2*delay, max(maxPollInterval, r.PollInterval))
				timer.Reset(delay)
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return &pollTimeoutError{what: what, timeout: r.PollTimeout, cause: pollCtx.Err(), pending: lastPending, readErr: lastReadErr}
	}
}

// Classify the read error independently of the polling deadline: a read cut
// off by the deadline wraps context.DeadlineExceeded and is still retryable,
// so poll reports it as a timeout once it observes the expired context.
func retryableReadTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}
