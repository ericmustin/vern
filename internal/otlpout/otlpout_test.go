package otlpout

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/score"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigFromEnv_EndpointResolution(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		url  string
	}{
		{"base", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318"}, "http://c:4318/v1/metrics"},
		{"base slash", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318/"}, "http://c:4318/v1/metrics"},
		{"metrics wins verbatim", map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT":         "http://c:4318",
			"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://c:4318/custom/v1/metrics"}, "http://c:4318/custom/v1/metrics"},
		{"none", map[string]string{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := ConfigFromEnv(env(tc.env))
			if c.MetricsURL != tc.url || c.Enabled() != (tc.url != "") {
				t.Fatalf("url = %q enabled=%v, want %q", c.MetricsURL, c.Enabled(), tc.url)
			}
		})
	}
}

func TestConfigFromEnv_HeadersAndResource(t *testing.T) {
	c := ConfigFromEnv(env(map[string]string{
		"OTEL_EXPORTER_OTLP_HEADERS":         "api-key=a%20b,x=1,bogus",
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS": "x=2",
		"OTEL_RESOURCE_ATTRIBUTES":           "vcs.ref.head.revision=abc,env=a%2Cb",
	}))
	if c.Headers["api-key"] != "a b" || c.Headers["x"] != "2" || len(c.Headers) != 2 {
		t.Fatalf("headers = %v", c.Headers)
	}
	if c.ResourceAttributes["vcs.ref.head.revision"] != "abc" || c.ResourceAttributes["env"] != "a,b" {
		t.Fatalf("resource = %v", c.ResourceAttributes)
	}
	if len(c.Warnings) != 1 || strings.Contains(c.Warnings[0], "bogus") {
		t.Fatalf("warnings = %v", c.Warnings)
	}
	c = c.WithHeaders([]string{"x=3", "k = v"}).WithEndpoint("http://f:1/v1/metrics/")
	if c.Headers["x"] != "3" || c.Headers["k"] != "v" || c.MetricsURL != "http://f:1/v1/metrics" {
		t.Fatalf("overrides: %v %s", c.Headers, c.MetricsURL)
	}
}

func testReport() *evaluate.Report {
	out := func(svc string, p bool) score.Outcome { return score.Outcome{ServiceName: svc, Passed: p} }
	return &evaluate.Report{
		SpecVersion: "0.9.9",
		EvaluatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Score: &score.Result{Scored: true, Overall: 71.26, Category: "Good", Services: []score.ServiceScore{
			{ServiceName: "a", Score: 80.04, Category: "Good"},
			{ServiceName: "b", Score: 60, Category: "Needs Improvement"},
		}},
		Rules: []evaluate.RuleResult{
			{RuleID: "R1", Impact: "Critical", Status: evaluate.StatusEvaluated, Scored: true, Outcomes: []score.Outcome{out("a", true), out("b", false)}},
			{RuleID: "R2", Impact: "Normal", Status: evaluate.StatusEvaluated, Scored: true, Outcomes: []score.Outcome{out("a", true), out("b", true)}},
			{RuleID: "R3", Impact: "Low", Status: evaluate.StatusEvaluated, Advisory: true, Outcomes: []score.Outcome{out("a", false), out("b", true)}},
			{RuleID: "R4", Impact: "Low", Status: evaluate.StatusNotImplemented},
		},
	}
}

func metricsByName(md pmetric.Metrics) map[string]pmetric.Metric {
	m := map[string]pmetric.Metric{}
	ms := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	for i := 0; i < ms.Len(); i++ {
		m[ms.At(i).Name()] = ms.At(i)
	}
	return m
}

