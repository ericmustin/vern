package elastic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ericmustin/vern/internal/engine"
)

// readyPollInterval is how often Ready polls GET /. A variable so tests can
// shrink it.
var readyPollInterval = 2 * time.Second

const (
	templatePath = "_component_template/metrics-otel@custom"
	// metricsTemplateBody lifts the metrics look-back window to 7 days so
	// samples with older data points are accepted. Never PUT back a GET'd body.
	metricsTemplateBody = `{"template":{"settings":{"index.look_back_time":"7d","index.number_of_replicas":0}},"_meta":{"managed_by":"vern"}}`
	minMajor, minMinor  = 9, 5
	otelMarker          = ".otel-"
)

// Ready reports whether Elasticsearch is usable.
//
// The first successful call bootstraps: it waits for GET / (polling every
// readyPollInterval until Config.ReadyTimeout), requires version >= 9.5.0,
// logs the license type, and checks the metrics-otel@custom component
// template. When it is absent, Ready creates it if Config.EnsureTemplate is
// set and otherwise logs a warning. Every later call is a cheap probe: one
// GET / bounded by ctx with the same version check and auth fail-fast, and no
// license log or template check. A failed bootstrap is never cached, and
// concurrent callers never repeat it, and none waits past its own ctx; once
// bootstrapped, probes take no lock.
//
// A 401 or 403 on GET / fails at once: retrying cannot fix credentials. A 403
// on the template GET or PUT only logs a warning: a least-privilege key may
// lack template privileges, in which case the template must be provisioned
// out of band.
func (e *Engine) Ready(ctx context.Context) error {
	if e.bootstrapped.Load() {
		return e.probeReady(ctx)
	}
	// One bootstrap at a time, but no caller waits past its own ctx.
	select {
	case e.bootstrapSem <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("elasticsearch at %s not ready: %w", e.redactedURL(), ctx.Err())
	}
	defer func() { <-e.bootstrapSem }()
	if e.bootstrapped.Load() { // another caller finished while we waited
		return e.probeReady(ctx)
	}
	if err := e.bootstrap(ctx); err != nil {
		return err
	}
	e.bootstrapped.Store(true)
	return nil
}

// probeReady is the post-bootstrap check: a single GET /, no retries.
func (e *Engine) probeReady(ctx context.Context) error {
	v, err := e.probeRoot(ctx)
	if err != nil {
		var ae *authError
		if errors.As(err, &ae) {
			return e.authFailure(ae)
		}
		return fmt.Errorf("elasticsearch at %s not ready: %s", e.redactedURL(), e.scrub(err))
	}
	return checkVersion(v)
}

func (e *Engine) authFailure(ae *authError) error {
	return fmt.Errorf("elasticsearch at %s rejected the credentials (GET /: HTTP %d): check VERN_API_KEY / VERN_API_KEY_FILE, or VERN_ELASTICSEARCH_USERNAME and VERN_ELASTICSEARCH_PASSWORD", e.redactedURL(), ae.status)
}

// scrub keeps the raw URL (which may carry userinfo) out of error text.
func (e *Engine) scrub(err error) string {
	return strings.ReplaceAll(err.Error(), e.cfg.URL, e.redactedURL())
}

func checkVersion(version string) error {
	major, minor, ok := parseVersion(version)
	if !ok || major < minMajor || (major == minMajor && minor < minMinor) {
		return fmt.Errorf("elasticsearch 9.5 or later required, found %s", version)
	}
	return nil
}

func (e *Engine) bootstrap(ctx context.Context) error {
	deadline := time.Now().Add(e.cfg.ReadyTimeout)
	at := e.redactedURL()
	var lastErr error
	var version string
	for {
		v, err := e.probeRoot(ctx)
		if err == nil {
			version = v
			break
		}
		var ae *authError
		if errors.As(err, &ae) {
			return e.authFailure(ae)
		}
		lastErr = err
		if ctx.Err() != nil {
			return fmt.Errorf("elasticsearch at %s not ready: %w", at, ctx.Err())
		}
		if !time.Now().Add(readyPollInterval).Before(deadline) {
			return fmt.Errorf("elasticsearch at %s not ready after %s: %s", at, e.cfg.ReadyTimeout, e.scrub(lastErr))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("elasticsearch at %s not ready: %w", at, ctx.Err())
		case <-time.After(readyPollInterval):
		}
	}

	if err := checkVersion(version); err != nil {
		return err
	}
	e.logLicense(ctx)
	return e.ensureTemplate(ctx)
}

// authError is an authentication or authorisation failure on GET /.
type authError struct{ status int }

func (a *authError) Error() string { return fmt.Sprintf("GET /: HTTP %d", a.status) }

