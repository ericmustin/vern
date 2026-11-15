package elastic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ericmustin/vern/internal/engine"
)

func init() { readyPollInterval = 5 * time.Millisecond }

type rec struct {
	mu    sync.Mutex
	calls []string // "METHOD path"
	bodys map[string]string
}

func (r *rec) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, _ := io.ReadAll(req.Body)
	k := req.Method + " " + req.URL.Path
	r.calls = append(r.calls, k)
	if r.bodys == nil {
		r.bodys = map[string]string{}
	}
	r.bodys[k] = string(b)
}

func (r *rec) has(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func rootOK(w http.ResponseWriter, v string) {
	_, _ = w.Write([]byte(`{"version":{"number":"` + v + `"}}`))
}

func TestReady_RetriesUntilUp(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			if atomic.AddInt32(&n, 1) <= 2 {
				w.WriteHeader(503)
				return
			}
			rootOK(w, "9.5.5")
		case "/_license":
			_, _ = w.Write([]byte(`{"license":{"type":"basic"}}`))
		default:
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	e := New(Config{URL: srv.URL, ReadyTimeout: 5 * time.Second})
	if err := e.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&n) != 3 {
		t.Errorf("polls = %d, want 3", n)
	}
}

func TestReady_TimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	e := New(Config{URL: srv.URL, ReadyTimeout: 50 * time.Millisecond})
	err := e.Ready(context.Background())
	if err == nil || !strings.Contains(err.Error(), srv.URL) || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("err = %v", err)
	}
}

func TestReady_RejectsOldVersion(t *testing.T) {
	for _, v := range []string{"9.4.2", "8.19.0", "9.4.9-SNAPSHOT"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rootOK(w, v) }))
		e := New(Config{URL: srv.URL, ReadyTimeout: time.Second})
		err := e.Ready(context.Background())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "found "+v) {
			t.Errorf("%s: err = %v", v, err)
		}
	}
}

func TestReady_VersionNumericComparison(t *testing.T) {
	for _, v := range []string{"9.10.0", "9.5.5-SNAPSHOT", "10.0.0", "9.5.0"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/" {
				rootOK(w, v)
				return
			}
			w.WriteHeader(200)
		}))
		e := New(Config{URL: srv.URL, ReadyTimeout: time.Second})
		err := e.Ready(context.Background())
		srv.Close()
		if err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
}

