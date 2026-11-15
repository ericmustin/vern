//go:build integration

// Native OTLP facts on Elasticsearch 9.5.5 (Basic license).
//
// These tests pin how the native OTLP endpoints (POST /_otlp/v1/{traces,logs,metrics})
// behave, because the elastic engine (sanitize, load, reconciliation, purge) is built
// on that behavior. They run against a local container started with `make es-test-up`
// (VERN_TEST_ES_URL, default http://localhost:9200). A failure here after an
// Elasticsearch upgrade means one of these behaviors changed.
package elastic

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/otlpin"
)

var esHTTP = &http.Client{Timeout: 60 * time.Second}

func esURL() string {
	if u := os.Getenv("VERN_TEST_ES_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:9200"
}

// esNamespace returns a fresh lowercase namespace and registers cleanup of
// every data stream whose namespace starts with it (covers "<ns>x" too).
func esNamespace(t *testing.T) string {
	t.Helper()
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random namespace: %v", err)
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	ns := "itest" + string(b)
	t.Cleanup(func() {
		names := esDataStreams(t, "*-*.otel-"+ns+"*")
		if len(names) == 0 {
			return
		}
		status, body := esDo(t, http.MethodDelete, "/_data_stream/"+strings.Join(names, ","), "", nil)
		if status != http.StatusOK {
			t.Errorf("cleanup: delete data streams %v: %d %s", names, status, body)
		}
	})
	return ns
}

func esDo(t *testing.T, method, path, contentType string, body []byte) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, esURL()+path, r)
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, path, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := esHTTP.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, out
}

