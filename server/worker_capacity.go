package server

import (
	"context"
	"errors"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

// Capacity is an execution preparation condition, not a failed worker result.
// Keep the same claimed intent and cancellation scope while waiting; never burn
// through the frontier or replay a tool that may have started. Each acquisition
// remains bounded by acquire_timeout_seconds; retries continue until capacity
// returns or the user/planner/task lifetime cancels this work.
func waitWorkerCapacity(ctx context.Context, retryDelay time.Duration, execute func() error, report func(int, time.Duration)) error {
	started := time.Now()
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := execute()
		if !errors.Is(err, runner.ErrCapacityUnavailable) || ctx.Err() != nil {
			return err
		}
		if report != nil {
			report(attempt+1, time.Since(started))
		}
		if sleepCtx(ctx, retryDelay) {
			return ctx.Err()
		}
	}
}
