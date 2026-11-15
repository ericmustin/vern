package otlpin

import (
	"math"
	"reflect"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

func ts(t time.Time) pcommon.Timestamp { return pcommon.NewTimestampFromTime(t) }

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newSample() *Sample {
	return &Sample{Traces: ptrace.NewTraces(), Logs: plog.NewLogs(), Metrics: pmetric.NewMetrics()}
}

func TestSummarize_CountsAndServices(t *testing.T) {
	s := newSample()
	rs := s.Traces.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	spans := rs.ScopeSpans().AppendEmpty().Spans()
	for i := 0; i < 2; i++ {
		sp := spans.AppendEmpty()
		sp.SetStartTimestamp(ts(t0))
		sp.SetEndTimestamp(ts(t0.Add(time.Second)))
	}
	spans.At(0).Events().AppendEmpty().SetTimestamp(ts(t0))
	rs2 := s.Traces.ResourceSpans().AppendEmpty() // no service.name
	rs2.ScopeSpans().AppendEmpty().Spans().AppendEmpty().SetStartTimestamp(ts(t0))

	rl := s.Logs.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "checkout")
	lrs := rl.ScopeLogs().AppendEmpty().LogRecords()
	lrs.AppendEmpty().SetTimestamp(ts(t0))
	lrs.AppendEmpty().SetTimestamp(ts(t0))

	rm := s.Metrics.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "")
	ms := rm.ScopeMetrics().AppendEmpty().Metrics()
	g := ms.AppendEmpty().SetEmptyGauge()
	g.DataPoints().AppendEmpty().SetTimestamp(ts(t0))
	g.DataPoints().AppendEmpty().SetTimestamp(ts(t0))
	sum := ms.AppendEmpty().SetEmptySum()
	sum.DataPoints().AppendEmpty().SetTimestamp(ts(t0))
	sum.DataPoints().AppendEmpty().SetTimestamp(ts(t0))

	got := Summarize(s)
	if got.Spans != 3 || got.SpanEvents != 1 || got.LogRecords != 2 || got.DataPoints != 4 {
		t.Fatalf("counts: %+v", got)
	}
	if got.Items() != 10 {
		t.Fatalf("items = %d", got.Items())
	}
	if want := []string{"(missing service.name)", "checkout"}; !reflect.DeepEqual(got.Services, want) {
		t.Fatalf("services = %v", got.Services)
	}
	if !got.Signals["traces"] || !got.Signals["logs"] || !got.Signals["metrics"] {
		t.Fatalf("signals = %v", got.Signals)
	}
}

func TestSummarize_RangeIgnoresZeroAndStartTime(t *testing.T) {
	s := newSample()
	m := s.Metrics.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	dp := m.SetEmptySum().DataPoints().AppendEmpty()
	dp.SetStartTimestamp(ts(t0.Add(-240 * time.Hour)))
	dp.SetTimestamp(ts(t0))
	lr := s.Logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetObservedTimestamp(ts(t0.Add(time.Second)))

	got := Summarize(s)
	if !got.Start.Equal(t0) || !got.End.Equal(t0.Add(time.Second)) {
		t.Fatalf("range = %v .. %v", got.Start, got.End)
	}
	if got.Span() != time.Second {
		t.Fatalf("span = %v", got.Span())
	}
	if e := Summarize(newSample()); !e.Start.IsZero() || !e.End.IsZero() || e.Span() != 0 || len(e.Signals) != 0 {
		t.Fatalf("empty: %+v", e)
	}
}

func TestOffsetToNow(t *testing.T) {
	sum := Summary{End: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	now := time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)
	if got, want := OffsetToNow(sum, now), 47*time.Hour+59*time.Minute; got != want {
		t.Fatalf("got %v want %v", got, want)
	}
	if OffsetToNow(Summary{}, now) != 0 {
		t.Fatal("zero End must give 0")
	}
}

