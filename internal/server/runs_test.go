package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/otlpin"
	"github.com/ericmustin/vern/internal/samplescore"
	"github.com/ericmustin/vern/internal/score"
	"github.com/ericmustin/vern/internal/sensitive"
)

type runResponse struct {
	Rules    []evaluate.RuleResult `json:"rules"`
	Sample   *evaluate.SampleInfo  `json:"sample"`
	Problems []string              `json:"problems"`
	Markdown *string               `json:"markdown"`
}

func decodeRun(t *testing.T, rec *httptest.ResponseRecorder) runResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var out runResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

type part struct {
	field, filename string
	data            []byte
}

func postMultipart(t *testing.T, parts []part) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		var w io.Writer
		var err error
		if p.filename == "" {
			w, err = mw.CreateFormField(p.field)
		} else {
			w, err = mw.CreateFormFile(p.field, p.filename)
		}
		if err != nil {
			t.Fatal(err)
		}
		w.Write(p.data)
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func spanJSON(service string) []byte {
	b, _ := json.Marshal(service)
	return []byte(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":` + string(b) + `}}]},"scopeSpans":[{"spans":[{"traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174","name":"op","startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000001000000000"}]}]}]}` + "\n")
}

func TestRuns_RawGzipOK(t *testing.T) {
	eng := &fakeEngine{}
	s := newTestServer(t, Config{Engine: eng})
	out := decodeRun(t, serve(s, postRaw(readDemo(t))))
	if len(out.Rules) == 0 {
		t.Fatal("response has no rules")
	}
	if out.Sample == nil || len(out.Sample.Files) != 1 || out.Sample.Files[0] != "upload" {
		t.Fatalf("sample.files = %v, want [upload]", out.Sample)
	}
	if out.Markdown != nil {
		t.Fatal("markdown present without ?markdown=1")
	}
	if eng.purgeCount() != 1 {
		t.Fatalf("Purge called %d times, want 1", eng.purgeCount())
	}
}

func TestRuns_MultipartFilesAndBaseName(t *testing.T) {
	s := newTestServer(t, Config{Engine: &fakeEngine{}})
	req := postMultipart(t, []part{
		{field: "files", filename: "a.json", data: spanJSON("svc-a")},
		{field: "note", data: []byte("ignored: no filename")},
		{field: "files", filename: "../../etc/x.json", data: spanJSON("svc-x")},
	})
	out := decodeRun(t, serve(s, req))
	if got := strings.Join(out.Sample.Files, ","); got != "a.json,x.json" {
		t.Fatalf("sample.files = %q, want a.json,x.json", got)
	}
	if got := strings.Join(out.Sample.Services, ","); got != "svc-a,svc-x" {
		t.Fatalf("sample.services = %q, want both files decoded", got)
	}
}

func TestRuns_MultipartNoFiles400(t *testing.T) {
	s := newTestServer(t, Config{Engine: &fakeEngine{}})
	req := postMultipart(t, []part{{field: "note", data: spanJSON("svc")}})
	rec := serve(s, req)
	if rec.Code != http.StatusBadRequest || errorBody(t, rec) != "no files in upload" {
		t.Fatalf("got %d %s, want 400 no files in upload", rec.Code, rec.Body.String())
	}
}

func TestRuns_MultipartMalformed400(t *testing.T) {
	s := newTestServer(t, Config{Engine: &fakeEngine{}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader("garbage"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xyz")
	if rec := serve(s, req); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed multipart = %d, want 400", rec.Code)
	}
}

func TestRuns_TooLarge413(t *testing.T) {
	lim := otlpin.Limits{MaxCompressedBytes: 1024, MaxDecompressedBytes: 1 << 20, MaxItems: 1000, MaxFiles: 4}
	s := newTestServer(t, Config{Engine: &fakeEngine{}, Limits: lim})

	// Over Limits.MaxCompressedBytes but under the MaxBytesReader cap: otlpin rejects it.
	big := bytes.Repeat([]byte(" "), 4096)
	big = append(big, spanJSON("svc")...)
	rec := serve(s, httptest.NewRequest(http.MethodPost, "/api/v1/runs", bytes.NewReader(big)))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("raw over limit = %d %s, want 413", rec.Code, rec.Body.String())
	}
	errorBody(t, rec)

	// Over the MaxBytesReader cap (limit + 1 MiB) in a multipart file part.
	huge := bytes.Repeat([]byte(" "), 2<<20)
	rec = serve(s, postMultipart(t, []part{{field: "f", filename: "big.json", data: huge}}))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("multipart over cap = %d %s, want 413", rec.Code, rec.Body.String())
	}

	// Too many files.
	var parts []part
	for range 5 {
		parts = append(parts, part{field: "f", filename: "s.json", data: []byte("{}")})
	}
	rec = serve(s, postMultipart(t, parts))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("too many files = %d %s, want 413", rec.Code, rec.Body.String())
	}
	if len(s.sem) != 0 {
		t.Fatalf("semaphore holds %d slot(s)", len(s.sem))
	}
}

