package samplescore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/otlpin"
	"github.com/ericmustin/vern/internal/score"
)

type fakeEngine struct {
	calls       []string
	loadedStart time.Time
	loadedEnd   time.Time
	stats       *engine.LoadStats
	exec        evaluate.Executor
	cancel      context.CancelFunc
	purgeCtxErr error
	purgeErr    error
	loadErr     error
	sweepArg    time.Duration
	sweepN      int
	sweepErr    error
	fields      mappings.Fields
	fieldsErr   error
	fieldsCalls int
}

func (f *fakeEngine) Ready(ctx context.Context) error { f.calls = append(f.calls, "Ready"); return nil }
func (f *fakeEngine) Load(ctx context.Context, id engine.RunID, s *otlpin.Sample) (*engine.LoadStats, error) {
	f.calls = append(f.calls, "Load")
	sum := otlpin.Summarize(s)
	f.loadedStart, f.loadedEnd = sum.Start, sum.End
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	if f.stats == nil {
		return &engine.LoadStats{}, nil
	}
	return f.stats, nil
}
func (f *fakeEngine) Session(id engine.RunID) (evaluate.Executor, config.IndexPatterns) {
	f.calls = append(f.calls, "Session")
	return f.exec, config.IndexPatterns{Traces: "t-*", Metrics: "m-*", Logs: "l-*"}
}
func (f *fakeEngine) Fields(ctx context.Context, id engine.RunID) (mappings.Fields, error) {
	f.fieldsCalls++
	return f.fields, f.fieldsErr
}
func (f *fakeEngine) Purge(ctx context.Context, id engine.RunID) error {
	f.calls = append(f.calls, "Purge")
	f.purgeCtxErr = ctx.Err()
	return f.purgeErr
}
func (f *fakeEngine) Sweep(ctx context.Context, d time.Duration) (int, error) {
	f.calls = append(f.calls, "Sweep")
	f.sweepArg = d
	return f.sweepN, f.sweepErr
}

type execFunc func(ctx context.Context, q string) ([]evaluate.Row, error)

func (e execFunc) Execute(ctx context.Context, q string) ([]evaluate.Row, error) { return e(ctx, q) }

func okExec() evaluate.Executor {
	return execFunc(func(ctx context.Context, q string) ([]evaluate.Row, error) {
		return []evaluate.Row{{ServiceName: "svc", Passed: true}}, nil
	})
}

var orig = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func sampleOf(service string) *otlpin.Sample {
	s := &otlpin.Sample{Traces: ptrace.NewTraces(), Logs: plog.NewLogs(), Metrics: pmetric.NewMetrics()}
	rs := s.Traces.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", service)
	sp := rs.ScopeSpans().AppendEmpty().Spans().AppendEmpty()
	sp.SetStartTimestamp(pcommon.NewTimestampFromTime(orig))
	sp.SetEndTimestamp(pcommon.NewTimestampFromTime(orig.Add(time.Minute)))
	return s
}

func fixedNow() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }

