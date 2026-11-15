package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/otlpin"
	"github.com/ericmustin/vern/internal/samplescore"
)

// demoSample is the example upload the UI offers.
const demoSample = "../../web/examples/demo.jsonl.gz"

// fakeEngine is a concurrency-safe engine.Engine for handler tests.
type fakeEngine struct {
	mu          sync.Mutex
	readyErr    error
	readyCalls  int
	purges      int
	sweeps      []time.Duration
	blockLoads  int           // this many Load calls block until their ctx is done
	loadEntered chan struct{} // receives once per blocking Load, if non-nil
	rows        []evaluate.Row
}

func (f *fakeEngine) setReadyErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readyErr = err
}

func (f *fakeEngine) Ready(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readyCalls++
	return f.readyErr
}

func (f *fakeEngine) Load(ctx context.Context, id engine.RunID, s *otlpin.Sample) (*engine.LoadStats, error) {
	f.mu.Lock()
	block := f.blockLoads > 0
	if block {
		f.blockLoads--
	}
	entered := f.loadEntered
	f.mu.Unlock()
	if block {
		if entered != nil {
			entered <- struct{}{}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &engine.LoadStats{}, nil
}

func (f *fakeEngine) Session(id engine.RunID) (evaluate.Executor, config.IndexPatterns) {
	f.mu.Lock()
	rows := f.rows
	f.mu.Unlock()
	if rows == nil {
		rows = []evaluate.Row{{ServiceName: "svc", Passed: true, Extent: 0}}
	}
	return execFunc(func(ctx context.Context, q string) ([]evaluate.Row, error) {
		return rows, nil
	}), config.IndexPatterns{Traces: "t", Logs: "l", Metrics: "m"}
}

func (f *fakeEngine) Fields(ctx context.Context, id engine.RunID) (mappings.Fields, error) {
	return mappings.Fields{}, nil
}

func (f *fakeEngine) Purge(ctx context.Context, id engine.RunID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purges++
	return nil
}

func (f *fakeEngine) Sweep(ctx context.Context, olderThan time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps = append(f.sweeps, olderThan)
	return 0, nil
}

func (f *fakeEngine) purgeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.purges
}

func (f *fakeEngine) sweepCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sweeps)
}

type execFunc func(ctx context.Context, q string) ([]evaluate.Row, error)

func (e execFunc) Execute(ctx context.Context, q string) ([]evaluate.Row, error) { return e(ctx, q) }

// fakeScore returns a ScoreFunc that, when block is non-nil, signals entered
// and waits for block to close (or ctx to end), then returns rep or err.
func fakeScore(entered chan<- struct{}, block <-chan struct{}, rep *evaluate.Report, err error) ScoreFunc {
	return func(ctx context.Context, eng engine.Engine, s *otlpin.Sample, opts samplescore.Options) (*evaluate.Report, error) {
		if entered != nil {
			entered <- struct{}{}
		}
		if block != nil {
			select {
			case <-block:
			case <-ctx.Done():
				return nil, fmt.Errorf("evaluate: %w", context.Cause(ctx))
			}
		}
		if err != nil {
			return nil, err
		}
		if rep == nil {
			return &evaluate.Report{Sample: &evaluate.SampleInfo{Files: opts.Files, RunID: "run-test"}}, nil
		}
		return rep, nil
	}
}

// logCapture collects Logf output.
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func newTestServer(t *testing.T, cfg Config) *server {
	t.Helper()
	if cfg.Logf == nil {
		cfg.Logf = t.Logf
	}
	return newServer(cfg)
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func readDemo(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(demoSample)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func postRaw(body []byte) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/gzip")
	return req
}

func errorBody(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body %q is not JSON: %v", rec.Body.String(), err)
	}
	if e.Error == "" {
		t.Fatalf("error body %q has no error message", rec.Body.String())
	}
	return e.Error
}

// waitFor polls cond for up to 5s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHealthReady(t *testing.T) {
	eng := &fakeEngine{readyErr: errors.New("dial tcp 10.9.8.7:9200: connection refused")}
	logs := &logCapture{}
	s := newTestServer(t, Config{Engine: eng, Logf: logs.logf})
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	get := func(path string) *httptest.ResponseRecorder {
		return serve(s, httptest.NewRequest(http.MethodGet, path, nil))
	}

	if rec := get("/healthz"); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("/healthz = %d %q, want 200 ok", rec.Code, rec.Body.String())
	}
	rec := get("/readyz")
	if rec.Code != http.StatusServiceUnavailable || strings.TrimSpace(rec.Body.String()) != "not ready" {
		t.Fatalf("/readyz = %d %q, want 503 not ready", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "10.9.8.7") {
		t.Fatalf("/readyz body leaks the engine error: %q", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Fatalf("engine error not logged; logs:\n%s", logs)
	}

	// The engine recovers, but the failure stays cached for the TTL.
	eng.setReadyErr(nil)
	if rec := get("/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz within the cache TTL = %d, want 503 (cached)", rec.Code)
	}
	if rec := get("/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("/healthz = %d while not ready, want 200", rec.Code)
	}
	now = now.Add(readyCacheTTL + time.Second)
	if rec := get("/readyz"); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "ready" {
		t.Fatalf("/readyz after TTL = %d %q, want 200 ready", rec.Code, rec.Body.String())
	}
	eng.mu.Lock()
	calls := eng.readyCalls
	eng.mu.Unlock()
	if calls != 2 {
		t.Fatalf("Ready called %d times, want 2 (one per cache miss)", calls)
	}
}

