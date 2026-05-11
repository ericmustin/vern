// Package semconv loads the OpenTelemetry semantic-convention YAMLs and
// produces two artifacts used by Vern's rule queries:
//
//   - AttributeKeys: every concrete (non-templated) semconv attribute ID.
//     Consumed by MET-006 ("metric names do not equal semconv attribute keys").
//
//   - Placement: per-attribute set of allowed OTLP levels (resource, span,
//     log, metric). Consumed by RES-004 ("semantic-convention attributes are
//     used at the right level").
//
// The generated Go files under this package are committed to the repo so
// `go install` works without network. `vern semconv sync` regenerates them
// from the upstream repo at the pinned ref.
package semconv

// Level enumerates the OTLP signal levels at which an attribute can appear.
type Level string

const (
	LevelResource Level = "resource"
	LevelSpan     Level = "span"
	LevelLog      Level = "log"
	LevelMetric   Level = "metric"
)

// Catalog is the in-memory representation of the semconv attribute registry
// and per-attribute placement set. Generated at sync time and consumed by
// the resolver to template into ES|QL queries.
type Catalog struct {
	// Version is the upstream tag/SHA this catalog was generated from.
	Version string

	// AttributeKeys is the sorted set of concrete attribute IDs (no template[]).
	AttributeKeys []string

	// Placement maps attribute ID → set of allowed levels.
	Placement map[string]map[Level]bool
}

// rawDoc and rawGroup model just enough of the upstream YAML to drive
// catalog generation. We deliberately ignore many fields (stability, brief,
// type, etc.) — drift in those fields should not break the build.
type rawDoc struct {
	Groups []rawGroup `yaml:"groups"`
}

type rawGroup struct {
	ID         string       `yaml:"id"`
	Type       string       `yaml:"type"`
	Attributes []rawAttribute `yaml:"attributes"`
}

type rawAttribute struct {
	// ID defines a new attribute (when present, this group is a registry group).
	ID string `yaml:"id"`
	// Ref references a previously registered attribute (when present, this
	// group is associating the attribute with its own signal level).
	Ref string `yaml:"ref"`
	// Type can be string, int, boolean, double, string[], or template[<...>].
	// Templated names are not concrete and excluded from MET-006.
	Type interface{} `yaml:"type"`
}

// isTemplate reports whether the attribute is a templated (key-prefix) type
// like `template[string]`. Templated attributes are excluded because they
// represent a key prefix, not a concrete key.
func (a rawAttribute) isTemplate() bool {
	s, ok := a.Type.(string)
	if !ok {
		return false
	}
	return len(s) > len("template") && s[:len("template")] == "template"
}
