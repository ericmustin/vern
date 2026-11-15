// Package elastic is the Elasticsearch implementation of engine.Engine. It
// loads samples through Elasticsearch's native OTLP endpoint into per-run data
// streams (namespace = RunID) and queries them with ES|QL.
package elastic

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/evaluate"
	esexec "github.com/ericmustin/vern/internal/evaluate/elastic"
	"github.com/ericmustin/vern/internal/redact"
)

var _ engine.Engine = (*Engine)(nil)

// Config configures an Engine.
type Config struct {
	URL, APIKey, Username, Password string
	HTTP                            *http.Client                     // nil: 60s-timeout client
	BatchBytes                      int                              // 0: 4 << 20
	ReadyTimeout                    time.Duration                    // 0: 90s
	Logf                            func(format string, args ...any) // nil: discard
	// EnsureTemplate lets Ready create the cluster-wide metrics-otel@custom
	// component template (7d look-back, 0 replicas) when it is absent. Off by
	// default: it changes every metrics-*-otel data stream on the cluster, so
	// enable it only for single-node or ephemeral clusters.
	EnsureTemplate bool
}

// Engine implements engine.Engine against one Elasticsearch cluster.
type Engine struct {
	cfg Config

	// bootstrapSem (capacity 1) admits one bootstrap at a time, acquired
	// ctx-aware; bootstrapped is set after the first successful bootstrap.
	bootstrapSem chan struct{}
	bootstrapped atomic.Bool
}

// New returns an Engine, filling Config defaults.
func New(cfg Config) *Engine {
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 60 * time.Second}
	}
	if cfg.BatchBytes <= 0 {
		cfg.BatchBytes = 4 << 20
	}
	if cfg.BatchBytes > maxBody {
		cfg.BatchBytes = maxBody
	}
	if cfg.ReadyTimeout == 0 {
		cfg.ReadyTimeout = 90 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Engine{cfg: cfg, bootstrapSem: make(chan struct{}, 1)}
}

// Session returns an executor and index patterns scoped to the run's data
// streams only (namespace == id). It makes no network calls.
func (e *Engine) Session(id engine.RunID) (evaluate.Executor, config.IndexPatterns) {
	ex := &esexec.Executor{
		BaseURL:  e.cfg.URL,
		APIKey:   e.cfg.APIKey,
		Username: e.cfg.Username,
		Password: e.cfg.Password,
		HTTP:     e.cfg.HTTP,
	}
	return ex, config.IndexPatterns{
		Traces:  "traces-*.otel-" + string(id),
		Logs:    "logs-*.otel-" + string(id),
		Metrics: "metrics-*.otel-" + string(id),
	}
}

// do sends one authenticated request and returns status and body. A non-nil
// error means a transport failure, not an HTTP error status.
func (e *Engine) do(ctx context.Context, method, path, contentType string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, e.cfg.URL+"/"+strings.TrimLeft(path, "/"), body)
	if err != nil {
		// The parse error quotes the URL, credentials included: drop it.
		return 0, nil, errors.New("build request: invalid elasticsearch URL")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// Same precedence as evaluate/elastic.Executor: ApiKey, else basic auth.
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "ApiKey "+e.cfg.APIKey)
	} else if e.cfg.Username != "" {
		req.SetBasicAuth(e.cfg.Username, e.cfg.Password)
	}
	resp, err := e.cfg.HTTP.Do(req)
	if err != nil {
		return 0, nil, redact.TransportError(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, raw, nil
}

// redactedURL is the cluster URL for errors and logs, without userinfo,
// query or fragment; an unparseable URL is not echoed at all.
func (e *Engine) redactedURL() string { return redact.URL(e.cfg.URL) }
