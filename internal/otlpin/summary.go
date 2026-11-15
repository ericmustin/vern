package otlpin

import (
	"sort"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/ericmustin/vern/internal/score"
)

// Summary describes the content of a Sample.
type Summary struct {
	Spans, SpanEvents, LogRecords, DataPoints int
	Services                                  []string        // sorted, unique
	Signals                                   map[string]bool // "traces", "logs", "metrics"
	Start, End                                time.Time       // range of counted timestamps; zero when none

	// Facts behind the sensitive-data blind spots (samplescore turns them
	// into sentences).

	// LongValues counts string values longer than LongValueChars characters
	// by where they land: attribute values (array elements at their key),
	// span, event and scope names, the status message and a string log
	// body. Whether Elasticsearch could still scan one (a match_only_text
	// field or ".text" subfield has no length limit) is for the caller to
	// decide. Map values are not counted: a map is one flattened field with
	// no length limit. Nil when there are none.
	LongValues map[ValueLocation]int
	// Longest is the longest value per field group, in characters, as the
	// scan reads it (multi-values joined, a map as JSON; approximate).
	Longest map[string]int
	// MetricDescriptions counts metrics with a non-empty description, and
	// ExemplarsWithAttributes exemplars with filtered attributes: neither is
	// stored, so neither can be scanned.
	MetricDescriptions, ExemplarsWithAttributes int
	// UntimedSpanEvents counts span events with no timestamp, and
	// UntimedLogRecords log records with neither a timestamp nor an observed
	// timestamp: they are indexed at 1970, outside every rule's time window.
	UntimedSpanEvents, UntimedLogRecords int
}

// Items is the total number of spans, span events, log records and data points.
func (s Summary) Items() int { return s.Spans + s.SpanEvents + s.LogRecords + s.DataPoints }

// Span is the observed duration, End minus Start.
func (s Summary) Span() time.Duration { return s.End.Sub(s.Start) }

// OffsetToNow is the shift that moves End to one minute before now.
func OffsetToNow(sum Summary, now time.Time) time.Duration {
	if sum.End.IsZero() {
		return 0
	}
	return now.Add(-time.Minute).Sub(sum.End)
}

type rangeTracker struct{ min, max pcommon.Timestamp }

func (r *rangeTracker) add(ts pcommon.Timestamp) {
	if ts == 0 {
		return
	}
	if r.min == 0 || ts < r.min {
		r.min = ts
	}
	if ts > r.max {
		r.max = ts
	}
}

func serviceName(res pcommon.Resource) string {
	if v, ok := res.Attributes().Get("service.name"); ok && v.Type() == pcommon.ValueTypeStr && v.Str() != "" {
		return v.Str()
	}
	return score.MissingServiceName
}

// Summarize counts the items in s and finds the range of their timestamps.
func Summarize(s *Sample) Summary {
	var sum Summary
	var rng rangeTracker
	services := map[string]struct{}{}

	var vals valueStats
	scope := func(sc pcommon.InstrumentationScope) {
		vals.str(GroupScope, "scope.name", sc.Name())
		vals.attrs(GroupScope, "scope.attributes.", sc.Attributes())
	}

	rss := s.Traces.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		rs := rss.At(i)
		services[serviceName(rs.Resource())] = struct{}{}
		vals.attrs(GroupResource, "resource.attributes.", rs.Resource().Attributes())
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			scope(rs.ScopeSpans().At(j).Scope())
			spans := rs.ScopeSpans().At(j).Spans()
			sum.Spans += spans.Len()
			for k := 0; k < spans.Len(); k++ {
				sp := spans.At(k)
				rng.add(sp.StartTimestamp())
				rng.add(sp.EndTimestamp())
				vals.str(GroupSpan, "name", sp.Name())
				vals.str(GroupSpan, "status.message", sp.Status().Message())
				vals.attrs(GroupSpan, "attributes.", sp.Attributes())
				for l := 0; l < sp.Links().Len(); l++ {
					vals.attrs(GroupSpan, "links.attributes.", sp.Links().At(l).Attributes())
				}
				sum.SpanEvents += sp.Events().Len()
				for e := 0; e < sp.Events().Len(); e++ {
					ev := sp.Events().At(e)
					rng.add(ev.Timestamp())
					if ev.Timestamp() == 0 {
						sum.UntimedSpanEvents++
					}
					vals.str(GroupSpanEvent, "event_name", ev.Name())
					vals.attrs(GroupSpanEvent, "attributes.", ev.Attributes())
				}
			}
		}
	}

	rls := s.Logs.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		rl := rls.At(i)
		services[serviceName(rl.Resource())] = struct{}{}
		vals.attrs(GroupResource, "resource.attributes.", rl.Resource().Attributes())
		for j := 0; j < rl.ScopeLogs().Len(); j++ {
			scope(rl.ScopeLogs().At(j).Scope())
			recs := rl.ScopeLogs().At(j).LogRecords()
			sum.LogRecords += recs.Len()
			for k := 0; k < recs.Len(); k++ {
				r := recs.At(k)
				ts := r.Timestamp()
				if ts == 0 {
					ts = r.ObservedTimestamp()
				}
				if ts == 0 {
					sum.UntimedLogRecords++
				}
				rng.add(ts)
				// A string body lands in body.text; a map or array body in
				// body.structured (one flattened field).
				if r.Body().Type() == pcommon.ValueTypeStr {
					vals.value(GroupLog, "body.text", r.Body())
				} else {
					vals.max(GroupLog, valueChars(r.Body()))
				}
				vals.str(GroupLog, "event_name", r.EventName())
				vals.attrs(GroupLog, "attributes.", r.Attributes())
			}
		}
	}

	rms := s.Metrics.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		services[serviceName(rm.Resource())] = struct{}{}
		vals.attrs(GroupResource, "resource.attributes.", rm.Resource().Attributes())
		for j := 0; j < rm.ScopeMetrics().Len(); j++ {
			scope(rm.ScopeMetrics().At(j).Scope())
			ms := rm.ScopeMetrics().At(j).Metrics()
			for k := 0; k < ms.Len(); k++ {
				m := ms.At(k)
				if m.Description() != "" {
					sum.MetricDescriptions++
				}
				visitDataPoints(m, func(p dataPoint, ex pmetric.ExemplarSlice) {
					sum.DataPoints++
					rng.add(p.Timestamp())
					vals.attrs(GroupMetric, "attributes.", p.Attributes())
					for e := 0; e < ex.Len(); e++ {
						if ex.At(e).FilteredAttributes().Len() > 0 {
							sum.ExemplarsWithAttributes++
						}
					}
				})
			}
		}
	}
	sum.LongValues, sum.Longest = vals.long, vals.longest

	for n := range services {
		sum.Services = append(sum.Services, n)
	}
	sort.Strings(sum.Services)
	sum.Signals = map[string]bool{}
	if sum.Spans > 0 {
		sum.Signals["traces"] = true
	}
	if sum.LogRecords > 0 {
		sum.Signals["logs"] = true
	}
	if sum.DataPoints > 0 {
		sum.Signals["metrics"] = true
	}
	if rng.max != 0 {
		sum.Start = rng.min.AsTime()
		sum.End = rng.max.AsTime()
	}
	return sum
}
