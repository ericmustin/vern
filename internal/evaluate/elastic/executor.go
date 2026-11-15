// Package elastic implements an evaluate.Executor that runs ES|QL queries
// against an Elasticsearch endpoint via the synchronous _query API.
package elastic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/redact"
)

// Executor runs ES|QL queries with POST
// {BaseURL}/_query?format=json&allow_partial_results=false and treats a
// partial result as an error. APIKey is sent as `Authorization: ApiKey ...`
// when non-empty; otherwise Username/Password are sent as basic auth when
// Username is set. A nil HTTP uses a shared default client (20s timeout).
type Executor struct {
	BaseURL string
	APIKey  string
	// Username/Password are used for basic auth only when APIKey is empty.
	Username string
	Password string
	HTTP     *http.Client
}

// defaultClient serves Executors built without an HTTP client.
var defaultClient = &http.Client{Timeout: 20 * time.Second}

// New returns an Executor pre-wired with a default HTTP client (20s timeout).
func New(baseURL, apiKey string) *Executor {
	return &Executor{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 20 * time.Second},
	}
}

// Execute runs query and returns one Row per service.
func (e *Executor) Execute(ctx context.Context, query string) ([]evaluate.Row, error) {
	if e.BaseURL == "" {
		return nil, errors.New("elastic executor: empty BaseURL")
	}
	base := strings.TrimRight(e.BaseURL, "/")

	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, fmt.Errorf("marshal query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/_query?format=json&allow_partial_results=false", bytes.NewReader(body))
	if err != nil {
		// The parse error quotes the URL, credentials included: drop it.
		return nil, errors.New("build request: invalid elasticsearch URL")
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "ApiKey "+e.APIKey)
	} else if e.Username != "" {
		req.SetBasicAuth(e.Username, e.Password)
	}

	client := e.HTTP
	if client == nil {
		client = defaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post query: %w", redact.TransportError(err))
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if evaluate.Busy(resp.StatusCode, raw) {
		return nil, fmt.Errorf("HTTP %d: %w", resp.StatusCode, evaluate.ErrBusy)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}

	var payload struct {
		IsPartial bool `json:"is_partial"`
		Columns   []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"columns"`
		Values [][]any `json:"values"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if payload.IsPartial {
		if evaluate.BusyPartial(raw) {
			return nil, fmt.Errorf("partial results: %w", evaluate.ErrBusy)
		}
		return nil, errors.New("partial results (shard failures): refusing to score this rule")
	}

	idx := columnIndex(payload.Columns)
	rows := make([]evaluate.Row, 0, len(payload.Values))
	for _, vals := range payload.Values {
		row, err := buildRow(vals, idx)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

type colIdx struct {
	rulePassed  int
	serviceName int
	extent      int
	example     int
}

func columnIndex(cols []struct {
	Name string `json:"name"`
	Type string `json:"type"`
}) colIdx {
	idx := colIdx{rulePassed: -1, serviceName: -1, extent: -1, example: -1}
	for i, c := range cols {
		switch c.Name {
		case "rule_passed":
			idx.rulePassed = i
		case "service.name", "service_name":
			idx.serviceName = i
		case "extent":
			idx.extent = i
		case "example":
			idx.example = i
		}
	}
	return idx
}

func buildRow(vals []any, idx colIdx) (evaluate.Row, error) {
	if idx.rulePassed < 0 || idx.serviceName < 0 || idx.extent < 0 {
		return evaluate.Row{}, fmt.Errorf("response missing one of rule_passed/service.name/extent columns")
	}
	r := evaluate.Row{}
	if v, ok := vals[idx.rulePassed].(bool); ok {
		r.Passed = v
	}
	if v, ok := vals[idx.serviceName].(string); ok {
		r.ServiceName = v
	}
	r.Extent = asFloat(vals[idx.extent])
	if idx.example >= 0 {
		if v, ok := vals[idx.example].(string); ok {
			r.Example = v
		}
	}
	return r, nil
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case json.Number:
		f, _ := x.Float64()
		return f
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