func (e *Engine) probeRoot(ctx context.Context) (string, error) {
	status, body, err := e.do(ctx, http.MethodGet, "", "", nil)
	if err != nil {
		return "", err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return "", &authError{status: status}
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("GET /: HTTP %d", status)
	}
	var r struct {
		Version struct {
			Number string `json:"number"`
		} `json:"version"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Version.Number == "" {
		return "", fmt.Errorf("GET /: unexpected response")
	}
	return r.Version.Number, nil
}

// parseVersion extracts major.minor from versions like "9.5.5-SNAPSHOT".
func parseVersion(v string) (major, minor int, ok bool) {
	core := v
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	var err error
	if major, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, false
	}
	if minor, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

var licenseRetryDelay = time.Second

const licenseAttempts = 3

func (e *Engine) logLicense(ctx context.Context) {
	// GET /_license can race node startup, so retry briefly, but only on
	// transient failures (transport errors and 5xx); a 4xx will not change.
	var (
		status int
		body   []byte
		err    error
	)
	for attempt := 0; attempt < licenseAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(licenseRetryDelay):
			}
		}
		status, body, err = e.do(ctx, http.MethodGet, "_license", "", nil)
		if err == nil && status < http.StatusInternalServerError {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	if err != nil || status != http.StatusOK {
		why := fmt.Sprintf("HTTP %d", status)
		if err != nil {
			why = strings.ReplaceAll(err.Error(), e.cfg.URL, e.redactedURL())
		}
		e.cfg.Logf("elasticsearch license: unavailable (%s)", why)
		return
	}
	var l struct {
		License struct {
			Type string `json:"type"`
		} `json:"license"`
	}
	if json.Unmarshal(body, &l) == nil && l.License.Type != "" {
		e.cfg.Logf("elasticsearch license: %s", l.License.Type)
	}
}

func (e *Engine) ensureTemplate(ctx context.Context) error {
	const warn = "warning: not permitted to manage component template metrics-otel@custom; continuing (old metric data points may be dropped unless it exists)"
	status, _, err := e.do(ctx, http.MethodGet, templatePath, "", nil)
	if err != nil {
		return fmt.Errorf("check component template: %w", err)
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusForbidden:
		e.cfg.Logf(warn)
		return nil
	case http.StatusNotFound:
	default:
		return fmt.Errorf("check component template: HTTP %d", status)
	}
	if !e.cfg.EnsureTemplate {
		e.cfg.Logf("warning: component template metrics-otel@custom is absent and template setup is disabled; metric data points older than the data stream's default look-back may be rejected (reported as dropped). Enable template setup (vern score --setup-template, or VERN_SETUP_TEMPLATE=true for vern serve) or use deploy/compose.yaml")
		return nil
	}
	status, body, err := e.do(ctx, http.MethodPut, templatePath, "application/json", strings.NewReader(metricsTemplateBody))
	if err != nil {
		return fmt.Errorf("create component template: %w", err)
	}
	switch {
	case status == http.StatusForbidden:
		e.cfg.Logf(warn)
		return nil
	case status/100 != 2:
		return fmt.Errorf("create component template: HTTP %d: %s", status, truncate(body))
	}
	e.cfg.Logf("created component template metrics-otel@custom (index.look_back_time 7d, 0 replicas)")
	return nil
}

func truncate(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return string(b)
}

// listStreams returns names of data streams matching the comma-joined
// wildcard patterns. No match yields an empty slice (Elasticsearch answers
// 200 with an empty list).
func (e *Engine) listStreams(ctx context.Context, patterns string) ([]string, error) {
	status, body, err := e.do(ctx, http.MethodGet, "_data_stream/"+patterns+"?expand_wildcards=all", "", nil)
	if err != nil {
		return nil, fmt.Errorf("list data streams: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("list data streams: HTTP %d: %s", status, truncate(body))
	}
	var r struct {
		DataStreams []struct {
			Name string `json:"name"`
		} `json:"data_streams"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("list data streams: decode: %w", err)
	}
	names := make([]string, 0, len(r.DataStreams))
	for _, d := range r.DataStreams {
		names = append(names, d.Name)
	}
	return names, nil
}

// deleteStreams deletes streams by exact name. It never sends an empty list.
func (e *Engine) deleteStreams(ctx context.Context, names []string) error {
	if len(names) == 0 {
		return nil
	}
	status, body, err := e.do(ctx, http.MethodDelete, "_data_stream/"+strings.Join(names, ","), "", nil)
	if err != nil {
		return fmt.Errorf("delete data streams: %w", err)
	}
	if status/100 != 2 && status != http.StatusNotFound {
		return fmt.Errorf("delete data streams: HTTP %d: %s", status, truncate(body))
	}
	return nil
}

// namespaceOf returns the text after the last ".otel-" in a stream name.
func namespaceOf(name string) (string, bool) {
	i := strings.LastIndex(name, otelMarker)
	if i < 0 {
		return "", false
	}
	return name[i+len(otelMarker):], true
}

// Purge deletes the run's data streams (traces, logs, metrics) by exact name.
// It is idempotent: when no stream exists it returns nil without sending a
// DELETE.
func (e *Engine) Purge(ctx context.Context, id engine.RunID) error {
	_, pats := e.Session(id)
	names, err := e.listStreams(ctx, pats.Traces+","+pats.Logs+","+pats.Metrics)
	if err != nil {
		return err
	}
	var own []string
	for _, n := range names {
		// Defensive: only delete streams whose namespace is exactly this run.
		if ns, ok := namespaceOf(n); ok && ns == string(id) {
			own = append(own, n)
		}
	}
	return e.deleteStreams(ctx, own)
}

// Sweep deletes the data streams of every vern run older than olderThan and
// returns the number of RUNS purged (not streams). Streams whose namespace is
// not a valid vern RunID are never touched.
func (e *Engine) Sweep(ctx context.Context, olderThan time.Duration) (int, error) {
	names, err := e.listStreams(ctx, "*-*.otel-vern*")
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	byRun := map[engine.RunID][]string{}
	for _, n := range names {
		ns, ok := namespaceOf(n)
		if !ok {
			continue
		}
		id, ok := engine.ParseRunID(ns)
		if !ok || !id.CreatedAt().Before(cutoff) {
			continue
		}
		byRun[id] = append(byRun[id], n)
	}
	ids := make([]string, 0, len(byRun))
	for id := range byRun {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	purged := 0
	var errs []string
	for _, id := range ids {
		if err := e.deleteStreams(ctx, byRun[engine.RunID(id)]); err != nil {
			errs = append(errs, fmt.Sprintf("run %s: %v", id, err))
			continue
		}
		purged++
	}
	if len(errs) > 0 {
		return purged, fmt.Errorf("sweep: %s", strings.Join(errs, "; "))
	}
	return purged, nil
}
