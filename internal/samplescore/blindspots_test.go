package samplescore

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/otlpin"
	"github.com/ericmustin/vern/internal/sensitive"
)

// The otlpin group names must be the mappings.Fields group names: blind
// spots join the two.
func TestGroupNamesMatchMappings(t *testing.T) {
	pairs := [][2]string{
		{otlpin.GroupResource, mappings.GroupResource}, {otlpin.GroupScope, mappings.GroupScope},
		{otlpin.GroupSpan, mappings.GroupSpan}, {otlpin.GroupSpanEvent, mappings.GroupSpanEvent},
		{otlpin.GroupLog, mappings.GroupLog}, {otlpin.GroupMetric, mappings.GroupMetric},
	}
	for _, p := range pairs {
		if p[0] != p[1] {
			t.Errorf("otlpin %q != mappings %q", p[0], p[1])
		}
	}
}

func scanMappings() *mappings.MappingsFile {
	return &mappings.MappingsFile{Rules: []mappings.RuleMapping{
		{SpecRuleID: "RES-010", Enabled: true, Sensitive: true, ScanGroups: []string{mappings.GroupResource}, Impact: "Critical", Query: "RES"},
		{SpecRuleID: "SPA-006", Enabled: true, Sensitive: true, ScanGroups: []string{mappings.GroupSpan, mappings.GroupSpanEvent, mappings.GroupScope}, Impact: "Critical", Query: "SPA"},
		{SpecRuleID: "MET-008", Enabled: true, Sensitive: true, ScanGroups: []string{mappings.GroupMetric, mappings.GroupScope, mappings.GroupMetricNames}, Impact: "Critical", Query: "MET"},
	}}
}

func evaluated(ids ...string) *evaluate.Report {
	rep := &evaluate.Report{}
	for _, id := range ids {
		rep.Rules = append(rep.Rules, evaluate.RuleResult{RuleID: id, Sensitive: true, Status: evaluate.StatusEvaluated})
	}
	return rep
}

func numbered(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%04d", prefix, i)
	}
	return out
}

func TestBlindSpots_Sentences(t *testing.T) {
	span := append(numbered("attributes.f", mappings.FieldCap), "attributes.q", "attributes.q.text")
	keys := numbered("attributes.k", sensitive.MaxKeyNameFields+3)
	for i := range keys {
		keys[i] += ".password"
	}
	fields := mappings.Fields{
		Resource: []string{"resource.attributes.cfg"},
		Span:     append(span, keys...),
		Metric:   []string{"attributes.route"},
		Dropped:  map[string]int{mappings.GroupSpanEvent: 7, mappings.GroupLog: 9, mappings.GroupMetricNames: 2},
	}
	sum := otlpin.Summary{
		LongValues: map[otlpin.ValueLocation]int{
			{Group: otlpin.GroupResource, Field: "resource.attributes.cfg"}: 2, // keyword: not scanned
			{Group: otlpin.GroupSpan, Field: "attributes.q"}:                5, // has a .text sibling
			{Group: otlpin.GroupSpan, Field: "name"}:                        1, // no length limit
			{Group: otlpin.GroupSpan, Field: "status.message"}:              1, // keyword: not scanned
			{Group: otlpin.GroupLog, Field: "body.text"}:                    4, // match_only_text; no log rule ran anyway
			{Group: otlpin.GroupLog, Field: "attributes.x"}:                 3, // no log rule ran
		},
		Longest:                 map[string]int{otlpin.GroupResource: 100, otlpin.GroupSpan: 5000},
		MetricDescriptions:      3,
		ExemplarsWithAttributes: 4,
		UntimedSpanEvents:       5,
		UntimedLogRecords:       6, // no log rule ran
	}
	got := blindSpots(evaluated("RES-010", "SPA-006", "MET-008"), scanMappings(), fields, sum)
	k := sensitive.ValueBudget(append(span, keys...)[:mappings.FieldCap], nil)
	want := []string{
		"7 span event fields couldn't be scanned: field discovery keeps at most 1000 per group",
		"2 metric names couldn't be checked: field discovery keeps at most 1000",
		"3 values longer than 1024 characters couldn't be scanned for sensitive data",
		"5 span events with no timestamp couldn't be scanned (outside the time window)",
		"3 metric descriptions couldn't be scanned (not stored)",
		"The attributes of 4 exemplars couldn't be scanned (not stored)",
		fmt.Sprintf("SPA-006: %d fields couldn't be scanned: one query scans at most 1000", len(span)+len(keys)-mappings.FieldCap),
		fmt.Sprintf("SPA-006: values longer than %d characters were scanned only up to the first %d characters", k, k),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blind spots =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestBlindSpots_KeyNameOverflow: sensitive-key fields past the 32 key
// checks are disclosed per rule.
func TestBlindSpots_KeyNameOverflow(t *testing.T) {
	keys := numbered("attributes.k", sensitive.MaxKeyNameFields+3)
	for i := range keys {
		keys[i] += ".token"
	}
	got := blindSpots(evaluated("SPA-006"), scanMappings(), mappings.Fields{Span: keys}, otlpin.Summary{})
	want := []string{"SPA-006: 3 fields with sensitive key names couldn't be checked by name (at most 32 per query); the values were still scanned"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blind spots = %q, want %q", got, want)
	}
}

// TestBlindSpots_OnlyForRulesThatRan: nothing is disclosed for rules that
// did not run (disabled, not applicable, errors).
func TestBlindSpots_OnlyForRulesThatRan(t *testing.T) {
	sum := otlpin.Summary{MetricDescriptions: 3, UntimedSpanEvents: 2,
		LongValues: map[otlpin.ValueLocation]int{{Group: otlpin.GroupResource, Field: "resource.attributes.cfg"}: 1}}
	fields := mappings.Fields{Dropped: map[string]int{mappings.GroupResource: 5}}
	rep := &evaluate.Report{Rules: []evaluate.RuleResult{
		{RuleID: "RES-010", Sensitive: true, Status: evaluate.StatusError},
		{RuleID: "SPA-006", Sensitive: true, Status: evaluate.StatusNotApplicable},
		{RuleID: "MET-008", Sensitive: true, Status: evaluate.StatusNotImplemented},
	}}
	if got := blindSpots(rep, scanMappings(), fields, sum); got != nil {
		t.Errorf("blind spots = %q, want none", got)
	}
}

// TestScore_BlindSpotsInReport: Score puts the blind spots on the sample.
func TestScore_BlindSpotsInReport(t *testing.T) {
	s := sampleOf("svc")
	sp := s.Traces.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0)
	sp.Events().AppendEmpty() // no timestamp
	eng := &fakeEngine{
		fields: mappings.Fields{Span: []string{"attributes.a"}},
		exec: execFunc(func(ctx context.Context, q string) ([]evaluate.Row, error) {
			return []evaluate.Row{{ServiceName: "svc", Passed: true, Example: "No sensitive data matched 11 checks"}}, nil
		}),
	}
	rep, err := Score(context.Background(), eng, s, Options{Now: fixedNow, Mappings: scanMappings()})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1 span event with no timestamp couldn't be scanned (outside the time window)"}
	if !reflect.DeepEqual(rep.Sample.BlindSpots, want) {
		t.Errorf("blind spots = %q, want %q", rep.Sample.BlindSpots, want)
	}
}
