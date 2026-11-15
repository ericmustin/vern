package elastic

import (
	"strings"
	"testing"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/mappings"
)

func testCfg() *config.Config {
	c := &config.Config{}
	c.ESQL.Schedule = "1h"
	c.ESQL.ResultIndex = "instrumentation-score-results"
	c.ESQL.AnnotationsIndex = "observability-annotations"
	return c
}

func TestRuleSlug(t *testing.T) {
	cases := map[string]string{
		"RES-005":            "res_005",
		"SPA-001":            "spa_001",
		"_SCORE_AGGREGATION": "score_aggregation",
	}
	for in, want := range cases {
		if got := ruleSlug(in); got != want {
			t.Errorf("ruleSlug(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestGenerate_TwoStepsPerRulePlusAggregation(t *testing.T) {
	resolved := &mappings.ResolveResult{
		Rules: []mappings.ResolvedMapping{
			{
				RuleMapping: mappings.RuleMapping{
					SpecRuleID: "RES-005", Impact: "Critical", Target: "Resource",
					Description: "service.name is present",
				},
				ResolvedQuery: "FROM traces-apm*",
			},
		},
		Aggregation: &mappings.ResolvedMapping{
			RuleMapping:   mappings.RuleMapping{SpecRuleID: "_AGG", IsAggregation: true},
			ResolvedQuery: "FROM results",
		},
	}

	cov := &coverage.Summary{
		SpecVersion:      "0.1",
		PartialScore:     true,
		ImplementedRules: []string{"RES-005"},
		EnabledRules:     []string{"RES-005"},
		MissingRules:     []string{"SDK-001"},
	}

	out, err := Generate(resolved, testCfg(), cov)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(out)

	for _, want := range []string{
		"name: eval_res_005",
		"name: store_res_005",
		"type: foreach",
		"name: index_res_005",
		"name: calculate_scores",
		"name: store_coverage",
		"rule_id: _COVERAGE",
		"partial_score: true",
		"SDK-001",
		"weight: 40",
		"impact: Critical",
		"rule_id: RES-005",
		// Liquid expressions must pass through verbatim
		"{{ consts.result_index }}",
		"{{ steps.eval_res_005.output.values }}",
		"{{ foreach.item[0] }}",
		"{{ foreach.item[1] }}",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing expected substring %q in output:\n%s", want, s)
		}
	}
}

func TestGenerate_UsesConfiguredAnnotationsIndex(t *testing.T) {
	resolved := &mappings.ResolveResult{
		Rules: []mappings.ResolvedMapping{{
			RuleMapping:   mappings.RuleMapping{SpecRuleID: "RES-005", Impact: "Critical", Target: "Resource"},
			ResolvedQuery: "FROM traces",
		}},
		Aggregation: &mappings.ResolvedMapping{
			ResolvedQuery: "FROM results | WHERE NOT (rule_id IN (\"_BOOTSTRAP\", \"_TOTAL\", \"_COVERAGE\"))",
		},
	}
	cfg := testCfg()
	cfg.ESQL.AnnotationsIndex = "custom-annotations"

	out, err := Generate(resolved, cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "index: custom-annotations") {
		t.Fatalf("expected configured annotations index in workflow:\n%s", s)
	}
	if strings.Contains(s, "index: observability-annotations") {
		t.Fatalf("unexpected default annotations index in workflow:\n%s", s)
	}
}

func TestGenerate_FailsWithNoRules(t *testing.T) {
	resolved := &mappings.ResolveResult{}
	if _, err := Generate(resolved, testCfg()); err == nil {
		t.Fatal("expected error for empty rules, got nil")
	}
}

func TestGenerate_StoresCanonicalImpact(t *testing.T) {
	resolved := &mappings.ResolveResult{
		Rules: []mappings.ResolvedMapping{{
			RuleMapping:   mappings.RuleMapping{SpecRuleID: "RES-005", Impact: "critical", Target: "Resource"},
			ResolvedQuery: "FROM traces",
		}},
	}
	out, err := Generate(resolved, testCfg())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(out)
	i := strings.Index(s, "name: index_res_005")
	if i < 0 {
		t.Fatalf("no index_res_005 step:\n%s", s)
	}
	step := s[i:]
	for _, want := range []string{"impact: Critical", "weight: 40"} {
		if !strings.Contains(step, want) {
			t.Errorf("missing %q in index_res_005:\n%s", want, step)
		}
	}
	if strings.Contains(s, "impact: critical") {
		t.Errorf("raw impact stored:\n%s", s)
	}
}

func TestGenerate_UnknownImpactFails(t *testing.T) {
	resolved := &mappings.ResolveResult{
		Rules: []mappings.ResolvedMapping{{
			RuleMapping:   mappings.RuleMapping{SpecRuleID: "RES-005", Impact: "Urgent", Target: "Resource"},
			ResolvedQuery: "FROM traces",
		}},
	}
	_, err := Generate(resolved, testCfg())
	if err == nil || !strings.Contains(err.Error(), `RES-005: unknown impact "Urgent"`) {
		t.Fatalf("err = %v, want unknown impact error", err)
	}
}

// TestGenerate_SkipsSensitiveRules: a sensitive-data rule scans the
// fields discovered for one run, which a scheduled workflow does not have,
// so the formatter emits no step for it even if it reaches the formatter.
func TestGenerate_SkipsSensitiveRules(t *testing.T) {
	resolved := &mappings.ResolveResult{Rules: []mappings.ResolvedMapping{
		{RuleMapping: mappings.RuleMapping{SpecRuleID: "RES-005", Impact: "Critical", Target: "Resource"}, ResolvedQuery: "FROM a"},
		{RuleMapping: mappings.RuleMapping{SpecRuleID: "RES-010", Impact: "Critical", Target: "Resource", Sensitive: true}, ResolvedQuery: "FROM b | WHERE false"},
	}}
	out, err := Generate(resolved, testCfg(), &coverage.Summary{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "name: eval_res_005") {
		t.Error("RES-005 step missing")
	}
	if strings.Contains(s, "res_010") || strings.Contains(s, "RES-010") {
		t.Errorf("sensitive rule RES-010 in the workflow:\n%s", s)
	}
}
