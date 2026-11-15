package mappings

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/sensitive"
)

func testConfig() *config.Config {
	c := &config.Config{}
	c.ESQL.IndexPatterns = config.IndexPatterns{
		Traces:  "traces-apm*",
		Metrics: "metrics-*.otel-*",
		Logs:    "logs-*.otel-*",
	}
	c.ESQL.TimeWindow = "1h"
	c.ESQL.ScoreLookback = "2h"
	c.ESQL.ResultIndex = "instrumentation-score-results"
	c.ESQL.CardinalityThreshold = 10000
	return c
}

func TestResolve_SubstitutesScoreLookback(t *testing.T) {
	rules := []RuleMapping{{
		SpecRuleID:    "_SCORE_AGGREGATION",
		Enabled:       true,
		IsAggregation: true,
		Query:         "FROM {{ .ResultIndex }} | WHERE evaluated_at > NOW() - {{ .ScoreLookback }}",
	}}
	cfg := testConfig()
	cfg.ESQL.ScoreLookback = "6h"

	got, err := Resolve(rules, cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Aggregation == nil {
		t.Fatal("expected aggregation")
	}
	if !strings.Contains(got.Aggregation.ResolvedQuery, "NOW() - 6h") {
		t.Fatalf("expected score lookback substitution, got %q", got.Aggregation.ResolvedQuery)
	}
}

func TestResolve_SubstitutesIndices(t *testing.T) {
	rules := []RuleMapping{{
		SpecRuleID: "RES-005",
		Enabled:    true,
		Impact:     "Critical",
		Target:     "Resource",
		Query:      "FROM {{ .Indices.Traces }} | WHERE @timestamp > NOW() - {{ .TimeWindow }}",
	}}

	got, err := Resolve(rules, testConfig())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got.Rules) != 1 {
		t.Fatalf("want 1 rule, got %d", len(got.Rules))
	}
	q := got.Rules[0].ResolvedQuery
	if !strings.Contains(q, "traces-apm*") {
		t.Errorf("expected traces-apm* in resolved query, got: %s", q)
	}
	if !strings.Contains(q, "NOW() - 1h") {
		t.Errorf("expected NOW() - 1h in resolved query, got: %s", q)
	}
}

func TestResolve_SeparatesAggregation(t *testing.T) {
	rules := []RuleMapping{
		{SpecRuleID: "RES-005", Enabled: true, Query: "FROM {{ .Indices.Traces }}"},
		{SpecRuleID: "_AGG", Enabled: true, IsAggregation: true, Query: "FROM {{ .ResultIndex }}"},
	}

	got, err := Resolve(rules, testConfig())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got.Rules) != 1 {
		t.Errorf("want 1 non-agg rule, got %d", len(got.Rules))
	}
	if got.Aggregation == nil {
		t.Fatal("expected aggregation, got nil")
	}
	if !strings.Contains(got.Aggregation.ResolvedQuery, "instrumentation-score-results") {
		t.Errorf("expected resolved ResultIndex, got: %s", got.Aggregation.ResolvedQuery)
	}
}

func TestResolve_SkipsDisabled(t *testing.T) {
	rules := []RuleMapping{
		{SpecRuleID: "RES-005", Enabled: true, Query: "FROM {{ .Indices.Traces }}"},
		{SpecRuleID: "SPA-002", Enabled: false, Query: "broken {{ .Nope }}"},
	}

	got, err := Resolve(rules, testConfig())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got.Rules) != 1 {
		t.Errorf("want 1 rule, got %d", len(got.Rules))
	}
	if len(got.Skipped) != 1 || got.Skipped[0].SpecRuleID != "SPA-002" {
		t.Errorf("expected SPA-002 in skipped, got %+v", got.Skipped)
	}
}

func TestResolve_EmptyScopeFilterByDefault(t *testing.T) {
	rules := []RuleMapping{{
		SpecRuleID: "RES-005",
		Enabled:    true,
		Query:      "FROM {{ .Indices.Traces }} | WHERE @timestamp > NOW() - {{ .TimeWindow }}{{ .ScopeFilter }}",
	}}

	got, err := Resolve(rules, testConfig())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	q := got.Rules[0].ResolvedQuery
	if strings.Contains(q, "AND") {
		t.Errorf("expected no AND clause from empty ScopeFilter, got: %s", q)
	}
}