func TestRuns_GzipBomb413(t *testing.T) {
	// 300 gzip members of 1 MiB of zeros each: one valid multistream gzip
	// that inflates to 300 MiB (compressing 300 MiB once is slow under -race).
	var member bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&member, gzip.BestCompression)
	zw.Write(make([]byte, 1<<20))
	zw.Close()
	gz := bytes.NewBuffer(bytes.Repeat(member.Bytes(), 300))
	if gz.Len() >= 1<<20 {
		t.Fatalf("bomb is %d bytes compressed, want < 1 MiB", gz.Len())
	}
	s := newTestServer(t, Config{Engine: &fakeEngine{}, MaxConcurrent: 1})
	rec := serve(s, postRaw(gz.Bytes()))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("gzip bomb = %d %s, want 413", rec.Code, rec.Body.String())
	}
	if msg := errorBody(t, rec); !strings.Contains(msg, otlpin.ErrTooLarge.Error()) {
		t.Fatalf("gzip bomb error = %q, want the decompressed size limit", msg)
	}
	if len(s.sem) != 0 {
		t.Fatalf("semaphore holds %d slot(s) after the bomb", len(s.sem))
	}
	// The slot is free: a normal run is accepted.
	decodeRun(t, serve(s, postRaw(readDemo(t))))
}

func TestRuns_Concurrency429(t *testing.T) {
	entered, block := make(chan struct{}), make(chan struct{})
	s := newTestServer(t, Config{Engine: &fakeEngine{}, MaxConcurrent: 1, Score: fakeScore(entered, block, nil, nil)})
	demo := readDemo(t)
	first := make(chan *httptest.ResponseRecorder)
	go func() { first <- serve(s, postRaw(demo)) }()
	<-entered

	rec := serve(s, postRaw(demo))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "10" {
		t.Fatalf("second run = %d Retry-After %q, want 429 and 10", rec.Code, rec.Header().Get("Retry-After"))
	}
	errorBody(t, rec)

	close(block)
	if rec := <-first; rec.Code != http.StatusOK {
		t.Fatalf("first run = %d, want 200", rec.Code)
	}
}

func TestRuns_ClientCancelReleasesSlot(t *testing.T) {
	logs := &logCapture{}
	eng := &fakeEngine{blockLoads: 1, loadEntered: make(chan struct{}, 1)}
	s := newTestServer(t, Config{Engine: eng, MaxConcurrent: 1, Logf: logs.logf}) // real samplescore.Score
	demo := readDemo(t)

	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeHTTP(rec, postRaw(demo).WithContext(ctx))
	}()
	<-eng.loadEntered
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the client cancelled")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("cancelled request got a body: %q", rec.Body.String())
	}
	if eng.purgeCount() != 1 {
		t.Fatalf("Purge called %d times, want 1", eng.purgeCount())
	}
	if !strings.Contains(logs.String(), "client closed") {
		t.Errorf("cancel not logged:\n%s", logs)
	}
	decodeRun(t, serve(s, postRaw(demo)))
}

func TestRuns_NotReady503(t *testing.T) {
	eng := &fakeEngine{}
	eng.readyErr = &readyErr{"engine at https://es.internal.example:9200 unreachable"}
	logs := &logCapture{}
	s := newTestServer(t, Config{Engine: eng, Logf: logs.logf, Score: fakeScore(nil, nil, nil, nil)})
	rec := serve(s, postRaw(readDemo(t)))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "15" {
		t.Fatalf("not ready = %d Retry-After %q, want 503 and 15", rec.Code, rec.Header().Get("Retry-After"))
	}
	if strings.Contains(rec.Body.String(), "es.internal") {
		t.Fatalf("503 body leaks the engine error: %s", rec.Body.String())
	}
	errorBody(t, rec)
	if !strings.Contains(logs.String(), "es.internal") {
		t.Fatalf("engine error not logged:\n%s", logs)
	}
}

