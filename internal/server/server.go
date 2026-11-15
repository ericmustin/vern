// Package server is the HTTP API behind `vern serve`: synchronous score runs
// over uploaded OTLP samples, the rule methodology, health checks, and the
// embedded UI's static files. It stores nothing: every run is scored through
// samplescore.Score (the same path as `vern score`) and purged.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/otlpin"
	"github.com/ericmustin/vern/internal/samplescore"
)

// ScoreFunc scores a decoded sample; samplescore.Score is the production one.
type ScoreFunc func(ctx context.Context, eng engine.Engine, s *otlpin.Sample, opts samplescore.Options) (*evaluate.Report, error)

// Config configures New. Zero values pick the documented defaults.
type Config struct {
	Engine engine.Engine
	Score  ScoreFunc // nil -> samplescore.Score
	// Options is the template for every run; Files and Sweep are overwritten
	// per run (Sweep = 0: RunJanitor owns sweeping).
	Options       samplescore.Options
	Limits        otlpin.Limits // zero -> otlpin.DefaultLimits
	MaxConcurrent int           // <= 0 -> 1 (more needs an Elasticsearch heap over 1 GiB)
	RunTimeout    time.Duration // <= 0 -> 120s
	Static        fs.FS         // nil -> no UI, API only
	HSTS          bool          // send Strict-Transport-Security
	Version       string
	Logf          func(string, ...any) // nil -> log.Printf
}

const (
	defaultMaxConcurrent = 1
	defaultRunTimeout    = 120 * time.Second
	readyTimeout         = 10 * time.Second
	readyCacheTTL        = 5 * time.Second

	contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

type server struct {
	cfg        Config
	score      ScoreFunc
	limits     otlpin.Limits
	runTimeout time.Duration
	logf       func(string, ...any)
	sem        chan struct{} // one token per running run
	rules      []byte        // pre-encoded GET /api/v1/rules body; nil if building failed
	handler    http.Handler

	now      func() time.Time
	readyMu  sync.Mutex
	readyAt  time.Time // when the cached result was taken; zero = none
	readyErr error
}

// New returns the handler for the API, health checks and static UI.
func New(cfg Config) http.Handler { return newServer(cfg) }

func newServer(cfg Config) *server {
	s := &server{
		cfg:        cfg,
		score:      cfg.Score,
		limits:     cfg.Limits,
		runTimeout: cfg.RunTimeout,
		logf:       cfg.Logf,
		now:        time.Now,
	}
	if s.score == nil {
		s.score = samplescore.Score
	}
	if s.limits == (otlpin.Limits{}) {
		s.limits = otlpin.DefaultLimits
	}
	if s.runTimeout <= 0 {
		s.runTimeout = defaultRunTimeout
	}
	if s.logf == nil {
		s.logf = log.Printf
	}
	n := cfg.MaxConcurrent
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	s.sem = make(chan struct{}, n)

	if rules, err := buildRules(cfg.Options); err != nil {
		s.logf("error: building rule methodology: %v", err)
	} else {
		s.rules = rules
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/runs", s.handleRuns)
	mux.HandleFunc("GET /api/v1/rules", s.handleRules)
	mux.HandleFunc("GET /api/v1/limits", s.handleLimits)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeText(w, http.StatusOK, "ok") })
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /", s.handleStatic)
	s.handler = s.securityHeaders(s.logRequests(recoverPanics(s.logf, mux)))
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// handleLimits reports the effective upload limits and run timeout, so the
// UI never states numbers the server does not enforce.
func (s *server) handleLimits(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		MaxCompressedBytes   int64 `json:"max_compressed_bytes"`
		MaxDecompressedBytes int64 `json:"max_decompressed_bytes"`
		MaxItems             int   `json:"max_items"`
		MaxFiles             int   `json:"max_files"`
		RunTimeoutSeconds    int   `json:"run_timeout_seconds"`
	}{s.limits.MaxCompressedBytes, s.limits.MaxDecompressedBytes, s.limits.MaxItems, s.limits.MaxFiles, int(s.runTimeout.Seconds())})
}

