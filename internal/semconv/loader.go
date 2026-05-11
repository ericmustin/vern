package semconv

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Build merges a stream of upstream model YAMLs into a single Catalog.
// docs is keyed by upstream path (for diagnostics). version is recorded
// on the catalog and emitted into the generated VERSION file.
//
// Build is tolerant of unknown group types — a group type we don't classify
// (e.g. "scope", "metric_group") contributes its attribute references to the
// attribute set but does NOT carry placement info. The placement map only
// records levels we recognize.
func Build(version string, docs map[string][]byte) (*Catalog, error) {
	keys := map[string]bool{}
	placement := map[string]map[Level]bool{}

	for path, body := range docs {
		var doc rawDoc
		if err := yaml.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}

		for _, g := range doc.Groups {
			level, hasLevel := classifyGroupType(g.Type)

			for _, attr := range g.Attributes {
				// Registry definition: attribute with id and (usually) type.
				if attr.ID != "" && !attr.isTemplate() && isConcreteKey(attr.ID) {
					keys[attr.ID] = true
					if hasLevel {
						addLevel(placement, attr.ID, level)
					}
				}
				// Reference: attribute used at a specific signal level.
				if attr.Ref != "" && hasLevel {
					if isConcreteKey(attr.Ref) {
						keys[attr.Ref] = true
						addLevel(placement, attr.Ref, level)
					}
				}
			}
		}
	}

	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	return &Catalog{
		Version:       version,
		AttributeKeys: sorted,
		Placement:     placement,
	}, nil
}

// classifyGroupType maps the upstream group.type to an OTLP Level. Returns
// (zero, false) for group types that don't correspond to a signal level
// (e.g. attribute_group is just registry metadata).
func classifyGroupType(t string) (Level, bool) {
	switch t {
	case "entity", "resource":
		return LevelResource, true
	case "span", "event":
		// "event" attributes in semconv can appear on spans (as span events)
		// or as log records depending on context. The convention treats event
		// types as span-level for placement purposes; log-record placement is
		// captured separately via "log".
		return LevelSpan, true
	case "log":
		return LevelLog, true
	case "metric", "metric_group":
		return LevelMetric, true
	default:
		// "attribute_group", "scope", "" — no placement info.
		return "", false
	}
}

func addLevel(m map[string]map[Level]bool, attr string, level Level) {
	set := m[attr]
	if set == nil {
		set = map[Level]bool{}
		m[attr] = set
	}
	set[level] = true
}

// isConcreteKey returns true when the attribute ID looks like a real OTLP
// key (dotted lowercase). It filters defensive edge cases like empty IDs
// or IDs containing whitespace.
func isConcreteKey(id string) bool {
	if id == "" {
		return false
	}
	if strings.ContainsAny(id, " \t\n") {
		return false
	}
	return true
}

// LevelKeys returns the sorted list of attribute IDs whose placement set is
// exactly {level}. Used for "X-only" predicates in RES-004 — e.g. an attr
// in ResourceOnly that appears at span-level is a violation.
func (c *Catalog) LevelKeys(level Level) []string {
	out := make([]string, 0, len(c.Placement))
	for attr, set := range c.Placement {
		if len(set) == 1 && set[level] {
			out = append(out, attr)
		}
	}
	sort.Strings(out)
	return out
}
