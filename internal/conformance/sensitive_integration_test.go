//go:build integration

package conformance

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/ericmustin/vern/configs"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/otlpin"
	"github.com/ericmustin/vern/internal/report"
	"github.com/ericmustin/vern/internal/samplescore"
	"github.com/ericmustin/vern/internal/score"
	"github.com/ericmustin/vern/internal/sensitive"
)

var sensitiveRuleIDs = []string{"RES-010", "SPA-006", "LOG-003", "MET-008"}

// sensitiveMappings returns the embedded ES|QL mappings, checking that the
// four sensitive-data rules ship enabled.
func sensitiveMappings(t *testing.T) *mappings.MappingsFile {
	t.Helper()
	mf, err := mappings.LoadBytes(configs.ESQLMappings, "embedded esql-mappings.yaml")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range mf.Rules {
		for _, id := range sensitiveRuleIDs {
			if r.SpecRuleID == id {
				if !r.Enabled || !r.Sensitive {
					t.Errorf("%s: enabled=%v sensitive=%v, want both", id, r.Enabled, r.Sensitive)
				}
				n++
			}
		}
	}
	if n != len(sensitiveRuleIDs) {
		t.Fatalf("found %d of the sensitive rules", n)
	}
	return mf
}

// canaries are the fixtures' matched values: none may appear in any output.
var canaries = []string{
	"alice@example.com", "sk-proj-canaryCANARY1234567890", "123-45-6789", "4111 1111 1111 1111",
	"4111111111111111", "hunter2", "opaque-session-value-1", "correct-horse", "MyS3cret!", "bob@corp.test",
	serviceNameCanary,
}

// serviceNameCanary is a service.name holding an email: it may appear only
// as its pseudonym.
const serviceNameCanary = "carol@example.org"

// serviceNamePseudonym is serviceNameCanary's pseudonym in sensitiveSample,
// the only service name a detector matches there.
const serviceNamePseudonym = "service-1 (name withheld: matches email address)"

// checkNoCanary fails if a canary appears anywhere in the report (its full
// JSON and its Markdown, every rule's examples included: ordinary rules'
// examples that match a detector are withheld) or in the run's log.
func checkNoCanary(t *testing.T, rep *evaluate.Report, logs string) {
	t.Helper()
	js, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{"report JSON": string(js), "Markdown": report.RenderMarkdown(rep), "log": logs} {
		for _, c := range canaries {
			if strings.Contains(out, c) {
				t.Errorf("%s contains canary %q", name, c)
			}
		}
		for _, sep := range []string{sensitive.FieldSep, sensitive.SegmentSep, `\u001e`, `\u001f`} {
			if strings.Contains(out, sep) {
				t.Errorf("%s contains a raw segment separator %q", name, sep)
			}
		}
	}
	for _, rr := range rep.Rules {
		for _, o := range rr.Outcomes {
			for _, c := range canaries {
				if strings.Contains(o.Example, c) {
					t.Errorf("%s/%s example contains canary %q", rr.RuleID, o.ServiceName, c)
				}
			}
		}
	}
}

// checkExamplesSafe fails unless rule id has outcomes and each example is
// either withheld or free of every canary. The rules' examples SAMPLE
// one document, so which one shows varies from run to run.
func checkExamplesSafe(t *testing.T, rep *evaluate.Report, id string) {
	t.Helper()
	rr, ok := findRule(rep, id)
	if !ok || len(rr.Outcomes) == 0 {
		t.Fatalf("%s: no outcomes (status %q)", id, rr.Status)
	}
	for _, o := range rr.Outcomes {
		t.Logf("%s/%s: %s", id, o.ServiceName, o.Example)
		if strings.HasPrefix(o.Example, "example withheld: it matches ") {
			continue
		}
		for _, c := range canaries {
			if strings.Contains(o.Example, c) {
				t.Errorf("%s/%s: example neither withheld nor canary-free", id, o.ServiceName)
			}
		}
	}
}

