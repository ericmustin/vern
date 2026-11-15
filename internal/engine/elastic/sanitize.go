package elastic

import (
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"

	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/otlpin"
)

// namespaceKey is the resource attribute Elasticsearch's native OTLP endpoint
// uses to pick the target data stream namespace.
const namespaceKey = "data_stream.namespace"

// forbiddenKey reports whether an uploaded attribute key could reroute a
// record or trigger ingest behaviour. Matching is deliberately defensive: keys
// are compared case-insensitively (we have no evidence Elasticsearch does not
// fold case); every key under the "data_stream.", "elasticsearch." and
// "elastic." namespaces is removed (this covers named controls such as
// elasticsearch.index and exporter-era keys such as elasticsearch.index.prefix);
// and the bare keys "data_stream", "elasticsearch" and "elastic" are removed
// whatever their value type, since a map-valued attribute might be flattened to
// a dotted routing key. Keys like "elasticsearch_version" survive.
func forbiddenKey(k string) bool {
	k = strings.ToLower(k)
	for _, p := range []string{"data_stream.", "elasticsearch.", "elastic."} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	switch k {
	case "data_stream", "elasticsearch", "elastic":
		return true
	}
	return false
}

func strip(m pcommon.Map) {
	m.RemoveIf(func(k string, _ pcommon.Value) bool { return forbiddenKey(k) })
}

func stripExemplars(es pmetric.ExemplarSlice) {
	for i := 0; i < es.Len(); i++ {
		strip(es.At(i).FilteredAttributes())
	}
}

// Sanitize removes routing/ingest-control attributes from every level of an
// untrusted sample, then tags every resource with the run's namespace so the
// data lands only in that run's data streams.
func Sanitize(s *otlpin.Sample, id engine.RunID) {
	tag := func(r pcommon.Resource) {
		strip(r.Attributes())
		r.Attributes().PutStr(namespaceKey, string(id))
	}

	rss := s.Traces.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		rs := rss.At(i)
		tag(rs.Resource())
		for j := 0; j < rs.ScopeSpans().Len(); j++ {
			ss := rs.ScopeSpans().At(j)
			strip(ss.Scope().Attributes())
			for k := 0; k < ss.Spans().Len(); k++ {
				sp := ss.Spans().At(k)
				strip(sp.Attributes())
				for e := 0; e < sp.Events().Len(); e++ {
					strip(sp.Events().At(e).Attributes())
				}
				for l := 0; l < sp.Links().Len(); l++ {
					strip(sp.Links().At(l).Attributes())
				}
			}
		}
	}

	rls := s.Logs.ResourceLogs()
	for i := 0; i < rls.Len(); i++ {
		rl := rls.At(i)
		tag(rl.Resource())
		for j := 0; j < rl.ScopeLogs().Len(); j++ {
			sl := rl.ScopeLogs().At(j)
			strip(sl.Scope().Attributes())
			for k := 0; k < sl.LogRecords().Len(); k++ {
				strip(sl.LogRecords().At(k).Attributes())
			}
		}
	}

	rms := s.Metrics.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		tag(rm.Resource())
		for j := 0; j < rm.ScopeMetrics().Len(); j++ {
			sm := rm.ScopeMetrics().At(j)
			strip(sm.Scope().Attributes())
			for k := 0; k < sm.Metrics().Len(); k++ {
				m := sm.Metrics().At(k)
				strip(m.Metadata())
				switch m.Type() {
				case pmetric.MetricTypeGauge:
					dps := m.Gauge().DataPoints()
					for d := 0; d < dps.Len(); d++ {
						strip(dps.At(d).Attributes())
						stripExemplars(dps.At(d).Exemplars())
					}
				case pmetric.MetricTypeSum:
					dps := m.Sum().DataPoints()
					for d := 0; d < dps.Len(); d++ {
						strip(dps.At(d).Attributes())
						stripExemplars(dps.At(d).Exemplars())
					}
				case pmetric.MetricTypeHistogram:
					dps := m.Histogram().DataPoints()
					for d := 0; d < dps.Len(); d++ {
						strip(dps.At(d).Attributes())
						stripExemplars(dps.At(d).Exemplars())
					}
				case pmetric.MetricTypeExponentialHistogram:
					dps := m.ExponentialHistogram().DataPoints()
					for d := 0; d < dps.Len(); d++ {
						strip(dps.At(d).Attributes())
						stripExemplars(dps.At(d).Exemplars())
					}
				case pmetric.MetricTypeSummary:
					dps := m.Summary().DataPoints()
					for d := 0; d < dps.Len(); d++ {
						strip(dps.At(d).Attributes())
					}
				}
			}
		}
	}
}
