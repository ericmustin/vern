package clickhouse

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecute_HappyPath(t *testing.T) {
	var gotBody, gotUser, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotUser = r.Header.Get("X-ClickHouse-User")
		gotKey = r.Header.Get("X-ClickHouse-Key")
		_, _ = w.Write([]byte(`{"service.name":"svc-a","rule_passed":true,"extent":0,"example":""}
{"service.name":"svc-b","rule_passed":false,"extent":0.42,"example":"trace-1"}
`))
	}))
	defer srv.Close()

	ex := New(srv.URL, "default", "secret", "otel")
	rows, err := ex.Execute(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].ServiceName != "svc-a" || !rows[0].Passed {
		t.Errorf("row[0] = %+v", rows[0])
	}
	if rows[1].ServiceName != "svc-b" || rows[1].Passed || rows[1].Extent != 0.42 || rows[1].Example != "trace-1" {
		t.Errorf("row[1] = %+v", rows[1])
	}
	if !strings.Contains(gotBody, "SELECT 1") {
		t.Errorf("server saw body %q", gotBody)
	}
	if gotUser != "default" || gotKey != "secret" {
		t.Errorf("auth headers = %q / %q", gotUser, gotKey)
	}
}

func TestExecute_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte("Code: 60. DB::Exception: Table otel.otel_traces doesn't exist."))
	}))
	defer srv.Close()
	ex := New(srv.URL, "", "", "")
	_, err := ex.Execute(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("err = %v", err)
	}
}

func TestExecute_AlternativeServiceNameKeys(t *testing.T) {
	// ClickHouse normally returns the column alias verbatim, but some setups
	// flatten dotted names to underscored.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"service_name":"svc-c","rule_passed":true,"extent":0,"example":""}`))
	}))
	defer srv.Close()

	ex := New(srv.URL, "", "", "")
	rows, err := ex.Execute(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rows[0].ServiceName != "svc-c" {
		t.Errorf("service = %q, want svc-c", rows[0].ServiceName)
	}
}

func TestExecute_EmptyURL(t *testing.T) {
	ex := New("", "", "", "")
	if _, err := ex.Execute(context.Background(), "SELECT 1"); err == nil {
		t.Error("want error for empty URL")
	}
}

func TestExecute_BoolAsInt(t *testing.T) {
	// Older ClickHouse versions sometimes serialise booleans as 0/1.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"service.name":"svc","rule_passed":1,"extent":0,"example":""}`))
	}))
	defer srv.Close()
	ex := New(srv.URL, "", "", "")
	rows, err := ex.Execute(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !rows[0].Passed {
		t.Errorf("want passed=true when ClickHouse returns 1")
	}
}
