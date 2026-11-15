package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/report"
	"github.com/ericmustin/vern/internal/score"
	"github.com/spf13/cobra"
)

func clearESEnv(t *testing.T) {
	for _, k := range []string{"VERN_ELASTICSEARCH_URL", "VERN_API_KEY", "VERN_API_KEY_FILE", "VERN_ELASTICSEARCH_USERNAME", "VERN_ELASTICSEARCH_PASSWORD", "ELASTIC_API_KEY", "KIBANA_API_KEY"} {
		t.Setenv(k, "")
	}
}

func resetScoreFlags(t *testing.T) {
	oJSON, oMD, oKeep, oMap, oRules, oCfg, oTmpl := scoreOutputJSON, scoreOutputMD, scoreKeep, scoreMappings, scoreRulesDir, configPath, scoreSetupTmpl
	oTimeout, oReady, oMin, oOut := scoreTimeout, scoreReadyTimeout, scoreMinScore, scoreOutputsFile
	oEP, oHd, oDebug := scoreOTLPEndpoint, scoreOTLPHeaders, scoreDebug
	t.Setenv("VERN_DEBUG", "")
	t.Cleanup(func() {
		scoreDebug = oDebug
		scoreOutputJSON, scoreOutputMD, scoreKeep, scoreMappings, scoreRulesDir, configPath, scoreSetupTmpl = oJSON, oMD, oKeep, oMap, oRules, oCfg, oTmpl
		scoreTimeout, scoreReadyTimeout, scoreMinScore, scoreOutputsFile = oTimeout, oReady, oMin, oOut
		scoreOTLPEndpoint, scoreOTLPHeaders = oEP, oHd
	})
}

func TestScoreCmd_RequiresURL(t *testing.T) {
	clearESEnv(t)
	err := runScore(scoreCmd, []string{"x.json"})
	if err == nil || !strings.Contains(err.Error(), "vern score needs VERN_ELASTICSEARCH_URL") {
		t.Fatalf("err = %v", err)
	}
}