func TestResolve_ScopeFilterRendersEnvironments(t *testing.T) {
	rules := []RuleMapping{{
		SpecRuleID: "RES-005",
		Enabled:    true,
		Query:      "FROM x | WHERE @timestamp > NOW() - {{ .TimeWindow }}{{ .ScopeFilter }}",
	}}
	cfg := testConfig()
	cfg.Filters.Environments = []string{"prod", "Production"}

	got, err := Resolve(rules, cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	q := got.Rules[0].ResolvedQuery
	if !strings.Contains(q, `TO_LOWER(COALESCE(resource.attributes.deployment.environment.name, ""))`) {
		t.Errorf("expected env predicate, got: %s", q)
	}
	if !strings.Contains(q, `"prod"`) || !strings.Contains(q, `"production"`) {
		t.Errorf("expected lowercased env values, got: %s", q)
	}
}

func TestResolve_ScopeFilterRendersBothEnvAndNamespace(t *testing.T) {
	rules := []RuleMapping{{
		SpecRuleID: "RES-005",
		Enabled:    true,
		Query:      "FROM x | WHERE @timestamp > NOW() - {{ .TimeWindow }}{{ .ScopeFilter }}",
	}}
	cfg := testConfig()
	cfg.Filters.Environments = []string{"prod"}
	cfg.Filters.ServiceNamespaces = []string{"payments"}

	got, err := Resolve(rules, cfg)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	q := got.Rules[0].ResolvedQuery
	if !strings.Contains(q, `resource.attributes.service.namespace IN ("payments")`) {
		t.Errorf("expected namespace predicate, got: %s", q)
	}
	// Both predicates must be joined by AND inside the same parens.
	if !strings.Contains(q, " AND (") {
		t.Errorf("expected AND-prefixed scope clause, got: %s", q)
	}
}

func TestResolveWithData_InjectsSemconvCatalog(t *testing.T) {
	rules := []RuleMapping{{
		SpecRuleID: "MET-006",
		Enabled:    true,
		Query:      `FROM x | EVAL bad = name IN ({{ .SemconvAttributeKeys }})`,
	}}
	override := TemplateData{
		SemconvAttributeKeys: `"http.request.method", "service.name"`,
	}

	got, err := ResolveWithData(rules, testConfig(), override)
	if err != nil {
		t.Fatalf("ResolveWithData: %v", err)
	}
	q := got.Rules[0].ResolvedQuery
	if !strings.Contains(q, `"http.request.method"`) {
		t.Errorf("expected semconv keys in query, got: %s", q)
	}
}

func TestResolve_CollectsAllErrors(t *testing.T) {
	rules := []RuleMapping{
		{SpecRuleID: "BAD-1", Enabled: true, Query: "FROM {{ .Nonexistent }}"},
		{SpecRuleID: "BAD-2", Enabled: true, Query: "FROM {{ .AlsoMissing }}"},
	}

	_, err := Resolve(rules, testConfig())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "BAD-1") || !strings.Contains(msg, "BAD-2") {
		t.Errorf("expected both rule IDs in error, got: %s", msg)
	}
}

func TestResolve_ErrorsAreCollectedInResult(t *testing.T) {
	rules := []RuleMapping{
		{SpecRuleID: "BAD-1", Enabled: true, Query: "FROM {{ .Nope }}"},
		{SpecRuleID: "BAD-2", Enabled: true, Query: "FROM {{ .Indices.Traces "},
		{SpecRuleID: "GOOD", Enabled: true, Query: "FROM x"},
	}
	res, err := Resolve(rules, testConfig())
	if err == nil {
		t.Fatal("expected error")
	}
	if res == nil {
		t.Fatal("expected non-nil result")
	}
	if len(res.Errors) != 2 {
		t.Fatalf("want 2 errors, got %+v", res.Errors)
	}
	if res.Errors[0].SpecRuleID != "BAD-1" || res.Errors[0].Mapping.SpecRuleID != "BAD-1" || res.Errors[0].Reason == "" {
		t.Errorf("unexpected error entry: %+v", res.Errors[0])
	}
	if len(res.Rules) != 1 || res.Rules[0].SpecRuleID != "GOOD" {
		t.Fatalf("want GOOD only, got %+v", res.Rules)
	}
}