// logBuffer collects a run's log lines (and echoes them to the test log).
type logBuffer struct {
	t  *testing.T
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) logf(format string, a ...any) {
	l.t.Helper()
	l.t.Logf(format, a...)
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format+"\n", a...)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// parseRedacted splits a redacted example, "<field>: <label> (<preview>)",
// into its field and detector ID.
func parseRedacted(example string) (field, detectorID string, ok bool) {
	field, rest, found := strings.Cut(example, ": ")
	if !found {
		return "", "", false
	}
	label, _, found := strings.Cut(rest, " (")
	if !found {
		return "", "", false
	}
	if label == "sensitive key name" {
		return field, sensitive.KeyDetectorID, true
	}
	for _, d := range sensitive.Detectors() {
		if d.Label == label {
			return field, d.ID, true
		}
	}
	return "", "", false
}

// outcomes maps service -> outcome for rule id; the rule must be evaluated.
func outcomes(t *testing.T, rep *evaluate.Report, id string) map[string]evaluate.RuleResult {
	t.Helper()
	rr, ok := findRule(rep, id)
	if !ok || rr.Status != evaluate.StatusEvaluated {
		t.Fatalf("%s: status %q (%s), want evaluated", id, rr.Status, rr.Reason)
	}
	out := map[string]evaluate.RuleResult{}
	for _, o := range rr.Outcomes {
		r := rr
		r.Outcomes = append(r.Outcomes[:0:0], o)
		out[o.ServiceName] = r
	}
	return out
}

type sensitiveWant struct {
	service, field, detector string // detector "" means the service passes
	extent                   float64
}

func checkSensitive(t *testing.T, rep *evaluate.Report, id string, wants []sensitiveWant) {
	t.Helper()
	got := outcomes(t, rep, id)
	for _, w := range wants {
		rr, ok := got[w.service]
		if !ok {
			t.Errorf("%s: no outcome for %s (have %v)", id, w.service, keys(got))
			continue
		}
		o := rr.Outcomes[0]
		t.Logf("%s/%s: passed=%v extent=%.2f %s", id, w.service, o.Passed, o.Extent, o.Example)
		if w.detector == "" {
			if !o.Passed || !strings.HasPrefix(o.Example, "No sensitive data matched ") || !strings.HasSuffix(o.Example, " checks") {
				t.Errorf("%s/%s: passed=%v example=%q, want a pass", id, w.service, o.Passed, o.Example)
			}
			continue
		}
		// The runner already redacted a failing example.
		field, det, parsed := parseRedacted(o.Example)
		if o.Passed || !parsed || field != w.field || det != w.detector {
			t.Errorf("%s/%s: passed=%v parsed=%v field=%q detector=%q, want fail on %s by %s (example %s)",
				id, w.service, o.Passed, parsed, field, det, w.field, w.detector, o.Example)
		}
		if w.extent != 0 && o.Extent != w.extent {
			t.Errorf("%s/%s: extent %v, want %v", id, w.service, o.Extent, w.extent)
		}
	}
}