func templateServer(tmplStatus int, rc *rec, putStatus int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		switch {
		case r.URL.Path == "/":
			rootOK(w, "9.5.5")
		case r.URL.Path == "/_license":
			_, _ = w.Write([]byte(`{"license":{"type":"basic"}}`))
		case r.URL.Path == "/_component_template/metrics-otel@custom" && r.Method == "GET":
			w.WriteHeader(tmplStatus)
		case r.URL.Path == "/_component_template/metrics-otel@custom" && r.Method == "PUT":
			w.WriteHeader(putStatus)
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestReady_CreatesTemplateOnlyWhenAbsent(t *testing.T) {
	rc := &rec{}
	srv := templateServer(404, rc, 200)
	var logs []string
	e := New(Config{URL: srv.URL, ReadyTimeout: time.Second, EnsureTemplate: true,
		Logf: func(f string, a ...any) { logs = append(logs, f) }})
	if err := e.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	body := rc.bodys["PUT /_component_template/metrics-otel@custom"]
	if !strings.Contains(body, `"index.look_back_time":"7d"`) || !strings.Contains(body, `"managed_by":"vern"`) {
		t.Errorf("PUT body = %q", body)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "created component template metrics-otel@custom") {
		t.Errorf("creation not logged: %v", logs)
	}

	rc2 := &rec{}
	srv2 := templateServer(200, rc2, 200)
	defer srv2.Close()
	e2 := New(Config{URL: srv2.URL, ReadyTimeout: time.Second, EnsureTemplate: true})
	if err := e2.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rc2.has("PUT ") {
		t.Errorf("unexpected PUT: %v", rc2.calls)
	}
}

func TestReady_TemplateAbsentWithoutEnsureOnlyWarns(t *testing.T) {
	rc := &rec{}
	srv := templateServer(404, rc, 200)
	defer srv.Close()
	var logs []string
	e := New(Config{URL: srv.URL, ReadyTimeout: time.Second,
		Logf: func(f string, a ...any) { logs = append(logs, f) }})
	if err := e.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rc.has("PUT ") {
		t.Errorf("unexpected PUT: %v", rc.calls)
	}
	all := strings.Join(logs, "\n")
	if !strings.Contains(all, "warning: component template metrics-otel@custom is absent") || !strings.Contains(all, "--setup-template") {
		t.Errorf("no absent-template warning: %v", logs)
	}
}

func TestReady_TemplateForbiddenWarnsAndContinues(t *testing.T) {
	for name, tc := range map[string]struct{ get, put int }{
		"put 403": {404, 403},
		"get 403": {403, 200},
	} {
		t.Run(name, func(t *testing.T) {
			rc := &rec{}
			srv := templateServer(tc.get, rc, tc.put)
			defer srv.Close()
			var logs []string
			e := New(Config{URL: srv.URL, ReadyTimeout: time.Second, EnsureTemplate: true,
				Logf: func(f string, a ...any) { logs = append(logs, f) }})
			if err := e.Ready(context.Background()); err != nil {
				t.Fatal(err)
			}
			warned := false
			for _, l := range logs {
				if strings.Contains(strings.ToLower(l), "warning") {
					warned = true
				}
			}
			if !warned {
				t.Errorf("no warning logged: %v", logs)
			}
			if tc.get == 403 && rc.has("PUT ") {
				t.Error("PUT attempted after GET 403")
			}
		})
	}
}

func TestReady_TemplateUnexpectedStatusFails(t *testing.T) {
	rc := &rec{}
	srv := templateServer(500, rc, 200)
	defer srv.Close()
	e := New(Config{URL: srv.URL, ReadyTimeout: time.Second})
	if err := e.Ready(context.Background()); err == nil {
		t.Fatal("want error")
	}
}

func TestSession_ScopedPatterns(t *testing.T) {
	e := New(Config{URL: "http://x", APIKey: "k", Username: "u", Password: "p"})
	ex, pats := e.Session(engine.RunID("vernabc12345678"))
	if pats.Traces != "traces-*.otel-vernabc12345678" || pats.Logs != "logs-*.otel-vernabc12345678" || pats.Metrics != "metrics-*.otel-vernabc12345678" {
		t.Errorf("patterns = %+v", pats)
	}
	if ex == nil {
		t.Fatal("nil executor")
	}
}

func newRunID(t *testing.T, at time.Time) engine.RunID {
	id := engine.NewRunID(at)
	if _, ok := engine.ParseRunID(string(id)); !ok {
		t.Fatal("bad id")
	}
	return id
}

func TestPurge_DeletesExactNames(t *testing.T) {
	id := newRunID(t, time.Now())
	rc := &rec{}
	var delPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		switch {
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/_data_stream/"):
			if r.URL.Query().Get("expand_wildcards") != "all" {
				t.Errorf("expand_wildcards missing: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"data_streams":[{"name":"traces-generic.otel-` + string(id) + `"},{"name":"logs-generic.otel-` + string(id) + `"}]}`))
		case r.Method == "DELETE":
			delPath = r.URL.Path
		}
	}))
	defer srv.Close()
	e := New(Config{URL: srv.URL})
	if err := e.Purge(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	want := "/_data_stream/traces-generic.otel-" + string(id) + ",logs-generic.otel-" + string(id)
	if delPath != want || strings.Contains(delPath, "*") {
		t.Errorf("delete path = %q, want %q", delPath, want)
	}
}

func TestPurge_NoStreamsIsNoop(t *testing.T) {
	rc := &rec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		_, _ = w.Write([]byte(`{"data_streams":[]}`))
	}))
	defer srv.Close()
	e := New(Config{URL: srv.URL})
	if err := e.Purge(context.Background(), newRunID(t, time.Now())); err != nil {
		t.Fatal(err)
	}
	if rc.has("DELETE") {
		t.Errorf("DELETE sent: %v", rc.calls)
	}
}

func TestPurge_IgnoresForeignNames(t *testing.T) {
	id := newRunID(t, time.Now())
	var deletes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deletes = append(deletes, r.URL.Path)
			return
		}
		_, _ = w.Write([]byte(`{"data_streams":[{"name":"traces-generic.otel-default"}]}`))
	}))
	defer srv.Close()
	e := New(Config{URL: srv.URL})
	if err := e.Purge(context.Background(), id); err != nil || len(deletes) != 0 {
		t.Fatalf("err=%v deletes=%v", err, deletes)
	}
}

func TestSweep_OnlyOldVernRuns(t *testing.T) {
	old := newRunID(t, time.Now().Add(-48*time.Hour))
	fresh := newRunID(t, time.Now())
	var deletes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deletes = append(deletes, r.URL.Path)
			return
		}
		_, _ = w.Write([]byte(`{"data_streams":[
			{"name":"traces-generic.otel-` + string(old) + `"},
			{"name":"logs-generic.otel-` + string(old) + `"},
			{"name":"traces-generic.otel-` + string(fresh) + `"},
			{"name":"traces-generic.otel-default"},
			{"name":"traces-generic.otel-vernshort"}]}`))
	}))
	defer srv.Close()
	e := New(Config{URL: srv.URL})
	n, err := e.Sweep(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("n = %d, want 1", n)
	}
	want := "/_data_stream/traces-generic.otel-" + string(old) + ",logs-generic.otel-" + string(old)
	if len(deletes) != 1 || deletes[0] != want {
		t.Errorf("deletes = %v, want [%s]", deletes, want)
	}
}

