package elastic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/mappings"
)

// caps builds a _field_caps body from field -> comma-separated types.
func caps(fields map[string]string) string {
	out := map[string]map[string]map[string]any{}
	for f, ts := range fields {
		out[f] = map[string]map[string]any{}
		for _, ty := range strings.Split(ts, ",") {
			out[f][ty] = map[string]any{"type": ty, "searchable": true}
		}
	}
	b, _ := json.Marshal(map[string]any{"indices": []string{"x"}, "fields": out})
	return string(b)
}

// fieldsServer answers _field_caps per signal pattern and records requests.
func fieldsServer(t *testing.T, traces, logs, metrics map[string]string) (*Engine, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var reqs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		var body string
		switch {
		case strings.HasPrefix(r.URL.Path, "/traces-"):
			body = caps(traces)
		case strings.HasPrefix(r.URL.Path, "/logs-"):
			body = caps(logs)
		case strings.HasPrefix(r.URL.Path, "/metrics-"):
			body = caps(metrics)
		default:
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(Config{URL: srv.URL}), &reqs
}

// Field-caps types as Elasticsearch 9.5.5 reports them, including the
// passthrough and ECS aliases that must not be scanned twice.
var (
	fcTraces = map[string]string{
		"resource.attributes.sample.res.str":        "keyword",
		"sample.res.str":                            "keyword", // passthrough alias
		"resource.attributes.sample.res.int":        "long",
		"resource.attributes.sample.res.map":        "flattened",
		"resource.attributes.sample.res.ts":         "date", // not scannable
		"resource.attributes.sample.res.conflict":   "keyword,date",
		"resource.attributes.sample.res.str.nested": "keyword",
		"scope.name":                        "keyword",
		"scope.version":                     "version",
		"scope.attributes.sample.scope.str": "keyword",
		"name":                              "keyword",
		"span.name":                         "keyword", // ECS alias
		"status.message":                    "keyword",
		"attributes.sample.span.str":        "keyword",
		"attributes.sample.span.int":        "long",
		"attributes.sample.span.map":        "flattened",
		"attributes.sample.ip":              "ip",
		"attributes.sample.timestamp":       "date",
		"attributes.conflict":               "keyword,date",
		"attributes.url.full":               "keyword",
		"attributes.url.full.text":          "text",
		"url.full":                          "keyword", // alias
		"links.attributes.sample.link.str":  "keyword",
		"links.attributes.sample.link.int":  "long",
		"@timestamp":                        "date",
		"_id":                               "_id",
		"attributes":                        "object",
		"duration":                          "long", // not a canonical field
	}
	fcLogs = map[string]string{
		"resource.attributes.sample.res.str": "keyword",
		"resource.attributes.log.only":       "keyword",
		"scope.name":                         "keyword",
		"scope.version":                      "version",
		"event_name":                         "keyword",
		"attributes.event.name":              "keyword",
		"attributes.sample.event.str":        "keyword",
		"attributes.sample.log.str":          "keyword",
		"attributes.sample.timestamp":        "keyword",
		"body.text":                          "text",
		"message":                            "text", // alias
		"body.structured":                    "flattened",
	}
	fcMetrics = map[string]string{
		"resource.attributes.sample.res.str":     "keyword",
		"scope.name":                             "keyword",
		"unit":                                   "keyword",
		"attributes.sample.dp.str":               "keyword",
		"attributes.sample.dp.map":               "flattened",
		"metrics.orders.alice@example.com.count": "double",
		"orders.alice@example.com.count":         "double", // passthrough alias
		"metrics.http.server.duration":           "histogram",
		"metrics.group":                          "object",
	}
)

func TestFields_ClassifiesAndCaps(t *testing.T) {
	traces := map[string]string{}
	for k, v := range fcTraces {
		traces[k] = v
	}
	for i := 0; i < 2000; i++ {
		traces[fmt.Sprintf("attributes.zk%04d", i)] = "keyword"
	}
	e, reqs := fieldsServer(t, traces, fcLogs, fcMetrics)
	f, err := e.Fields(context.Background(), "vern-test")
	if err != nil {
		t.Fatal(err)
	}

	// 9 real canonical span fields + 2000 synthetic.
	if len(f.Span) != mappings.FieldCap || f.Dropped["Span"] != 2000+9-mappings.FieldCap {
		t.Errorf("Span len=%d dropped=%d, want %d and %d", len(f.Span), f.Dropped["Span"], mappings.FieldCap, 2000+9-mappings.FieldCap)
	}
	if len(f.Dropped) != 1 {
		t.Errorf("Dropped = %v, want only Span", f.Dropped)
	}
	// The cap truncates in priority order (fixed fields, links, then
	// attributes), so the fixed fields survive and only attributes overflow.
	want := []string{"name", "status.message", "links.attributes.sample.link.int", "links.attributes.sample.link.str",
		"attributes.sample.span.int", "attributes.sample.span.map", "attributes.sample.span.str",
		"attributes.url.full", "attributes.url.full.text"}
	for i := 0; i < 2000; i++ {
		want = append(want, fmt.Sprintf("attributes.zk%04d", i))
	}
	if !reflect.DeepEqual(f.Span, want[:mappings.FieldCap]) {
		t.Errorf("Span is not the first %d fields in priority order; head=%v", mappings.FieldCap, f.Span[:4])
	}
	for _, w := range []string{"name", "status.message"} {
		if !contains(f.Span, w) {
			t.Errorf("fixed field %s lost to the cap", w)
		}
	}
	for _, bad := range []string{"span.name", "url.full", "sample.res.str", "attributes.sample.ip", "attributes.sample.timestamp", "attributes.conflict", "duration", "@timestamp", "_id", "attributes"} {
		if contains(f.Span, bad) {
			t.Errorf("Span wrongly has %s", bad)
		}
	}
	for _, list := range [][]string{f.Resource, f.Scope, f.Span, f.SpanEvent, f.Log, f.Metric, f.MetricNames} {
		for i := 1; i < len(list); i++ {
			if mappings.FieldPriority(list[i-1]) > mappings.FieldPriority(list[i]) || (mappings.FieldPriority(list[i-1]) == mappings.FieldPriority(list[i]) && list[i-1] >= list[i]) {
				t.Fatalf("list not in priority order/unique at %q, %q", list[i-1], list[i])
			}
		}
	}

	if want := []string{
		"resource.attributes.log.only",
		"resource.attributes.sample.res.int",
		"resource.attributes.sample.res.map",
		"resource.attributes.sample.res.str",
		"resource.attributes.sample.res.str.nested",
	}; !reflect.DeepEqual(f.Resource, want) {
		t.Errorf("Resource = %v, want %v", f.Resource, want)
	}
	if want := []string{"scope.name", "scope.version", "scope.attributes.sample.scope.str"}; !reflect.DeepEqual(f.Scope, want) {
		t.Errorf("Scope = %v, want %v", f.Scope, want)
	}
	if want := []string{"event_name", "attributes.event.name", "attributes.sample.event.str", "attributes.sample.log.str", "attributes.sample.timestamp"}; !reflect.DeepEqual(f.SpanEvent, want) {
		t.Errorf("SpanEvent = %v, want %v", f.SpanEvent, want)
	}
	if want := []string{"body.structured", "body.text", "event_name", "attributes.event.name", "attributes.sample.event.str", "attributes.sample.log.str", "attributes.sample.timestamp"}; !reflect.DeepEqual(f.Log, want) {
		t.Errorf("Log = %v, want %v", f.Log, want)
	}
	if want := []string{"unit", "attributes.sample.dp.map", "attributes.sample.dp.str"}; !reflect.DeepEqual(f.Metric, want) {
		t.Errorf("Metric = %v, want %v", f.Metric, want)
	}
	if want := []string{"http.server.duration", "orders.alice@example.com.count"}; !reflect.DeepEqual(f.MetricNames, want) {
		t.Errorf("MetricNames = %v, want %v", f.MetricNames, want)
	}

	// One request per signal, scoped to the run, without empty fields.
	if len(*reqs) != 3 {
		t.Fatalf("requests = %v, want 3", *reqs)
	}
	for _, r := range *reqs {
		if !strings.HasPrefix(r, "GET ") || !strings.Contains(r, "otel-vern-test/_field_caps") ||
			!strings.Contains(r, "include_empty_fields=false") || !strings.Contains(r, "fields=%2A") {
			t.Errorf("request %q", r)
		}
	}
}

func TestFields_CapsMetricNamesAndResource(t *testing.T) {
	m := map[string]string{}
	r := map[string]string{}
	for i := 0; i < 1100; i++ {
		m[fmt.Sprintf("metrics.m%04d", i)] = "double"
		r[fmt.Sprintf("resource.attributes.r%04d", i)] = "keyword"
	}
	e, _ := fieldsServer(t, r, nil, m)
	f, err := e.Fields(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.MetricNames) != mappings.FieldCap || f.Dropped["MetricNames"] != 100 ||
		len(f.Resource) != mappings.FieldCap || f.Dropped["Resource"] != 100 {
		t.Errorf("names=%d/%d resource=%d/%d", len(f.MetricNames), f.Dropped["MetricNames"], len(f.Resource), f.Dropped["Resource"])
	}
}

func TestFields_FixedFieldsSurviveCapInLogsAndMetrics(t *testing.T) {
	l := map[string]string{"body.text": "text", "body.structured": "flattened", "event_name": "keyword"}
	m := map[string]string{"unit": "keyword"}
	for i := 0; i < 1200; i++ {
		l[fmt.Sprintf("attributes.a%04d", i)] = "keyword"
		m[fmt.Sprintf("attributes.a%04d", i)] = "keyword"
	}
	e, _ := fieldsServer(t, nil, l, m)
	f, err := e.Fields(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Log[:3], []string{"body.structured", "body.text", "event_name"}) || f.Dropped["Log"] != 203 {
		t.Errorf("Log head=%v dropped=%d", f.Log[:3], f.Dropped["Log"])
	}
	if f.Metric[0] != "unit" || f.Dropped["Metric"] != 201 {
		t.Errorf("Metric head=%v dropped=%d", f.Metric[0], f.Dropped["Metric"])
	}
	if f.Dropped["SpanEvent"] != 201 || f.SpanEvent[0] != "event_name" {
		t.Errorf("SpanEvent head=%v dropped=%d", f.SpanEvent[0], f.Dropped["SpanEvent"])
	}
}

func TestFields_SkipsUnscannableTypes(t *testing.T) {
	e, _ := fieldsServer(t, map[string]string{
		"attributes.ip":     "ip",
		"attributes.ts":     "date",
		"attributes.f":      "float",
		"attributes.d":      "double",
		"attributes.b":      "boolean",
		"attributes.conf":   "keyword,date",
		"attributes.ok":     "keyword",
		"attributes.n":      "long",
		"attributes.obj":    "object",
		"attributes.scaled": "scaled_float",
	}, nil, nil)
	f, err := e.Fields(context.Background(), "r")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"attributes.n", "attributes.ok"}; !reflect.DeepEqual(f.Span, want) {
		t.Errorf("Span = %v, want %v", f.Span, want)
	}
	if f.Dropped != nil {
		t.Errorf("Dropped = %v, want nil", f.Dropped)
	}
}

func TestFields_ErrorsWithoutLeakingURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/logs-") {
			w.WriteHeader(500)
			_, _ = w.Write([]byte("nope"))
			return
		}
		_, _ = w.Write([]byte(caps(nil)))
	}))
	defer srv.Close()
	e := New(Config{URL: strings.Replace(srv.URL, "http://", "http://user:secret@", 1)})
	_, err := e.Fields(context.Background(), engine.RunID("r"))
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
