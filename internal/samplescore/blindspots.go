package samplescore

import (
	"fmt"
	"slices"

	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/otlpin"
	"github.com/ericmustin/vern/internal/sensitive"
)

// scannedAtAnyLength are the fields Elasticsearch scans whatever a value's
// length: body.text and exception.message are match_only_text, and span and
// event names are keywords with no length limit. Every other string field is a keyword that drops values longer
// than otlpin.LongValueChars.
var scannedAtAnyLength = map[string]bool{
	"body.text": true, "attributes.exception.message": true, "name": true, "event_name": true,
}

// groupOrder fixes the order of group-level sentences.
var groupOrder = []string{
	mappings.GroupResource, mappings.GroupScope, mappings.GroupSpan,
	mappings.GroupSpanEvent, mappings.GroupLog, mappings.GroupMetric,
}

var groupNoun = map[string]string{
	mappings.GroupResource: "resource", mappings.GroupScope: "scope", mappings.GroupSpan: "span",
	mappings.GroupSpanEvent: "span event", mappings.GroupLog: "log", mappings.GroupMetric: "metric data point",
}

// blindSpots returns plain sentences disclosing what the sensitive-data
// rules that ran (status evaluated) could not scan in this sample: field
// discovery and per-query caps, the key-name check bound, values cut to the
// per-query budget, long keyword values, unstored metric descriptions and
// exemplar attributes, and items outside the time window. Only the field
// groups those rules scan count. nil when no sensitive rule ran or nothing
// was left out.
func blindSpots(rep *evaluate.Report, mf *mappings.MappingsFile, fields mappings.Fields, sum otlpin.Summary) []string {
	type ran struct {
		id     string
		groups []string
	}
	var rules []ran
	scope := map[string]bool{}
	for _, rr := range rep.Rules {
		if !rr.Sensitive || rr.Status != evaluate.StatusEvaluated {
			continue
		}
		for _, rm := range mf.Rules {
			if rm.SpecRuleID == rr.RuleID && rm.Sensitive {
				rules = append(rules, ran{rr.RuleID, rm.ScanGroups})
				for _, g := range rm.ScanGroups {
					scope[g] = true
				}
				break
			}
		}
	}
	if len(rules) == 0 {
		return nil
	}

	var out []string
	add := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }

	// Discovery cap, per group: SpanEvent and Log overlap (span events are
	// log documents), so each is its own sentence, never a sum.
	for _, g := range groupOrder {
		if n := fields.Dropped[g]; n > 0 && scope[g] {
			add("%s couldn't be scanned: field discovery keeps at most %d per group",
				count(n, groupNoun[g]+" field", groupNoun[g]+" fields"), mappings.FieldCap)
		}
	}
	if n := fields.Dropped[mappings.GroupMetricNames]; n > 0 && scope[mappings.GroupMetricNames] {
		add("%s couldn't be checked: field discovery keeps at most %d", count(n, "metric name", "metric names"), mappings.FieldCap)
	}

	// Long values in keyword fields, unless a ".text" subfield holds them.
	long := 0
	for loc, n := range sum.LongValues {
		if scope[loc.Group] && !scannedAtAnyLength[loc.Field] && !slices.Contains(groupFields(fields, loc.Group), loc.Field+".text") {
			long += n
		}
	}
	if long > 0 {
		add("%s longer than %d characters couldn't be scanned for sensitive data", count(long, "value", "values"), otlpin.LongValueChars)
	}

	if n := sum.UntimedSpanEvents; n > 0 && scope[mappings.GroupSpanEvent] {
		add("%s with no timestamp couldn't be scanned (outside the time window)", count(n, "span event", "span events"))
	}
	if n := sum.UntimedLogRecords; n > 0 && scope[mappings.GroupLog] {
		add("%s with no timestamp couldn't be scanned (outside the time window)", count(n, "log record", "log records"))
	}
	metrics := scope[mappings.GroupMetric] || scope[mappings.GroupMetricNames]
	if n := sum.MetricDescriptions; n > 0 && metrics {
		add("%s couldn't be scanned (not stored)", count(n, "metric description", "metric descriptions"))
	}
	if n := sum.ExemplarsWithAttributes; n > 0 && metrics {
		add("The attributes of %s couldn't be scanned (not stored)", count(n, "exemplar", "exemplars"))
	}

	// Per rule: the query's field cap, the key-name bound, the value budget.
	for _, r := range rules {
		scan, err := fields.SensitiveScan(r.groups)
		if err != nil {
			continue
		}
		if n := len(scan.Overflow); n > 0 {
			add("%s: %s couldn't be scanned: one query scans at most %d", r.id, count(n, "field", "fields"), mappings.FieldCap)
		}
		if _, over := sensitive.KeyNameFields(scan.Fields); len(over) > 0 {
			add("%s: %s with sensitive key names couldn't be checked by name (at most %d per query); the values were still scanned",
				r.id, count(len(over), "field", "fields"), sensitive.MaxKeyNameFields)
		}
		longest := 0
		for _, g := range r.groups {
			longest = max(longest, sum.Longest[g])
		}
		if k := scan.ValueBudget; k > 0 && longest > k {
			add("%s: values longer than %d characters were scanned only up to the first %d characters", r.id, k, k)
		}
	}
	return out
}

// groupFields returns the named group of fields.
func groupFields(f mappings.Fields, group string) []string {
	switch group {
	case mappings.GroupResource:
		return f.Resource
	case mappings.GroupScope:
		return f.Scope
	case mappings.GroupSpan:
		return f.Span
	case mappings.GroupSpanEvent:
		return f.SpanEvent
	case mappings.GroupLog:
		return f.Log
	case mappings.GroupMetric:
		return f.Metric
	}
	return nil
}

// count renders n with the matching noun form: "1 value", "3 values".
func count(n int, one, many string) string {
	noun := many
	if n == 1 {
		noun = one
	}
	return fmt.Sprintf("%d %s", n, noun)
}