func keys(m map[string]evaluate.RuleResult) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sensitiveSample has one leak location per service, a clean service, and
// placeholder values that must not count.
func sensitiveSample() *otlpin.Sample {
	now := pcommon.NewTimestampFromTime(time.Now().Add(-time.Minute))
	later := pcommon.NewTimestampFromTime(time.Now().Add(-time.Minute + time.Millisecond))
	td := ptrace.NewTraces()
	var n byte
	span := func(service string, res map[string]string, attrs map[string]string) ptrace.Span {
		rs := td.ResourceSpans().AppendEmpty()
		rs.Resource().Attributes().PutStr("service.name", service)
		for k, v := range res {
			rs.Resource().Attributes().PutStr(k, v)
		}
		ss := rs.ScopeSpans().AppendEmpty()
		ss.Scope().SetName("conformance")
		sp := ss.Spans().AppendEmpty()
		n++
		sp.SetName("GET /api")
		sp.SetTraceID(pcommon.TraceID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, n})
		sp.SetSpanID(pcommon.SpanID{1, 2, 3, 4, 5, 6, 7, n})
		sp.SetStartTimestamp(now)
		sp.SetEndTimestamp(later)
		for k, v := range attrs {
			sp.Attributes().PutStr(k, v)
		}
		return sp
	}
	span("res-leak", map[string]string{"custom.config": "api_key=sk-proj-canaryCANARY1234567890"}, nil)
	span("span-leak", nil, map[string]string{"db.query.text": "SELECT * FROM users WHERE email='alice@example.com'"})
	ev := span("event-leak", nil, nil).Events().AppendEmpty()
	ev.SetName("exception")
	ev.SetTimestamp(later)
	ev.Attributes().PutStr("exception.message", "Login failed: password=hunter2")
	span("key-leak", nil, map[string]string{"http.request.header.authorization": "opaque-session-value-1"})
	span("key-placeholder", nil, map[string]string{
		"http.request.header.authorization": "[REDACTED]", "password": "********", "note": "password=null token=None",
	})
	// Raw separators inside a value must not forge a segment.
	span("forge", nil, map[string]string{"note": "x\x1eattributes.fake\x1fsee alice@example.com"})
	clean := span("clean", nil, map[string]string{
		"request.id": "3f2b8c1e-9a4d-4e6f-8b2a-1c3d5e7f9a0b", "sha": "da39a3ee5e6b4b0d3255bfef95601890afd80709",
		"started": "2026-11-14T16:13:29.123Z", "version": "v1.71.0", "peer": "10.0.0.1", "ratio": "0,4182736451827364",
	})
	clean.Attributes().PutInt("epoch", 1731600000)
	// Numeric arrays joined into one value must not read as a card.
	ports := clean.Attributes().PutEmptySlice("net.ports")
	for _, p := range []int64{4317, 4318, 8080, 9090} {
		ports.AppendEmpty().SetInt(p)
	}
	codes := clean.Attributes().PutEmptySlice("bin.codes")
	for _, c := range []string{"5100", "5101", "5102", "5103"} {
		codes.AppendEmpty().SetStr(c)
	}
	cev := clean.Events().AppendEmpty()
	cev.SetName("retry")
	cev.SetTimestamp(later)
	cev.Attributes().PutInt("attempt", 2)
	span("log-leak", nil, nil)
	// A service.name that is itself sensitive data.
	span(serviceNameCanary, nil, nil)

	ld := plog.NewLogs()
	logRec := func(service, body string) {
		rl := ld.ResourceLogs().AppendEmpty()
		rl.Resource().Attributes().PutStr("service.name", service)
		lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
		lr.SetTimestamp(now)
		lr.SetObservedTimestamp(now)
		lr.SetSeverityNumber(plog.SeverityNumberInfo)
		lr.Body().SetStr(body)
	}
	logRec("log-leak", "Payment processed for card 4111 1111 1111 1111")
	logRec("event-leak", "user logged in")
	logRec("clean", "GET /api/v1/orders/12345 200")

	md := pmetric.NewMetrics()
	gauge := func(service, name string) {
		rm := md.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutStr("service.name", service)
		m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName(name)
		dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
		dp.SetTimestamp(now)
		dp.SetIntValue(1)
		dp.Attributes().PutStr("http.route", "/api")
	}
	gauge("metric-leak", "orders.alice@example.com.count")
	gauge("clean", "http.server.active_requests")
	return &otlpin.Sample{Traces: td, Logs: ld, Metrics: md}
}

