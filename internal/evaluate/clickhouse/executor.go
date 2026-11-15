// Package clickhouse implements an evaluate.Executor that runs SQL queries
// against a ClickHouse HTTP interface and parses JSONEachRow responses.
package clickhouse

import (
	"bufio"
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
)

// Executor runs SQL queries against ClickHouse via {URL}/?default_format=JSONEachRow.
// User/Password are sent in X-ClickHouse-User / X-ClickHouse-Key headers.
type Executor struct {
	URL      string
	Username string
	Password string
	Database string
	HTTP     *http.Client
}

// New builds an Executor with a default 60s HTTP timeout.
func New(url, username, password, database string) *Executor {
	return &Executor{
		URL:      url,
		Username: username,
		Password: password,
		Database: database,
		HTTP:     &http.Client{Timeout: 60 * time.Second},
	}
}

// Execute posts query to ClickHouse and parses the JSONEachRow response into
// one Row per service.
func (e *Executor) Execute(ctx context.Context, query string) ([]evaluate.Row, error) {
	if e.URL == "" {
		return nil, errors.New("clickhouse executor: empty URL")
	}
	endpoint := strings.TrimRight(e.URL, "/") + "/?default_format=JSONEachRow"
	if e.Database != "" {
		endpoint += "&database=" + e.Database
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(query)))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if e.Username != "" {
		req.Header.Set("X-ClickHouse-User", e.Username)
	}
	if e.Password != "" {
		req.Header.Set("X-ClickHouse-Key", e.Password)
	}

	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}

	return parseJSONEachRow(resp.Body)
}

// parseJSONEachRow reads newline-delimited JSON objects and maps each one
// into a Row. The expected fields are `service.name`, `rule_passed`,
// `extent`, `example`. Extra fields are ignored.
func parseJSONEachRow(r io.Reader) ([]evaluate.Row, error) {
	scanner := bufio.NewScanner(r)
	// Default Scanner buffer is 64KB. Bump to 1MB to accommodate wide rows
	// from chatty services in a single line.
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	var rows []evaluate.Row
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(line, &obj); err != nil {
			return nil, fmt.Errorf("parse JSONEachRow line: %w", err)
		}
		row := evaluate.Row{}
		row.ServiceName = readString(obj, "service.name", "service_name", "ServiceName")
		row.Passed = readBool(obj, "rule_passed")
		row.Extent = readFloat(obj, "extent")
		row.Example = readString(obj, "example")
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return rows, nil
}

func readString(obj map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	return ""
}

func readBool(obj map[string]json.RawMessage, key string) bool {
	raw, ok := obj[key]
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	// ClickHouse may serialise UInt8 booleans as 0/1.
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n != 0
	}
	return false
}

func readFloat(obj map[string]json.RawMessage, key string) float64 {
	raw, ok := obj[key]
	if !ok {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f
	}
	// ClickHouse occasionally returns floats as quoted strings.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		var f float64
		if _, err := fmt.Sscanf(s, "%f", &f); err == nil {
			return f
		}
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