func TestBuild_MetricsShape(t *testing.T) {
	md := Build(testReport(), Config{ServiceVersion: "1.2.3", ResourceAttributes: map[string]string{"service.version": "override", "x": "y"}})
	res := md.ResourceMetrics().At(0).Resource().Attributes()
	get := func(m pcommon.Map, k string) string {
		if v, ok := m.Get(k); ok {
			return v.AsString()
		}
		return ""
	}
	if get(res, "service.name") != "vern" || get(res, "service.version") != "override" ||
		get(res, "instrumentation_score.spec.version") != "0.9.9" || get(res, "x") != "y" {
		t.Fatalf("resource = %v", res.AsRaw())
	}
	sc := md.ResourceMetrics().At(0).ScopeMetrics().At(0)
	if sc.Scope().Name() != "github.com/ericmustin/vern" {
		t.Fatalf("scope %q", sc.Scope().Name())
	}
	ms := metricsByName(md)
	for name, unit := range map[string]string{"instrumentation_score": "{score}", "instrumentation_score.rule.passed": "1", "instrumentation_score.rules": "{rule}"} {
		m, ok := ms[name]
		if !ok || m.Unit() != unit || m.Type() != pmetric.MetricTypeGauge {
			t.Fatalf("%s: ok=%v unit=%q type=%v", name, ok, m.Unit(), m.Type())
		}
	}
	// score gauge
	dps := ms["instrumentation_score"].Gauge().DataPoints()
	if dps.Len() != 3 {
		t.Fatalf("score points = %d", dps.Len())
	}
	seen := map[string]float64{}
	for i := 0; i < dps.Len(); i++ {
		p := dps.At(i)
		if p.Timestamp().AsTime() != testReport().EvaluatedAt {
			t.Fatalf("timestamp %v", p.Timestamp().AsTime())
		}
		key := get(p.Attributes(), "instrumentation_score.scope") + ":" + get(p.Attributes(), "instrumentation_score.service.name")
		if get(p.Attributes(), "instrumentation_score.category") == "" {
			t.Fatalf("missing category on %s", key)
		}
		seen[key] = p.DoubleValue()
	}
	if seen["overall:"] != 71.3 || seen["service:a"] != 80 || seen["service:b"] != 60 {
		t.Fatalf("scores = %v", seen)
	}
	// rule.passed
	rp := ms["instrumentation_score.rule.passed"].Gauge().DataPoints()
	if rp.Len() != 6 {
		t.Fatalf("rule.passed points = %d", rp.Len())
	}
	for i := 0; i < rp.Len(); i++ {
		a := rp.At(i).Attributes()
		id, _ := a.Get("instrumentation_score.rule.scored")
		if get(a, "instrumentation_score.rule.id") == "R3" == id.Bool() {
			t.Fatalf("scored flag wrong for %v", a.AsRaw())
		}
		if get(a, "instrumentation_score.rule.impact") == "" || get(a, "instrumentation_score.service.name") == "" {
			t.Fatalf("attrs %v", a.AsRaw())
		}
	}
	// rules by status
	rs := ms["instrumentation_score.rules"].Gauge().DataPoints()
	got := map[string]int64{}
	for i := 0; i < rs.Len(); i++ {
		got[get(rs.At(i).Attributes(), "instrumentation_score.rule.status")] = rs.At(i).IntValue()
	}
	if len(got) != 2 || got["evaluated"] != 3 || got["not_implemented"] != 1 {
		t.Fatalf("rules = %v", got)
	}
	// no data point uses service.name
	for _, m := range ms {
		dps := m.Gauge().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			if _, ok := dps.At(i).Attributes().Get("service.name"); ok {
				t.Fatalf("%s data point has service.name", m.Name())
			}
		}
	}
}

func TestBuild_NotScoredOmitsScoreGauge(t *testing.T) {
	rep := testReport()
	rep.Score = &score.Result{Scored: false}
	if _, ok := metricsByName(Build(rep, Config{}))["instrumentation_score"]; ok {
		t.Fatal("instrumentation_score emitted for an unscored run")
	}
	rep.Score = nil
	if _, ok := metricsByName(Build(rep, Config{}))["instrumentation_score"]; ok {
		t.Fatal("instrumentation_score emitted with nil score")
	}
}

func TestExport_PostsProtobuf(t *testing.T) {
	var gotCT, gotKey string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT, gotKey = r.Header.Get("Content-Type"), r.Header.Get("api-key")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := Config{MetricsURL: srv.URL + "/v1/metrics", Headers: map[string]string{"api-key": "s3cret"}, ServiceVersion: "v"}
	if err := Export(context.Background(), testReport(), c); err != nil {
		t.Fatal(err)
	}
	if gotCT != "application/x-protobuf" || gotKey != "s3cret" {
		t.Fatalf("ct=%q key=%q", gotCT, gotKey)
	}
	req := pmetricotlp.NewExportRequest()
	if err := req.UnmarshalProto(body); err != nil {
		t.Fatal(err)
	}
	if req.Metrics().MetricCount() != 3 {
		t.Fatalf("metrics = %d", req.Metrics().MetricCount())
	}
}