func TestScoreCmd_APIKeyFile(t *testing.T) {
	clearESEnv(t)
	p := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(p, []byte("abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VERN_API_KEY_FILE", p)
	c, err := elasticCredentials()
	if err != nil || c.APIKey != "abc" {
		t.Fatalf("got %q, %v", c.APIKey, err)
	}
	t.Setenv("VERN_API_KEY", "direct")
	if c, _ = elasticCredentials(); c.APIKey != "direct" {
		t.Fatalf("VERN_API_KEY should win, got %q", c.APIKey)
	}
}

func TestScoreCmd_IgnoresLegacyElasticVars(t *testing.T) {
	clearESEnv(t)
	t.Setenv("ELASTIC_API_KEY", "should-not-be-used")
	t.Setenv("KIBANA_API_KEY", "should-not-be-used")
	c, err := elasticCredentials()
	if err != nil || c.APIKey != "" {
		t.Fatalf("got %q, %v", c.APIKey, err)
	}
}

// fakeMode selects how fakeES answers rule queries and index counts.
type fakeMode int

const (
	rulesNoRows  fakeMode = iota // every rule query returns zero rows (not scored)
	rulesAllFail                 // every rule query returns one failing row
	rulesAllPass                 // every rule query returns one passing row
	rulesError                   // every rule query fails with HTTP 500
	dropAll                      // nothing is indexed: counts 0, failure stores full
)

// fakeES is a minimal Elasticsearch 9.5.5 for vern score. Index counts
// report everything indexed unless dropAll is among the modes.
func fakeES(t *testing.T, modes ...fakeMode) *httptest.Server {
	t.Helper()
	rules, drop := rulesNoRows, false
	for _, m := range modes {
		if m == dropAll {
			drop = true
		} else {
			rules = m
		}
	}
	const cols = `{"columns":[{"name":"rule_passed","type":"boolean"},{"name":"service.name","type":"keyword"},{"name":"extent","type":"double"},{"name":"example","type":"keyword"}],"values":`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case p == "/":
			_, _ = w.Write([]byte(`{"version":{"number":"9.5.5"}}`))
		case p == "/_license":
			_, _ = w.Write([]byte(`{"license":{"type":"basic"}}`))
		case strings.HasPrefix(p, "/_component_template/"):
			if r.Method == http.MethodGet {
				w.WriteHeader(404)
			}
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		case strings.HasPrefix(p, "/_otlp/v1/"):
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.WriteHeader(200)
		case strings.HasSuffix(p, "/_field_caps"):
			_, _ = w.Write([]byte(`{"indices":[],"fields":{}}`))
		case strings.HasSuffix(p, "/_refresh"):
			_, _ = w.Write([]byte(`{"_shards":{"failed":0}}`))
		case p == "/_query":
			body, _ := io.ReadAll(r.Body)
			q := string(body)
			if strings.Contains(q, "STATS c = COUNT(*)") {
				// Index counts: everything indexed and no failures, unless dropAll.
				n := 0
				if strings.Contains(q, "::failures") == drop {
					n = 1 << 30
				}
				_, _ = fmt.Fprintf(w, `{"columns":[{"name":"c","type":"long"}],"values":[[%d]]}`, n)
				return
			}
			switch rules {
			case rulesAllFail:
				_, _ = w.Write([]byte(cols + `[[false,"svc",1.0,"bad"]]}`))
			case rulesAllPass:
				_, _ = w.Write([]byte(cols + `[[true,"svc",0.0,""]]}`))
			case rulesError:
				w.WriteHeader(500)
				_, _ = w.Write([]byte(`{"error":"kaboom"}`))
			default:
				_, _ = w.Write([]byte(cols + `[]}`))
			}
		case strings.HasPrefix(p, "/_data_stream/"):
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"data_streams":[]}`))
			} else {
				_, _ = w.Write([]byte(`{"acknowledged":true}`))
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestScoreCmd_EndToEndAgainstFakeES(t *testing.T) {
	clearESEnv(t)
	resetScoreFlags(t)
	srv := fakeES(t)
	t.Setenv("VERN_ELASTICSEARCH_URL", srv.URL)
	dir := t.TempDir()
	scoreOutputJSON = filepath.Join(dir, "r.json")
	scoreOutputMD = filepath.Join(dir, "r.md")
	configPath = filepath.Join(dir, "absent.yaml")

	if err := runScore(scoreCmd, []string{"../web/examples/demo.jsonl.gz"}); err != nil {
		t.Fatalf("runScore: %v", err)
	}
	raw, err := os.ReadFile(scoreOutputJSON)
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Sample *json.RawMessage `json:"sample"`
		Rules  []any            `json:"rules"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Sample == nil {
		t.Error("no sample block")
	}
	if len(rep.Rules) != 26 {
		t.Errorf("rules = %d, want 26", len(rep.Rules))
	}
	if _, err := os.Stat(scoreOutputMD); err != nil {
		t.Error(err)
	}
}