func checkSecurityHeaders(t *testing.T, what string, rec *httptest.ResponseRecorder) {
	t.Helper()
	want := map[string]string{
		"Content-Security-Policy": "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Errorf("%s: header %s = %q, want %q", what, k, got, v)
		}
	}
}

func TestSecurityHeadersEverywhere(t *testing.T) {
	static := fstest.MapFS{
		"index.html":             {Data: []byte("<!doctype html><title>vern</title>")},
		"examples/demo.jsonl.gz": {Data: []byte{0x1f, 0x8b}},
	}
	entered, block := make(chan struct{}), make(chan struct{})
	s := newTestServer(t, Config{
		Engine:        &fakeEngine{},
		Static:        static,
		MaxConcurrent: 1,
		Score:         fakeScore(entered, block, nil, nil),
	})

	rec := serve(s, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>vern</title>") {
		t.Fatalf("GET / = %d %q, want index.html", rec.Code, rec.Body.String())
	}
	checkSecurityHeaders(t, "GET /", rec)

	rec = serve(s, httptest.NewRequest(http.MethodGet, "/examples/demo.jsonl.gz", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("example = %d %q, want 200 application/gzip", rec.Code, rec.Header().Get("Content-Type"))
	}
	checkSecurityHeaders(t, "GET example", rec)

	rec = serve(s, httptest.NewRequest(http.MethodGet, "/api/v1/rules", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/rules = %d", rec.Code)
	}
	checkSecurityHeaders(t, "GET /api/v1/rules", rec)

	for _, p := range []string{"/nope", "/examples/", "/examples", "/index.html/x"} {
		rec = serve(s, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", p, rec.Code)
		}
		checkSecurityHeaders(t, "404 "+p, rec)
	}

	// Occupy the only slot, then get a 429.
	demo := readDemo(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		serve(s, postRaw(demo))
	}()
	<-entered
	rec = serve(s, postRaw(demo))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second run = %d, want 429", rec.Code)
	}
	checkSecurityHeaders(t, "429", rec)
	close(block)
	<-done
}

func TestStaticNilServesNoUI(t *testing.T) {
	s := newTestServer(t, Config{Engine: &fakeEngine{}})
	if rec := serve(s, httptest.NewRequest(http.MethodGet, "/", nil)); rec.Code != http.StatusNotFound {
		t.Fatalf("GET / without Static = %d, want 404", rec.Code)
	}
}

// TestStaticContentTypes pins the types the UI needs: with nosniff, a wrong
// type for a script or stylesheet (from the host's mime.types) breaks the page.
func TestStaticContentTypes(t *testing.T) {
	want := map[string]string{
		"/":                       "text/html; charset=utf-8",
		"/index.html":             "text/html; charset=utf-8",
		"/app.js":                 "text/javascript; charset=utf-8",
		"/app.css":                "text/css; charset=utf-8",
		"/examples/demo.jsonl.gz": "application/gzip",
		"/icon.svg":               "image/svg+xml",
		"/data.json":              "application/json",
		"/notes.txt":              "application/octet-stream",
		"/Upper.JS":               "text/javascript; charset=utf-8",
	}
	static := fstest.MapFS{}
	for p := range want {
		if p != "/" {
			static[strings.TrimPrefix(p, "/")] = &fstest.MapFile{Data: []byte("x")}
		}
	}
	s := newTestServer(t, Config{Engine: &fakeEngine{}, Static: static})
	for p, ctype := range want {
		rec := serve(s, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", p, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); got != ctype {
			t.Errorf("GET %s Content-Type = %q, want %q", p, got, ctype)
		}
	}
}

func TestPanicRecovered(t *testing.T) {
	s := newTestServer(t, Config{
		Engine:        &fakeEngine{},
		MaxConcurrent: 1,
		Score: func(context.Context, engine.Engine, *otlpin.Sample, samplescore.Options) (*evaluate.Report, error) {
			panic("kaboom")
		},
	})
	demo := readDemo(t)
	rec := serve(s, postRaw(demo))
	if rec.Code != http.StatusInternalServerError || errorBody(t, rec) != "internal error" {
		t.Fatalf("panicking run = %d %q, want 500 internal error", rec.Code, rec.Body.String())
	}
	checkSecurityHeaders(t, "panic 500", rec)
	// The slot was released despite the panic.
	if len(s.sem) != 0 {
		t.Fatalf("semaphore holds %d slot(s) after a panic", len(s.sem))
	}
}

func TestNoBodyInLogs(t *testing.T) {
	logs := &logCapture{}
	eng := &fakeEngine{}
	s := newTestServer(t, Config{Engine: eng, Logf: logs.logf})

	send := func(body string, contentType string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs?token=QS-MARKER", strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Authorization", "Bearer HDR-MARKER")
		req.Header.Set("X-Custom", "HDR-MARKER")
		return serve(s, req)
	}
	// Unrecognized JSON (400), malformed JSON (400) and a valid run (200).
	if rec := send(`{"note":"SECRET-MARKER"}`, "application/json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unrecognized body = %d, want 400", rec.Code)
	}
	if rec := send(`SECRET-MARKER`, "text/plain"); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", rec.Code)
	}
	span := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"SECRET-MARKER"}}]},"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"SECRET-MARKER","startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000001000000000"}]}]}]}`
	if rec := send(span, "application/json"); rec.Code != http.StatusOK {
		t.Fatalf("valid run = %d %s, want 200", rec.Code, rec.Body.String())
	}
	serve(s, httptest.NewRequest(http.MethodGet, "/readyz?token=QS-MARKER", nil))

	out := logs.String()
	for _, m := range []string{"SECRET-MARKER", "QS-MARKER", "HDR-MARKER", "token="} {
		if strings.Contains(out, m) {
			t.Errorf("logs contain %q:\n%s", m, out)
		}
	}
	if !strings.Contains(out, `POST "/api/v1/runs" 200`) || !strings.Contains(out, "run_id=") {
		t.Errorf("logs lack a request line with status and run_id:\n%s", out)
	}
}