func TestExport_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()
	u := strings.Replace(srv.URL, "http://", "http://user:pw@", 1) + "/v1/metrics?token=zzz"
	err := Export(context.Background(), testReport(), Config{MetricsURL: u, Headers: map[string]string{"api-key": "s3cret"}})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	for _, leak := range []string{"pw", "s3cret", "zzz"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error leaks %q: %v", leak, err)
		}
	}
	// connection failure also redacts
	srv.Close()
	err = Export(context.Background(), testReport(), Config{MetricsURL: u})
	if err == nil || strings.Contains(err.Error(), "pw@") || strings.Contains(err.Error(), "zzz") {
		t.Fatalf("conn err = %v", err)
	}
}

func TestExport_DisabledIsNoop(t *testing.T) {
	if err := Export(context.Background(), testReport(), Config{}); err != nil {
		t.Fatal(err)
	}
}

func TestBaseToMetricsURL_QueryAndSlashes(t *testing.T) {
	for in, want := range map[string]string{
		"http://c:4318?token=x":         "http://c:4318/v1/metrics?token=x",
		"http://c:4318/v1/metrics/?x=1": "http://c:4318/v1/metrics?x=1",
		"http://c:4318/base/?a=b":       "http://c:4318/base/v1/metrics?a=b",
		"http://c:4318/v1/metrics":      "http://c:4318/v1/metrics",
	} {
		if got := BaseToMetricsURL(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
		if got := ConfigFromEnv(env(map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": in})).MetricsURL; got != want {
			t.Errorf("env %s -> %s, want %s", in, got, want)
		}
	}
}

func TestExport_RedirectIsError(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	err := Export(context.Background(), testReport(), Config{MetricsURL: srv.URL, Headers: map[string]string{"api-key": "s"}})
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") || hits != 0 {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
}

func TestConfig_HeaderKeysLowercasedFlagWins(t *testing.T) {
	c := ConfigFromEnv(env(map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "API-Key=env"}))
	c = c.WithHeaders([]string{"api-KEY=flag"})
	if len(c.Headers) != 1 || c.Headers["api-key"] != "flag" {
		t.Fatalf("headers = %v", c.Headers)
	}
}

func TestBuild_NoRulePassedMetricWithoutPoints(t *testing.T) {
	rep := testReport()
	rep.Rules = rep.Rules[3:]
	if _, ok := metricsByName(Build(rep, Config{}))["instrumentation_score.rule.passed"]; ok {
		t.Fatal("empty rule.passed metric emitted")
	}
}

// rowsExec answers every query with the same rows.
type rowsExec []evaluate.Row

func (r rowsExec) Execute(context.Context, string) ([]evaluate.Row, error) { return r, nil }

// TestBuild_ServiceNamePseudonym: a service.name that a detector
// matches reaches the OTLP export only as its pseudonym, never raw; a clean
// service name is exported as is. The report comes from evaluate.Run, the
// one place names are masked.
func TestBuild_ServiceNamePseudonym(t *testing.T) {
	const canary = "alice@example.com"
	mf := &mappings.MappingsFile{Backend: "esql", Rules: []mappings.RuleMapping{
		{SpecRuleID: "RES-005", Enabled: true, Impact: "Critical", Target: "Resource", Query: "PLAIN"},
	}}
	cfg := config.Default()
	rep, err := evaluate.Run(context.Background(), cfg, rowsExec{
		{ServiceName: canary, Passed: false, Extent: 1, Example: "span GET /"},
		{ServiceName: "checkout", Passed: true},
	}, evaluate.RunOptions{Mappings: mf, Sample: &evaluate.SampleInfo{Services: []string{"checkout", canary}}})
	if err != nil {
		t.Fatal(err)
	}
	js, err := (&pmetric.JSONMarshaler{}).MarshalMetrics(Build(rep, Config{}))
	if err != nil {
		t.Fatal(err)
	}
	out := string(js)
	if strings.Contains(out, canary) {
		t.Errorf("OTLP export carries the raw service name: %s", out)
	}
	for _, want := range []string{"service-1 (name withheld: matches email address)", `"checkout"`} {
		if !strings.Contains(out, want) {
			t.Errorf("OTLP export lacks %s", want)
		}
	}
}
