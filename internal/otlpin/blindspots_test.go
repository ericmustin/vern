package otlpin

import (
	"reflect"
	"strings"
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"
)

// allTypesMetrics has one metric of each type, two data points each, every
// point with a "k" attribute, and one exemplar (with an attribute) on each
// point type that has exemplars.
func allTypesMetrics(ms pmetric.MetricSlice) {
	attr := func(p dataPoint) { p.Attributes().PutStr("k", "v"); p.SetTimestamp(ts(t0)) }
	ex := func(e pmetric.ExemplarSlice) { e.AppendEmpty().FilteredAttributes().PutStr("user.id", "u-1") }
	g := ms.AppendEmpty().SetEmptyGauge().DataPoints()
	s := ms.AppendEmpty().SetEmptySum().DataPoints()
	h := ms.AppendEmpty().SetEmptyHistogram().DataPoints()
	e := ms.AppendEmpty().SetEmptyExponentialHistogram().DataPoints()
	q := ms.AppendEmpty().SetEmptySummary().DataPoints()
	for i := 0; i < 2; i++ {
		gp, sp, hp, ep, qp := g.AppendEmpty(), s.AppendEmpty(), h.AppendEmpty(), e.AppendEmpty(), q.AppendEmpty()
		for _, p := range []dataPoint{gp, sp, hp, ep, qp} {
			attr(p)
		}
		ex(gp.Exemplars())
		ex(sp.Exemplars())
		ex(hp.Exemplars())
		ex(ep.Exemplars())
	}
}

func TestVisitDataPoints_AllMetricTypes(t *testing.T) {
	ms := pmetric.NewMetricSlice()
	allTypesMetrics(ms)
	types := map[pmetric.MetricType]int{}
	exemplars := 0
	for i := 0; i < ms.Len(); i++ {
		m := ms.At(i)
		visitDataPoints(m, func(p dataPoint, ex pmetric.ExemplarSlice) {
			if v, ok := p.Attributes().Get("k"); !ok || v.Str() != "v" {
				t.Errorf("%s: point without its attribute", m.Type())
			}
			types[m.Type()]++
			exemplars += ex.Len()
		})
	}
	want := map[pmetric.MetricType]int{
		pmetric.MetricTypeGauge: 2, pmetric.MetricTypeSum: 2, pmetric.MetricTypeHistogram: 2,
		pmetric.MetricTypeExponentialHistogram: 2, pmetric.MetricTypeSummary: 2,
	}
	if !reflect.DeepEqual(types, want) {
		t.Errorf("points by type = %v, want %v", types, want)
	}
	if exemplars != 8 {
		t.Errorf("exemplars = %d, want 8 (summary points have none)", exemplars)
	}

	// Both users of the visitor see every type: Summarize counts all ten
	// points (and the exemplars' attributes), Shift moves all ten.
	s := newSample()
	allTypesMetrics(s.Metrics.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics())
	sum := Summarize(s)
	if sum.DataPoints != 10 || sum.ExemplarsWithAttributes != 8 {
		t.Errorf("Summarize: data points %d, exemplars with attributes %d; want 10, 8", sum.DataPoints, sum.ExemplarsWithAttributes)
	}
	Shift(s, 1)
	moved := 0
	sms := s.Metrics.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	for i := 0; i < sms.Len(); i++ {
		visitDataPoints(sms.At(i), func(p dataPoint, _ pmetric.ExemplarSlice) {
			if p.Timestamp() == ts(t0)+1 {
				moved++
			}
		})
	}
	if moved != 10 {
		t.Errorf("Shift moved %d points, want 10", moved)
	}
}

