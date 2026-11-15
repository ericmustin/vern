package opensearch

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecute_HappyPath(t *testing.T) {
	var gotBody, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{
			"schema":[
				{"name":"rule_passed","type":"boolean"},
				{"name":"service.name","type":"string"},
				{"name":"example","type":"string"},
				{"name":"extent","type":"double"}
			],
			"datarows":[
				[true, "svc-a", "", 0.0],
				[false, "svc-b", "trace-1", 0.42]
			],
			"total":2,
			"size":2
		}`))
	}))
	defer srv.Close()

	ex := New(srv.URL, "admin", "pass")
	rows, err := ex.Execute(context.Background(), "source = otel-* | stats count() by serviceName")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[1].ServiceName != "svc-b" || rows[1].Passed || rows[1].Extent != 0.42 || rows[1].Example != "trace-1" {
		t.Errorf("row[1] = %+v", rows[1])
	}
	if !strings.Contains(gotBody, `"query":`) {
		t.Errorf("body = %q", gotBody)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:pass"))
	if gotAuth != wantAuth {
		t.Errorf("auth = %q, want %q", gotAuth, wantAuth)
	}
}

func TestExecute_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"reason":"syntax error"}}`))
	}))
	defer srv.Close()
	ex := New(srv.URL, "", "")
	_, err := ex.Execute(context.Background(), "broken")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("err = %v", err)
	}
}

func TestExecute_NoAuthHeaderWhenUnset(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"schema":[],"datarows":[]}`))
	}))
	defer srv.Close()
	ex := New(srv.URL, "", "")
	if _, err := ex.Execute(context.Background(), "x"); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("auth = %q, want empty when no creds", gotAuth)
	}
}

func TestExecute_AlternativeServiceNameCol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// PPL may surface the column as `serviceName` rather than the dotted alias.
		_, _ = w.Write([]byte(`{
			"schema":[
				{"name":"rule_passed","type":"boolean"},
				{"name":"serviceName","type":"string"},
				{"name":"extent","type":"double"},
				{"name":"example","type":"string"}
			],
			"datarows":[[true,"svc-c",0,""]]
		}`))
	}))
	defer srv.Close()
	ex := New(srv.URL, "", "")
	rows, err := ex.Execute(context.Background(), "x")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if rows[0].ServiceName != "svc-c" {
		t.Errorf("service = %q", rows[0].ServiceName)
	}
}

func TestExecute_EmptyURL(t *testing.T) {
	ex := New("", "", "")
	if _, err := ex.Execute(context.Background(), "x"); err == nil {
		t.Error("want error")
	}
}
