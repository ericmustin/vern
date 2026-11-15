//go:build integration

package elastic

import (
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/ericmustin/vern/internal/otlpin"
)

// TestSpanEventDiscriminator pins how span events are told apart: in the logs
// index, every log record carries observed_timestamp (0 when the record had
// none) and no span event does, even a log record shaped like an event
// (event_name, trace and span IDs, no body) or a span event with no name or
// timestamp. SPA-006 keeps logs-index docs WHERE observed_timestamp IS NULL;
// LOG-003 (like LOG-001/002) keeps the others.
func TestSpanEventDiscriminator(t *testing.T) {
	now := time.Now().Add(-time.Minute)
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "svc")
	sp := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	sp.SetName("span")
	sp.SetTraceID(pcommon.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	sp.SetSpanID(pcommon.SpanID{1, 2, 3, 4, 5, 6, 7, 8})
	sp.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	sp.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(time.Millisecond)))
	ev := sp.Events().AppendEmpty()
	ev.SetName("ev")
	ev.SetTimestamp(pcommon.NewTimestampFromTime(now))
	ev.Attributes().PutStr("which", "span-event")
	bare := sp.Events().AppendEmpty() // no name, no timestamp
	bare.Attributes().PutStr("which", "bare-span-event")

	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "svc")
	recs := rl.ScopeLogs().AppendEmpty().LogRecords()
	a := recs.AppendEmpty() // timestamp only: no observed timestamp, no body
	a.SetTimestamp(pcommon.NewTimestampFromTime(now))
	a.Attributes().PutStr("which", "log-ts-only")
	b := recs.AppendEmpty() // shaped like an event
	b.SetTimestamp(pcommon.NewTimestampFromTime(now))
	b.SetEventName("my.event")
	b.SetTraceID(sp.TraceID())
	b.SetSpanID(sp.SpanID())
	b.Attributes().PutStr("which", "log-event-like")
	c := recs.AppendEmpty() // nothing set
	c.Attributes().PutStr("which", "log-empty")

	pats := sensitiveLoadSample(t, &otlpin.Sample{Traces: td, Logs: ld, Metrics: pmetric.NewMetrics()})
	got := map[string]bool{}
	for _, row := range sensitiveRows(t, "FROM "+pats.Logs+" | EVAL span_event = observed_timestamp IS NULL | KEEP attributes.which, span_event") {
		got[row[0].(string)] = row[1].(bool)
	}
	want := map[string]bool{"span-event": true, "bare-span-event": true, "log-ts-only": false, "log-event-like": false, "log-empty": false}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s: observed_timestamp IS NULL = %v (present %v), want %v", k, g, ok, v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("logs docs = %v", got)
	}
}