func (s *server) handleRules(w http.ResponseWriter, r *http.Request) {
	if s.rules == nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(s.rules)
}

// handleReady answers from a result cached for readyCacheTTL, so probes
// cannot hammer the engine. The engine error is logged, never returned.
func (s *server) handleReady(w http.ResponseWriter, r *http.Request) {
	s.readyMu.Lock()
	fresh := !s.readyAt.IsZero() && s.now().Sub(s.readyAt) < readyCacheTTL
	err := s.readyErr
	s.readyMu.Unlock()

	if !fresh {
		// Not under the lock: Ready may take up to readyTimeout, and other
		// probes must not queue behind it past their own deadlines.
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		err = s.cfg.Engine.Ready(ctx)
		cancel()
		if err != nil && r.Context().Err() != nil {
			// The prober went away; its cancellation says nothing about the engine.
			infoFrom(r.Context()).clientClosed = true
			return
		}
		s.readyMu.Lock()
		s.readyAt, s.readyErr = s.now(), err
		s.readyMu.Unlock()
		if err != nil {
			s.logf("readyz: engine not ready: %v", err)
		}
	}
	if err != nil {
		writeText(w, http.StatusServiceUnavailable, "not ready")
		return
	}
	writeText(w, http.StatusOK, "ready")
}

// staticTypes maps the extensions the UI serves to their Content-Type. It is
// explicit rather than mime.TypeByExtension, whose answers for .js and .css
// come from the host's mime.types; with nosniff a wrong type breaks the page.
var staticTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".gz":   "application/gzip",
	".svg":  "image/svg+xml",
	".json": "application/json",
}

// handleStatic serves files from cfg.Static: index.html for "/", regular
// files by path, 404 for everything else (no directory listings).
func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Static == nil {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	info, err := fs.Stat(s.cfg.Static, name)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(s.cfg.Static, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctype, ok := staticTypes[strings.ToLower(path.Ext(name))]
	if !ok {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	http.ServeContent(w, r, name, info.ModTime(), bytes.NewReader(data))
}

// securityHeaders sets the CSP and related headers on every response, and
// keeps API responses out of caches.
func (s *server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if s.cfg.HSTS {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// reqInfo carries per-request facts that handlers report to logRequests.
type reqInfo struct {
	runID        string
	clientClosed bool
}

type reqInfoKey struct{}

func infoFrom(ctx context.Context) *reqInfo {
	if ri, ok := ctx.Value(reqInfoKey{}).(*reqInfo); ok {
		return ri
	}
	return &reqInfo{}
}

// statusRecorder remembers the status code written.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	if sr.status == 0 {
		sr.status = code
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	return sr.ResponseWriter.Write(b)
}

func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }

// logRequests writes one line per request: method, path (never the query),
// status, duration and run_id when known. Nothing else from the request.
func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ri := &reqInfo{}
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			status := rec.status
			switch {
			case ri.clientClosed:
				status = 499 // client closed request; nothing was written
			case status == 0:
				status = http.StatusOK
			}
			line := "%s %q %d %dms"
			args := []any{r.Method, r.URL.Path, status, time.Since(start).Milliseconds()}
			if ri.runID != "" {
				line += " run_id=%s"
				args = append(args, ri.runID)
			}
			s.logf(line, args...)
		}()
		next.ServeHTTP(rec, r.WithContext(context.WithValue(r.Context(), reqInfoKey{}, ri)))
	})
}

// recoverPanics turns a handler panic into a 500 (when nothing was written yet).
func recoverPanics(logf func(string, ...any), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			// Only the value's type: its text could quote uploaded data.
			logf("panic serving %s %q: %T\n%s", r.Method, r.URL.Path, v, debug.Stack())
			if sr, ok := w.(*statusRecorder); ok && sr.status != 0 {
				return
			}
			writeError(w, http.StatusInternalServerError, "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}

func writeText(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	w.Write([]byte(msg + "\n"))
}

// writeJSON encodes v (HTML-escaped, so '<', '>' and '&' in uploaded values
// become < etc.) and writes it with status.
func writeJSON(w http.ResponseWriter, status int, v any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, err := w.Write(buf.Bytes())
	return err
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{msg})
}