func TestAuthHeaders(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"data_streams":[]}`))
	}))
	defer srv.Close()
	_ = New(Config{URL: srv.URL, APIKey: "abc"}).Purge(context.Background(), newRunID(t, time.Now()))
	_ = New(Config{URL: srv.URL, Username: "u", Password: "p"}).Purge(context.Background(), newRunID(t, time.Now()))
	if len(got) != 2 || got[0] != "ApiKey abc" || !strings.HasPrefix(got[1], "Basic ") {
		t.Errorf("auth = %v", got)
	}
}

func TestReady_FailsFastOnAuthError(t *testing.T) {
	for _, code := range []int{401, 403} {
		var n int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&n, 1)
			w.WriteHeader(code)
		}))
		e := New(Config{URL: srv.URL, ReadyTimeout: 5 * time.Second})
		start := time.Now()
		err := e.Ready(context.Background())
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "HTTP "+strconv.Itoa(code)) ||
			!strings.Contains(err.Error(), "VERN_API_KEY") || !strings.Contains(err.Error(), "VERN_API_KEY_FILE") ||
			!strings.Contains(err.Error(), "VERN_ELASTICSEARCH_USERNAME") {
			t.Fatalf("%d: err = %v", code, err)
		}
		if got := atomic.LoadInt32(&n); got != 1 {
			t.Errorf("%d: polls = %d, want 1 (no retry)", code, got)
		}
		if time.Since(start) > time.Second {
			t.Errorf("%d: took %s", code, time.Since(start))
		}
	}
}

func TestReady_ErrorsRedactUserinfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	withCreds := strings.Replace(srv.URL, "http://", "http://vern:s3cret@", 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, e := range map[string]*Engine{
		"timeout":   New(Config{URL: withCreds, ReadyTimeout: 20 * time.Millisecond}),
		"invalid":   New(Config{URL: "http://vern:s3cret@[::1", ReadyTimeout: 20 * time.Millisecond}),
		"refused":   New(Config{URL: "http://vern:s3cret@127.0.0.1:1", ReadyTimeout: 20 * time.Millisecond}),
		"cancelled": New(Config{URL: withCreds, ReadyTimeout: time.Second}),
	} {
		c := context.Background()
		if name == "cancelled" {
			c = ctx
		}
		err := e.Ready(c)
		if err == nil || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer auth.Close()
	err := New(Config{URL: strings.Replace(auth.URL, "http://", "http://vern:s3cret@", 1)}).Ready(context.Background())
	if err == nil || strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("auth: err = %v", err)
	}
}

func TestLogLicense_RetriesThenLogsType(t *testing.T) {
	old := licenseRetryDelay
	licenseRetryDelay = time.Millisecond
	defer func() { licenseRetryDelay = old }()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"license":{"type":"basic"}}`))
	}))
	defer srv.Close()
	var logs []string
	e := New(Config{URL: srv.URL, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	e.logLicense(context.Background())
	if len(logs) != 1 || !strings.Contains(logs[0], "basic") {
		t.Errorf("logs = %v", logs)
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer down.Close()
	logs = nil
	e = New(Config{URL: down.URL, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	e.logLicense(context.Background())
	if len(logs) != 1 || logs[0] != "elasticsearch license: unavailable (HTTP 503)" {
		t.Errorf("logs = %v", logs)
	}
}

func TestLogLicense_NoRetryOn4xx(t *testing.T) {
	old := licenseRetryDelay
	licenseRetryDelay = time.Millisecond
	defer func() { licenseRetryDelay = old }()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(403)
	}))
	defer srv.Close()
	var logs []string
	e := New(Config{URL: srv.URL, Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	e.logLicense(context.Background())
	if got := atomic.LoadInt32(&n); got != 1 {
		t.Errorf("GET /_license called %d times, want 1 (403 is not transient)", got)
	}
	if len(logs) != 1 || logs[0] != "elasticsearch license: unavailable (HTTP 403)" {
		t.Errorf("logs = %v", logs)
	}
}

func TestLogLicense_RetriesTransportErrors(t *testing.T) {
	old := licenseRetryDelay
	licenseRetryDelay = time.Millisecond
	defer func() { licenseRetryDelay = old }()
	// A listener that drops every connection: each attempt is a transport error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var n int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&n, 1)
			c.Close()
		}
	}()
	var logs []string
	e := New(Config{URL: "http://" + ln.Addr().String(), Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	e.logLicense(context.Background())
	if got := atomic.LoadInt32(&n); got != licenseAttempts {
		t.Errorf("connections = %d, want %d (transport errors are retried)", got, licenseAttempts)
	}
	if len(logs) != 1 || !strings.HasPrefix(logs[0], "elasticsearch license: unavailable (") {
		t.Errorf("logs = %v", logs)
	}
}

func countingServer(rc *rec, rootStatus *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.add(r)
		switch r.URL.Path {
		case "/":
			if rootStatus != nil && rootStatus.Load() != 0 {
				w.WriteHeader(int(rootStatus.Load()))
				return
			}
			rootOK(w, "9.5.5")
		case "/_license":
			_, _ = w.Write([]byte(`{"license":{"type":"basic"}}`))
		case "/_component_template/metrics-otel@custom":
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}))
}

func (r *rec) count(call string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c == call {
			n++
		}
	}
	return n
}

