package otlpin

import (
	"math"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// dataPoint is what every metric data point type offers: its timestamps
// and attributes.
type dataPoint interface {
	Timestamp() pcommon.Timestamp
	SetTimestamp(pcommon.Timestamp)
	StartTimestamp() pcommon.Timestamp
	SetStartTimestamp(pcommon.Timestamp)
	Attributes() pcommon.Map
}

// visitDataPoints calls fn for every data point of m, whatever its type,
// with the point's exemplars (an empty slice for summary points, which have
// none). Shift and Summarize both walk data points through it, so a metric
// type cannot be handled by one and missed by the other.
func visitDataPoints(m pmetric.Metric, fn func(p dataPoint, ex pmetric.ExemplarSlice)) {
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		dps := m.Gauge().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i), dps.At(i).Exemplars())
		}
	case pmetric.MetricTypeSum:
		dps := m.Sum().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i), dps.At(i).Exemplars())
		}
	case pmetric.MetricTypeHistogram:
		dps := m.Histogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i), dps.At(i).Exemplars())
		}
	case pmetric.MetricTypeExponentialHistogram:
		dps := m.ExponentialHistogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i), dps.At(i).Exemplars())
		}
	case pmetric.MetricTypeSummary:
		dps := m.Summary().DataPoints()
		none := pmetric.NewExemplarSlice()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i), none)
		}
	}
}

// shiftTimestamp moves ts by d without wraparound. Zero ("unset") stays zero.
// A negative shift that would reach or pass zero clamps to 1, the smallest set
// value; a positive shift past MaxUint64 clamps to MaxUint64.
func shiftTimestamp(ts pcommon.Timestamp, d time.Duration) pcommon.Timestamp {
	if ts == 0 || d == 0 {
		return ts
	}
	if d < 0 {
		abs := uint64(-(d + 1)) + 1 // safe for math.MinInt64
		if uint64(ts) <= abs {
			return 1
		}
		return ts - pcommon.Timestamp(abs)
	}
	if uint64(ts) > math.MaxUint64-uint64(d) {
		return math.MaxUint64
	}
	return ts + pcommon.Timestamp(d)
}

// Shift moves every non-zero timestamp in s by d. Zero timestamps stay zero.
// Results are clamped rather than wrapped: a negative shift that would reach
// or pass zero yields 1, and a positive shift past the uint64 range yields
// MaxUint64.
func Shift(s *Sample, d time.Duration) {
	mv := func(ts pcommon.Timestamp) pcommon.Timestamp { return shiftTimestamp(ts, d) }
	shiftEx := func(ex pmetric.ExemplarSlice) {
		for i := 0; i < ex.Len(); i++ {
			ex.At(i).SetTimestamp(mv(ex.At(i).Timestamp()))
		}
	}

	rss := s.Traces.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		for j := 0; j < rss.At(i).ScopeSpans().Len(); j++ {
			spans := rss.At(i).ScopeSpans().At(j).Spans()
			for k := 0; k < spans.Len(); k++ {
				sp := spans.At(k)
				sp.SetStartTimestamp(mv(sp.StartTimestamp()))
				sp.SetEndTimestamp(mv(sp.EndTimestamp()))
				for e := 0; e < sp.Events().Len(); e++ {
					ev := sp.Events().At(e)
					ev.SetTimestamp(mv(ev.Timestamp()))
				}
			}
		}
	}

	rls := s.Logs.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		for j := 0; j < rls.At(i).ScopeLogs().Len(); j++ {
			recs := rls.At(i).ScopeLogs().At(j).LogRecords()
			for k := 0; k < recs.Len(); k++ {
				r := recs.At(k)
				r.SetTimestamp(mv(r.Timestamp()))
				r.SetObservedTimestamp(mv(r.ObservedTimestamp()))
			}
		}
	}

	rms := s.Metrics.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		for j := 0; j < rms.At(i).ScopeMetrics().Len(); j++ {
			ms := rms.At(i).ScopeMetrics().At(j).Metrics()
			for k := 0; k < ms.Len(); k++ {
				visitDataPoints(ms.At(k), func(p dataPoint, ex pmetric.ExemplarSlice) {
					p.SetTimestamp(mv(p.Timestamp()))
					p.SetStartTimestamp(mv(p.StartTimestamp()))
					shiftEx(ex)
				})
			}
		}
	}
}