// rejectedBatchErr mimics how samplescore wraps an engine rejection that
// quotes the backend's response body, which can echo uploaded values.
func rejectedBatchErr(cause error) error {
	return fmt.Errorf("load sample: elasticsearch rejected logs batch: status 400: %q: %w",
		`{"error":"failed to parse field [attributes.user] value [SECRET-MARKER]"}`, cause)
}

// TestNoUploadValuesInErrorLogs covers the 500, 504 and panic paths: their
// errors can carry upload values, so only a bounded classification is logged.
func TestNoUploadValuesInErrorLogs(t *testing.T) {
	cases := map[string]struct {
		score      ScoreFunc
		timeout    time.Duration
		wantStatus int
		wantLog    []string
	}{
		"rejected batch": {
			score:      fakeScore(nil, nil, nil, rejectedBatchErr(errors.New("SECRET-MARKER"))),
			wantStatus: http.StatusInternalServerError,
			wantLog:    []string{`stage="load sample"`, "http_status=400"},
		},
		"joined purge failure": {
			score:      fakeScore(nil, nil, nil, errors.Join(rejectedBatchErr(errors.New("x")), errors.New("purge run r1: SECRET-MARKER"))),
			wantStatus: http.StatusInternalServerError,
			wantLog:    []string{`stage="load sample"`, "purge_failed=true"},
		},
		"unknown stage": {
			score:      fakeScore(nil, nil, nil, errors.New("SECRET-MARKER: status 503")),
			wantStatus: http.StatusInternalServerError,
			wantLog:    []string{`stage="unknown"`, "http_status=503"},
		},
		"timeout": {
			score: func(ctx context.Context, eng engine.Engine, s *otlpin.Sample, opts samplescore.Options) (*evaluate.Report, error) {
				<-ctx.Done()
				return nil, fmt.Errorf("evaluate: SECRET-MARKER: %w", context.Cause(ctx))
			},
			timeout:    20 * time.Millisecond,
			wantStatus: http.StatusGatewayTimeout,
			wantLog:    []string{`stage="evaluate"`},
		},
		"panic": {
			score: func(context.Context, engine.Engine, *otlpin.Sample, samplescore.Options) (*evaluate.Report, error) {
				panic(errors.New("SECRET-MARKER"))
			},
			wantStatus: http.StatusInternalServerError,
			wantLog:    []string{"panic", "*errors.errorString"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			logs := &logCapture{}
			s := newTestServer(t, Config{Engine: &fakeEngine{}, Score: tc.score, RunTimeout: tc.timeout, Logf: logs.logf})
			rec := serve(s, postRaw(readDemo(t)))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			out := logs.String()
			if strings.Contains(out, "SECRET-MARKER") || strings.Contains(rec.Body.String(), "SECRET-MARKER") {
				t.Fatalf("upload value leaked; logs:\n%s\nbody: %s", out, rec.Body.String())
			}
			for _, w := range tc.wantLog {
				if !strings.Contains(out, w) {
					t.Errorf("logs lack %q:\n%s", w, out)
				}
			}
		})
	}
}
