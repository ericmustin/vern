package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/otlpin"
	"github.com/spf13/pflag"
)

// parseServeFlags registers the serve flags on a fresh FlagSet, parses args
// and resolves env defaults from env.
func parseServeFlags(t *testing.T, args []string, env map[string]string) (serveFlags, error) {
	t.Helper()
	var o serveFlags
	f := pflag.NewFlagSet("serve", pflag.ContinueOnError)
	o.register(f)
	if err := f.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	err := o.resolve(f, func(k string) string { return env[k] })
	return o, err
}

func TestServeFlags_Defaults(t *testing.T) {
	o, err := parseServeFlags(t, nil, nil)
	if err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	want := serveFlags{listen: ":8080", maxConcurrent: 1, runTimeout: 120 * time.Second, sweepAge: 10 * time.Minute, sweepEvery: 5 * time.Minute, readyTimeout: 90 * time.Second}
	if o != want {
		t.Fatalf("defaults = %+v, want %+v", o, want)
	}
}

func TestServeFlags_Validation(t *testing.T) {
	_, err := parseServeFlags(t, []string{"--run-timeout=11m", "--sweep-age=5m", "--max-concurrent=0"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, name := range []string{"--run-timeout", "--sweep-age", "--max-concurrent"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name %s: %v", name, err)
		}
	}
	// Each problem on its own line: one joined error, not the first failure.
	if n := strings.Count(err.Error(), "\n"); n < 2 {
		t.Errorf("want three joined errors, got %q", err)
	}
}

func TestServeFlags_EnvDefaults(t *testing.T) {
	env := map[string]string{
		"VERN_LISTEN_ADDR":         "127.0.0.1:9999",
		"VERN_MAX_CONCURRENT_RUNS": "5",
		"VERN_RUN_TIMEOUT":         "3m",
		"VERN_SWEEP_AGE":           "20m",
		"VERN_SWEEP_EVERY":         "1m",
		"VERN_SETUP_TEMPLATE":      "true",
	}
	o, err := parseServeFlags(t, nil, env)
	if err != nil {
		t.Fatal(err)
	}
	want := serveFlags{listen: "127.0.0.1:9999", maxConcurrent: 5, runTimeout: 3 * time.Minute, sweepAge: 20 * time.Minute, sweepEvery: time.Minute, setupTemplate: true, readyTimeout: 90 * time.Second}
	if o != want {
		t.Fatalf("got %+v, want %+v", o, want)
	}

	// A flag set on the command line beats its env var.
	o, err = parseServeFlags(t, []string{"--max-concurrent=1"}, env)
	if err != nil {
		t.Fatal(err)
	}
	if o.maxConcurrent != 1 {
		t.Fatalf("flag did not override env: max-concurrent = %d", o.maxConcurrent)
	}

	// Unparseable env values are reported together, naming the variable.
	_, err = parseServeFlags(t, nil, map[string]string{"VERN_MAX_CONCURRENT_RUNS": "many", "VERN_RUN_TIMEOUT": "soon"})
	if err == nil || !strings.Contains(err.Error(), "VERN_MAX_CONCURRENT_RUNS") || !strings.Contains(err.Error(), "VERN_RUN_TIMEOUT") {
		t.Fatalf("err = %v", err)
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) logf(format string, a ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.WriteString(strings.TrimSuffix(fmt.Sprintf(format, a...), "\n") + "\n")
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// closedPort returns a loopback address nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// startServe runs serve in the background and returns its bound address and
// a channel that receives serve's result.
func startServe(t *testing.T, ctx context.Context, o serveFlags, creds esCredentials, logs *syncBuffer, h serveHooks) (string, <-chan error) {
	t.Helper()
	addrCh := make(chan string, 1)
	h.listening = func(addr string) { addrCh <- addr }
	done := make(chan error, 1)
	go func() { done <- serve(ctx, o, creds, config.Default(), logs.logf, h) }()
	select {
	case addr := <-addrCh:
		return addr, done
	case err := <-done:
		t.Fatalf("serve returned before listening: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not start listening")
	}
	return "", nil
}

func testServeFlags() serveFlags {
	return serveFlags{listen: "127.0.0.1:0", maxConcurrent: 2, runTimeout: 5 * time.Second, sweepAge: 10 * time.Minute, sweepEvery: time.Hour, readyTimeout: 200 * time.Millisecond}
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func TestServe_StartsWithESDown(t *testing.T) {
	old := serveReadyBackoffStart
	serveReadyBackoffStart = 10 * time.Millisecond
	defer func() { serveReadyBackoffStart = old }()

	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	esAddr := closedPort(t)
	creds := esCredentials{URL: "http://vern:pw@" + esAddr + "/?q=q-secret"}
	addr, done := startServe(t, ctx, testServeFlags(), creds, logs, serveHooks{})

	if got := getStatus(t, "http://"+addr+"/healthz"); got != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", got)
	}
	if got := getStatus(t, "http://"+addr+"/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503", got)
	}
	// Let the background Ready loop fail and log a few times.
	deadline := time.Now().Add(5 * time.Second)
	for strings.Count(logs.String(), "retrying in") < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return within 2s of cancellation")
	}

	out := logs.String()
	if n := strings.Count(out, "retrying in"); n < 3 {
		t.Fatalf("want at least 3 ready retries logged, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "listening on "+addr+" (elasticsearch http://"+esAddr+"/)") {
		t.Errorf("no startup line with the listen address and ES host:\n%s", out)
	}
	// Startup line, ready retries, janitor and /readyz: no userinfo or query anywhere.
	for _, secret := range []string{"vern:", "pw", "q-secret"} {
		if strings.Contains(out, secret) {
			t.Errorf("logs contain URL userinfo or query %q:\n%s", secret, out)
		}
	}
}

// emptyExecutor answers every rule query with no rows.
type emptyExecutor struct{}

func (emptyExecutor) Execute(context.Context, string) ([]evaluate.Row, error) { return nil, nil }

// readyEngine is an engine.Engine that is always ready and holds no data.
type readyEngine struct{}

func (readyEngine) Ready(context.Context) error { return nil }
func (readyEngine) Load(context.Context, engine.RunID, *otlpin.Sample) (*engine.LoadStats, error) {
	return &engine.LoadStats{}, nil
}
func (readyEngine) Session(engine.RunID) (evaluate.Executor, config.IndexPatterns) {
	return emptyExecutor{}, config.IndexPatterns{}
}
func (readyEngine) Fields(context.Context, engine.RunID) (mappings.Fields, error) {
	return mappings.Fields{}, nil
}
func (readyEngine) Purge(context.Context, engine.RunID) error         { return nil }
func (readyEngine) Sweep(context.Context, time.Duration) (int, error) { return 0, nil }

// slowEngine's Load takes 300ms and its Purge 50ms; it counts purges.
type slowEngine struct {
	readyEngine
	started chan struct{}
	purges  atomic.Int32
}

func (e *slowEngine) Load(ctx context.Context, _ engine.RunID, _ *otlpin.Sample) (*engine.LoadStats, error) {
	close(e.started)
	select {
	case <-time.After(300 * time.Millisecond):
		return &engine.LoadStats{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *slowEngine) Purge(context.Context, engine.RunID) error {
	time.Sleep(50 * time.Millisecond)
	e.purges.Add(1)
	return nil
}

func TestServe_ShutdownWaitsForInFlight(t *testing.T) {
	sample, err := os.ReadFile("../web/examples/demo.jsonl.gz")
	if err != nil {
		t.Fatal(err)
	}
	// The real samplescore.Score, so the run purges as in production.
	eng := &slowEngine{started: make(chan struct{})}

	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, done := startServe(t, ctx, testServeFlags(), esCredentials{URL: "http://" + closedPort(t)}, logs, serveHooks{engine: eng})

	type result struct {
		status int
		err    error
	}
	resCh := make(chan result, 1)
	go func() {
		resp, err := http.Post("http://"+addr+"/api/v1/runs", "application/gzip", bytes.NewReader(sample))
		if err != nil {
			resCh <- result{err: err}
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		resCh <- result{status: resp.StatusCode}
	}()

	select {
	case <-eng.started:
	case <-time.After(5 * time.Second):
		t.Fatalf("run never started; logs:\n%s", logs.String())
	}
	cancel() // shutdown begins while the run is in flight

	var serveErr error
	select {
	case serveErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the in-flight run finished")
	}
	// Read before anything else can happen: the purge must be done by the
	// time serve returns.
	purges := eng.purges.Load()
	if serveErr != nil {
		t.Fatalf("serve returned %v, want nil", serveErr)
	}
	if purges != 1 {
		t.Fatalf("purges when serve returned = %d, want 1", purges)
	}
	select {
	case r := <-resCh:
		if r.err != nil || r.status != http.StatusOK {
			t.Fatalf("in-flight run: status %d, err %v; logs:\n%s", r.status, r.err, logs.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight run got no response")
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
		}
	}))
	defer ok.Close()
	if err := healthcheck(context.Background(), ok.URL+"/healthz"); err != nil {
		t.Errorf("healthy server: %v", err)
	}
	if err := healthcheck(context.Background(), ok.URL+"/missing"); err == nil {
		t.Error("404: want an error")
	}
	if err := healthcheck(context.Background(), "http://"+closedPort(t)+"/healthz"); err == nil {
		t.Error("closed port: want an error")
	}
}

// flakyEngine fails Ready until it has been called failures+1 times.
type flakyEngine struct {
	readyEngine
	mu       sync.Mutex
	calls    int
	failures int
}

func (f *flakyEngine) Ready(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failures {
		return errors.New("connection refused")
	}
	return nil
}

func TestWaitReady_RetriesUntilReady(t *testing.T) {
	eng := &flakyEngine{failures: 1}
	logs := &syncBuffer{}
	done := make(chan struct{})
	go func() { waitReady(context.Background(), eng, logs.logf); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitReady did not return once the engine became ready")
	}
	if eng.calls != 2 || !strings.Contains(logs.String(), "elasticsearch is ready") {
		t.Fatalf("calls = %d, logs:\n%s", eng.calls, logs.String())
	}
}

func TestWaitReady_StopsOnCancel(t *testing.T) {
	eng := &flakyEngine{failures: 1 << 30}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { waitReady(ctx, eng, func(string, ...any) {}); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitReady did not stop on cancel")
	}
}