func TestReady_BootstrapOnceThenProbe(t *testing.T) {
	rc := &rec{}
	srv := countingServer(rc, nil)
	defer srv.Close()
	e := New(Config{URL: srv.URL, ReadyTimeout: time.Second})
	for i := 0; i < 3; i++ {
		if err := e.Ready(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := rc.count("GET /_license"); n != 1 {
		t.Errorf("_license requests = %d, want 1", n)
	}
	if n := rc.count("GET /_component_template/metrics-otel@custom"); n != 1 {
		t.Errorf("_component_template requests = %d, want 1", n)
	}
	if n := rc.count("GET /"); n != 3 {
		t.Errorf("GET / requests = %d, want 3", n)
	}
}

func TestReady_ProbeKeepsAuthAndVersionChecks(t *testing.T) {
	rc := &rec{}
	var status atomic.Int32
	srv := countingServer(rc, &status)
	defer srv.Close()
	e := New(Config{URL: srv.URL, ReadyTimeout: time.Second})
	if err := e.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	status.Store(401)
	err := e.Ready(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rejected the credentials") {
		t.Errorf("probe 401 error = %v", err)
	}
	status.Store(503)
	start := time.Now()
	if err := e.Ready(context.Background()); err == nil {
		t.Error("probe 503 should fail")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Error("probe must not poll")
	}
}

func TestReady_FailureNotCached(t *testing.T) {
	rc := &rec{}
	var status atomic.Int32
	status.Store(503)
	srv := countingServer(rc, &status)
	defer srv.Close()
	e := New(Config{URL: srv.URL, ReadyTimeout: 20 * time.Millisecond})
	if err := e.Ready(context.Background()); err == nil {
		t.Fatal("first call should fail")
	}
	status.Store(0)
	if err := e.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := rc.count("GET /_license"); n != 1 {
		t.Errorf("_license requests = %d, want 1 (second call bootstraps)", n)
	}
}

func TestReady_ConcurrentSingleBootstrap(t *testing.T) {
	rc := &rec{}
	srv := countingServer(rc, nil)
	defer srv.Close()
	e := New(Config{URL: srv.URL, ReadyTimeout: time.Second})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.Ready(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := rc.count("GET /_license"); n != 1 {
		t.Errorf("_license requests = %d, want 1", n)
	}
}

func TestReady_WarningTextGeneric(t *testing.T) {
	rc := &rec{}
	srv := templateServer(404, rc, 200)
	defer srv.Close()
	var logs []string
	e := New(Config{URL: srv.URL, ReadyTimeout: time.Second,
		Logf: func(f string, a ...any) { logs = append(logs, f) }})
	if err := e.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	const want = "warning: component template metrics-otel@custom is absent and template setup is disabled; metric data points older than the data stream's default look-back may be rejected (reported as dropped). Enable template setup (vern score --setup-template, or VERN_SETUP_TEMPLATE=true for vern serve) or use deploy/compose.yaml"
	found := false
	for _, l := range logs {
		found = found || l == want
	}
	if !found {
		t.Errorf("warning not logged verbatim: %v", logs)
	}
}

func TestReady_WaiterHonoursOwnCtx(t *testing.T) {
	rc := &rec{}
	var status atomic.Int32
	status.Store(503)
	srv := countingServer(rc, &status)
	defer srv.Close()
	e := New(Config{URL: srv.URL, ReadyTimeout: 30 * time.Second})
	bg, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = e.Ready(bg) }()
	time.Sleep(50 * time.Millisecond) // let the bootstrap take the slot
	ctx, c2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer c2()
	start := time.Now()
	err := e.Ready(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("waiter took %s, want ~100ms", d)
	}
	cancel()
	<-done
}