func TestScore_ShiftsLoadsRunsAndPurges(t *testing.T) {
	eng := &fakeEngine{exec: okExec(), stats: &engine.LoadStats{
		Sent: map[string]int{"traces": 5}, Indexed: map[string]int{"traces": 3}}}
	rep, err := Score(context.Background(), eng, sampleOf("svc"), Options{Now: fixedNow, Files: []string{"a.json"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(eng.calls, ","); got != "Ready,Load,Session,Purge" {
		t.Fatalf("calls = %s", got)
	}
	now := fixedNow()
	if d := now.Sub(eng.loadedEnd); d < 0 || d > 2*time.Minute {
		t.Fatalf("loaded end %v not near now", eng.loadedEnd)
	}
	if !rep.Sample.OriginalEnd.Equal(orig.Add(time.Minute)) || !rep.Sample.OriginalStart.Equal(orig) {
		t.Fatalf("original range = %v..%v", rep.Sample.OriginalStart, rep.Sample.OriginalEnd)
	}
	if rep.Sample.Dropped["traces"] != 2 || rep.Sample.Spans != 1 || len(rep.Sample.Files) != 1 {
		t.Fatalf("sample = %+v", rep.Sample)
	}
	if strings.Join(rep.Sample.Signals, ",") != "traces" {
		t.Fatalf("signals = %v", rep.Sample.Signals)
	}
	if rep.Backend != "esql" {
		t.Fatalf("backend = %s", rep.Backend)
	}
}

func TestScore_KeepSkipsPurge(t *testing.T) {
	eng := &fakeEngine{exec: okExec()}
	if _, err := Score(context.Background(), eng, sampleOf("svc"), Options{Now: fixedNow, Keep: true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range eng.calls {
		if c == "Purge" {
			t.Fatal("purge called with Keep")
		}
	}
}

func TestScore_PurgesWhenRunFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	eng := &fakeEngine{cancel: cancel}
	eng.exec = execFunc(func(c context.Context, q string) ([]evaluate.Row, error) {
		cancel()
		return nil, errors.New("boom")
	})
	_, _ = Score(ctx, eng, sampleOf("svc"), Options{Now: fixedNow})
	if got := eng.calls[len(eng.calls)-1]; got != "Purge" {
		t.Fatalf("calls = %v", eng.calls)
	}
	if eng.purgeCtxErr != nil {
		t.Fatalf("purge ctx was cancelled: %v", eng.purgeCtxErr)
	}
}

func TestScore_SweepWhenConfigured(t *testing.T) {
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	eng := &fakeEngine{exec: okExec(), sweepN: 2}
	if _, err := Score(context.Background(), eng, sampleOf("svc"), Options{Now: fixedNow, Sweep: time.Hour, Logf: logf}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(eng.calls, ","); got != "Ready,Sweep,Load,Session,Purge" {
		t.Fatalf("calls = %s", got)
	}
	if eng.sweepArg != time.Hour {
		t.Fatalf("sweep arg = %v", eng.sweepArg)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "swept 2") {
		t.Fatalf("logs = %v", logs)
	}

	// A sweep failure is logged, never fatal.
	logs = nil
	eng = &fakeEngine{exec: okExec(), sweepErr: errors.New("nope")}
	if _, err := Score(context.Background(), eng, sampleOf("svc"), Options{Now: fixedNow, Sweep: time.Hour, Logf: logf}); err != nil {
		t.Fatalf("sweep failure must not fail the run: %v", err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "nope") {
		t.Fatalf("logs = %v", logs)
	}
}

func TestScore_NoSweepWhenZero(t *testing.T) {
	eng := &fakeEngine{exec: okExec()}
	if _, err := Score(context.Background(), eng, sampleOf("svc"), Options{Now: fixedNow}); err != nil {
		t.Fatal(err)
	}
	for _, c := range eng.calls {
		if c == "Sweep" {
			t.Fatal("Sweep called with Sweep == 0")
		}
	}
}

func TestScore_PurgeErrorJoinedOnErrorPath(t *testing.T) {
	eng := &fakeEngine{exec: okExec(), loadErr: errors.New("load broke"), purgeErr: errors.New("purge broke")}
	_, err := Score(context.Background(), eng, sampleOf("svc"), Options{Now: fixedNow})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"load broke", "purge run vern", "purge broke"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q lacks %q", err, want)
		}
	}
	if !errors.Is(err, eng.purgeErr) || !errors.Is(err, eng.loadErr) {
		t.Errorf("err does not wrap both causes: %v", err)
	}
}

func TestScore_PurgeErrorOnSuccessIsNote(t *testing.T) {
	eng := &fakeEngine{exec: okExec(), purgeErr: errors.New("delete data streams at https://es.internal.example:9200: purge broke")}
	s := sampleOf("svc")
	s.Notes = make([]string, 1, 8) // spare capacity would expose aliasing
	s.Notes[0] = "decoder note"
	var logs []string
	logf := func(format string, a ...any) { logs = append(logs, fmt.Sprintf(format, a...)) }
	rep, err := Score(context.Background(), eng, s, Options{Now: fixedNow, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	// The report (a public API response under vern serve) gets a fixed
	// sentence; the engine error, which names the cluster, goes to the log.
	if n := rep.Sample.Notes; len(n) != 2 || n[1] != CleanupFailedNote {
		t.Fatalf("notes = %v, want the fixed cleanup note", n)
	}
	if strings.Contains(strings.Join(rep.Sample.Notes, " "), "es.internal") {
		t.Fatalf("notes leak the engine error: %v", rep.Sample.Notes)
	}
	if got := strings.Join(logs, "\n"); !strings.Contains(got, "purge broke") || !strings.Contains(got, rep.Sample.RunID) {
		t.Fatalf("purge error not logged with the run id; logs: %q", got)
	}
	if got := s.Notes[:cap(s.Notes)][1]; got != "" {
		t.Fatalf("report notes alias the sample's backing array: %q", got)
	}
	if rep.Sample.RunID == "" || !strings.HasPrefix(rep.Sample.RunID, "vern") {
		t.Fatalf("run id = %q", rep.Sample.RunID)
	}
}

func TestScore_KeepLogsRunID(t *testing.T) {
	var logs []string
	eng := &fakeEngine{exec: okExec()}
	rep, err := Score(context.Background(), eng, sampleOf("svc"), Options{Now: fixedNow, Keep: true,
		Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], rep.Sample.RunID) ||
		!strings.Contains(logs[0], "t-*") || !strings.Contains(logs[0], "l-*") || !strings.Contains(logs[0], "m-*") {
		t.Fatalf("logs = %v", logs)
	}
}

func TestScore_IgnoresConfigBackend(t *testing.T) {
	cfg := config.Default()
	cfg.Backend = "clickhouse"
	var mu sync.Mutex // rules run in parallel
	var queries []string
	eng := &fakeEngine{exec: execFunc(func(c context.Context, q string) ([]evaluate.Row, error) {
		mu.Lock()
		defer mu.Unlock()
		queries = append(queries, q)
		return nil, nil
	})}
	if _, err := Score(context.Background(), eng, sampleOf("svc"), Options{Now: fixedNow, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if len(queries) == 0 {
		t.Fatal("no queries")
	}
	for _, q := range queries {
		if !strings.Contains(q, "t-*") && !strings.Contains(q, "l-*") && !strings.Contains(q, "m-*") {
			t.Fatalf("query not scoped to the run's patterns:\n%s", q)
		}
	}
}

func TestScore_EmptySample(t *testing.T) {
	eng := &fakeEngine{exec: okExec()}
	empty := &otlpin.Sample{Traces: ptrace.NewTraces(), Logs: plog.NewLogs(), Metrics: pmetric.NewMetrics()}
	_, err := Score(context.Background(), eng, empty, Options{Now: fixedNow})
	if !errors.Is(err, otlpin.ErrEmpty) {
		t.Fatalf("err = %v", err)
	}
	for _, c := range eng.calls {
		if c == "Load" {
			t.Fatal("Load called")
		}
	}
}

func TestScore_MissingServiceNameStillEvaluated(t *testing.T) {
	eng := &fakeEngine{}
	eng.exec = execFunc(func(c context.Context, q string) ([]evaluate.Row, error) {
		if strings.Contains(q, "missing_svc") {
			return []evaluate.Row{{ServiceName: "", Passed: false, Extent: 1}}, nil
		}
		return []evaluate.Row{{ServiceName: "svc", Passed: true}}, nil
	})
	rep, err := Score(context.Background(), eng, sampleOf("svc"), Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rep.Rules {
		if r.RuleID != "RES-005" {
			continue
		}
		if r.Status != evaluate.StatusEvaluated {
			t.Fatalf("status = %s (%s)", r.Status, r.Reason)
		}
		if len(r.Outcomes) != 1 || r.Outcomes[0].ServiceName != score.MissingServiceName {
			t.Fatalf("outcomes = %+v", r.Outcomes)
		}
		return
	}
	t.Fatal("RES-005 not in report")
}

func TestWindowFor(t *testing.T) {
	cases := map[time.Duration]string{0: "1h", 90 * time.Minute: "3h", 15 * 24 * time.Hour: "361h"}
	for in, want := range cases {
		if got := WindowFor(in); got != want {
			t.Errorf("WindowFor(%v) = %s, want %s", in, got, want)
		}
	}
}

// TestScore_CancelledDuringEvaluateFails: evaluate.Run turns cancelled rule
// queries into per-rule error statuses; Score must still report the
// cancellation as an error (no partial report) and purge.
func TestScore_CancelledDuringEvaluateFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	eng := &fakeEngine{exec: execFunc(func(ctx context.Context, q string) ([]evaluate.Row, error) {
		cancel()
		return nil, ctx.Err()
	})}
	rep, err := Score(ctx, eng, sampleOf("svc"), Options{Now: fixedNow})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if rep != nil {
		t.Fatalf("partial report returned: %+v", rep)
	}
	if got := strings.Join(eng.calls, ","); !strings.HasSuffix(got, "Purge") {
		t.Fatalf("calls = %s, want Purge last", got)
	}
	if eng.purgeCtxErr != nil {
		t.Fatalf("purge ctx was cancelled: %v", eng.purgeCtxErr)
	}
}

// sensitiveMappings has one sensitive rule that echoes its fields into the
// query text and one ordinary rule.
func sensitiveMappings() *mappings.MappingsFile {
	return &mappings.MappingsFile{Rules: []mappings.RuleMapping{
		{SpecRuleID: "SPA-006", Enabled: true, Sensitive: true, Impact: "Critical", Signal: "traces",
			Query: "SENS {{ range .Fields.Span }}{{ . }};{{ end }}"},
		{SpecRuleID: "RES-001", Enabled: true, Impact: "Critical", Signal: "traces",
			Query: "PLAIN"},
	}}
}

func TestScore_PassesFieldsToRun(t *testing.T) {
	var queries []string
	var mu sync.Mutex
	eng := &fakeEngine{
		fields: mappings.Fields{Span: []string{"attributes.a", "attributes.b"}},
		exec: execFunc(func(ctx context.Context, q string) ([]evaluate.Row, error) {
			mu.Lock()
			queries = append(queries, q)
			mu.Unlock()
			return []evaluate.Row{{ServiceName: "svc", Passed: true}}, nil
		}),
	}
	if _, err := Score(context.Background(), eng, sampleOf("svc"), Options{Now: fixedNow, Mappings: sensitiveMappings()}); err != nil {
		t.Fatal(err)
	}
	if eng.fieldsCalls != 1 {
		t.Fatalf("Fields calls = %d, want 1", eng.fieldsCalls)
	}
	found := false
	for _, q := range queries {
		if strings.HasSuffix(q, "SENS attributes.a;attributes.b;") {
			found = true
		}
	}
	if !found {
		t.Fatalf("fields did not reach the template data: %q", queries)
	}
}

func TestScore_FieldsErrorOnlyAffectsSensitiveRules(t *testing.T) {
	var queries []string
	var mu sync.Mutex
	eng := &fakeEngine{
		fieldsErr: errors.New("boom https://cluster.internal"),
		exec: execFunc(func(ctx context.Context, q string) ([]evaluate.Row, error) {
			mu.Lock()
			queries = append(queries, q)
			mu.Unlock()
			return []evaluate.Row{{ServiceName: "svc", Passed: true}}, nil
		}),
	}
	rep, err := Score(context.Background(), eng, sampleOf("svc"), Options{Now: fixedNow, Mappings: sensitiveMappings()})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]evaluate.RuleResult{}
	for _, r := range rep.Rules {
		got[r.RuleID] = r
	}
	if r := got["SPA-006"]; r.Status != evaluate.StatusError || r.Reason != "field discovery failed" {
		t.Fatalf("SPA-006 = %s %q, want error / field discovery failed", r.Status, r.Reason)
	}
	if r := got["RES-001"]; r.Status != evaluate.StatusEvaluated {
		t.Fatalf("RES-001 = %s %q, want evaluated", r.Status, r.Reason)
	}
	if len(queries) != 1 || !strings.HasSuffix(queries[0], "PLAIN") {
		t.Fatalf("queries = %q, want only the ordinary rule", queries)
	}
	if b, _ := json.Marshal(rep); strings.Contains(string(b), "cluster.internal") {
		t.Fatalf("engine error leaked into the report")
	}
}