// TestSummarize_CountsLongValues: string values over 1024 characters are
// counted where they land (field group and Elasticsearch field), array
// elements at their key; map leaves are not (a map is one flattened field,
// scanned at any length).
func TestSummarize_CountsLongValues(t *testing.T) {
	long := strings.Repeat("x", LongValueChars+1)
	exact := strings.Repeat("x", LongValueChars)
	wide := strings.Repeat("é", LongValueChars) // 1024 characters, 2048 bytes
	s := newSample()
	rs := s.Traces.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "svc")
	rs.Resource().Attributes().PutStr("cfg", long)
	ss := rs.ScopeSpans().AppendEmpty()
	ss.Scope().SetName(long)
	ss.Scope().Attributes().PutStr("lib", long)
	sp := ss.Spans().AppendEmpty()
	sp.SetName(long)
	sp.Status().SetMessage(long)
	sp.Attributes().PutStr("q", long)
	sp.Attributes().PutStr("ok", exact)
	sp.Attributes().PutStr("wide", wide)
	arr := sp.Attributes().PutEmptySlice("args")
	arr.AppendEmpty().SetStr(long)
	arr.AppendEmpty().SetStr("short")
	arr.AppendEmpty().SetStr(long)
	sp.Attributes().PutEmptyMap("m").PutStr("inner", long)
	sp.Links().AppendEmpty().Attributes().PutStr("l", long)
	ev := sp.Events().AppendEmpty()
	ev.SetTimestamp(ts(t0))
	ev.SetName(long)
	ev.Attributes().PutStr("exception.message", long)

	lr := s.Logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetTimestamp(ts(t0))
	lr.Body().SetStr(long)
	lr.Attributes().PutStr("q", long)
	lr.SetEventName(long)

	dp := s.Metrics.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetTimestamp(ts(t0))
	dp.Attributes().PutStr("route", long)

	got := Summarize(s).LongValues
	want := map[ValueLocation]int{
		{GroupResource, "resource.attributes.cfg"}:       1,
		{GroupScope, "scope.name"}:                       1,
		{GroupScope, "scope.attributes.lib"}:             1,
		{GroupSpan, "name"}:                              1,
		{GroupSpan, "status.message"}:                    1,
		{GroupSpan, "attributes.q"}:                      1,
		{GroupSpan, "attributes.args"}:                   2,
		{GroupSpan, "links.attributes.l"}:                1,
		{GroupSpanEvent, "event_name"}:                   1,
		{GroupSpanEvent, "attributes.exception.message"}: 1,
		{GroupLog, "body.text"}:                          1,
		{GroupLog, "attributes.q"}:                       1,
		{GroupLog, "event_name"}:                         1,
		{GroupMetric, "attributes.route"}:                1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LongValues =\n%v\nwant\n%v", got, want)
	}
}

// TestSummarize_LongestValuePerGroup: the longest value per field group, in
// characters, as the sensitive-data scan sees it (multi-values joined with
// "; ", a map as one value).
func TestSummarize_LongestValuePerGroup(t *testing.T) {
	s := newSample()
	rs := s.Traces.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "svc")
	sp := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	sp.SetName("op")
	arr := sp.Attributes().PutEmptySlice("args")
	arr.AppendEmpty().SetStr(strings.Repeat("a", 300))
	arr.AppendEmpty().SetStr(strings.Repeat("b", 300))
	lr := s.Logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.Body().SetEmptyMap().PutStr("msg", strings.Repeat("é", 2000))

	got := Summarize(s).Longest
	if got[GroupSpan] != 602 {
		t.Errorf("Span longest = %d, want 602 (300 + \"; \" + 300)", got[GroupSpan])
	}
	if got[GroupLog] < 2000 || got[GroupLog] > 2020 {
		t.Errorf("Log longest = %d, want about 2000 (the map body)", got[GroupLog])
	}
	if got[GroupResource] != 3 {
		t.Errorf("Resource longest = %d, want 3 (svc)", got[GroupResource])
	}
}

// TestSummarize_CountsUnstoredAndUntimed: metric descriptions and exemplar
// attributes are not stored; span events with no timestamp and log records
// with neither timestamp land at 1970, outside every rule's time window.
func TestSummarize_CountsUnstoredAndUntimed(t *testing.T) {
	s := newSample()
	sp := s.Traces.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	sp.SetStartTimestamp(ts(t0))
	sp.Events().AppendEmpty().SetTimestamp(ts(t0))
	sp.Events().AppendEmpty() // no timestamp
	sp.Events().AppendEmpty() // no timestamp

	recs := s.Logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords()
	recs.AppendEmpty().SetTimestamp(ts(t0))
	recs.AppendEmpty().SetObservedTimestamp(ts(t0)) // observed only: indexed at the observed time
	recs.AppendEmpty()                              // neither

	ms := s.Metrics.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics()
	m := ms.AppendEmpty()
	m.SetDescription("orders by alice@example.com")
	p := m.SetEmptySum().DataPoints().AppendEmpty()
	p.SetTimestamp(ts(t0))
	p.Exemplars().AppendEmpty() // no attributes
	p.Exemplars().AppendEmpty().FilteredAttributes().PutStr("user.id", "u-1")
	ms.AppendEmpty().SetEmptyGauge().DataPoints().AppendEmpty().SetTimestamp(ts(t0)) // no description

	sum := Summarize(s)
	if sum.MetricDescriptions != 1 || sum.ExemplarsWithAttributes != 1 || sum.UntimedSpanEvents != 2 || sum.UntimedLogRecords != 1 {
		t.Errorf("descriptions %d, exemplars %d, untimed span events %d, untimed logs %d; want 1, 1, 2, 1",
			sum.MetricDescriptions, sum.ExemplarsWithAttributes, sum.UntimedSpanEvents, sum.UntimedLogRecords)
	}
}
