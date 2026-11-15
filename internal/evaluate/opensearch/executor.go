// Package opensearch implements an evaluate.Executor that runs PPL queries
// against OpenSearch's synchronous _plugins/_ppl endpoint.
package opensearch

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ericmustin/vern/internal/evaluate"
)

// Executor runs PPL queries against {URL}/_plugins/_ppl?format=json. Auth is
// HTTP Basic when Username is set.
type Executor struct {
	URL      string
	Username string
	Password string
	HTTP     *http.Client
}

// New builds an Executor with a 60s default HTTP timeout.
func New(url, username, password string) *Executor {
	return &Executor{
		URL:      url,
		Username: username,
		Password: password,
		HTTP:     &http.Client{Timeout: 60 * time.Second},
	}
}

// Execute posts the PPL query to OpenSearch and decodes the schema+datarows
// response into one Row per service.
func (e *Executor) Execute(ctx context.Context, query string) ([]evaluate.Row, error) {
	if e.URL == "" {
		return nil, errors.New("opensearch executor: empty URL")
	}
	// The PPL endpoint's default (jdbc) response already carries the
	// {schema, datarows} shape we parse — there is no "json" format, and
	// asking for one is rejected with "unknown response format: json".
	endpoint := strings.TrimRight(e.URL, "/") + "/_plugins/_ppl"

	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.Username != "" {
		token := base64.StdEncoding.EncodeToString([]byte(e.Username + ":" + e.Password))
		req.Header.Set("Authorization", "Basic "+token)
	}

	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post query: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}

	var payload struct {
		Schema []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"schema"`
		Datarows [][]any `json:"datarows"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	idx := columnIndex(payload.Schema)
	rows := make([]evaluate.Row, 0, len(payload.Datarows))
	for _, vals := range payload.Datarows {
		rows = append(rows, buildRow(vals, idx))
	}
	return rows, nil
}

type colIdx struct {
	rulePassed  int
	serviceName int
	extent      int
	example     int
}

func columnIndex(schema []struct {
	Name string `json:"name"`
	Type string `json:"type"`
}) colIdx {
	idx := colIdx{rulePassed: -1, serviceName: -1, extent: -1, example: -1}
	for i, c := range schema {
		switch c.Name {
		case "rule_passed":
			idx.rulePassed = i
		case "service.name", "service_name", "serviceName":
			idx.serviceName = i
		case "extent":
			idx.extent = i
		case "example":
			idx.example = i
		}
	}
	return idx
}

func buildRow(vals []any, idx colIdx) evaluate.Row {
	r := evaluate.Row{}
	if idx.rulePassed >= 0 {
		r.Passed = asBool(vals[idx.rulePassed])
	}
	if idx.serviceName >= 0 {
		if v, ok := vals[idx.serviceName].(string); ok {
			r.ServiceName = v
		}
	}
	if idx.extent >= 0 {
		r.Extent = asFloat(vals[idx.extent])
	}
	if idx.example >= 0 {
		if v, ok := vals[idx.example].(string); ok {
			r.Example = v
		}
	}
	return r
}

func asBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0
	case int:
		return x != 0
	}
	return false
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