// TestScoreCmd_CancelledStillPurges cancels the command context while the
// sample is being loaded and checks that the run's data streams are still
// deleted.
func TestScoreCmd_CancelledStillPurges(t *testing.T) {
	clearESEnv(t)
	resetScoreFlags(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var deletes []string
	var runNS string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case p == "/":
			_, _ = w.Write([]byte(`{"version":{"number":"9.5.5"}}`))
		case p == "/_license":
			_, _ = w.Write([]byte(`{"license":{"type":"basic"}}`))
		case strings.HasPrefix(p, "/_component_template/"):
			_, _ = w.Write([]byte(`{}`))
		case strings.HasPrefix(p, "/_otlp/v1/"):
			// Simulate Ctrl-C mid-load; the client sees its context cancelled.
			cancel()
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.WriteHeader(200)
		case strings.HasPrefix(p, "/_data_stream/") && r.Method == http.MethodGet:
			pats := strings.TrimPrefix(p, "/_data_stream/")
			if strings.HasPrefix(pats, "*-*") { // startup sweep: nothing old
				_, _ = w.Write([]byte(`{"data_streams":[]}`))
				return
			}
			// Purge listing: traces-*.otel-<id>,logs-*...,metrics-*...
			first := strings.Split(pats, ",")[0]
			ns := first[strings.LastIndex(first, ".otel-")+len(".otel-"):]
			mu.Lock()
			runNS = ns
			mu.Unlock()
			_, _ = w.Write([]byte(`{"data_streams":[{"name":"traces-generic.otel-` + ns + `"}]}`))
		case strings.HasPrefix(p, "/_data_stream/") && r.Method == http.MethodDelete:
			mu.Lock()
			deletes = append(deletes, strings.TrimPrefix(p, "/_data_stream/"))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"acknowledged":true}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("VERN_ELASTICSEARCH_URL", srv.URL)
	dir := t.TempDir()
	scoreOutputJSON = filepath.Join(dir, "r.json")
	scoreOutputMD = filepath.Join(dir, "r.md")
	configPath = filepath.Join(dir, "absent.yaml")

	c := &cobra.Command{}
	c.SetContext(ctx)
	var stderr strings.Builder
	c.SetErr(&stderr)
	err := runScore(c, []string{"../web/examples/demo.jsonl.gz"})
	if err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("err = %v, want context canceled", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if runNS == "" || len(deletes) != 1 || deletes[0] != "traces-generic.otel-"+runNS {
		t.Fatalf("deletes = %v (run %q)", deletes, runNS)
	}
	if _, statErr := os.Stat(scoreOutputJSON); statErr == nil {
		t.Error("report written for a cancelled run")
	}
}

const demoSample = "../web/examples/demo.jsonl.gz"

// scoreHarness points vern score at a fakeES in the given modes, sends the
// reports to a temp dir, and returns a command whose stdout and stderr are
// captured.
func scoreHarness(t *testing.T, modes ...fakeMode) (dir string, c *cobra.Command, stdout *bytes.Buffer) {
	t.Helper()
	clearESEnv(t)
	resetScoreFlags(t)
	srv := fakeES(t, modes...)
	t.Setenv("VERN_ELASTICSEARCH_URL", srv.URL)
	dir = t.TempDir()
	scoreOutputJSON = filepath.Join(dir, "r.json")
	scoreOutputMD = filepath.Join(dir, "r.md")
	scoreOutputsFile = ""
	scoreMinScore = optionalFloat{}
	configPath = filepath.Join(dir, "absent.yaml")
	c = &cobra.Command{}
	stdout = &bytes.Buffer{}
	c.SetOut(stdout)
	c.SetErr(io.Discard)
	return dir, c, stdout
}

func minScore(v float64) optionalFloat { return optionalFloat{set: true, v: v} }

func wantExit(t *testing.T, err error, code int, msg string) {
	t.Helper()
	var ee *exitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v (%T), want exitError code %d", err, err, code)
	}
	if ee.code != code {
		t.Fatalf("exit code = %d, want %d (err %v)", ee.code, code, err)
	}
	if exitCode(err) != code {
		t.Fatalf("exitCode = %d, want %d", exitCode(err), code)
	}
	if msg != "" && !strings.Contains(err.Error(), msg) {
		t.Fatalf("err = %q, want it to contain %q", err, msg)
	}
}

func wantReports(t *testing.T) {
	t.Helper()
	for _, p := range []string{scoreOutputJSON, scoreOutputMD} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("report missing: %v", err)
		}
	}
}

func TestScoreCmd_MinScoreBelowExits2(t *testing.T) {
	_, c, _ := scoreHarness(t, rulesAllFail)
	scoreMinScore = minScore(50)
	err := runScore(c, []string{demoSample})
	wantExit(t, err, 2, "below --min-score 50")
	wantReports(t)
}

func TestScoreCmd_MinScoreMetExits0(t *testing.T) {
	_, c, out := scoreHarness(t, rulesAllPass)
	scoreMinScore = minScore(50)
	if err := runScore(c, []string{demoSample}); err != nil {
		t.Fatalf("runScore: %v", err)
	}
	wantReports(t)
	if !strings.Contains(out.String(), "overall:  100.0") {
		t.Errorf("summary:\n%s", out)
	}
}

func TestScoreCmd_NotScoredWithMinScoreExits2(t *testing.T) {
	_, c, _ := scoreHarness(t, rulesNoRows)
	scoreMinScore = minScore(1)
	err := runScore(c, []string{demoSample})
	wantExit(t, err, 2, "not scored: no rule could be evaluated")
	wantReports(t)

	_, c, _ = scoreHarness(t, rulesNoRows)
	if err := runScore(c, []string{demoSample}); err != nil {
		t.Fatalf("not scored without --min-score: err = %v, want nil", err)
	}
}

