// Package dashboard renders a Kibana saved-objects NDJSON containing the
// Vern overview + service drill-down dashboards. Imported into Kibana via
// POST /api/saved_objects/_import.
package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
)

const defaultSpecBaseURL = "https://github.com/instrumentation-score/spec/blob/main/rules"

// Generate renders the dashboards NDJSON. The data view's index pattern is
// derived from cfg.ESQL.ResultIndex with "*" appended.
func Generate(cfg *config.Config, summaries ...*coverage.Summary) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	cfgCopy := *cfg
	cfgCopy.ApplyDefaults()
	cfg = &cfgCopy
	if cfg.ESQL.ResultIndex == "" {
		return nil, fmt.Errorf("config: esql.result_index is required")
	}
	var cov *coverage.Summary
	if len(summaries) > 0 {
		cov = summaries[0]
	}

	b := &builder{
		resultIndex:  cfg.ESQL.ResultIndex,
		indexPattern: ensureWildcard(cfg.ESQL.ResultIndex),
		specBaseURL:  defaultSpecBaseURL,
		coverage:     cov,
	}

	objects := b.buildAll()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, o := range objects {
		if err := enc.Encode(o); err != nil {
			return nil, fmt.Errorf("encode %s/%s: %w", o.Type, o.ID, err)
		}
	}
	return buf.Bytes(), nil
}

func ensureWildcard(s string) string {
	if strings.HasSuffix(s, "*") {
		return s
	}
	return s + "*"
}

// jsonMarshalNoEscape marshals without escaping HTML characters; called by
// dashboards.go's helper. Defined here so the package keeps a single config
// import location.
func jsonMarshalNoEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// json.Encoder.Encode appends a trailing newline; trim it for inline use.
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return out, nil
}