type readyErr struct{ msg string }

func (e *readyErr) Error() string { return e.msg }

func TestRuns_Deadline504(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	s := newTestServer(t, Config{Engine: &fakeEngine{}, RunTimeout: 50 * time.Millisecond, Score: fakeScore(nil, block, nil, nil)})
	rec := serve(s, postRaw(readDemo(t)))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("deadline = %d %s, want 504", rec.Code, rec.Body.String())
	}
	errorBody(t, rec)
}

func TestRuns_InternalError500(t *testing.T) {
	s := newTestServer(t, Config{Engine: &fakeEngine{}, Score: fakeScore(nil, nil, nil, &readyErr{"POST https://es.internal.example:9200/_bulk: 500"})})
	rec := serve(s, postRaw(readDemo(t)))
	if rec.Code != http.StatusInternalServerError || errorBody(t, rec) != "internal error" {
		t.Fatalf("score failure = %d %s, want 500 internal error", rec.Code, rec.Body.String())
	}
}

func TestRuns_DecodeError400(t *testing.T) {
	s := newTestServer(t, Config{Engine: &fakeEngine{}})
	cases := map[string]string{
		"unrecognized": `{"hello":"world"}`,
		"console":      `{"name":"op","context":{"trace_id":"0x1"}}`,
		"empty":        "",
	}
	for name, body := range cases {
		rec := serve(s, httptest.NewRequest(http.MethodPost, "/api/v1/runs", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s, want 400", name, rec.Code, rec.Body.String())
			continue
		}
		errorBody(t, rec)
	}
}

func TestRuns_HTMLInValuesEscaped(t *testing.T) {
	const evil = "<script>alert(1)</script>"
	eng := &fakeEngine{rows: []evaluate.Row{{ServiceName: evil, Passed: false, Extent: 1, Example: evil}}}
	s := newTestServer(t, Config{Engine: eng})
	for _, q := range []string{"", "?markdown=1"} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs"+q, bytes.NewReader(spanJSON(evil)))
		rec := serve(s, req)
		out := decodeRun(t, rec)
		if strings.Contains(rec.Body.String(), "<script>") || strings.Contains(rec.Body.String(), "<") {
			t.Fatalf("%q: response contains a raw '<'", q)
		}
		if len(out.Sample.Services) != 1 || out.Sample.Services[0] != evil {
			t.Fatalf("%q: services = %v, want the value round-tripped", q, out.Sample.Services)
		}
	}
}

func TestRuns_MarkdownParam(t *testing.T) {
	s := newTestServer(t, Config{Engine: &fakeEngine{}})
	req := postRaw(readDemo(t))
	req.URL.RawQuery = "markdown=1"
	out := decodeRun(t, serve(s, req))
	if out.Markdown == nil || !strings.HasPrefix(*out.Markdown, "# Vern Instrumentation Score Report") {
		t.Fatalf("markdown = %v, want the rendered report", out.Markdown)
	}
	if len(out.Rules) == 0 || out.Sample == nil {
		t.Fatal("markdown response lost the report fields")
	}
}

func TestRuns_ProblemsSurface(t *testing.T) {
	rep := &evaluate.Report{
		Rules:  []evaluate.RuleResult{{RuleID: "RES-001", Status: evaluate.StatusError, Reason: "query failed"}},
		Sample: &evaluate.SampleInfo{Files: []string{"upload"}},
	}
	s := newTestServer(t, Config{Engine: &fakeEngine{}, Score: fakeScore(nil, nil, rep, nil)})
	out := decodeRun(t, serve(s, postRaw(readDemo(t))))
	if len(out.Problems) == 0 || !strings.Contains(out.Problems[0], "RES-001") {
		t.Fatalf("problems = %v, want the rule error surfaced", out.Problems)
	}
}