func TestScoreCmd_OutputsFile(t *testing.T) {
	dir, c, _ := scoreHarness(t, rulesAllFail)
	scoreOutputsFile = filepath.Join(dir, "github_output")
	if err := os.WriteFile(scoreOutputsFile, []byte("existing=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runScore(c, []string{demoSample}); err != nil {
		t.Fatalf("runScore: %v", err)
	}
	raw, err := os.ReadFile(scoreOutputsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if lines[0] != "existing=1" {
		t.Fatalf("existing line lost:\n%s", raw)
	}
	for _, l := range lines {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			t.Fatalf("bad line %q", l)
		}
		got[k] = v
	}
	var rep struct {
		Rules []struct {
			Scored bool `json:"scored"`
		} `json:"rules"`
	}
	b, _ := os.ReadFile(scoreOutputJSON)
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	scored := 0
	for _, r := range rep.Rules {
		if r.Scored {
			scored++
		}
	}
	want := map[string]string{
		"score": "0.0", "category": "Poor", "scored": "true",
		"rules_scored": fmt.Sprint(scored), "rules_total": fmt.Sprint(len(rep.Rules)),
		"report_json": scoreOutputJSON, "report_md": scoreOutputMD,
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || g != v {
			t.Errorf("%s = %q (present %v), want %q\n%s", k, g, ok, v, raw)
		}
	}
	if scored == 0 {
		t.Error("no rule scored; fixture should score")
	}

	// Not requested reports are empty values; not scored leaves score empty.
	dir, c, _ = scoreHarness(t, rulesNoRows)
	scoreOutputsFile = filepath.Join(dir, "out2")
	scoreOutputMD = ""
	if err := runScore(c, []string{demoSample}); err != nil {
		t.Fatalf("runScore: %v", err)
	}
	raw, _ = os.ReadFile(scoreOutputsFile)
	for _, l := range []string{"score=\n", "category=\n", "scored=false\n", "rules_scored=0\n", "report_md=\n", "report_json=" + scoreOutputJSON + "\n"} {
		if !strings.Contains(string(raw), l) {
			t.Errorf("missing %q in\n%s", l, raw)
		}
	}
}

func TestScoreCmd_RuleErrorsExit1(t *testing.T) {
	_, c, _ := scoreHarness(t, rulesError)
	scoreMinScore = minScore(50) // not scored too: exit 1 must win over 2
	err := runScore(c, []string{demoSample})
	wantExit(t, err, 1, "failed with an error")
	wantReports(t)
	if !strings.Contains(err.Error(), "RES-005") {
		t.Errorf("rule IDs not listed: %v", err)
	}
}

func TestScoreCmd_NothingIndexedExits1(t *testing.T) {
	_, c, out := scoreHarness(t, dropAll)
	err := runScore(c, []string{demoSample})
	wantExit(t, err, 1, "no items were indexed")
	wantReports(t)
	if !strings.Contains(out.String(), "dropped:") {
		t.Errorf("summary has no dropped line:\n%s", out)
	}
}

func TestScoreCmd_SummaryOnCmdOut(t *testing.T) {
	_, c, out := scoreHarness(t, rulesAllPass)
	if err := runScore(c, []string{demoSample}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "mappings=embedded") {
		t.Errorf("summary header:\n%s", s)
	}
	if strings.Contains(s, "dropped:") {
		t.Errorf("dropped line without drops:\n%s", s)
	}
}

func TestScoreCmd_TimeoutLimits(t *testing.T) {
	_, c, _ := scoreHarness(t)
	scoreTimeout = 51 * time.Minute
	err := runScore(c, []string{demoSample})
	if err == nil || !strings.Contains(err.Error(), "--timeout") || !strings.Contains(err.Error(), "50m") {
		t.Fatalf("err = %v", err)
	}
	scoreTimeout = 50 * time.Minute
	if err := runScore(c, []string{demoSample}); err != nil {
		t.Fatalf("50m must be allowed: %v", err)
	}
}

func TestScoreCmd_MinScoreRange(t *testing.T) {
	_, c, _ := scoreHarness(t)
	for _, v := range []float64{-1, 101, math.NaN(), math.Inf(1), math.Inf(-1)} {
		scoreMinScore = minScore(v)
		if err := runScore(c, []string{demoSample}); err == nil || !strings.Contains(err.Error(), "--min-score") {
			t.Errorf("min-score %v: err = %v", v, err)
		}
	}
}

func TestScoreCmd_ReadyTimeout(t *testing.T) {
	clearESEnv(t)
	resetScoreFlags(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	t.Setenv("VERN_ELASTICSEARCH_URL", srv.URL)
	configPath = filepath.Join(t.TempDir(), "absent.yaml")
	scoreReadyTimeout = 50 * time.Millisecond
	c := &cobra.Command{}
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	err := runScore(c, []string{demoSample})
	if err == nil || !strings.Contains(err.Error(), "not ready after 50ms") {
		t.Fatalf("err = %v", err)
	}
}

func TestScoreCmd_InvalidURLRedacted(t *testing.T) {
	clearESEnv(t)
	resetScoreFlags(t)
	for _, u := range []string{"http://user:s3cret@[::1", "ftp://user:s3cret@host", "user:s3cret@host"} {
		t.Setenv("VERN_ELASTICSEARCH_URL", u)
		err := runScore(scoreCmd, []string{demoSample})
		if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "VERN_ELASTICSEARCH_URL") {
			t.Errorf("%s: err = %v", u, err)
		}
	}
}

func TestExitCode(t *testing.T) {
	inner := errors.New("boom")
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{inner, 1},
		{&exitError{code: 2, err: inner}, 2},
		{fmt.Errorf("wrapped: %w", &exitError{code: 2, err: inner}), 2},
		{&exitError{code: 1, err: inner}, 1},
	}
	for _, c := range cases {
		if got := exitCode(c.err); got != c.want {
			t.Errorf("exitCode(%v) = %d, want %d", c.err, got, c.want)
		}
	}
	ee := &exitError{code: 2, err: inner}
	if ee.Error() != "boom" || !errors.Is(ee, inner) {
		t.Errorf("Error/Unwrap: %q", ee.Error())
	}
}

