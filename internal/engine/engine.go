// Package engine is the vendor-neutral seam between the scorer and a
// storage/query backend.
package engine

import (
	"context"
	"time"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/otlpin"
)

// ErrBusy is returned (wrapped) when the backend sheds load, so callers can
// tell "try again shortly" from a failure.
var ErrBusy = evaluate.ErrBusy

// LoadStats reports what happened when a sample was loaded. Keys are signals.
type LoadStats struct {
	// Sent is the number of items sent (logs: log records plus span events).
	Sent map[string]int
	// Indexed is the document count after refresh. -1 means "not checkable":
	// for metrics, Elasticsearch groups data points into documents, so the
	// document count cannot be compared to Sent.
	Indexed map[string]int
	// Failed is the number rejected via partial_success or found in the
	// failure store. For metrics it is a lower bound: failure-store
	// documents may each hold several data points.
	Failed map[string]int
}

// Dropped returns, per signal, max(Failed, Sent-Indexed when Indexed >= 0).
// Signals with no drops are omitted.
func (s LoadStats) Dropped() map[string]int {
	out := map[string]int{}
	signals := map[string]struct{}{}
	for k := range s.Sent {
		signals[k] = struct{}{}
	}
	for k := range s.Failed {
		signals[k] = struct{}{}
	}
	for k := range signals {
		d := s.Failed[k]
		if idx, ok := s.Indexed[k]; ok && idx >= 0 {
			if lost := s.Sent[k] - idx; lost > d {
				d = lost
			}
		}
		if d > 0 {
			out[k] = d
		}
	}
	return out
}

// Engine loads a sample into a backend, exposes a query session scoped to
// that run's data, and cleans up afterwards.
type Engine interface {
	Ready(ctx context.Context) error
	Load(ctx context.Context, id RunID, s *otlpin.Sample) (*LoadStats, error)
	// Fields discovers the scannable fields of the run's data, per signal,
	// for the sensitive-data rules.
	Fields(ctx context.Context, id RunID) (mappings.Fields, error)
	Session(id RunID) (evaluate.Executor, config.IndexPatterns)
	Purge(ctx context.Context, id RunID) error
	Sweep(ctx context.Context, olderThan time.Duration) (int, error)
}
