//go:build integration

package elastic

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/otlpin"
)

// sensitiveLoadSample shifts s to now and loads it through the engine into a
// fresh run that is purged when the test ends. It fails on any dropped item.
func sensitiveLoadSample(t *testing.T, s *otlpin.Sample) config.IndexPatterns {
	t.Helper()
	otlpin.Shift(s, otlpin.OffsetToNow(otlpin.Summarize(s), time.Now().Add(-time.Minute)))
	e := loadTestEngine(t)
	id := loadTestRunID(t, e) // purges the run in t.Cleanup
	st, err := e.Load(t.Context(), id, s)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if d := st.Dropped(); len(d) != 0 || st.Failed["metrics"] != 0 {
		t.Fatalf("dropped = %v (sent=%v indexed=%v failed=%v)", d, st.Sent, st.Indexed, st.Failed)
	}
	_, pats := e.Session(id)
	return pats
}

// sensitiveRows runs an ES|QL query and returns its rows; it fails on error.
func sensitiveRows(t *testing.T, q string) [][]any {
	t.Helper()
	res, err := esQuery(t, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return res.Values
}

// sensitiveValue runs a query expected to return exactly one row and column.
func sensitiveValue(t *testing.T, q string) any {
	t.Helper()
	rows := sensitiveRows(t, q)
	if len(rows) != 1 || len(rows[0]) != 1 {
		t.Fatalf("%s: want one value, got %v", q, rows)
	}
	return rows[0][0]
}

// sensitiveQueryError runs a query that must fail and returns the error text.
func sensitiveQueryError(t *testing.T, q string) string {
	t.Helper()
	_, err := esQuery(t, q)
	if err == nil {
		t.Fatalf("%s: expected an error", q)
	}
	return err.Error()
}

// fieldCaps is the decoded _field_caps response: field -> type -> caps.
type fieldCaps struct {
	Indices []string                              `json:"indices"`
	Fields  map[string]map[string]json.RawMessage `json:"fields"`
}

// types returns the sorted field-caps types of field ("" when absent).
func (fc fieldCaps) types(field string) string {
	var ts []string
	for ty := range fc.Fields[field] {
		ts = append(ts, ty)
	}
	sort.Strings(ts)
	return strings.Join(ts, ",")
}

// sensitiveFieldCaps calls GET /<patterns>/_field_caps?fields=*[&include_empty_fields=false].
func sensitiveFieldCaps(t *testing.T, patterns string, includeEmpty bool) fieldCaps {
	t.Helper()
	q := url.Values{"fields": {"*"}}
	if !includeEmpty {
		q.Set("include_empty_fields", "false")
	}
	status, body := esDo(t, http.MethodGet, "/"+patterns+"/_field_caps?"+q.Encode(), "", nil)
	if status != http.StatusOK {
		t.Fatalf("field caps %s: %d %s", patterns, status, body)
	}
	var fc fieldCaps
	if err := json.Unmarshal(body, &fc); err != nil {
		t.Fatalf("decode field caps: %v", err)
	}
	return fc
}
