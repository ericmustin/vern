package elastic

import (
	"testing"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/otlpin"
)

var forbiddenKeys = []string{
	"data_stream.dataset", "data_stream.namespace", "data_stream.type", "data_stream.custom",
	"elasticsearch.index", "elasticsearch.document_id", "elasticsearch.ingest_pipeline",
	"elasticsearch.mapping.hints", "elastic.mapping.mode",
}

func seed(m pcommon.Map) {
	for _, k := range forbiddenKeys {
		m.PutStr(k, "evil")
	}
	m.PutStr("Data_Stream.Namespace", "evil")
	m.PutStr("ELASTICSEARCH.INDEX", "evil")
	m.PutEmptyMap("data_stream").PutStr("namespace", "evil")
	m.PutEmptyMap("elasticsearch").PutStr("index", "evil")
	m.PutStr("elasticsearch.index.prefix", "evil")
	m.PutStr("Elastic.Anything", "evil")
	m.PutStr("http.route", "/keep")
	m.PutStr("elasticsearch_version", "keep")
	m.PutStr("elastic_apm.foo", "keep")
}

func checkMap(t *testing.T, where string, m pcommon.Map) {
	t.Helper()
	for _, k := range append([]string{"Data_Stream.Namespace", "ELASTICSEARCH.INDEX", "data_stream", "elasticsearch", "elasticsearch.index.prefix", "Elastic.Anything"}, forbiddenKeys...) {
		if _, ok := m.Get(k); ok {
			t.Errorf("%s: forbidden key %q survived", where, k)
		}
	}
	if v, ok := m.Get("http.route"); !ok || v.Str() != "/keep" {
		t.Errorf("%s: http.route lost", where)
	}
	for _, k := range []string{"elasticsearch_version", "elastic_apm.foo"} {
		if _, ok := m.Get(k); !ok {
			t.Errorf("%s: %q should survive", where, k)
		}
	}
}

func TestSanitize_StripsEveryLevelAndTagsNamespace(t *testing.T) {
	s := &otlpin.Sample{Traces: ptrace.NewTraces(), Logs: plog.NewLogs(), Metrics: pmetric.NewMetrics()}

	rs := s.Traces.ResourceSpans().AppendEmpty()
	seed(rs.Resource().Attributes())
	ss := rs.ScopeSpans().AppendEmpty()
	seed(ss.Scope().Attributes())
	sp := ss.Spans().AppendEmpty()
	seed(sp.Attributes())
	seed(sp.Events().AppendEmpty().Attributes())
	seed(sp.Links().AppendEmpty().Attributes())

	rl := s.Logs.ResourceLogs().AppendEmpty()
	seed(rl.Resource().Attributes())
	sl := rl.ScopeLogs().AppendEmpty()
	seed(sl.Scope().Attributes())
	seed(sl.LogRecords().AppendEmpty().Attributes())

	rm := s.Metrics.ResourceMetrics().AppendEmpty()
	seed(rm.Resource().Attributes())
	sm := rm.ScopeMetrics().AppendEmpty()
	seed(sm.Scope().Attributes())
	// second resource/scope/span to catch index-reuse bugs
	rs2 := s.Traces.ResourceSpans().AppendEmpty()
	seed(rs2.Resource().Attributes())
	ss2 := rs2.ScopeSpans().AppendEmpty()
	seed(ss2.Scope().Attributes())
	seed(ss2.Spans().AppendEmpty().Attributes())
	ss2b := rs2.ScopeSpans().AppendEmpty()
	seed(ss2b.Scope().Attributes())
	seed(ss2b.Spans().AppendEmpty().Attributes())
	g := sm.Metrics().AppendEmpty().SetEmptyGauge().DataPoints().AppendEmpty()
	seed(g.Attributes())
	seed(g.Exemplars().AppendEmpty().FilteredAttributes())
	su := sm.Metrics().AppendEmpty().SetEmptySum().DataPoints().AppendEmpty()
	seed(su.Attributes())
	seed(su.Exemplars().AppendEmpty().FilteredAttributes())
	h := sm.Metrics().AppendEmpty().SetEmptyHistogram().DataPoints().AppendEmpty()
	seed(h.Attributes())
	seed(h.Exemplars().AppendEmpty().FilteredAttributes())
	eh := sm.Metrics().AppendEmpty().SetEmptyExponentialHistogram().DataPoints().AppendEmpty()
	seed(eh.Attributes())
	seed(eh.Exemplars().AppendEmpty().FilteredAttributes())
	seed(sm.Metrics().AppendEmpty().SetEmptySummary().DataPoints().AppendEmpty().Attributes())

	for i := 0; i < sm.Metrics().Len(); i++ {
		seed(sm.Metrics().At(i).Metadata())
	}

	id := engine.RunID("run-abc")
	Sanitize(s, id)

	for _, r := range []pcommon.Resource{rs.Resource(), rs2.Resource(), rl.Resource(), rm.Resource()} {
		v, ok := r.Attributes().Get("data_stream.namespace")
		if !ok || v.Str() != string(id) {
			t.Errorf("resource namespace = %v, want %s", v.AsString(), id)
		}
		if r.Attributes().Len() != 4 { // namespace + 3 kept keys
			t.Errorf("resource has %d attrs, want 4", r.Attributes().Len())
		}
		if v, ok := r.Attributes().Get("http.route"); !ok || v.Str() != "/keep" {
			t.Error("resource http.route lost")
		}
	}
	checkMap(t, "trace scope", ss.Scope().Attributes())
	checkMap(t, "span", sp.Attributes())
	checkMap(t, "span event", sp.Events().At(0).Attributes())
	checkMap(t, "span link", sp.Links().At(0).Attributes())
	checkMap(t, "span2", ss2.Spans().At(0).Attributes())
	checkMap(t, "scope2", ss2.Scope().Attributes())
	checkMap(t, "scope2b", ss2b.Scope().Attributes())
	checkMap(t, "span2b", ss2b.Spans().At(0).Attributes())
	for i := 0; i < sm.Metrics().Len(); i++ {
		checkMap(t, "metric metadata", sm.Metrics().At(i).Metadata())
	}
	checkMap(t, "log scope", sl.Scope().Attributes())
	checkMap(t, "log record", sl.LogRecords().At(0).Attributes())
	checkMap(t, "metric scope", sm.Scope().Attributes())
	checkMap(t, "gauge dp", g.Attributes())
	checkMap(t, "gauge exemplar", g.Exemplars().At(0).FilteredAttributes())
	checkMap(t, "sum dp", su.Attributes())
	checkMap(t, "sum exemplar", su.Exemplars().At(0).FilteredAttributes())
	checkMap(t, "hist dp", h.Attributes())
	checkMap(t, "hist exemplar", h.Exemplars().At(0).FilteredAttributes())
	checkMap(t, "exphist dp", eh.Attributes())
	checkMap(t, "exphist exemplar", eh.Exemplars().At(0).FilteredAttributes())
	checkMap(t, "summary dp", sm.Metrics().At(4).Summary().DataPoints().At(0).Attributes())
}