// TestScoreCmd_ScoreFormattingAgrees: 6.25 is an exact binary tie. The gate,
// score=, the stdout summary and the Markdown report must all agree on 6.2,
// so --min-score 6.3 fails.
func TestScoreCmd_ScoreFormattingAgrees(t *testing.T) {
	rep := &evaluate.Report{
		Backend: "esql",
		Rules:   []evaluate.RuleResult{{RuleID: "RES-001", Status: evaluate.StatusEvaluated, Scored: true}},
		Score: &score.Result{Scored: true, Overall: 6.25, Category: "Poor", Services: []score.ServiceScore{
			{ServiceName: "svc", Score: 6.25, Category: "Poor"},
		}},
	}
	err := scoreOutcome(rep, minScore(6.3))
	wantExit(t, err, 2, "score 6.2 is below --min-score 6.3")
	if err := scoreOutcome(rep, minScore(6.2)); err != nil {
		t.Fatalf("--min-score 6.2: %v", err)
	}

	p := filepath.Join(t.TempDir(), "out")
	if err := writeOutputsFile(p, rep, "r.json", "r.md"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if !strings.Contains(string(raw), "score=6.2\n") {
		t.Errorf("outputs:\n%s", raw)
	}
	var sum bytes.Buffer
	printSummary(&sum, rep, "r.json", "r.md")
	if !strings.Contains(sum.String(), "overall:  6.2 (Poor)") {
		t.Errorf("summary:\n%s", sum.String())
	}
	md := report.RenderMarkdown(rep)
	for _, want := range []string{"- Score: 6.2 (Poor)", "### svc — 6.2 (Poor)"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown lacks %q:\n%s", want, md)
		}
	}
	if strings.Contains(sum.String()+md+string(raw), "6.3") {
		t.Error("6.3 shown somewhere")
	}
}

func clearOTLPEnv(t *testing.T) {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_METRICS_HEADERS", "OTEL_RESOURCE_ATTRIBUTES"} {
		t.Setenv(k, "")
	}
}

