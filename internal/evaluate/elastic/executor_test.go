package elastic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExecute_HappyPath(t *testing.T) {
	var gotBody, gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"columns":[
				{"name":"rule_passed","type":"boolean"},
				{"name":"service.name","type":"keyword"},
				{"name":"example","type":"keyword"},
				{"name":"extent","type":"double"}
			],
			"values":[
				[true,"svc-a","",0.0],
				[false,"svc-b","trace-1",0.42]
			]
		}`))
	}))
	defer srv.Close()

	ex := New(srv.URL, "abc123")
	rows, err := ex.Execute(context.Background(), "FROM traces-* | KEEP rule_passed, service.name, example, extent")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].ServiceName != "svc-a" || !rows[0].Passed || rows[0].Extent != 0 {
		t.Errorf("row[0] = %+v", rows[0])
	}
	if rows[1].ServiceName != "svc-b" || rows[1].Passed || rows[1].Extent != 0.42 || rows[1].Example != "trace-1" {
		t.Errorf("row[1] = %+v", rows[1])
	}
	if !strings.Contains(gotBody, "FROM traces-*") {
		t.Errorf("server saw body %q", gotBody)
	}
	if gotAuth != "ApiKey abc123" {
		t.Errorf("auth header = %q, want 'ApiKey abc123'", gotAuth)
	}
	if !strings.HasPrefix(gotPath, "/_query?format=json") {
		t.Errorf("path = %q", gotPath)
	}
}

func TestExecute_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"reason":"missing authentication"}}`))
	}))
	defer srv.Close()

	ex := New(srv.URL, "")
	_, err := ex.Execute(context.Background(), "FROM x")
	if err == nil {
		t.Fatal("Execute: want error, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("err = %v", err)
	}
}

func TestExecute_EmptyBaseURL(t *testing.T) {
	ex := New("", "")
	if _, err := ex.Execute(context.Background(), "FROM x"); err == nil {
		t.Error("want error for empty BaseURL")
	}
}

func TestExecute_ColumnOrderIndependence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Reverse the canonical order — executor should look up by name.
		_, _ = w.Write([]byte(`{
			"columns":[
				{"name":"extent","type":"double"},
				{"name":"example","type":"keyword"},
				{"name":"service.name","type":"keyword"},
				{"name":"rule_passed","type":"boolean"}
			],
			"values":[
				[0.5,"sample-id","svc-c",false]
			]
		}`))
	}))
	defer srv.Close()

	ex := New(srv.URL, "")
	rows, err := ex.Execute(context.Background(), "FROM x")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rows[0].ServiceName != "svc-c" || rows[0].Passed || rows[0].Extent != 0.5 || rows[0].Example != "sample-id" {
		t.Errorf("row = %+v", rows[0])
	}
}

const okBody = `{"columns":[{"name":"rule_passed","type":"boolean"},{"name":"service.name","type":"keyword"},{"name":"extent","type":"double"}],"values":[[true,"a",0.0]]}`

func TestExecute_DisallowsPartialResults(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("allow_partial_results")
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "").Execute(context.Background(), "FROM x"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got != "false" {
		t.Errorf("allow_partial_results = %q, want false", got)
	}
}

func TestExecute_IsPartialIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"is_partial":true,"columns":[{"name":"rule_passed","type":"boolean"},{"name":"service.name","type":"keyword"},{"name":"extent","type":"double"}],"values":[[true,"a",0.0]]}`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "").Execute(context.Background(), "FROM x")
	if err == nil || !strings.Contains(err.Error(), "partial results") {
		t.Fatalf("err = %v, want partial results error", err)
	}
}

func TestExecute_BasicAuthWhenNoAPIKey(t *testing.T) {
	var auth, user, pass string
	var hasBasic bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		user, pass, hasBasic = r.BasicAuth()
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()

	ex := New(srv.URL, "")
	ex.Username, ex.Password = "u", "p"
	if _, err := ex.Execute(context.Background(), "FROM x"); err != nil {
		t.Fatal(err)
	}
	if !hasBasic || user != "u" || pass != "p" {
		t.Errorf("basic auth = (%q,%q,%v)", user, pass, hasBasic)
	}

	ex = New(srv.URL, "k")
	ex.Username = "u"
	if _, err := ex.Execute(context.Background(), "FROM x"); err != nil {
		t.Fatal(err)
	}
	if auth != "ApiKey k" || hasBasic {
		t.Errorf("auth = %q basic=%v, want ApiKey k and no basic", auth, hasBasic)
	}
}

func TestExecute_NilHTTPClientUsesDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(okBody))
	}))
	defer srv.Close()

	ex := &Executor{BaseURL: srv.URL} // constructed directly, no HTTP client
	rows, err := ex.Execute(context.Background(), "FROM x")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows) == 0 {
		t.Error("no rows")
	}
	if ex.HTTP != nil {
		t.Error("Execute must not mutate the Executor's HTTP field")
	}
	if defaultClient.Timeout != 20*time.Second {
		t.Errorf("default timeout = %v, want 20s", defaultClient.Timeout)
	}
}

func TestExecute_InvalidURLNotEchoed(t *testing.T) {
	e := New("http://vern:s3cret@[::1", "")
	_, err := e.Execute(context.Background(), "FROM x")
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err = %v", err)
	}
}
