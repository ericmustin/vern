package mappings

import (
	"strings"
	"testing"

	"github.com/ericmustin/vern/internal/config"
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