func TestResolve_SkippedCarriesMapping(t *testing.T) {
	rules := []RuleMapping{{SpecRuleID: "X-1", Enabled: false, Status: "blocked: x"}}
	res, err := Resolve(rules, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped[0].Mapping.Status != "blocked: x" || res.Skipped[0].Reason != "disabled" {
		t.Errorf("got %+v", res.Skipped[0])
	}
}

func TestResolve_UnmappedFieldsOnlyForESQL(t *testing.T) {
	const prefix = `SET unmapped_fields="nullify";`
	rules := []RuleMapping{
		{SpecRuleID: "R-1", Enabled: true, Query: "FROM x"},
		{SpecRuleID: "_AGG", Enabled: true, IsAggregation: true, Query: "FROM y"},
	}
	res, err := Resolve(rules, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Rules[0].ResolvedQuery, prefix+"\n") {
		t.Errorf("missing prefix: %q", res.Rules[0].ResolvedQuery)
	}
	if strings.Contains(res.Aggregation.ResolvedQuery, "SET unmapped_fields") {
		t.Errorf("aggregation prefixed: %q", res.Aggregation.ResolvedQuery)
	}
	// Overrides must not clobber the prefix.
	res, _ = ResolveWithData(rules, testConfig(), TemplateData{SampleMode: true})
	if !strings.HasPrefix(res.Rules[0].ResolvedQuery, prefix) {
		t.Errorf("override clobbered prefix: %q", res.Rules[0].ResolvedQuery)
	}

	off := testConfig()
	off.ESQL.UnmappedFields = "off"
	res, _ = Resolve(rules, off)
	if strings.Contains(res.Rules[0].ResolvedQuery, "SET unmapped_fields") {
		t.Errorf("off still prefixed: %q", res.Rules[0].ResolvedQuery)
	}

	for _, b := range []string{"clickhouse", "opensearch"} {
		c := testConfig()
		c.Backend = b
		res, _ := Resolve(rules, c)
		if strings.Contains(res.Rules[0].ResolvedQuery, "SET unmapped_fields") {
			t.Errorf("%s prefixed: %q", b, res.Rules[0].ResolvedQuery)
		}
	}
}

func TestResolve_AllSignalsAndSampleMode(t *testing.T) {
	cfg := testConfig()
	cfg.ESQL.IndexPatterns = config.IndexPatterns{Traces: "traces-*.otel-*", Logs: "logs-*.otel-*", Metrics: "metrics-*.otel-*"}
	rules := []RuleMapping{{SpecRuleID: "R-1", Enabled: true, Query: "FROM {{ .AllSignals }}{{ if .SampleMode }} S{{ end }}"}}
	res, err := ResolveWithData(rules, cfg, TemplateData{SampleMode: true})
	if err != nil {
		t.Fatal(err)
	}
	q := res.Rules[0].ResolvedQuery
	if !strings.Contains(q, "traces-*.otel-*,logs-*.otel-*,metrics-*.otel-*") || !strings.Contains(q, " S") {
		t.Errorf("got %q", q)
	}
}

func TestLoadBytes_ParsesNewFields(t *testing.T) {
	y := `backend: esql
rules:
  - spec_rule_id: R-1
    enabled: true
    signal: traces
    min_observation: 336h
    advisory: true
    heuristic: true
    deviation: dev
    remediation: fix
    query: FROM x
`
	mf, err := LoadBytes([]byte(y), "test.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := mf.Rules[0]
	if r.Signal != "traces" || r.MinObservation != "336h" || !r.Advisory || !r.Heuristic || r.Deviation != "dev" || r.Remediation != "fix" {
		t.Errorf("got %+v", r)
	}
}

func TestApplyOptInGates(t *testing.T) {
	rules := []RuleMapping{
		{SpecRuleID: "SDK-001", Enabled: false, OptInFlag: "filters.enable_sdk_rules"},
		{SpecRuleID: "X-001", Enabled: true, OptInFlag: "filters.unknown_flag"},
		{SpecRuleID: "RES-005", Enabled: true},
		{SpecRuleID: "RES-001", Enabled: false},
	}
	cfg := testConfig()
	cfg.Filters.EnableSDKRules = true
	ApplyOptInGates(rules, cfg)
	want := map[string]bool{"SDK-001": true, "X-001": false, "RES-005": true, "RES-001": false}
	for _, r := range rules {
		if r.Enabled != want[r.SpecRuleID] {
			t.Errorf("%s enabled = %v, want %v", r.SpecRuleID, r.Enabled, want[r.SpecRuleID])
		}
	}
	cfg.Filters.EnableSDKRules = false
	ApplyOptInGates(rules, cfg)
	if rules[0].Enabled {
		t.Error("SDK-001 still enabled with the flag off")
	}
}

func TestResolve_AdvisoryRuleIDsFromEnabledRules(t *testing.T) {
	rules := []RuleMapping{
		{SpecRuleID: "SPA-003", Enabled: true, Advisory: true, Query: "FROM x"},
		{SpecRuleID: "ZZZ-001", Enabled: false, Advisory: true, Query: "FROM x"},
		{SpecRuleID: "ABC-001", Enabled: true, Advisory: true, Query: "FROM x"},
		{SpecRuleID: "RES-005", Enabled: true, Query: "FROM x"},
		{SpecRuleID: "_AGG", Enabled: true, IsAggregation: true, Query: `IN ("_TOTAL"{{ if .AdvisoryRuleIDs }}, {{ .AdvisoryRuleIDs }}{{ end }})`},
	}
	res, err := Resolve(rules, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.Aggregation.ResolvedQuery, `IN ("_TOTAL", "ABC-001", "SPA-003")`; got != want {
		t.Errorf("aggregation = %q, want %q", got, want)
	}
}

func TestFieldsScan_DedupesOrdersAndCaps(t *testing.T) {
	f := Fields{Span: []string{"name", "attributes.b"}, SpanEvent: []string{"attributes.b", "event_name"}, Scope: []string{"scope.attributes.s", "scope.name"}}
	got, over, err := f.Scan([]string{GroupSpan, GroupSpanEvent, GroupScope})
	if err != nil || len(over) != 0 {
		t.Fatalf("err=%v overflow=%v", err, over)
	}
	if strings.Join(got, ",") != "event_name,name,scope.name,scope.attributes.s,attributes.b" {
		t.Errorf("Scan = %v", got)
	}
	if _, _, err := f.Scan([]string{"Spans"}); err == nil {
		t.Error("unknown group must be an error")
	}
}

// The cap applies to the joined list of one query, fixed fields first.
func TestFieldsScan_CapsJoinedGroups(t *testing.T) {
	var span, events []string
	for i := 0; i < 700; i++ {
		span = append(span, fmt.Sprintf("attributes.s%04d", i))
		events = append(events, fmt.Sprintf("attributes.e%04d", i))
	}
	f := Fields{
		Span:      append([]string{"name", "status.message", "links.attributes.l"}, span...),
		SpanEvent: append([]string{"event_name", "attributes.s0000"}, events...), // one repeat
		Scope:     []string{"scope.name", "scope.version", "scope.attributes.a"},
	}
	got, over, err := f.Scan([]string{GroupSpan, GroupSpanEvent, GroupScope})
	if err != nil {
		t.Fatal(err)
	}
	total := 3 + 700 + 1 + 700 + 3 // distinct fields
	if len(got) != FieldCap || len(over) != total-FieldCap {
		t.Fatalf("fields %d, overflow %d; want %d and %d", len(got), len(over), FieldCap, total-FieldCap)
	}
	want := "event_name,name,scope.name,scope.version,status.message,links.attributes.l,scope.attributes.a,attributes.e0000"
	if strings.Join(got[:8], ",") != want {
		t.Errorf("head = %v", got[:8])
	}
	// attributes.e* sort before attributes.s*: the overflow is the tail of s*.
	if over[0] != fmt.Sprintf("attributes.s%04d", 1000-7-700) || over[len(over)-1] != "attributes.s0699" {
		t.Errorf("overflow = %s .. %s", over[0], over[len(over)-1])
	}
	seen := map[string]bool{}
	for _, x := range append(append([]string(nil), got...), over...) {
		if seen[x] {
			t.Errorf("%s repeated", x)
		}
		seen[x] = true
	}

	// Through the resolver: the query scans exactly FieldCap fields.
	rules := []RuleMapping{{SpecRuleID: "X-1", Enabled: true, ScanGroups: []string{GroupSpan, GroupSpanEvent, GroupScope},
		Query: "FROM x\n{{ sensitiveQuery .ScanFields }}"}}
	res, err := ResolveWithData(rules, testConfig(), TemplateData{Fields: f})
	if err != nil {
		t.Fatal(err)
	}
	q := res.Rules[0].ResolvedQuery
	if n := strings.Count(q, "COALESCE(CONCAT("); n != FieldCap {
		t.Errorf("rendered %d segments, want %d", n, FieldCap)
	}
	for _, fixed := range []string{"`name`", "`status.message`", "`event_name`", "`scope.name`", "`scope.version`"} {
		if !strings.Contains(q, "TO_STRING("+fixed+")") {
			t.Errorf("fixed field %s dropped", fixed)
		}
	}
	if strings.Contains(q, "`attributes.s0699`") {
		t.Error("overflow field rendered")
	}
	rules[0].ScanGroups = []string{"Nope"}
	if res, _ := ResolveWithData(rules, testConfig(), TemplateData{Fields: f}); len(res.Errors) != 1 {
		t.Errorf("unknown scan group must fail the rule, got %v", res.Errors)
	}
}

// SensitiveScan gives the runner everything a rule scanned: fields, overflow,
// metric names (only when listed) and the per-value budget the query used.
func TestFieldsSensitiveScan(t *testing.T) {
	f := Fields{Metric: []string{"unit", "attributes.dp"}, Scope: []string{"scope.name"}, Resource: []string{"resource.attributes.a"},
		MetricNames: []string{"http.server.duration", "orders.alice@example.com.count"}}
	met, err := f.SensitiveScan([]string{GroupMetric, GroupScope, GroupMetricNames})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(met.Fields, ",") != "scope.name,unit,attributes.dp" || len(met.MetricNames) != 2 || len(met.Overflow) != 0 {
		t.Errorf("MET scan = %+v", met)
	}
	if met.ValueBudget != sensitive.ValueBudget(met.Fields, met.MetricNames) || met.ValueBudget < 1000 {
		t.Errorf("budget = %d", met.ValueBudget)
	}
	res, err := f.SensitiveScan([]string{GroupResource})
	if err != nil || res.MetricNames != nil || len(res.Fields) != 1 {
		t.Errorf("RES scan = %+v, %v", res, err)
	}
	rules := []RuleMapping{{SpecRuleID: "X-1", Enabled: true, ScanGroups: []string{GroupMetric, GroupScope, GroupMetricNames},
		Query: "FROM x\n{{ sensitiveMetricQuery .ScanFields .ScanMetricNames }}"}}
	out, err := ResolveWithData(rules, testConfig(), TemplateData{Fields: f})
	if err != nil {
		t.Fatal(err)
	}
	q := out.Rules[0].ResolvedQuery
	if !strings.Contains(q, fmt.Sprintf(`"; "), %d), `, met.ValueBudget)) || !strings.Contains(q, "`metrics.orders.alice@example.com.count` IS NOT NULL") {
		t.Errorf("rendered query does not match the scan:\n%s", q)
	}
}

// TestLoadBytes_SensitiveNeedsScanGroups: a sensitive rule without
// scan_groups would scan nothing and pass silently, so loading rejects it,
// naming every offending rule.
func TestLoadBytes_SensitiveNeedsScanGroups(t *testing.T) {
	y := `rules:
  - spec_rule_id: S-1
    sensitive: true
    query: FROM x
  - spec_rule_id: S-2
    sensitive: true
    scan_groups: [Span]
    query: FROM x
  - spec_rule_id: S-3
    enabled: false
    sensitive: true
    scan_groups: []
    query: FROM x
`
	_, err := LoadBytes([]byte(y), "test.yaml")
	if err == nil {
		t.Fatal("want an error for sensitive rules without scan_groups")
	}
	for _, want := range []string{"test.yaml", "S-1", "S-3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "S-2") {
		t.Errorf("error %q names S-2, which has scan_groups", err)
	}
}