func TestShift_PreservesDurationsAndZeros(t *testing.T) {
	d := 48 * time.Hour
	a, b := ts(t0), ts(t0.Add(time.Second))
	s := newSample()
	sp := s.Traces.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	sp.SetStartTimestamp(a)
	sp.SetEndTimestamp(b)
	sp.Events().AppendEmpty().SetTimestamp(a)
	sp.Events().AppendEmpty() // zero
	sp.Links().AppendEmpty()
	zs := s.Traces.ResourceSpans().At(0).ScopeSpans().At(0).Spans().AppendEmpty() // all zero

	lr := s.Logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetObservedTimestamp(b)

	ms := s.Metrics.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics()
	setEx := func(ex pmetric.ExemplarSlice) { ex.AppendEmpty().SetTimestamp(a); ex.AppendEmpty() }
	gd := ms.AppendEmpty().SetEmptyGauge().DataPoints().AppendEmpty()
	gd.SetTimestamp(a)
	setEx(gd.Exemplars())
	sd := ms.AppendEmpty().SetEmptySum().DataPoints().AppendEmpty()
	sd.SetStartTimestamp(a)
	sd.SetTimestamp(b)
	setEx(sd.Exemplars())
	hd := ms.AppendEmpty().SetEmptyHistogram().DataPoints().AppendEmpty()
	hd.SetStartTimestamp(a)
	hd.SetTimestamp(b)
	setEx(hd.Exemplars())
	ed := ms.AppendEmpty().SetEmptyExponentialHistogram().DataPoints().AppendEmpty()
	ed.SetStartTimestamp(a)
	ed.SetTimestamp(b)
	setEx(ed.Exemplars())
	qd := ms.AppendEmpty().SetEmptySummary().DataPoints().AppendEmpty()
	qd.SetStartTimestamp(a)
	qd.SetTimestamp(b)
	zd := ms.AppendEmpty().SetEmptyGauge().DataPoints().AppendEmpty() // zero

	Shift(s, d)

	sh := func(p pcommon.Timestamp) pcommon.Timestamp { return p + pcommon.Timestamp(d.Nanoseconds()) }
	eq := func(name string, got, want pcommon.Timestamp) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	eq("span start", sp.StartTimestamp(), sh(a))
	eq("span end", sp.EndTimestamp(), sh(b))
	eq("event", sp.Events().At(0).Timestamp(), sh(a))
	eq("zero event", sp.Events().At(1).Timestamp(), 0)
	eq("zero span start", zs.StartTimestamp(), 0)
	eq("zero span end", zs.EndTimestamp(), 0)
	eq("log time (zero)", lr.Timestamp(), 0)
	eq("log observed", lr.ObservedTimestamp(), sh(b))
	eq("gauge ts", gd.Timestamp(), sh(a))
	eq("gauge start (zero)", gd.StartTimestamp(), 0)
	eq("gauge exemplar", gd.Exemplars().At(0).Timestamp(), sh(a))
	eq("gauge zero exemplar", gd.Exemplars().At(1).Timestamp(), 0)
	eq("sum start", sd.StartTimestamp(), sh(a))
	eq("sum ts", sd.Timestamp(), sh(b))
	eq("sum exemplar", sd.Exemplars().At(0).Timestamp(), sh(a))
	eq("hist start", hd.StartTimestamp(), sh(a))
	eq("hist ts", hd.Timestamp(), sh(b))
	eq("hist exemplar", hd.Exemplars().At(0).Timestamp(), sh(a))
	eq("exp start", ed.StartTimestamp(), sh(a))
	eq("exp ts", ed.Timestamp(), sh(b))
	eq("exp exemplar", ed.Exemplars().At(0).Timestamp(), sh(a))
	eq("summary start", qd.StartTimestamp(), sh(a))
	eq("summary ts", qd.Timestamp(), sh(b))
	eq("zero dp", zd.Timestamp(), 0)

	// durations preserved
	if sp.EndTimestamp()-sp.StartTimestamp() != b-a {
		t.Error("span duration changed")
	}
}

func TestShift_NegativeOffset(t *testing.T) {
	d := -48 * time.Hour
	a, b := ts(t0), ts(t0.Add(time.Second))
	s := newSample()
	sp := s.Traces.ResourceSpans().AppendEmpty().ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	sp.SetStartTimestamp(a)
	sp.SetEndTimestamp(b)
	sp.Events().AppendEmpty().SetTimestamp(a)
	lr := s.Logs.ResourceLogs().AppendEmpty().ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetTimestamp(a)
	lr.SetObservedTimestamp(b)
	dp := s.Metrics.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetEmptySum().DataPoints().AppendEmpty()
	dp.SetStartTimestamp(a)
	dp.SetTimestamp(b)
	dp.Exemplars().AppendEmpty().SetTimestamp(a)

	Shift(s, d)

	back := func(p pcommon.Timestamp) pcommon.Timestamp {
		return p - pcommon.Timestamp((48 * time.Hour).Nanoseconds())
	}
	for name, p := range map[string][2]pcommon.Timestamp{
		"span start": {sp.StartTimestamp(), back(a)}, "span end": {sp.EndTimestamp(), back(b)},
		"event": {sp.Events().At(0).Timestamp(), back(a)}, "log": {lr.Timestamp(), back(a)},
		"observed": {lr.ObservedTimestamp(), back(b)}, "dp start": {dp.StartTimestamp(), back(a)},
		"dp": {dp.Timestamp(), back(b)}, "exemplar": {dp.Exemplars().At(0).Timestamp(), back(a)},
	} {
		if p[0] != p[1] {
			t.Errorf("%s = %d, want %d", name, p[0], p[1])
		}
	}
}

func TestShift_NegativeOffsetClampsInsteadOfWrapping(t *testing.T) {
	d := -time.Hour
	abs := pcommon.Timestamp(time.Hour.Nanoseconds())
	s := newSample()
	dp := s.Metrics.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetEmptySum().DataPoints().AppendEmpty()
	dp.SetStartTimestamp(5) // smaller than |d|
	dp.SetTimestamp(abs)    // equal to |d|
	dp.Exemplars().AppendEmpty().SetTimestamp(abs + 10)
	Shift(s, d)
	if dp.StartTimestamp() != 1 || dp.Timestamp() != 1 {
		t.Fatalf("start=%d ts=%d, want 1 and 1", dp.StartTimestamp(), dp.Timestamp())
	}
	if got := dp.Exemplars().At(0).Timestamp(); got != 10 {
		t.Fatalf("exemplar = %d, want 10", got)
	}
}

func TestShift_PositiveOverflowClamps(t *testing.T) {
	s := newSample()
	dp := s.Metrics.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty().SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetTimestamp(math.MaxUint64 - 5)
	Shift(s, time.Hour)
	if dp.Timestamp() != math.MaxUint64 {
		t.Fatalf("ts = %d, want MaxUint64", dp.Timestamp())
	}
}

func TestSummarize_NonStringServiceNameIsMissing(t *testing.T) {
	s := newSample()
	rl := s.Logs.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutInt("service.name", 7)
	rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().SetTimestamp(ts(t0))
	if got := Summarize(s).Services; !reflect.DeepEqual(got, []string{"(missing service.name)"}) {
		t.Fatalf("services = %v", got)
	}
}
