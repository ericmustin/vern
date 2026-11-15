package mappings

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ericmustin/vern/internal/sensitive"
)

// FieldCap is the most fields of one group that a sensitive-data query
// scans; the overflow is recorded in Fields.Dropped and
// disclosed as a blind spot.
const FieldCap = 1000

// Group names used as Fields.Dropped keys.
const (
	GroupResource    = "Resource"
	GroupScope       = "Scope"
	GroupSpan        = "Span"
	GroupSpanEvent   = "SpanEvent"
	GroupLog         = "Log"
	GroupMetric      = "Metric"
	GroupMetricNames = "MetricNames"
)

// Fields are a run's scannable Elasticsearch fields, discovered from
// _field_caps. Each list is ordered fixed fields first (name, status.message,
// body.*, event_name, unit, scope.name/version), then links.attributes.* and
// scope.attributes.*, then attributes.* / resource.attributes.*, each tier
// sorted, so the cap drops free-form attribute keys first. Each list holds
// canonical field paths only (no passthrough or ECS alias duplicates) of
// scannable types, and has at most FieldCap entries.
//
// Resource and Scope are the union over the run's three signals (the same
// resource values occur in every signal). SpanEvent holds the logs-index
// fields that span events use (span events are log documents), so it overlaps
// Log. MetricNames holds metric names (the field names under "metrics.",
// prefix removed): metric names exist only as field names.
type Fields struct {
	Resource, Scope, Span, SpanEvent, Log, Metric []string
	MetricNames                                   []string
	// Dropped counts fields beyond FieldCap, per group name; groups with no
	// overflow are absent.
	Dropped map[string]int
}

// fixedFields are the rules' explicitly named locations; a cap must never
// drop them in favour of free-form attribute keys.
var fixedFields = map[string]bool{
	"name": true, "status.message": true, "body.text": true, "body.structured": true,
	"event_name": true, "unit": true, "scope.name": true, "scope.version": true,
}

// FieldPriority is a field's cap tier: 0 for the fixed fields, 1 for
// links.attributes.* and scope.attributes.*, 2 for attributes.* and
// resource.attributes.*.
func FieldPriority(field string) int {
	switch {
	case fixedFields[field]:
		return 0
	case strings.HasPrefix(field, "links.attributes.") || strings.HasPrefix(field, "scope.attributes."):
		return 1
	default:
		return 2
	}
}

// SortByPriority sorts fields in place by FieldPriority, each tier
// lexically, so truncating the list drops free-form keys first.
func SortByPriority(list []string) {
	sort.Slice(list, func(i, j int) bool {
		if pi, pj := FieldPriority(list[i]), FieldPriority(list[j]); pi != pj {
			return pi < pj
		}
		return list[i] < list[j]
	})
}

// group returns the named field group, or false for an unknown name.
func (f Fields) group(name string) ([]string, bool) {
	switch name {
	case GroupResource:
		return f.Resource, true
	case GroupScope:
		return f.Scope, true
	case GroupSpan:
		return f.Span, true
	case GroupSpanEvent:
		return f.SpanEvent, true
	case GroupLog:
		return f.Log, true
	case GroupMetric:
		return f.Metric, true
	}
	return nil, false
}

// Scan returns the fields one sensitive-data rule's query scans, given the
// rule's scan_groups (RuleMapping.ScanGroups): the groups joined without
// repeats, in priority order (SortByPriority), at most FieldCap of them
// (the cap applies per query). overflow holds the rest, in the
// same order, for blind-spot disclosure. GroupMetricNames is skipped (metric
// names are not field values; SensitiveScan returns them). Unknown group
// names are an error.
func (f Fields) Scan(groups []string) (fields, overflow []string, err error) {
	seen := map[string]bool{}
	for _, g := range groups {
		if g == GroupMetricNames {
			continue // names, not values: see SensitiveScan
		}
		list, ok := f.group(g)
		if !ok {
			return nil, nil, fmt.Errorf("unknown field group %q", g)
		}
		for _, fl := range list {
			if !seen[fl] {
				seen[fl] = true
				fields = append(fields, fl)
			}
		}
	}
	SortByPriority(fields)
	if len(fields) > FieldCap {
		return fields[:FieldCap:FieldCap], fields[FieldCap:], nil
	}
	return fields, nil, nil
}

// SensitiveScan is everything one sensitive-data rule's query scans and
// leaves out, from the rule's scan_groups: the resolver renders the query
// from it and the runner discloses its limits without re-deriving
// anything.
type SensitiveScan struct {
	// Fields and Overflow are Fields.Scan(groups): the scanned fields (at
	// most FieldCap) and the ones beyond the cap.
	Fields, Overflow []string
	// MetricNames are the run's metric names when groups lists
	// GroupMetricNames (MET-008), checked against the detectors in Go.
	MetricNames []string
	// ValueBudget is K: each field's value (all its values joined) is
	// scanned up to K characters, so the query's CONCAT stays under ES|QL's
	// 1 MiB limit. 0 when there are no fields.
	ValueBudget int
}

// SensitiveScan returns the scan of a rule with the given scan_groups.
func (f Fields) SensitiveScan(groups []string) (SensitiveScan, error) {
	fields, overflow, err := f.Scan(groups)
	if err != nil {
		return SensitiveScan{}, err
	}
	s := SensitiveScan{Fields: fields, Overflow: overflow}
	for _, g := range groups {
		if g == GroupMetricNames {
			s.MetricNames = f.MetricNames
		}
	}
	s.ValueBudget = sensitive.ValueBudget(s.Fields, s.MetricNames)
	return s, nil
}
