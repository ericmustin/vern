// Package samplescore is the single path that takes a decoded OTLP sample
// through a self-managed engine and produces a scored report.
package samplescore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/ericmustin/vern/configs"
	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/otlpin"
	"github.com/ericmustin/vern/spec"
)

// CleanupFailedNote is the report note when purging a successful run fails.
const CleanupFailedNote = "cleanup of this run's data failed; a later sweep will remove it"

// Options tune a Score call. The zero value uses embedded defaults.
type Options struct {
	Config       *config.Config         // nil -> config.Default()
	Mappings     *mappings.MappingsFile // nil -> embedded ES|QL mappings
	MappingsFile string                 // report label for Mappings; "" -> embedded
	SpecRules    []coverage.SpecRule    // nil -> embedded spec rules
	Files        []string               // names shown in the report
	Keep         bool                   // skip purge (debugging)
	Now          func() time.Time       // nil -> time.Now
	// Sweep, when > 0, purges leftover runs older than this before loading
	// (best effort: failures are logged, never fatal). 0 disables it.
	Sweep time.Duration
	Logf  func(format string, args ...any) // nil -> discard
	// QueryErrorDetail lets Logf receive the full error of a failed
	// sensitive-data rule query (evaluate.RunOptions.QueryErrorDetail): set
	// it only when the log is the user's own terminal, as `vern score` does.
	QueryErrorDetail bool
}

// Score shifts the sample to the present, loads it into eng, evaluates the
// rules against that run's data only, and purges the data. It mutates s: the
// sample is time-shifted to the present and sanitized while loading, so callers
// must not reuse it. The report's Problems field is set from evaluate.Problems.
func Score(ctx context.Context, eng engine.Engine, s *otlpin.Sample, opts Options) (rep *evaluate.Report, err error) {
	sum := otlpin.Summarize(s)
	if sum.Items() == 0 {
		return nil, otlpin.ErrEmpty
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	cfg := opts.Config
	if cfg == nil {
		cfg = config.Default()
	}
	mf := opts.Mappings
	if mf == nil {
		var err error
		if mf, err = mappings.LoadBytes(configs.ESQLMappings, "embedded esql-mappings.yaml"); err != nil {
			return nil, fmt.Errorf("load embedded mappings: %w", err)
		}
	}
	rules := opts.SpecRules
	if rules == nil {
		var err error
		if rules, err = coverage.LoadSpecRulesFS(spec.FS, "rules"); err != nil {
			return nil, fmt.Errorf("load embedded spec rules: %w", err)
		}
	}

	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	if err := eng.Ready(ctx); err != nil {
		return nil, fmt.Errorf("engine not ready: %w", err)
	}
	if opts.Sweep > 0 {
		// Clean up runs left behind by interrupted or crashed invocations.
		n, serr := eng.Sweep(ctx, opts.Sweep)
		if serr != nil {
			logf("warning: sweeping old runs: %v", serr)
		}
		if n > 0 {
			logf("swept %d leftover run(s) older than %s", n, opts.Sweep)
		}
	}
	t := now()
	id := engine.NewRunID(t)

	otlpin.Shift(s, otlpin.OffsetToNow(sum, t))
	var stats *engine.LoadStats
	stats, err = eng.Load(ctx, id, s)
	// Purge even when Load fails midway; a fresh context lets it run after cancellation.
	if !opts.Keep {
		defer func() {
			pctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			perr := eng.Purge(pctx, id)
			switch {
			case perr == nil:
			case rep != nil && rep.Sample != nil:
				// The report can be a public API response: keep the engine
				// error (it names the cluster) out of it, in the log only.
				logf("warning: cleanup of run %s failed: %v", id, perr)
				rep.Sample.Notes = append(rep.Sample.Notes, CleanupFailedNote)
			case err != nil:
				err = errors.Join(err, fmt.Errorf("purge run %s: %w", id, perr))
			}
		}()
	} else {
		_, pats := eng.Session(id)
		logf("keeping run %s: %s, %s, %s", id, pats.Traces, pats.Logs, pats.Metrics)
	}
	if err != nil {
		return nil, fmt.Errorf("load sample: %w", err)
	}

	if stats == nil {
		stats = &engine.LoadStats{}
	}
	// A discovery failure only affects the sensitive-data rules; the rest of
	// the run proceeds. The error goes to the log, never into the report.
	fields, ferr := eng.Fields(ctx, id)
	if ferr != nil {
		logf("warning: field discovery for run %s failed: %v", id, ferr)
	}
	exec, patterns := eng.Session(id)
	c := *cfg
	c.Backend = "esql" // the engine speaks ES|QL whatever backend vern.yaml names
	c.ESQL.IndexPatterns = patterns
	c.ESQL.TimeWindow = WindowFor(sum.Span())

	var signals []string
	for k, v := range sum.Signals {
		if v {
			signals = append(signals, k)
		}
	}
	sort.Strings(signals)

	info := &evaluate.SampleInfo{
		Files:         opts.Files,
		Spans:         sum.Spans,
		SpanEvents:    sum.SpanEvents,
		LogRecords:    sum.LogRecords,
		DataPoints:    sum.DataPoints,
		Services:      sum.Services,
		Signals:       signals,
		OriginalStart: sum.Start,
		OriginalEnd:   sum.End,
		Dropped:       stats.Dropped(),
		Notes:         append([]string(nil), s.Notes...),
		RunID:         string(id),
	}
	rep, err = evaluate.Run(ctx, &c, exec, evaluate.RunOptions{
		Backend:          "esql",
		MappingsFile:     opts.MappingsFile,
		SpecRules:        rules,
		Signals:          sum.Signals,
		SampleMode:       true,
		ObservedSpan:     sum.Span(),
		Mappings:         mf,
		Sample:           info,
		Fields:           fields,
		FieldsFailed:     ferr != nil,
		FieldsDiscovered: ferr == nil,
		Logf:             logf,
		QueryErrorDetail: opts.QueryErrorDetail,
	})
	if err != nil {
		return nil, fmt.Errorf("evaluate: %w", err)
	}
	info.BlindSpots = blindSpots(rep, mf, fields, sum)
	// evaluate.Run records cancelled queries as per-rule errors and returns
	// nil; an interrupted or timed-out run must not yield a partial report.
	if ctx.Err() != nil {
		return nil, fmt.Errorf("evaluate: %w", context.Cause(ctx))
	}
	rep.Problems = evaluate.Problems(rep)
	return rep, nil
}

// WindowFor returns the ES|QL time window covering span: ceil(span hours)+1,
// minimum "1h".
func WindowFor(span time.Duration) string {
	h := int(math.Ceil(span.Hours())) + 1
	if h < 1 {
		h = 1
	}
	return fmt.Sprintf("%dh", h)
}
