package mappings

import (
	"fmt"
	"strings"

	"github.com/ericmustin/vern/internal/config"
)

// buildScopeFilter renders an ES|QL WHERE-clause fragment that narrows
// rule evaluation to the environments / service namespaces configured
// by the user. The returned string starts with " AND " when non-empty so
// callers can splice it directly after their own WHERE clause; it is empty
// when no filters are configured.
//
// Each rule template uses `{{ .ScopeFilter }}` immediately after its main
// time-window predicate; when filters are empty this is a no-op.
func buildScopeFilter(f config.FilterConfig) string {
	var parts []string

	if env := quoteCSV(normalizeLower(f.Environments)); env != "" {
		parts = append(parts, fmt.Sprintf(
			"TO_LOWER(COALESCE(resource.attributes.deployment.environment.name, \"\")) IN (%s)",
			env,
		))
	}

	if ns := quoteCSV(f.ServiceNamespaces); ns != "" {
		parts = append(parts, fmt.Sprintf(
			"resource.attributes.service.namespace IN (%s)",
			ns,
		))
	}

	if len(parts) == 0 {
		return ""
	}
	return " AND (" + strings.Join(parts, " AND ") + ")"
}

func normalizeLower(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		out = append(out, strings.ToLower(s))
	}
	return out
}

// quoteCSV renders ["a","b"] as `"a", "b"` (suitable for ES|QL IN (...)).
// Returns empty string for empty input.
func quoteCSV(values []string) string {
	if len(values) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		// ES|QL string literals are double-quoted; escape any embedded quotes.
		v = strings.ReplaceAll(v, `"`, `\"`)
		quoted = append(quoted, `"`+v+`"`)
	}
	if len(quoted) == 0 {
		return ""
	}
	return strings.Join(quoted, ", ")
}
