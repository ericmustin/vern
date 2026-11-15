package elastic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
)

// scannableTypes are the _field_caps types a sensitive-data scan can read:
// strings read directly, long/version/flattened through
// TO_STRING. _field_caps reports type families, so match_only_text shows as
// text and wildcard as keyword. ip, date and float are not scanned: a value
// that parses is not sensitive text, and a malformed one is null (blind spot).
var scannableTypes = map[string]bool{
	"keyword": true, "constant_keyword": true, "text": true,
	"long": true, "integer": true, "short": true, "byte": true, "unsigned_long": true,
	"version": true, "flattened": true,
}

// fieldCapsResponse is the part of a _field_caps response used here.
type fieldCapsResponse struct {
	Fields map[string]map[string]json.RawMessage `json:"fields"`
}

// signalLayout lists a signal's canonical fields. Everything else
// (passthrough aliases such as `sample.region` for `resource.attributes.sample.region`, ECS aliases such as
// `span.name` and `message`, metadata fields) duplicates a canonical field
// and is skipped so each value is scanned once.
type signalLayout struct {
	prefixes []string // a field under one of these prefixes is canonical
	fixed    []string // exact canonical names
}

var (
	resourcePrefix = "resource.attributes."
	scopePrefix    = "scope.attributes."
	scopeFixed     = []string{"scope.name", "scope.version"}

	tracesLayout  = signalLayout{prefixes: []string{"attributes.", "links.attributes."}, fixed: []string{"name", "status.message"}}
	logsLayout    = signalLayout{prefixes: []string{"attributes."}, fixed: []string{"event_name", "body.text", "body.structured"}}
	metricsLayout = signalLayout{prefixes: []string{"attributes."}, fixed: []string{"unit"}}
)

const metricNamePrefix = "metrics."

func (l signalLayout) has(field string) bool {
	for _, f := range l.fixed {
		if field == f {
			return true
		}
	}
	for _, p := range l.prefixes {
		if strings.HasPrefix(field, p) {
			return true
		}
	}
	return false
}

// scannable reports whether every type of the field can be scanned. A field
// with conflicting types across backing indices is only scanned if all are.
func scannable(types map[string]json.RawMessage) bool {
	if len(types) == 0 {
		return false
	}
	for t := range types {
		if !scannableTypes[t] {
			return false
		}
	}
	return true
}

// signalFields is one signal's canonical scannable fields, sorted.
type signalFields struct {
	resource, scope, own, metricNames []string
}

func classify(fc fieldCapsResponse, l signalLayout, metrics bool) signalFields {
	var sf signalFields
	for field, types := range fc.Fields {
		if metrics && strings.HasPrefix(field, metricNamePrefix) {
			// Metric names are field names; any value-bearing type
			// qualifies, objects (the path prefixes) do not.
			if !isObject(types) {
				sf.metricNames = append(sf.metricNames, strings.TrimPrefix(field, metricNamePrefix))
			}
			continue
		}
		if !scannable(types) {
			continue
		}
		switch {
		case strings.HasPrefix(field, resourcePrefix):
			sf.resource = append(sf.resource, field)
		case strings.HasPrefix(field, scopePrefix) || field == scopeFixed[0] || field == scopeFixed[1]:
			sf.scope = append(sf.scope, field)
		case l.has(field):
			sf.own = append(sf.own, field)
		}
	}
	mappings.SortByPriority(sf.resource)
	mappings.SortByPriority(sf.scope)
	mappings.SortByPriority(sf.own)
	sort.Strings(sf.metricNames)
	return sf
}

func isObject(types map[string]json.RawMessage) bool {
	if len(types) == 0 {
		return true
	}
	for t := range types {
		if t == "object" || t == "nested" || t == "unmapped" {
			return true
		}
	}
	return false
}

