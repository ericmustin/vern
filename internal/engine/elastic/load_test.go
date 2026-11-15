package elastic

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/ericmustin/vern/internal/otlpin"
)

type post struct {
	path, ctype string
	body        []byte
}

type loadSrv struct {
	mu        sync.Mutex
	posts     []post
	counts    map[string]int // ES|QL query -> count
	otlp      func(w http.ResponseWriter, path string)
	queries   []string
	refreshes []string
}

func (l *loadSrv) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		defer l.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/_otlp/"):
			l.posts = append(l.posts, post{r.URL.Path, r.Header.Get("Content-Type"), b})
			if l.otlp != nil {
				l.otlp(w, r.URL.Path)
			}
		case strings.HasSuffix(r.URL.Path, "/_refresh"):
			l.refreshes = append(l.refreshes, r.URL.Path)
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/_query":
			var q string
			if i := strings.Index(string(b), `"FROM `); i >= 0 {
				q = string(b)[i+1:]
				q = q[len("FROM "):strings.Index(q, `|`)]
				q = strings.TrimSpace(q)
			}
			l.queries = append(l.queries, q)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"columns":[{"name":"c","type":"long"}],"values":[[%d]]}`, l.counts[q])))
		default:
			w.WriteHeader(404)
		}
	})
}

func sampleTraces(n int) *otlpin.Sample {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "svc")
	sp := rs.ScopeSpans().AppendEmpty().Spans()
	for i := 0; i < n; i++ {
		s := sp.AppendEmpty()
		s.SetName("op")
		s.Attributes().PutStr("k", strings.Repeat("v", 20))
	}
	return &otlpin.Sample{Traces: td, Logs: plog.NewLogs(), Metrics: pmetric.NewMetrics()}
}

func fullSample() *otlpin.Sample {
	s := sampleTraces(3)
	s.Traces.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Events().AppendEmpty().SetName("ev")
	lr := s.Logs.ResourceLogs().AppendEmpty()
	lr.Resource().Attributes().PutStr("service.name", "svc")
	lr.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty().Body().SetStr("hi")
	rm := s.Metrics.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "svc")
	g := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	g.SetName("m")
	dp := g.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetDoubleValue(1)
	dp.SetTimestamp(pcommon.NewTimestampFromTime(time.Now()))
	return s
}

func newLoadEngine(t *testing.T, ls *loadSrv, batch int) *Engine {
	srv := httptest.NewServer(ls.handler())
	t.Cleanup(srv.Close)
	return New(Config{URL: srv.URL, BatchBytes: batch})
}

func TestLoad_PostsProtobufPerSignal(t *testing.T) {
	ls := &loadSrv{}
	e := newLoadEngine(t, ls, 0)
	id := newRunID(t, time.Now())
	st, err := e.Load(t.Context(), id, fullSample())
	if err != nil {
		t.Fatal(err)
	}
	if len(ls.posts) != 3 {
		t.Fatalf("posts = %d", len(ls.posts))
	}
	for _, p := range ls.posts {
		if p.ctype != "application/x-protobuf" {
			t.Errorf("%s content type %q", p.path, p.ctype)
		}
		switch p.path {
		case "/_otlp/v1/traces":
			req := ptraceotlp.NewExportRequest()
			if err := req.UnmarshalProto(p.body); err != nil {
				t.Fatal(err)
			}
			if req.Traces().SpanCount() != 3 {
				t.Errorf("spans = %d", req.Traces().SpanCount())
			}
			assertNS(t, req.Traces().ResourceSpans().At(0).Resource().Attributes(), string(id))
		case "/_otlp/v1/logs":
			req := plogotlp.NewExportRequest()
			if err := req.UnmarshalProto(p.body); err != nil {
				t.Fatal(err)
			}
			if req.Logs().LogRecordCount() != 1 {
				t.Errorf("logs = %d", req.Logs().LogRecordCount())
			}
			assertNS(t, req.Logs().ResourceLogs().At(0).Resource().Attributes(), string(id))
		case "/_otlp/v1/metrics":
			req := pmetricotlp.NewExportRequest()
			if err := req.UnmarshalProto(p.body); err != nil {
				t.Fatal(err)
			}
			if req.Metrics().DataPointCount() != 1 {
				t.Errorf("points = %d", req.Metrics().DataPointCount())
			}
			assertNS(t, req.Metrics().ResourceMetrics().At(0).Resource().Attributes(), string(id))
		default:
			t.Errorf("unexpected path %s", p.path)
		}
	}
	if st.Sent["traces"] != 3 || st.Sent["logs"] != 2 || st.Sent["metrics"] != 1 {
		t.Errorf("sent = %v (logs = records + span events)", st.Sent)
	}
	if st.Indexed["metrics"] != -1 {
		t.Errorf("metrics indexed = %d, want -1", st.Indexed["metrics"])
	}
}

func assertNS(t *testing.T, m pcommon.Map, want string) {
	t.Helper()
	v, ok := m.Get("data_stream.namespace")
	if !ok || v.Str() != want {
		t.Errorf("data_stream.namespace = %v, want %s", v.AsString(), want)
	}
}

func TestLoad_SplitsBatches(t *testing.T) {
	ls := &loadSrv{}
	e := newLoadEngine(t, ls, 2<<10)
	if _, err := e.Load(t.Context(), newRunID(t, time.Now()), sampleTraces(200)); err != nil {
		t.Fatal(err)
	}
	if len(ls.posts) < 2 {
		t.Fatalf("posts = %d, want > 1", len(ls.posts))
	}
	total := 0
	for _, p := range ls.posts {
		if len(p.body) > 2<<10 {
			t.Errorf("body %d bytes exceeds 2 KiB", len(p.body))
		}
		req := ptraceotlp.NewExportRequest()
		if err := req.UnmarshalProto(p.body); err != nil {
			t.Fatal(err)
		}
		total += req.Traces().SpanCount()
	}
	if total != 200 {
		t.Errorf("spans across batches = %d, want 200", total)
	}
}

func TestLoad_OversizedRecordSentAlone(t *testing.T) {
	ls := &loadSrv{}
	e := newLoadEngine(t, ls, 1<<10)
	s := sampleTraces(2)
	s.Traces.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Attributes().PutStr("big", strings.Repeat("x", 3<<10))
	if _, err := e.Load(t.Context(), newRunID(t, time.Now()), s); err != nil {
		t.Fatal(err)
	}
	if len(ls.posts) != 2 {
		t.Fatalf("posts = %d, want 2", len(ls.posts))
	}
}

func TestLoad_RecordOverElasticsearchLimitErrors(t *testing.T) {
	ls := &loadSrv{}
	e := newLoadEngine(t, ls, 0)
	s := sampleTraces(1)
	s.Traces.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Attributes().PutStr("big", strings.Repeat("x", 9<<20))
	_, err := e.Load(t.Context(), newRunID(t, time.Now()), s)
	if err == nil || !strings.Contains(err.Error(), "traces") || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("err = %v", err)
	}
	if len(ls.posts) != 0 {
		t.Error("oversized record was posted")
	}
}

func TestLoad_SplitsOversizedMetricByPoint(t *testing.T) {
	ls := &loadSrv{}
	e := newLoadEngine(t, ls, 1<<10)
	s := &otlpin.Sample{Traces: ptrace.NewTraces(), Logs: plog.NewLogs(), Metrics: pmetric.NewMetrics()}
	rm := s.Metrics.ResourceMetrics().AppendEmpty()
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("big")
	dps := m.SetEmptySum().DataPoints()
	for i := 0; i < 100; i++ {
		dp := dps.AppendEmpty()
		dp.SetIntValue(int64(i))
		dp.Attributes().PutStr("k", strings.Repeat("v", 30))
	}
	if _, err := e.Load(t.Context(), newRunID(t, time.Now()), s); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, p := range ls.posts {
		if len(p.body) > 1<<10 {
			t.Errorf("body %d > 1 KiB", len(p.body))
		}
		req := pmetricotlp.NewExportRequest()
		if err := req.UnmarshalProto(p.body); err != nil {
			t.Fatal(err)
		}
		total += req.Metrics().DataPointCount()
	}
	if total != 100 || len(ls.posts) < 2 {
		t.Errorf("points = %d in %d posts", total, len(ls.posts))
	}
}

func TestLoad_429Aborts(t *testing.T) {
	ls := &loadSrv{otlp: func(w http.ResponseWriter, _ string) { w.WriteHeader(429) }}
	e := newLoadEngine(t, ls, 0)
	_, err := e.Load(t.Context(), newRunID(t, time.Now()), fullSample())
	if !errors.Is(err, ErrTooManyRequests) || err.Error() != "elasticsearch rejected the sample (429 Too Many Requests); run aborted" {
		t.Fatalf("err = %v", err)
	}
	if len(ls.posts) != 1 {
		t.Errorf("posts = %d, want 1 (no retry, no further signals)", len(ls.posts))
	}
}

func TestLoad_ErrorStatusIsPrintable(t *testing.T) {
	ls := &loadSrv{otlp: func(w http.ResponseWriter, _ string) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte("\x12\x4aInvalid OTLP protobuf payload\x00\xff"))
	}}
	e := newLoadEngine(t, ls, 0)
	_, err := e.Load(t.Context(), newRunID(t, time.Now()), sampleTraces(1))
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "traces") {
		t.Fatalf("err = %v", err)
	}
	for _, r := range err.Error() {
		if r < 0x20 || r == 0xfffd {
			t.Errorf("unprintable %q in %q", r, err)
		}
	}
}

func TestLoad_PartialSuccessCountsAsFailed(t *testing.T) {
	ls := &loadSrv{otlp: func(w http.ResponseWriter, _ string) {
		resp := ptraceotlp.NewExportResponse()
		resp.PartialSuccess().SetRejectedSpans(2)
		b, _ := resp.MarshalProto()
		_, _ = w.Write(b)
	}}
	e := newLoadEngine(t, ls, 0)
	st, err := e.Load(t.Context(), newRunID(t, time.Now()), sampleTraces(3))
	if err != nil {
		t.Fatal(err)
	}
	if st.Failed["traces"] != 2 {
		t.Errorf("failed = %v", st.Failed)
	}
}

func TestLoad_ReconcilesCounts(t *testing.T) {
	ls := &loadSrv{}
	e := newLoadEngine(t, ls, 0)
	id := newRunID(t, time.Now())
	ls.counts = map[string]int{"traces-*.otel-" + string(id): 8}
	ls.counts["traces-*.otel-"+string(id)+"::failures"] = 1
	st, err := e.Load(t.Context(), id, sampleTraces(10))
	if err != nil {
		t.Fatal(err)
	}
	if st.Sent["traces"] != 10 || st.Indexed["traces"] != 8 || st.Failed["traces"] != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if d := st.Dropped()["traces"]; d != 2 {
		t.Errorf("dropped = %d, want 2", d)
	}
}

func TestLoad_SplitsHugeMetricQuickly(t *testing.T) {
	ls := &loadSrv{}
	e := newLoadEngine(t, ls, 4<<10)
	s := &otlpin.Sample{Traces: ptrace.NewTraces(), Logs: plog.NewLogs(), Metrics: pmetric.NewMetrics()}
	rm := s.Metrics.ResourceMetrics().AppendEmpty()
	sm := rm.ScopeMetrics().AppendEmpty().Metrics()
	g := sm.AppendEmpty()
	g.SetName("g")
	gd := g.SetEmptyGauge().DataPoints()
	for i := 0; i < 50000; i++ {
		dp := gd.AppendEmpty()
		dp.SetIntValue(int64(i))
		dp.Attributes().PutStr("k", "v")
	}
	su := sm.AppendEmpty()
	su.SetName("s")
	su.SetUnit("By")
	su.SetDescription("d")
	sum := su.SetEmptySum()
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	sum.SetIsMonotonic(true)
	for i := 0; i < 20000; i++ {
		sum.DataPoints().AppendEmpty().SetIntValue(int64(i))
	}
	start := time.Now()
	if _, err := e.Load(t.Context(), newRunID(t, time.Now()), s); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v", d)
	}
	gauges, sums := 0, 0
	for _, p := range ls.posts {
		if len(p.body) > 4<<10 {
			t.Errorf("body %d > 4 KiB", len(p.body))
		}
		req := pmetricotlp.NewExportRequest()
		if err := req.UnmarshalProto(p.body); err != nil {
			t.Fatal(err)
		}
		rms := req.Metrics().ResourceMetrics()
		for i := 0; i < rms.Len(); i++ {
			ms := rms.At(i).ScopeMetrics().At(0).Metrics()
			for j := 0; j < ms.Len(); j++ {
				m := ms.At(j)
				switch m.Name() {
				case "g":
					gauges += m.Gauge().DataPoints().Len()
				case "s":
					sums += m.Sum().DataPoints().Len()
					if m.Unit() != "By" || m.Description() != "d" || !m.Sum().IsMonotonic() ||
						m.Sum().AggregationTemporality() != pmetric.AggregationTemporalityDelta {
						t.Fatalf("sum skeleton lost: %v %v", m.Unit(), m.Sum().AggregationTemporality())
					}
				default:
					t.Fatalf("unexpected metric %q", m.Name())
				}
			}
		}
	}
	if gauges != 50000 || sums != 20000 {
		t.Errorf("points: gauge %d sum %d", gauges, sums)
	}
}

func TestNew_ClampsBatchBytes(t *testing.T) {
	if got := New(Config{URL: "http://x", BatchBytes: 100 << 20}).cfg.BatchBytes; got != 8<<20 {
		t.Errorf("BatchBytes = %d, want 8 MiB", got)
	}
	if got := New(Config{URL: "http://x"}).cfg.BatchBytes; got != 4<<20 {
		t.Errorf("default = %d", got)
	}
}

func TestLoad_RefreshesFailuresOnlyForSentSignals(t *testing.T) {
	ls := &loadSrv{}
	e := newLoadEngine(t, ls, 0)
	id := newRunID(t, time.Now())
	if _, err := e.Load(t.Context(), id, sampleTraces(2)); err != nil {
		t.Fatal(err)
	}
	if len(ls.refreshes) != 1 {
		t.Fatalf("refreshes = %v", ls.refreshes)
	}
	p := ls.refreshes[0]
	if !strings.Contains(p, "traces-*.otel-"+string(id)+"::failures") {
		t.Errorf("no traces ::failures in %q", p)
	}
	if strings.Contains(p, "logs-") || strings.Contains(p, "metrics-") {
		t.Errorf("unsent signals refreshed: %q", p)
	}
	ls.refreshes = nil
	if _, err := e.Load(t.Context(), id, fullSample()); err != nil {
		t.Fatal(err)
	}
	for _, sig := range []string{"traces", "logs", "metrics"} {
		if !strings.Contains(ls.refreshes[0], sig+"-*.otel-"+string(id)+"::failures") {
			t.Errorf("missing %s ::failures in %q", sig, ls.refreshes[0])
		}
	}
}
