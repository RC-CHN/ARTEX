package server

import (
	"context"
	"errors"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

// Capacity and unavailable brokers are preparation conditions, not failed work.
// Keep the same claimed intent and cancellation scope while waiting; never burn
// through the frontier or replay a tool that may have started. Each acquisition
// remains bounded by acquire_timeout_seconds; retries continue until capacity
// returns or the user/planner/task lifetime cancels this work.
func waitWorkerPreparation(ctx context.Context, retryDelay time.Duration, execute func() error, report func(error, int, time.Duration)) error {
	started := time.Now()
	var lastReport time.Time
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := execute()
		if errors.Is(err, runner.ErrUnknownOutcome) || (!errors.Is(err, runner.ErrCapacityUnavailable) && !errors.Is(err, errRunnerPreparationUnavailable)) || ctx.Err() != nil {
			return err
		}
		if report != nil && (lastReport.IsZero() || time.Since(lastReport) >= 30*time.Second) {
			report(err, attempt+1, time.Since(started))
			lastReport = time.Now()
		}
		if sleepCtx(ctx, retryDelay) {
			return ctx.Err()
		}
	}
}