// union returns the sorted, de-duplicated union of lists.
func union(lists ...[]string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, l := range lists {
		for _, f := range l {
			if _, ok := seen[f]; !ok {
				seen[f] = struct{}{}
				out = append(out, f)
			}
		}
	}
	mappings.SortByPriority(out)
	return out
}

// capGroup truncates a priority-sorted list to mappings.FieldCap and records the
// overflow in dropped under group.
func capGroup(list []string, group string, dropped map[string]int) []string {
	if len(list) > mappings.FieldCap {
		dropped[group] = len(list) - mappings.FieldCap
		return list[:mappings.FieldCap]
	}
	return list
}

// fieldCaps fetches the scannable-candidate fields of one index pattern.
// include_empty_fields=false drops template fields with no data and fields
// whose only values were ignored (over-long keywords): those are counted by
// the blind-spot logic, not scanned.
func (e *Engine) fieldCaps(ctx context.Context, pattern string) (fieldCapsResponse, error) {
	q := url.Values{"fields": {"*"}, "include_empty_fields": {"false"}}
	status, body, err := e.do(ctx, http.MethodGet, "/"+pattern+"/_field_caps?"+q.Encode(), "", nil)
	if err != nil {
		return fieldCapsResponse{}, err
	}
	if evaluate.Busy(status, body) {
		return fieldCapsResponse{}, fmt.Errorf("HTTP %d: %w", status, engine.ErrBusy)
	}
	if status/100 != 2 {
		return fieldCapsResponse{}, fmt.Errorf("HTTP %d: %s", status, truncate(body))
	}
	var fc fieldCapsResponse
	if err := json.Unmarshal(body, &fc); err != nil {
		return fieldCapsResponse{}, fmt.Errorf("decode response: %w", err)
	}
	return fc, nil
}

// Fields discovers the run's scannable fields, one _field_caps call per
// signal pattern (the same attribute key can have a different type per
// signal). See mappings.Fields for the group semantics.
func (e *Engine) Fields(ctx context.Context, id engine.RunID) (mappings.Fields, error) {
	_, pats := e.Session(id)
	get := func(signal, pattern string) (fieldCapsResponse, error) {
		fc, err := e.fieldCaps(ctx, pattern)
		if err != nil {
			return fc, fmt.Errorf("field caps (%s) at %s: %w", signal, e.redactedURL(), err)
		}
		return fc, nil
	}
	tfc, err := get("traces", pats.Traces)
	if err != nil {
		return mappings.Fields{}, err
	}
	lfc, err := get("logs", pats.Logs)
	if err != nil {
		return mappings.Fields{}, err
	}
	mfc, err := get("metrics", pats.Metrics)
	if err != nil {
		return mappings.Fields{}, err
	}
	t := classify(tfc, tracesLayout, false)
	l := classify(lfc, logsLayout, false)
	m := classify(mfc, metricsLayout, true)

	dropped := map[string]int{}
	f := mappings.Fields{
		Resource:    capGroup(union(t.resource, l.resource, m.resource), mappings.GroupResource, dropped),
		Scope:       capGroup(union(t.scope, l.scope, m.scope), mappings.GroupScope, dropped),
		Span:        capGroup(t.own, mappings.GroupSpan, dropped),
		Log:         capGroup(l.own, mappings.GroupLog, dropped),
		Metric:      capGroup(m.own, mappings.GroupMetric, dropped),
		MetricNames: capGroup(m.metricNames, mappings.GroupMetricNames, dropped),
	}
	// Span events are log documents: they carry event_name and attributes.*
	// but not the log body.
	f.SpanEvent = capGroup(without(l.own, "body.text", "body.structured"), mappings.GroupSpanEvent, dropped)
	if len(dropped) > 0 {
		f.Dropped = dropped
	}
	return f, nil
}

func without(list []string, drop ...string) []string {
	var out []string
	for _, f := range list {
		keep := true
		for _, d := range drop {
			if f == d {
				keep = false
			}
		}
		if keep {
			out = append(out, f)
		}
	}
	return out
}