func esDoJSON(t *testing.T, method, path string, v any) (int, []byte) {
	t.Helper()
	var body []byte
	if v != nil {
		var err error
		if body, err = json.Marshal(v); err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	return esDo(t, method, path, "application/json", body)
}

// esDataStreams lists data stream names matching pattern (empty when none).
func esDataStreams(t *testing.T, pattern string) []string {
	t.Helper()
	status, body := esDo(t, http.MethodGet, "/_data_stream/"+pattern+"?expand_wildcards=all", "", nil)
	if status == http.StatusNotFound {
		return nil
	}
	if status != http.StatusOK {
		t.Fatalf("GET _data_stream/%s: %d %s", pattern, status, body)
	}
	var parsed struct {
		DataStreams []struct {
			Name string `json:"name"`
		} `json:"data_streams"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode data streams: %v", err)
	}
	var names []string
	for _, ds := range parsed.DataStreams {
		names = append(names, ds.Name)
	}
	return names
}

func esRefresh(t *testing.T, ns string) {
	t.Helper()
	status, body := esDo(t, http.MethodPost, "/*-*.otel-"+ns+"*/_refresh?allow_no_indices=true&ignore_unavailable=true&expand_wildcards=all", "", nil)
	if status != http.StatusOK {
		t.Fatalf("refresh: %d %s", status, body)
	}
}

type esqlResult struct {
	Columns []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"columns"`
	Values [][]any `json:"values"`
}

// esQuery runs a query; a non-200 is returned as an error with the body text.
func esQuery(t *testing.T, query string) (*esqlResult, error) {
	t.Helper()
	status, body := esDoJSON(t, http.MethodPost, "/_query?format=json", map[string]string{"query": query})
	if status != http.StatusOK {
		return nil, fmt.Errorf("esql %d: %s", status, body)
	}
	var res esqlResult
	if err := json.Unmarshal(body, &res); err != nil {
		return nil, fmt.Errorf("decode esql: %w", err)
	}
	return &res, nil
}

// esCount returns COUNT(*) over pattern, or an error (e.g. unknown index).
func esCount(t *testing.T, pattern string) (int, error) {
	t.Helper()
	res, err := esQuery(t, "FROM "+pattern+" | STATS c = COUNT(*)")
	if err != nil {
		return 0, err
	}
	if len(res.Values) != 1 || len(res.Values[0]) != 1 {
		return 0, fmt.Errorf("unexpected esql shape: %+v", res.Values)
	}
	n, ok := res.Values[0][0].(float64)
	if !ok {
		return 0, fmt.Errorf("count is %T", res.Values[0][0])
	}
	return int(n), nil
}

// esSearch returns the _source of up to 100 docs in pattern.
func esSearch(t *testing.T, pattern string) []map[string]any {
	t.Helper()
	status, body := esDo(t, http.MethodGet, "/"+pattern+"/_search?size=100&expand_wildcards=all", "", nil)
	if status != http.StatusOK {
		t.Fatalf("search %s: %d %s", pattern, status, body)
	}
	var parsed struct {
		Hits struct {
			Hits []struct {
				Index  string         `json:"_index"`
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	var out []map[string]any
	for _, h := range parsed.Hits.Hits {
		h.Source["_index"] = h.Index
		out = append(out, h.Source)
	}
	return out
}

func esPost(t *testing.T, signal string, payload []byte) (int, []byte) {
	t.Helper()
	return esDo(t, http.MethodPost, "/_otlp/v1/"+signal, "application/x-protobuf", payload)
}

func esTraces(ns string, spans int) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "itest-svc")
	rs.Resource().Attributes().PutStr("data_stream.namespace", ns)
	ss := rs.ScopeSpans().AppendEmpty()
	ss.Scope().SetName("itest")
	now := time.Now()
	for i := 0; i < spans; i++ {
		sp := ss.Spans().AppendEmpty()
		sp.SetName(fmt.Sprintf("span-%d", i))
		sp.SetTraceID(pcommon.TraceID([16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, byte(i + 1)}))
		sp.SetSpanID(pcommon.SpanID([8]byte{1, 2, 3, 4, 5, 6, 7, byte(i + 1)}))
		sp.SetKind(ptrace.SpanKindServer)
		sp.SetStartTimestamp(pcommon.NewTimestampFromTime(now.Add(-time.Second)))
		sp.SetEndTimestamp(pcommon.NewTimestampFromTime(now))
	}
	return td
}

func esMarshalTraces(t *testing.T, td ptrace.Traces) []byte {
	t.Helper()
	b, err := ptraceotlp.NewExportRequestFromTraces(td).MarshalProto()
	if err != nil {
		t.Fatalf("marshal traces: %v", err)
	}
	return b
}

func esMarshalLogs(t *testing.T, ld plog.Logs) []byte {
	t.Helper()
	b, err := plogotlp.NewExportRequestFromLogs(ld).MarshalProto()
	if err != nil {
		t.Fatalf("marshal logs: %v", err)
	}
	return b
}

func esMarshalMetrics(t *testing.T, md pmetric.Metrics) []byte {
	t.Helper()
	b, err := pmetricotlp.NewExportRequestFromMetrics(md).MarshalProto()
	if err != nil {
		t.Fatalf("marshal metrics: %v", err)
	}
	return b
}

func esTraceResponse(t *testing.T, body []byte) ptraceotlp.ExportResponse {
	t.Helper()
	resp := ptraceotlp.NewExportResponse()
	if err := resp.UnmarshalProto(body); err != nil {
		t.Fatalf("unmarshal trace response %q: %v", body, err)
	}
	return resp
}

func esMetricResponse(t *testing.T, body []byte) pmetricotlp.ExportResponse {
	t.Helper()
	resp := pmetricotlp.NewExportResponse()
	if err := resp.UnmarshalProto(body); err != nil {
		t.Fatalf("unmarshal metric response %q: %v", body, err)
	}
	return resp
}

type gaugeSpec struct {
	name  string
	attrs map[string]string
}

// esGauge appends one resource (namespace ns) with one gauge point per spec,
// all at the same timestamp.
func esGauge(md pmetric.Metrics, ns string, at time.Time, metrics ...gaugeSpec) {
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "itest-svc")
	rm.Resource().Attributes().PutStr("data_stream.namespace", ns)
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("itest")
	for i, m := range metrics {
		mm := sm.Metrics().AppendEmpty()
		mm.SetName(m.name)
		dp := mm.SetEmptyGauge().DataPoints().AppendEmpty()
		dp.SetTimestamp(pcommon.NewTimestampFromTime(at))
		dp.SetDoubleValue(float64(i + 1))
		for k, v := range m.attrs {
			dp.Attributes().PutStr(k, v)
		}
	}
}

func esMustCount(t *testing.T, pattern string) int {
	t.Helper()
	n, err := esCount(t, pattern)
	if err != nil {
		t.Fatalf("count %s: %v", pattern, err)
	}
	return n
}

func esRequireStatus200EmptyPartialSuccess(t *testing.T, signal string, status int, body []byte) {
	t.Helper()
	if status != http.StatusOK {
		t.Fatalf("POST /_otlp/v1/%s: status %d, body %q", signal, status, body)
	}
	// Fact: a fully accepted request answers 200 with an empty protobuf body
	// (an ExportResponse with no partial_success).
	if len(body) != 0 {
		t.Fatalf("POST /_otlp/v1/%s: want empty 200 body, got %q", signal, body)
	}
}

func esEqualStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const lookBackTemplate = `{"template":{"settings":{"index.look_back_time":"7d","index.number_of_replicas":0}},"_meta":{"managed_by":"vern"}}`

// esGetComponentTemplate returns the stored component_template body, or nil on 404.
func esGetComponentTemplate(t *testing.T, name string) json.RawMessage {
	t.Helper()
	status, body := esDo(t, http.MethodGet, "/_component_template/"+name, "", nil)
	if status == http.StatusNotFound {
		return nil
	}
	if status != http.StatusOK {
		t.Fatalf("GET _component_template/%s: %d %s", name, status, body)
	}
	var parsed struct {
		ComponentTemplates []struct {
			ComponentTemplate json.RawMessage `json:"component_template"`
		} `json:"component_templates"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.ComponentTemplates) != 1 {
		t.Fatalf("decode component template %s: %v %s", name, err, body)
	}
	// GET returns system-managed fields that a PUT rejects (400
	// action_request_validation_exception), so drop them to allow a restore.
	var tmpl map[string]any
	if err := json.Unmarshal(parsed.ComponentTemplates[0].ComponentTemplate, &tmpl); err != nil {
		t.Fatalf("decode component template body %s: %v", name, err)
	}
	delete(tmpl, "created_date_millis")
	delete(tmpl, "modified_date_millis")
	out, err := json.Marshal(tmpl)
	if err != nil {
		t.Fatalf("re-encode component template %s: %v", name, err)
	}
	return out
}

// esWithoutLookBackTemplate makes sure metrics-otel@custom is absent for the
// test, restoring a pre-existing one (e.g. created by the engine's Ready) afterwards.
func esWithoutLookBackTemplate(t *testing.T) {
	t.Helper()
	existing := esGetComponentTemplate(t, "metrics-otel@custom")
	if existing == nil {
		return
	}
	status, body := esDo(t, http.MethodDelete, "/_component_template/metrics-otel@custom", "", nil)
	if status != http.StatusOK {
		t.Fatalf("delete metrics-otel@custom: %d %s", status, body)
	}
	t.Cleanup(func() {
		status, body := esDo(t, http.MethodPut, "/_component_template/metrics-otel@custom", "application/json", existing)
		if status != http.StatusOK {
			t.Errorf("restore metrics-otel@custom: %d %s", status, body)
		}
	})
}

// esFailureErrorTypes returns error.type for each failure-store doc under pattern.
func esFailureErrorTypes(t *testing.T, pattern string) []string {
	t.Helper()
	res, err := esQuery(t, "FROM "+pattern+"::failures | KEEP error.type, error.message")
	if err != nil {
		t.Fatalf("read failure store %s: %v", pattern, err)
	}
	var out []string
	for _, row := range res.Values {
		t.Logf("failure store %s: error.type=%v error.message=%v", pattern, row[0], row[1])
		s, _ := row[0].(string)
		out = append(out, s)
	}
	return out
}

// TestNativeOTLP_RejectsJSON pins that the endpoint is protobuf-only.
// Observed (facts file): application/json → 406 with
// `Content-Type header [application/json] is not supported`; JSON bytes sent as
// application/x-protobuf → 400 `Invalid OTLP protobuf payload: ...` (the 400 body
// is a protobuf google.rpc.Status, not JSON). Nothing is indexed in either case.
func TestNativeOTLP_RejectsJSON(t *testing.T) {
	ns := esNamespace(t)
	jsonBody, err := ptraceotlp.NewExportRequestFromTraces(esTraces(ns, 1)).MarshalJSON()
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}

	status, body := esDo(t, http.MethodPost, "/_otlp/v1/traces", "application/json", jsonBody)
	t.Logf("application/json: status %d body %s", status, body)
	if status < 400 || status > 499 {
		t.Fatalf("want 4xx for application/json, got %d", status)
	}
	if status != http.StatusNotAcceptable || !strings.Contains(string(body), "Content-Type header [application/json] is not supported") {
		t.Errorf("want 406 'Content-Type header [application/json] is not supported', got %d %s", status, body)
	}

	status, body = esDo(t, http.MethodPost, "/_otlp/v1/traces", "application/x-protobuf", jsonBody)
	t.Logf("json bytes as application/x-protobuf: status %d body %q", status, body)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "Invalid OTLP protobuf payload") {
		t.Errorf("want 400 'Invalid OTLP protobuf payload', got %d %q", status, body)
	}

	if names := esDataStreams(t, "*-*.otel-"+ns+"*"); len(names) != 0 {
		t.Errorf("rejected requests created data streams: %v", names)
	}
}

// TestNativeOTLP_RoutesByNamespaceResourceAttribute is the gate for run-scoped
// ingest. Observed (facts file): resource attribute data_stream.namespace=<ns>
// routes spans to exactly traces-generic.otel-<ns>; the attribute is consumed
// (not kept under resource.attributes) and surfaces as data_stream.namespace.
func TestNativeOTLP_RoutesByNamespaceResourceAttribute(t *testing.T) {
	ns := esNamespace(t)
	status, body := esPost(t, "traces", esMarshalTraces(t, esTraces(ns, 3)))
	esRequireStatus200EmptyPartialSuccess(t, "traces", status, body)
	if rejected := esTraceResponse(t, body).PartialSuccess().RejectedSpans(); rejected != 0 {
		t.Fatalf("rejected_spans = %d", rejected)
	}
	esRefresh(t, ns)

	names := esDataStreams(t, "traces-*.otel-"+ns)
	t.Logf("data streams: %v", names)
	if len(names) < 1 {
		t.Fatalf("GET _data_stream/traces-*.otel-%s listed no streams", ns)
	}
	if want := []string{"traces-generic.otel-" + ns}; !esEqualStrings(names, want) {
		t.Errorf("streams = %v, want %v", names, want)
	}
	if n := esMustCount(t, "traces-*.otel-"+ns); n != 3 {
		t.Fatalf("FROM traces-*.otel-%s | STATS c = COUNT(*) = %d, want 3", ns, n)
	}
	// The failure store is enabled on OTel data streams; with no failures the
	// ::failures selector counts 0 without error.
	if n := esMustCount(t, "traces-*.otel-"+ns+"::failures"); n != 0 {
		t.Errorf("failure store count = %d, want 0", n)
	}
	for _, doc := range esSearch(t, "traces-*.otel-"+ns) {
		ds, _ := doc["data_stream"].(map[string]any)
		if ds["namespace"] != ns || ds["type"] != "traces" || ds["dataset"] != "generic.otel" {
			t.Errorf("data_stream = %v, want namespace %s, type traces, dataset generic.otel", ds, ns)
		}
		res, _ := doc["resource"].(map[string]any)
		attrs, _ := res["attributes"].(map[string]any)
		if _, ok := attrs["data_stream.namespace"]; ok {
			t.Errorf("resource.attributes kept data_stream.namespace: %v", attrs)
		}
	}
}

// TestNativeOTLP_RecordLevelRoutingAttributes pins that routing attributes below
// the resource ESCAPE the resource namespace, which is why sanitize strips them at
// every level. Observed (facts file), with resource namespace <ns>:
//   - span attribute data_stream.namespace=<ns>x → span in traces-generic.otel-<ns>x
//   - span attribute data_stream.dataset=custom → span in traces-custom.otel-<ns>
//   - span attribute elasticsearch.index=<name> → span in that data stream
//   - scope attribute data_stream.namespace → reroutes the scope's spans
//   - span event attribute data_stream.namespace → reroutes the event's log doc
//   - events of a span rerouted by a span attribute stay in logs-generic.otel-<ns>
//   - log record attribute data_stream.namespace → reroutes the log doc
//   - data point attribute data_stream.namespace → reroutes the metric doc
func TestNativeOTLP_RecordLevelRoutingAttributes(t *testing.T) {
	ns := esNamespace(t)
	now := time.Now()

	td := esTraces(ns, 4)
	spans := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans()
	spans.At(0).Attributes().PutStr("data_stream.namespace", ns+"x")
	ev := spans.At(0).Events().AppendEmpty()
	ev.SetName("event-of-rerouted-span")
	ev.SetTimestamp(pcommon.NewTimestampFromTime(now))
	spans.At(1).Attributes().PutStr("data_stream.dataset", "custom")
	spans.At(2).Attributes().PutStr("elasticsearch.index", "traces-generic.otel-"+ns+"i")
	ev = spans.At(3).Events().AppendEmpty()
	ev.SetName("event-level-namespace")
	ev.SetTimestamp(pcommon.NewTimestampFromTime(now))
	ev.Attributes().PutStr("data_stream.namespace", ns+"e")
	status, body := esPost(t, "traces", esMarshalTraces(t, td))
	esRequireStatus200EmptyPartialSuccess(t, "traces", status, body)

	scoped := esTraces(ns, 1)
	scoped.ResourceSpans().At(0).ScopeSpans().At(0).Scope().Attributes().PutStr("data_stream.namespace", ns+"s")
	status, body = esPost(t, "traces", esMarshalTraces(t, scoped))
	esRequireStatus200EmptyPartialSuccess(t, "traces", status, body)

	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("data_stream.namespace", ns)
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetTimestamp(pcommon.NewTimestampFromTime(now))
	lr.Body().SetStr("record-level namespace")
	lr.Attributes().PutStr("data_stream.namespace", ns+"l")
	status, body = esPost(t, "logs", esMarshalLogs(t, ld))
	esRequireStatus200EmptyPartialSuccess(t, "logs", status, body)

	md := pmetric.NewMetrics()
	esGauge(md, ns, now.Add(-time.Minute), gaugeSpec{"itest.gauge", map[string]string{"data_stream.namespace": ns + "m"}})
	status, body = esPost(t, "metrics", esMarshalMetrics(t, md))
	esRequireStatus200EmptyPartialSuccess(t, "metrics", status, body)

	esRefresh(t, ns)
	got := esDataStreams(t, "*-*.otel-"+ns+"*")
	t.Logf("data streams: %v", got)
	for _, d := range esSearch(t, "*-*.otel-"+ns+"*") {
		t.Logf("doc in %v: name=%v event_name=%v attributes=%v", d["_index"], d["name"], d["event_name"], d["attributes"])
	}

	want := []string{
		"logs-generic.otel-" + ns,          // event of the span rerouted to <ns>x
		"logs-generic.otel-" + ns + "e",    // event-level namespace
		"logs-generic.otel-" + ns + "l",    // log-record-level namespace
		"metrics-generic.otel-" + ns + "m", // data-point-level namespace
		"traces-custom.otel-" + ns,         // span-level dataset
		"traces-generic.otel-" + ns,        // span 3, untouched
		"traces-generic.otel-" + ns + "i",  // span-level elasticsearch.index
		"traces-generic.otel-" + ns + "s",  // scope-level namespace
		"traces-generic.otel-" + ns + "x",  // span-level namespace
	}
	if !esEqualStrings(got, want) {
		t.Fatalf("data streams = %v\nwant %v", got, want)
	}
	checks := map[string]int{
		"traces-*.otel-" + ns:              2, // span 3 + span 1 (traces-custom)
		"traces-generic.otel-" + ns + "x":  1,
		"traces-generic.otel-" + ns + "i":  1,
		"traces-generic.otel-" + ns + "s":  1,
		"logs-generic.otel-" + ns:          1,
		"logs-generic.otel-" + ns + "e":    1,
		"logs-generic.otel-" + ns + "l":    1,
		"metrics-generic.otel-" + ns + "m": 1,
	}
	for pattern, n := range checks {
		if c := esMustCount(t, pattern); c != n {
			t.Errorf("count %s = %d, want %d", pattern, c, n)
		}
	}
}

// TestNativeOTLP_SpanEventsBecomeLogDocs pins that every span event becomes its
// own doc in logs-generic.otel-<ns> (and not a traces doc). Observed (facts file):
// span-event docs carry top-level event_name (also attributes."event.name"),
// trace_id and span_id, and have no observed_timestamp and no body; log records
// carry observed_timestamp and body.text and no event_name.
func TestNativeOTLP_SpanEventsBecomeLogDocs(t *testing.T) {
	ns := esNamespace(t)
	now := time.Now()
	td := esTraces(ns, 1)
	sp := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	for i := 0; i < 2; i++ {
		ev := sp.Events().AppendEmpty()
		ev.SetName(fmt.Sprintf("event-%d", i))
		ev.SetTimestamp(pcommon.NewTimestampFromTime(now))
	}
	status, body := esPost(t, "traces", esMarshalTraces(t, td))
	esRequireStatus200EmptyPartialSuccess(t, "traces", status, body)
	esRefresh(t, ns)

	if n := esMustCount(t, "logs-*.otel-"+ns); n != 2 {
		t.Fatalf("logs-*.otel-%s count = %d, want 2 (one doc per span event)", ns, n)
	}
	if n := esMustCount(t, "traces-*.otel-"+ns); n != 1 {
		t.Fatalf("traces-*.otel-%s count = %d, want 1", ns, n)
	}
	if names := esDataStreams(t, "logs-*.otel-"+ns); !esEqualStrings(names, []string{"logs-generic.otel-" + ns}) {
		t.Errorf("log streams = %v, want [logs-generic.otel-%s]", names, ns)
	}

	// Add one real log record to see what tells the two apart.
	ld := plog.NewLogs()
	rl := ld.ResourceLogs().AppendEmpty()
	rl.Resource().Attributes().PutStr("data_stream.namespace", ns)
	lr := rl.ScopeLogs().AppendEmpty().LogRecords().AppendEmpty()
	lr.SetTimestamp(pcommon.NewTimestampFromTime(now))
	lr.SetObservedTimestamp(pcommon.NewTimestampFromTime(now))
	lr.SetTraceID(sp.TraceID())
	lr.Body().SetStr("a log record")
	status, body = esPost(t, "logs", esMarshalLogs(t, ld))
	esRequireStatus200EmptyPartialSuccess(t, "logs", status, body)
	esRefresh(t, ns)

	if n := esMustCount(t, "logs-*.otel-"+ns); n != 3 {
		t.Fatalf("logs count = %d, want 3 (2 span events + 1 log record)", n)
	}
	var events, records int
	for _, d := range esSearch(t, "logs-*.otel-"+ns) {
		_, hasEventName := d["event_name"]
		_, hasObserved := d["observed_timestamp"]
		_, hasBody := d["body"]
		_, hasTrace := d["trace_id"]
		_, hasSpan := d["span_id"]
		t.Logf("log doc: event_name=%v observed_timestamp=%v body=%v trace_id=%v span_id=%v",
			d["event_name"], d["observed_timestamp"], d["body"], d["trace_id"], d["span_id"])
		switch {
		case hasEventName:
			events++
			if hasObserved || hasBody || !hasTrace || !hasSpan {
				t.Errorf("span-event doc shape changed: %v", d)
			}
			if attrs, _ := d["attributes"].(map[string]any); attrs["event.name"] != d["event_name"] {
				t.Errorf("span-event doc attributes.event.name = %v, event_name = %v", attrs["event.name"], d["event_name"])
			}
		default:
			records++
			if !hasObserved || !hasBody || !hasTrace {
				t.Errorf("log-record doc shape changed: %v", d)
			}
		}
	}
	if events != 2 || records != 1 {
		t.Errorf("span-event docs = %d, log-record docs = %d; want 2 and 1", events, records)
	}
	if n := esMustCount(t, "logs-*.otel-"+ns+" | WHERE event_name IS NOT NULL"); n != 2 {
		t.Errorf("ES|QL span-event docs (event_name IS NOT NULL) = %d, want 2", n)
	}
}

// TestNativeOTLP_MetricPointsPerDoc pins that metric points are NOT 1:1 with docs.
// Observed (facts file): metrics data streams are time_series; points sharing
// resource, scope, attributes and timestamp are grouped into one doc with a
// `metrics.<name>` field per point (plus `_metric_names_hash`). 3 points → 2 docs.
func TestNativeOTLP_MetricPointsPerDoc(t *testing.T) {
	ns := esNamespace(t)
	md := pmetric.NewMetrics()
	esGauge(md, ns, time.Now().Add(-time.Minute),
		gaugeSpec{"itest.a", map[string]string{"k": "same"}},
		gaugeSpec{"itest.b", map[string]string{"k": "same"}},
		gaugeSpec{"itest.c", map[string]string{"k": "other"}},
	)
	status, body := esPost(t, "metrics", esMarshalMetrics(t, md))
	esRequireStatus200EmptyPartialSuccess(t, "metrics", status, body)
	esRefresh(t, ns)

	n := esMustCount(t, "metrics-*.otel-"+ns)
	t.Logf("3 gauge points → %d docs", n)
	if n != 2 {
		t.Fatalf("metrics doc count = %d, want 2 (points with equal attributes and timestamp share a doc)", n)
	}
	var grouped bool
	for _, d := range esSearch(t, "metrics-*.otel-"+ns) {
		m, _ := d["metrics"].(map[string]any)
		t.Logf("metric doc: attributes=%v metrics=%v", d["attributes"], m)
		if _, a := m["itest.a"]; a {
			if _, b := m["itest.b"]; b {
				grouped = true
			}
		}
	}
	if !grouped {
		t.Errorf("no doc holds both itest.a and itest.b")
	}
}

// TestNativeOTLP_OldMetricPointRejectedWithoutLookBack pins how an out-of-range
// metric point fails. Observed (facts file): HTTP 200 with an EMPTY body (no
// partial_success, rejected_data_points 0), nothing indexed, and one failure-store
// doc with error.type `timestamp_error` ("the document timestamp [...] is outside
// of ranges of currently writable indices [...]"). Default writable range is
// about [now-2h, now+9m].
func TestNativeOTLP_OldMetricPointRejectedWithoutLookBack(t *testing.T) {
	esWithoutLookBackTemplate(t)
	ns := esNamespace(t)
	md := pmetric.NewMetrics()
	esGauge(md, ns, time.Now().Add(-3*time.Hour), gaugeSpec{"itest.old", nil})
	status, body := esPost(t, "metrics", esMarshalMetrics(t, md))
	ps := esMetricResponse(t, body).PartialSuccess()
	t.Logf("3h-old point: status %d body %q rejected_data_points %d error_message %q",
		status, body, ps.RejectedDataPoints(), ps.ErrorMessage())
	esRequireStatus200EmptyPartialSuccess(t, "metrics", status, body)
	if ps.RejectedDataPoints() != 0 || ps.ErrorMessage() != "" {
		t.Errorf("partial_success now reports rejections (%d, %q): reconciliation can use it", ps.RejectedDataPoints(), ps.ErrorMessage())
	}
	esRefresh(t, ns)

	if n := esMustCount(t, "metrics-*.otel-"+ns); n != 0 {
		t.Errorf("indexed %d docs, want 0", n)
	}
	failures := esMustCount(t, "metrics-*.otel-"+ns+"::failures")
	t.Logf("FROM metrics-*.otel-%s::failures | STATS c = COUNT(*) = %d", ns, failures)
	if failures != 1 {
		t.Fatalf("failure store count = %d, want 1", failures)
	}
	if types := esFailureErrorTypes(t, "metrics-*.otel-"+ns); !esEqualStrings(types, []string{"timestamp_error"}) {
		t.Errorf("failure error.type = %v, want [timestamp_error]", types)
	}
}

// TestNativeOTLP_OldMetricPointAcceptedWithLookBack pins that the Global
// Constraints metrics-otel@custom template (look_back_time 7d) applies to data
// streams created after it. Observed (facts file): a 3h-old point is indexed; an
// 8-day-old point still goes to the failure store as timestamp_error.
func TestNativeOTLP_OldMetricPointAcceptedWithLookBack(t *testing.T) {
	if esGetComponentTemplate(t, "metrics-otel@custom") == nil {
		status, body := esDo(t, http.MethodPut, "/_component_template/metrics-otel@custom", "application/json", []byte(lookBackTemplate))
		if status != http.StatusOK {
			t.Fatalf("PUT metrics-otel@custom: %d %s", status, body)
		}
		t.Cleanup(func() {
			status, body := esDo(t, http.MethodDelete, "/_component_template/metrics-otel@custom", "", nil)
			if status != http.StatusOK {
				t.Errorf("delete metrics-otel@custom: %d %s", status, body)
			}
		})
	} else {
		t.Log("metrics-otel@custom already present; using it as is")
	}

	ns := esNamespace(t)
	for _, age := range []time.Duration{3 * time.Hour, 8 * 24 * time.Hour} {
		md := pmetric.NewMetrics()
		esGauge(md, ns, time.Now().Add(-age), gaugeSpec{"itest.old", map[string]string{"age": age.String()}})
		status, body := esPost(t, "metrics", esMarshalMetrics(t, md))
		esRequireStatus200EmptyPartialSuccess(t, "metrics", status, body)
	}
	esRefresh(t, ns)

	if n := esMustCount(t, "metrics-*.otel-"+ns); n != 1 {
		t.Fatalf("indexed %d docs, want 1 (the 3h-old point)", n)
	}
	if n := esMustCount(t, "metrics-*.otel-"+ns+" | WHERE attributes.age == \"3h0m0s\""); n != 1 {
		t.Errorf("3h-old point not indexed")
	}
	if types := esFailureErrorTypes(t, "metrics-*.otel-"+ns); !esEqualStrings(types, []string{"timestamp_error"}) {
		t.Errorf("failure error.type = %v, want [timestamp_error] for the 8d-old point", types)
	}
}

// TestNativeOTLP_DeleteDataStreamsByExactName pins the purge sequence and the
// "nothing there" answers. Observed (facts file): GET _data_stream/<wildcard>
// with no match → 200 {"data_streams":[]}; GET of a missing exact name → 404
// index_not_found_exception; ES|QL FROM <wildcard> with no match → COUNT 0, no
// error; FROM <missing exact name> → 400 verification_exception "Unknown index".
func TestNativeOTLP_DeleteDataStreamsByExactName(t *testing.T) {
	ns := esNamespace(t)
	td := esTraces(ns, 1)
	ev := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Events().AppendEmpty()
	ev.SetName("event")
	ev.SetTimestamp(pcommon.NewTimestampFromTime(time.Now()))
	status, body := esPost(t, "traces", esMarshalTraces(t, td))
	esRequireStatus200EmptyPartialSuccess(t, "traces", status, body)

	pattern := "*-*.otel-" + ns
	names := esDataStreams(t, pattern)
	want := []string{"logs-generic.otel-" + ns, "traces-generic.otel-" + ns}
	if !esEqualStrings(names, want) {
		t.Fatalf("streams = %v, want %v", names, want)
	}
	status, body = esDo(t, http.MethodDelete, "/_data_stream/"+strings.Join(names, ","), "", nil)
	t.Logf("DELETE _data_stream/%s: %d %s", strings.Join(names, ","), status, body)
	if status != http.StatusOK || !strings.Contains(string(body), `"acknowledged":true`) {
		t.Fatalf("delete: %d %s", status, body)
	}

	status, body = esDo(t, http.MethodGet, "/_data_stream/"+pattern+"?expand_wildcards=all", "", nil)
	if status != http.StatusOK || strings.TrimSpace(string(body)) != `{"data_streams":[]}` {
		t.Errorf("GET wildcard after delete: %d %s, want 200 {\"data_streams\":[]}", status, body)
	}
	status, body = esDo(t, http.MethodGet, "/_data_stream/"+names[0], "", nil)
	if status != http.StatusNotFound || !strings.Contains(string(body), "index_not_found_exception") {
		t.Errorf("GET exact name after delete: %d %s, want 404 index_not_found_exception", status, body)
	}
	if n, err := esCount(t, "traces-*.otel-"+ns); err != nil || n != 0 {
		t.Errorf("ES|QL on wildcard with no match: count %d err %v, want 0 and no error", n, err)
	}
	if n, err := esCount(t, "traces-*.otel-"+ns+"::failures"); err != nil || n != 0 {
		t.Errorf("ES|QL ::failures on wildcard with no match: count %d err %v, want 0 and no error", n, err)
	}
	_, err := esCount(t, names[1])
	t.Logf("ES|QL on missing exact name: %v", err)
	if err == nil || !strings.Contains(err.Error(), "esql 400") || !strings.Contains(err.Error(), "Unknown index") {
		t.Errorf("ES|QL on missing exact name: err %v, want 400 verification_exception Unknown index", err)
	}
}

// TestNativeOTLP_BodyLimit8MiB pins the request size limit behind the 4 MiB batch
// rule. Observed (facts file): a 9 MiB body → 413 with protobuf Status
// "request body too large, max [8388608] bytes"; a 7 MiB body → 200.
func TestNativeOTLP_BodyLimit8MiB(t *testing.T) {
	ns := esNamespace(t)
	td := esTraces(ns, 1)
	attrs := td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Attributes()

	attrs.PutStr("filler", strings.Repeat("a", 9<<20))
	status, body := esPost(t, "traces", esMarshalTraces(t, td))
	t.Logf("9 MiB: status %d body %q", status, body)
	if status != http.StatusRequestEntityTooLarge || !strings.Contains(string(body), "request body too large, max [8388608] bytes") {
		t.Errorf("9 MiB: got %d %q, want 413 'request body too large, max [8388608] bytes'", status, body)
	}

	attrs.PutStr("filler", strings.Repeat("a", 7<<20))
	status, body = esPost(t, "traces", esMarshalTraces(t, td))
	esRequireStatus200EmptyPartialSuccess(t, "traces", status, body)
}

// --- Engine.Load integration tests ---

func loadTestEngine(t *testing.T) *Engine {
	t.Helper()
	e := New(Config{URL: esURL(), Logf: t.Logf, EnsureTemplate: true})
	if err := e.Ready(t.Context()); err != nil {
		t.Fatalf("ready: %v", err)
	}
	return e
}

func loadTestRunID(t *testing.T, e *Engine) engine.RunID {
	t.Helper()
	id := engine.NewRunID(time.Now())
	t.Cleanup(func() {
		if err := e.Purge(context.Background(), id); err != nil {
			t.Errorf("cleanup purge: %v", err)
		}
	})
	return id
}

func TestEngine_LoadRoundTrip(t *testing.T) {
	f, err := os.Open("../../../web/examples/demo.jsonl.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := otlpin.Decode([]otlpin.Input{{Name: "demo.jsonl.gz", R: f}}, otlpin.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	otlpin.Shift(s, otlpin.OffsetToNow(otlpin.Summarize(s), time.Now()))

	e := loadTestEngine(t)
	id := loadTestRunID(t, e)
	st, err := e.Load(t.Context(), id, s)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Logf("stats: sent=%v indexed=%v failed=%v", st.Sent, st.Indexed, st.Failed)
	if d := st.Dropped(); len(d) != 0 {
		t.Fatalf("dropped = %v (sent=%v indexed=%v failed=%v)", d, st.Sent, st.Indexed, st.Failed)
	}
	if st.Sent["traces"] == 0 || st.Sent["logs"] == 0 || st.Sent["metrics"] == 0 {
		t.Errorf("demo sample should have all three signals: %v", st.Sent)
	}

	if err := e.Purge(t.Context(), id); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if left := esDataStreams(t, "*-*.otel-"+string(id)); len(left) != 0 {
		t.Errorf("streams remain after purge: %v", left)
	}
}

func TestEngine_LoadDisclosesOldMetricPoints(t *testing.T) {
	now := time.Now()
	s := &otlpin.Sample{Traces: ptrace.NewTraces(), Logs: plog.NewLogs(), Metrics: pmetric.NewMetrics()}
	rm := s.Metrics.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "oldpoints")
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("old.gauge")
	dps := m.SetEmptyGauge().DataPoints()
	newest := dps.AppendEmpty()
	newest.SetDoubleValue(1)
	newest.SetTimestamp(pcommon.NewTimestampFromTime(now))
	old := dps.AppendEmpty()
	old.SetDoubleValue(2)
	old.SetTimestamp(pcommon.NewTimestampFromTime(now.Add(-8 * 24 * time.Hour)))
	otlpin.Shift(s, otlpin.OffsetToNow(otlpin.Summarize(s), now))

	e := loadTestEngine(t)
	id := loadTestRunID(t, e)
	st, err := e.Load(t.Context(), id, s)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.Dropped()["metrics"] <= 0 {
		t.Fatalf("old metric point not disclosed: sent=%v indexed=%v failed=%v", st.Sent, st.Indexed, st.Failed)
	}
	if st.Failed["metrics"] != 1 {
		t.Errorf("failed[metrics] = %d, want 1", st.Failed["metrics"])
	}
}
