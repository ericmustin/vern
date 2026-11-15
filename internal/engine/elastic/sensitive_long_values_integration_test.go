//go:build integration

package elastic

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/ericmustin/vern/internal/otlpin"
	"github.com/ericmustin/vern/internal/sensitive"
)

// TestSensitiveLongValues pins, on a live node, why values are cut: ES|QL CONCAT fails
// the whole query beyond 1 MiB and REPLACE returns null when it rewrites a
// string over 1 MiB, so the generated query cuts every value to
// sensitive.ValueBudget characters before REPLACE and CONCAT. A value over
// 1 MiB, a multi-valued field whose join is over 1 MiB, and a span with 400
// fields of 3,000 bytes each all scan without error and still find a canary
// placed early in a value.
func TestSensitiveLongValues(t *testing.T) {
	now := pcommon.NewTimestampFromTime(time.Now().Add(-time.Minute))
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("service.name", "long-log")
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetTimestamp(now)
	lr.SetObservedTimestamp(now)
	lr.Body().SetStr("charged card 4111 1111 1111 1111 " + strings.Repeat("y", 1_150_000))
	arr := lr.Attributes().PutEmptySlice("bigarr")
	for i := 0; i < 1500; i++ {
		arr.AppendEmpty().SetStr(fmt.Sprintf("%04d", i) + strings.Repeat("z", 1000))
	}
	lr.Attributes().PutStr("exception.message", "ex "+strings.Repeat("m", 1_100_000))

	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "wide-span")
	sp := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	sp.SetName("wide")
	sp.SetTraceID(pcommon.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16})
	sp.SetSpanID(pcommon.SpanID{1, 2, 3, 4, 5, 6, 7, 8})
	sp.SetStartTimestamp(now)
	sp.SetEndTimestamp(now + 1000)
	euro := strings.Repeat("€", 1000) // 3,000 bytes, under ignore_above 1024 characters
	for i := 0; i < 400; i++ {
		sp.Attributes().PutStr(fmt.Sprintf("w.%03d", i), euro)
	}
	sp.Attributes().PutStr("w.399", "mail alice@example.com "+strings.Repeat("\u20ac", 950))

	pats := sensitiveLoadSample(t, &otlpin.Sample{Traces: td, Logs: ld, Metrics: pmetric.NewMetrics()})
	fieldsOf := func(pattern string) []string {
		var out []string
		for f := range sensitiveFieldCaps(t, pattern, false).Fields {
			if f == "body.text" || f == "name" || strings.HasPrefix(f, "attributes.") && !strings.HasSuffix(f, ".keyword") {
				out = append(out, f)
			}
		}
		return out
	}
	logFields, spanFields := fieldsOf(pats.Logs), fieldsOf(pats.Traces)
	// Without the bound the scan's CONCAT fails the whole query once its
	// result is used (a bare filter on it can be evaluated differently).
	unbounded := func(fields []string) string {
		var parts []string
		for _, f := range fields {
			parts = append(parts, `COALESCE(CONCAT("`+f+`", MV_CONCAT(TO_STRING(`+"`"+f+"`"+`), "; ")), "")`)
		}
		return " | EVAL seg = CONCAT(" + strings.Join(parts, ", ") + ") | EVAL n = LENGTH(seg) | KEEP n"
	}
	for _, c := range []struct {
		pattern string
		fields  []string
	}{{pats.Logs, logFields}, {pats.Traces, spanFields}} {
		if msg := sensitiveQueryError(t, "FROM "+c.pattern+unbounded(c.fields)); !strings.Contains(msg, "concatenating more than [1048576] bytes is not supported") {
			t.Errorf("unbounded CONCAT on %s: %s", c.pattern, msg)
		}
	}
	// REPLACE that rewrites a string over 1 MiB returns null (with a
	// warning): LEFT has to come first.
	if v := sensitiveValue(t, "FROM "+pats.Logs+` | EVAL x = REPLACE(body.text, "y", "z") IS NULL | KEEP x`); v != true {
		t.Errorf("REPLACE over 1 MiB = %v, want null", v)
	}
	if n := len(spanFields); n < 400 {
		t.Fatalf("span fields = %d", n)
	}

	for _, c := range []struct {
		pattern         string
		fields          []string
		field, detector string
	}{
		{pats.Logs, logFields, "body.text", "credit_card"},
		{pats.Traces, spanFields, "attributes.w.399", "email"},
	} {
		k := sensitive.ValueBudget(c.fields, nil)
		rows := sensitiveRows(t, "SET unmapped_fields=\"nullify\";\nFROM "+c.pattern+"\n"+sensitive.Query(c.fields))
		if len(rows) != 1 || rows[0][0] != false {
			t.Fatalf("%s: rows %.200v, want one failing row", c.pattern, rows)
		}
		ex, _ := rows[0][2].(string) // rule_passed, service.name, example, extent
		f, id, _, ok := sensitive.Locate(ex)
		t.Logf("%s: %d fields, budget %d chars, example %d bytes, located %s %s", c.pattern, len(c.fields), k, len(ex), f, id)
		if !ok || f != c.field || id != c.detector || len(ex) > 1<<20 {
			t.Errorf("%s: located %q %q %v, example %d bytes", c.pattern, f, id, ok, len(ex))
		}
	}
}
