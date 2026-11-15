package server

import (
	"context"
	"time"

	"github.com/ericmustin/vern/internal/engine"
)

// sweepTimeout bounds one sweep, so a hung engine call cannot stall the janitor.
const sweepTimeout = time.Minute

// RunJanitor purges runs older than olderThan: once immediately, then every
// `every`, until ctx is done. Runs are purged by the request that made them;
// the janitor catches what a crash or a failed purge left behind. Failures are
// logged and retried on the next tick. With every <= 0 it sweeps once and
// returns.
func RunJanitor(ctx context.Context, eng engine.Engine, olderThan, every time.Duration, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	sweep := func() {
		sctx, cancel := context.WithTimeout(ctx, sweepTimeout)
		defer cancel()
		n, err := eng.Sweep(sctx, olderThan)
		switch {
		case err != nil && ctx.Err() == nil:
			logf("janitor: sweep failed: %v", err)
		case n > 0:
			logf("janitor: swept %d leftover run(s) older than %s", n, olderThan)
		}
	}
	sweep()
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}