// TestSensitiveRules_Live scores a crafted sample with the four rules
// enabled through samplescore.Score (the `vern score` path).
func TestSensitiveRules_Live(t *testing.T) {
	e := newEngine(t)
	scoreSlots <- struct{}{}
	defer func() { <-scoreSlots }()
	logs := &logBuffer{t: t}
	rep, err := samplescore.Score(t.Context(), e, sensitiveSample(), samplescore.Options{Mappings: sensitiveMappings(t), Logf: logs.logf})
	if err != nil {
		t.Fatal(err)
	}
	if d := rep.Sample.Dropped; len(d) != 0 {
		t.Fatalf("items not indexed: %v", d)
	}
	checkSensitive(t, rep, "RES-010", []sensitiveWant{
		{"res-leak", "resource.attributes.custom.config", "api_key", 1},
		{"span-leak", "", "", 0}, {"clean", "", "", 0}, {"metric-leak", "", "", 0},
		// The service.name canary fails RES-010 and shows only as its pseudonym.
		{serviceNamePseudonym, "resource.attributes.service.name", "email", 1},
	})
	if !slices.Contains(rep.Sample.Services, serviceNamePseudonym) {
		t.Errorf("Sample.Services lacks the pseudonym: %q", rep.Sample.Services)
	}
	if !slices.ContainsFunc(rep.Score.Services, func(s score.ServiceScore) bool { return s.ServiceName == serviceNamePseudonym }) {
		t.Errorf("score services lack the pseudonym")
	}
	checkSensitive(t, rep, "SPA-006", []sensitiveWant{
		// A doc where every other field is null still yields its segment:
		// the segments are null-safe.
		{"span-leak", "attributes.db.query.text", "email", 1},
		// Span events are scanned with spans: 1 hit of 2 docs.
		{"event-leak", "attributes.exception.message", "secret_kv", 0.5},
		{"key-leak", "attributes.http.request.header.authorization", sensitive.KeyDetectorID, 1},
		{"forge", "attributes.note", "email", 1},
		{"key-placeholder", "", "", 0}, {"clean", "", "", 0}, {"res-leak", "", "", 0}, {"log-leak", "", "", 0},
	})
	checkSensitive(t, rep, "LOG-003", []sensitiveWant{
		{"log-leak", "body.text", "credit_card", 1},
		// The span event's password stays with SPA-006.
		{"event-leak", "", "", 0}, {"clean", "", "", 0},
	})
	checkSensitive(t, rep, "MET-008", []sensitiveWant{
		{"metric-leak", sensitive.MetricNameField, "email", 1},
		{"clean", "", "", 0},
	})
	checkNoCanary(t, rep, logs.String())
	t.Logf("blind spots: %q", rep.Sample.BlindSpots)
}

// TestSensitiveRules_CanaryFixture scores testdata/sensitive/canaries.jsonl,
// which has canaries in every location the four rules list: every rule fails
// for the fixture's service with an example Go can locate.
func TestSensitiveRules_CanaryFixture(t *testing.T) {
	e := newEngine(t)
	s, err := decodeFile("../../testdata/sensitive/canaries.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	scoreSlots <- struct{}{}
	defer func() { <-scoreSlots }()
	logs := &logBuffer{t: t}
	rep, err := samplescore.Score(t.Context(), e, s, samplescore.Options{Mappings: sensitiveMappings(t), Logf: logs.logf})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range sensitiveRuleIDs {
		for svc, rr := range outcomes(t, rep, id) {
			o := rr.Outcomes[0]
			field, det, ok := parseRedacted(o.Example)
			t.Logf("%s/%s: passed=%v extent=%.2f %s", id, svc, o.Passed, o.Extent, o.Example)
			if o.Passed || !ok {
				t.Errorf("%s/%s: passed=%v parsed=%v (%s %s)", id, svc, o.Passed, ok, field, det)
			}
		}
	}
	checkNoCanary(t, rep, logs.String())
	// Ordinary rules quote scope names (RES-001, RES-007) and metric names
	// (MET-002), most of which hold canaries in this fixture.
	for _, id := range []string{"RES-001", "RES-007", "MET-002"} {
		checkExamplesSafe(t, rep, id)
	}
	// The fixture has 1,500-character values in keyword fields, metric
	// descriptions and exemplar attributes: all disclosed.
	t.Logf("blind spots: %q", rep.Sample.BlindSpots)
	if !slices.ContainsFunc(rep.Sample.BlindSpots, func(s string) bool { return strings.Contains(s, "longer than 1024 characters couldn't be scanned") }) {
		t.Errorf("blind spots lack the long keyword values: %q", rep.Sample.BlindSpots)
	}
}