func TestRuns_OptionsPerRun(t *testing.T) {
	var got []string
	var sweep time.Duration = -1
	s := newTestServer(t, Config{
		Engine:  &fakeEngine{},
		Options: samplescore.Options{Sweep: time.Hour, Files: []string{"stale"}},
		Score: func(ctx context.Context, eng engine.Engine, smp *otlpin.Sample, opts samplescore.Options) (*evaluate.Report, error) {
			got, sweep = opts.Files, opts.Sweep
			return &evaluate.Report{Sample: &evaluate.SampleInfo{Files: opts.Files}}, nil
		},
	})
	decodeRun(t, serve(s, postRaw(readDemo(t))))
	if strings.Join(got, ",") != "upload" || sweep != 0 {
		t.Fatalf("opts.Files = %v, Sweep = %v; want [upload] and 0", got, sweep)
	}
}

const (
	canaryEmail = "alice@example.com"
	canaryKey   = "sk-proj-canaryCANARY1234567890"
	// canaryService is a service.name holding an email: it may
	// appear only as its pseudonym, canaryServicePseudo.
	canaryService       = "carol@example.org"
	canaryServicePseudo = "service-1 (name withheld: matches email address)"
)

// withCanaryService appends to a gzipped JSONL upload one more gzip member
// (gzip is multistream) holding a span whose service.name is canaryService.
func withCanaryService(t *testing.T, gz []byte) []byte {
	t.Helper()
	line := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"` + canaryService +
		`"}}]},"scopeSpans":[{"scope":{"name":"canary"},"spans":[{"traceId":"4bf92f3577b34da6a3ce929d0e0e0f01","spanId":"00f067aa0ba90f01","name":"GET /","kind":2,"startTimeUnixNano":"1794672000000000000","endTimeUnixNano":"1794672000040000000"}]}]}]}`
	var member bytes.Buffer
	zw := gzip.NewWriter(&member)
	if _, err := zw.Write([]byte("\n" + line + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return append(append([]byte(nil), gz...), member.Bytes()...)
}

// canaryEngine discovers fields (one metric name holds a canary) and answers
// every sensitive-data query with failing rows whose raw examples carry the
// canaries; MET-008's query fails with an error that echoes the canary, as
// an Elasticsearch error quoting the query would. Every ordinary rule fails
// with an example quoting the canary metric name.
type canaryEngine struct{ *fakeEngine }

func (canaryEngine) Fields(context.Context, engine.RunID) (mappings.Fields, error) {
	return mappings.Fields{
		Resource:    []string{"resource.attributes.custom.config"},
		Span:        []string{"attributes.user.email", "attributes.note"},
		Log:         []string{"body.text"},
		Metric:      []string{"attributes.route"},
		MetricNames: []string{"orders." + canaryEmail + ".count"},
	}, nil
}

func (e canaryEngine) Session(id engine.RunID) (evaluate.Executor, config.IndexPatterns) {
	_, pats := e.fakeEngine.Session(id)
	seg := func(field, value string) string {
		return sensitive.SegmentSep + sensitive.FormatExample(field, value)
	}
	return execFunc(func(ctx context.Context, q string) ([]evaluate.Row, error) {
		switch {
		case strings.Contains(q, "`metrics.orders."):
			return nil, errors.New(`HTTP 400: {"error":{"reason":"line 9: Unknown column [metrics.orders.` + canaryEmail + `.count]"}}`)
		case strings.Contains(q, "No sensitive data matched"):
			return []evaluate.Row{
				{ServiceName: "leaky", Passed: false, Extent: 1, Example: seg("attributes.user.email", canaryEmail) + seg("attributes.note", "key "+canaryKey)},
				{ServiceName: "keyonly", Passed: false, Extent: 1, Example: seg("attributes.note", "key "+canaryKey) + seg("body.text", "mail "+canaryEmail)},
				{ServiceName: "clean", Passed: true, Example: "No sensitive data matched 55 checks"},
				{ServiceName: canaryService, Passed: false, Extent: 1, Example: seg("resource.attributes.service.name", canaryService)},
			}, nil
		}
		// Ordinary rules quote names verbatim; this one quotes a metric
		// name holding the canary.
		return []evaluate.Row{
			{ServiceName: "leaky", Passed: false, Extent: 1, Example: "orders." + canaryEmail + ".count"},
			{ServiceName: canaryService, Passed: true},
		}, nil
	}), pats
}

// TestRuns_SensitiveCanaryNeverInBodyOrLogs runs the real samplescore and
// runner path behind POST /api/v1/runs: the matched values reach neither
// the JSON body, its Markdown nor the log, even when the configured options
// ask for full query errors (the server forces that off).
func TestRuns_SensitiveCanaryNeverInBodyOrLogs(t *testing.T) {
	logs := &logCapture{}
	s := newTestServer(t, Config{
		Engine:  canaryEngine{&fakeEngine{}},
		Options: samplescore.Options{QueryErrorDetail: true, Logf: logs.logf},
		Logf:    logs.logf,
	})
	req := postRaw(withCanaryService(t, readDemo(t)))
	req.URL.RawQuery = "markdown=1"
	rec := serve(s, req)
	out := decodeRun(t, rec)

	body, logText := rec.Body.String(), logs.String()
	for _, c := range []string{canaryEmail, canaryKey, canaryService} {
		if strings.Contains(body, c) {
			t.Errorf("response body contains %q", c)
		}
		if strings.Contains(logText, c) {
			t.Errorf("logs contain %q:\n%s", c, logText)
		}
	}
	// The rules ran, failed with redacted evidence, and MET-008 failed with
	// a fixed reason.
	byID := map[string]evaluate.RuleResult{}
	for _, r := range out.Rules {
		byID[r.RuleID] = r
	}
	var examples []string
	for _, id := range []string{"RES-010", "SPA-006", "LOG-003"} {
		r := byID[id]
		if r.Status != evaluate.StatusEvaluated || !r.Sensitive {
			t.Errorf("%s: status %s sensitive=%v (%s)", id, r.Status, r.Sensitive, r.Reason)
		}
		for _, o := range r.Outcomes {
			examples = append(examples, o.Example)
		}
	}
	joined := strings.Join(examples, "\n")
	for _, want := range []string{
		"attributes.user.email: sensitive key name (al…om (17 chars))",
		"body.text: email address (ma…om (22 chars))",
		"No sensitive data matched 55 checks",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("examples lack %q:\n%s", want, joined)
		}
	}
	// Ordinary rules' examples that match a detector are withheld.
	if r := byID["RES-005"]; r.Sensitive || len(r.Outcomes) == 0 || r.Outcomes[0].Example != "example withheld: it matches email address" {
		t.Errorf("RES-005 = sensitive %v outcomes %+v, want the withheld example", r.Sensitive, r.Outcomes)
	}
	if r := byID["MET-008"]; r.Status != evaluate.StatusError || r.Reason != "query failed (HTTP 400)" {
		t.Errorf("MET-008 = %s %q, want error / query failed (HTTP 400)", r.Status, r.Reason)
	}
	if !strings.Contains(logText, "MET-008: query failed (HTTP 400) (detail withheld") {
		t.Errorf("logs lack the withheld MET-008 error:\n%s", logText)
	}
	if out.Markdown == nil || !strings.Contains(*out.Markdown, "sensitive key name (al…om (17 chars))") {
		t.Errorf("markdown lacks the redacted example")
	}
	// The service.name canary shows only as its pseudonym: in the sample's
	// services, the outcomes, the per-service scores and the Markdown.
	if out.Sample == nil || !slices.Contains(out.Sample.Services, canaryServicePseudo) {
		t.Errorf("sample services lack the pseudonym: %+v", out.Sample)
	}
	if !slices.ContainsFunc(byID["RES-010"].Outcomes, func(o score.Outcome) bool { return o.ServiceName == canaryServicePseudo && !o.Passed }) {
		t.Errorf("RES-010 has no failing outcome for the pseudonym")
	}
	if !strings.Contains(body, `"service_name":"`+canaryServicePseudo+`"`) {
		t.Errorf("response body lacks the pseudonym as a service name")
	}
	if out.Markdown == nil || !strings.Contains(*out.Markdown, canaryServicePseudo) {
		t.Errorf("markdown lacks the pseudonym")
	}
}

func TestRuns_ErrorBodiesCarryNoEngineText(t *testing.T) {
	const leak = "http://es-internal:9200/_query field=user.email.secret_field"
	cases := []struct {
		name       string
		err        error
		status     int
		body       string
		retryAfter string
	}{
		{"internal", errors.New("evaluate: post query: " + leak), http.StatusInternalServerError, "internal error", ""},
		{"busy", fmt.Errorf("load sample: %s: %w", leak, engine.ErrBusy), http.StatusServiceUnavailable, "busy, try again shortly", "30"},
		{"timeout", fmt.Errorf("evaluate: %s: %w", leak, context.DeadlineExceeded), http.StatusGatewayTimeout, "scoring took too long and was stopped", ""},
	}
	for _, c := range cases {
		logs := &logCapture{}
		s := newTestServer(t, Config{Engine: &fakeEngine{}, Score: fakeScore(nil, nil, nil, c.err), Logf: logs.logf})
		rec := serve(s, postRaw(readDemo(t)))
		if rec.Code != c.status || errorBody(t, rec) != c.body {
			t.Errorf("%s: %d %s, want %d %q", c.name, rec.Code, rec.Body.String(), c.status, c.body)
		}
		if got := rec.Header().Get("Retry-After"); got != c.retryAfter {
			t.Errorf("%s: Retry-After = %q, want %q", c.name, got, c.retryAfter)
		}
		for _, part := range []string{"es-internal", "9200", "secret_field"} {
			if strings.Contains(rec.Body.String(), part) || strings.Contains(logs.String(), part) {
				t.Errorf("%s: %q leaked into the body or log", c.name, part)
			}
		}
	}
}

func TestRuns_UploadErrorsAreFixedMessages(t *testing.T) {
	const name = "customer-acme-secrets.json"
	s := newTestServer(t, Config{Engine: &fakeEngine{}})
	bodies := map[string][]byte{
		"unrecognized": []byte(`{"hello":"world"}`),
		"truncated":    []byte(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"upload-secret-value"`),
		"bad gzip":     append([]byte{0x1f, 0x8b}, []byte("upload-secret-value")...),
	}
	for label, data := range bodies {
		rec := serve(s, postMultipart(t, []part{{field: "files", filename: name, data: data}}))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", label, rec.Code, rec.Body.String())
			continue
		}
		if strings.Contains(rec.Body.String(), "acme") || strings.Contains(rec.Body.String(), "upload-secret-value") {
			t.Errorf("%s: upload value in body %s", label, rec.Body.String())
		}
	}
	// The messages a user needs are still specific.
	rec := serve(s, postRaw(nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(errorBody(t, rec), "no telemetry") {
		t.Errorf("empty upload: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRuns_ReportReasonsAreFixed(t *testing.T) {
	const leak = "http://es-internal:9200 secret_field"
	eng := &fakeEngine{}
	eng.rowsErr = errors.New("HTTP 400: " + leak)
	s := newTestServer(t, Config{Engine: eng, Score: samplescore.Score})
	rec := serve(s, postRaw(readDemo(t)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "es-internal") || strings.Contains(rec.Body.String(), "secret_field") {
		t.Fatalf("engine text in the report: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "query failed (HTTP 400)") {
		t.Fatalf("no classified reason in the report")
	}
}

func TestRuns_BusyEngineAbortsWholeRun(t *testing.T) {
	eng := &fakeEngine{}
	eng.rowsErr = fmt.Errorf("HTTP 429: %w", engine.ErrBusy)
	s := newTestServer(t, Config{Engine: eng, Score: samplescore.Score})
	rec := serve(s, postRaw(readDemo(t)))
	if rec.Code != http.StatusServiceUnavailable || errorBody(t, rec) != "busy, try again shortly" || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("busy run = %d %s (Retry-After %q)", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
	}
	if eng.purgeCount() != 1 {
		t.Fatalf("Purge called %d times, want 1", eng.purgeCount())
	}
}

func TestRuns_CacheHeaders(t *testing.T) {
	for _, hsts := range []bool{false, true} {
		s := newTestServer(t, Config{Engine: &fakeEngine{}, Score: fakeScore(nil, nil, nil, nil), HSTS: hsts, Static: fstest.MapFS{"index.html": {Data: []byte("x")}}})
		reqs := map[string]*http.Request{
			"run":    postRaw(readDemo(t)),
			"rules":  httptest.NewRequest(http.MethodGet, "/api/v1/rules", nil),
			"limits": httptest.NewRequest(http.MethodGet, "/api/v1/limits", nil),
			"404":    httptest.NewRequest(http.MethodGet, "/api/v1/nope", nil),
		}
		for name, req := range reqs {
			rec := serve(s, req)
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("hsts=%v %s: Cache-Control = %q", hsts, name, got)
			}
			want := ""
			if hsts {
				want = "max-age=31536000"
			}
			if got := rec.Header().Get("Strict-Transport-Security"); got != want {
				t.Errorf("hsts=%v %s: Strict-Transport-Security = %q, want %q", hsts, name, got, want)
			}
		}
		if got := serve(s, httptest.NewRequest(http.MethodGet, "/", nil)).Header().Get("Cache-Control"); got == "no-store" {
			t.Errorf("static page carries no-store")
		}
	}
}

func TestLimitsEndpoint(t *testing.T) {
	s := newTestServer(t, Config{Engine: &fakeEngine{}, RunTimeout: 90 * time.Second})
	rec := serve(s, httptest.NewRequest(http.MethodGet, "/api/v1/limits", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got map[string]int64
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{
		"max_compressed_bytes":   otlpin.DefaultLimits.MaxCompressedBytes,
		"max_decompressed_bytes": otlpin.DefaultLimits.MaxDecompressedBytes,
		"max_items":              int64(otlpin.DefaultLimits.MaxItems),
		"max_files":              int64(otlpin.DefaultLimits.MaxFiles),
		"run_timeout_seconds":    90,
	}
	if len(got) != len(want) {
		t.Errorf("limits = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
}

// A run that outlives the server's initial WriteTimeout still gets its
// response, because the handler extends the deadline.
func TestServe_WriteDeadline(t *testing.T) {
	h := newTestServer(t, Config{
		Engine: &fakeEngine{},
		Score: func(ctx context.Context, eng engine.Engine, s *otlpin.Sample, opts samplescore.Options) (*evaluate.Report, error) {
			time.Sleep(600 * time.Millisecond)
			return &evaluate.Report{Sample: &evaluate.SampleInfo{RunID: "run-slow"}}, nil
		},
	})
	srv := httptest.NewUnstartedServer(h)
	srv.Config.WriteTimeout = 200 * time.Millisecond
	srv.Start()
	defer srv.Close()

	res, err := http.Post(srv.URL+"/api/v1/runs", "application/gzip", bytes.NewReader(readDemo(t)))
	if err != nil {
		t.Fatalf("slow run got no response: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
}

func TestRuns_BusyFieldDiscoveryAbortsWholeRun(t *testing.T) {
	eng := &fakeEngine{fieldsErr: fmt.Errorf("field caps (traces) at http://es-internal:9200: HTTP 429: %w", engine.ErrBusy)}
	s := newTestServer(t, Config{Engine: eng, Score: samplescore.Score})
	rec := serve(s, postRaw(readDemo(t)))
	if rec.Code != http.StatusServiceUnavailable || errorBody(t, rec) != "busy, try again shortly" || rec.Header().Get("Retry-After") != "30" {
		t.Fatalf("busy discovery = %d %s (Retry-After %q)", rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
	}
	if eng.purgeCount() != 1 {
		t.Fatalf("Purge called %d times, want 1", eng.purgeCount())
	}
}

// While the engine bootstraps, an upload gets 503 "warming up" with
// Retry-After, and neither takes the run slot nor touches the engine.
func TestRuns_WarmingUp503(t *testing.T) {
	var ready atomic.Bool
	eng := &fakeEngine{}
	s := newTestServer(t, Config{Engine: eng, Ready: ready.Load})

	rec := serve(s, postRaw(readDemo(t)))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "15" {
		t.Fatalf("warming-up run = %d Retry-After %q, want 503 and 15", rec.Code, rec.Header().Get("Retry-After"))
	}
	if msg := errorBody(t, rec); msg != "warming up, try again shortly" {
		t.Fatalf("error = %q", msg)
	}
	if eng.readyCalls != 0 || len(s.sem) != 0 {
		t.Fatalf("engine probed %d times, %d slots held; want 0 and 0", eng.readyCalls, len(s.sem))
	}

	// /readyz answers 503 at once, without probing.
	rec = serve(s, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || eng.readyCalls != 0 {
		t.Fatalf("/readyz = %d after %d probes, want 503 and 0", rec.Code, eng.readyCalls)
	}

	ready.Store(true)
	decodeRun(t, serve(s, postRaw(readDemo(t))))
	if rec := serve(s, httptest.NewRequest(http.MethodGet, "/readyz", nil)); rec.Code != http.StatusOK {
		t.Fatalf("/readyz after bootstrap = %d, want 200", rec.Code)
	}
}