func TestScoreCmd_ExportFailureAfterThresholdExits1(t *testing.T) {
	clearOTLPEnv(t)
	_, c, _ := scoreHarness(t, rulesAllFail)
	var stderr bytes.Buffer
	c.SetErr(&stderr)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer collector.Close()
	scoreOTLPEndpoint = collector.URL
	scoreMinScore = minScore(50) // score is below this: exit 2 would apply
	err := runScore(c, []string{demoSample})
	wantExit(t, err, 1, "HTTP 500")
	wantReports(t)
	// The error is returned (and printed once by Execute), not logged too.
	if strings.Contains(stderr.String(), "HTTP 500") {
		t.Errorf("export error logged as well as returned:\n%s", stderr.String())
	}
	var printed bytes.Buffer
	if code := printError(&printed, err); code != 1 || strings.Count(printed.String(), "HTTP 500") != 1 || !strings.HasPrefix(printed.String(), "error: ") {
		t.Errorf("printError = %d, %q", code, printed.String())
	}
}

func TestPrintError_Prefix(t *testing.T) {
	cases := []struct {
		err    error
		code   int
		prefix string
	}{
		{errors.New("boom"), 1, "error: boom\n"},
		{&exitError{code: 1, err: errors.New("boom")}, 1, "error: boom\n"},
		{&exitError{code: 2, err: errors.New("score 6.2 is below --min-score 70")}, 2, "gate: score 6.2 is below --min-score 70\n"},
	}
	for _, c := range cases {
		var b bytes.Buffer
		if got := printError(&b, c.err); got != c.code || b.String() != c.prefix {
			t.Errorf("printError(%v) = %d, %q; want %d, %q", c.err, got, b.String(), c.code, c.prefix)
		}
	}
}

func TestScoreCmd_ExportSuccess(t *testing.T) {
	clearOTLPEnv(t)
	_, c, _ := scoreHarness(t, rulesAllPass)
	var path, key string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path, r.Header.Get("x-key")
		w.WriteHeader(200)
	}))
	defer collector.Close()
	scoreOTLPEndpoint = collector.URL + "/"
	scoreOTLPHeaders = []string{"x-key=v1"}
	if err := runScore(c, []string{demoSample}); err != nil {
		t.Fatal(err)
	}
	if path != "/v1/metrics" || key != "v1" {
		t.Fatalf("path=%q key=%q", path, key)
	}
}

func TestScoreCmd_ExportHasOwnDeadline(t *testing.T) {
	clearOTLPEnv(t)
	_, c, _ := scoreHarness(t, rulesAllPass)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(900 * time.Millisecond) // longer than the whole --timeout
		w.WriteHeader(200)
	}))
	defer collector.Close()
	scoreOTLPEndpoint = collector.URL
	scoreTimeout = 700 * time.Millisecond
	if err := runScore(c, []string{demoSample}); err != nil {
		t.Fatalf("export should have its own deadline: %v", err)
	}
}

// TestScoreCmd_QueryErrorDetail: stderr can be a public CI log, so a
// failed sensitive-rule query logs only its fixed reason unless debugging
// is switched on (--debug or VERN_DEBUG).
func TestScoreCmd_QueryErrorDetail(t *testing.T) {
	const withheld = "RES-010: query failed (HTTP 500) (detail withheld"
	const detail = `RES-010: query failed (HTTP 500): HTTP 500: {"error":"kaboom"}`
	cases := map[string]struct {
		flag     bool
		env      string
		want     string
		wantNot  string
		wantFail string
	}{
		"default withholds":    {want: withheld, wantNot: "kaboom"},
		"--debug shows detail": {flag: true, want: detail},
		"VERN_DEBUG shows it":  {env: "true", want: detail},
		"VERN_DEBUG=0 is off":  {env: "0", want: withheld, wantNot: "kaboom"},
		"invalid VERN_DEBUG":   {env: "loud", wantFail: "VERN_DEBUG"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, c, _ := scoreHarness(t, rulesError)
			var stderr bytes.Buffer
			c.SetErr(&stderr)
			scoreDebug = tc.flag
			t.Setenv("VERN_DEBUG", tc.env)
			err := runScore(c, []string{demoSample})
			if tc.wantFail != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantFail) {
					t.Fatalf("err = %v, want one naming %s", err, tc.wantFail)
				}
				return
			}
			wantExit(t, err, 1, "failed with an error")
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr lacks %q:\n%s", tc.want, stderr.String())
			}
			if tc.wantNot != "" && strings.Contains(stderr.String(), tc.wantNot) {
				t.Errorf("stderr contains %q:\n%s", tc.wantNot, stderr.String())
			}
		})
	}
}
