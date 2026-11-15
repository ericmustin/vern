package coverage

import (
	"testing"

	"github.com/ericmustin/vern/internal/mappings"
)

func TestDefaultMappingsHaveNoMissingVendoredSpecRules(t *testing.T) {
	specRules, err := LoadSpecRules("../../spec/rules")
	if err != nil {
		t.Fatalf("LoadSpecRules: %v", err)
	}
	mf, err := mappings.Load("../../configs/esql-mappings.yaml")
	if err != nil {
		t.Fatalf("Load mappings: %v", err)
	}

	summary := Build(specRules, mf)
	if len(summary.MissingRules) != 0 {
		t.Fatalf("expected every vendored spec rule to have a mapping, missing: %v", summary.MissingRules)
	}
	for _, rule := range summary.Rules {
		if rule.Status == "disabled" && rule.Reason == "" {
			t.Fatalf("disabled rule %s must document a reason", rule.ID)
		}
	}
}

func TestIsHeuristicHonoursHeuristicFlag(t *testing.T) {
	cases := []struct {
		name string
		rule mappings.RuleMapping
		want bool
	}{
		{"flag", mappings.RuleMapping{Enabled: true, Heuristic: true}, true},
		{"status", mappings.RuleMapping{Enabled: true, Status: "heuristic"}, true},
		{"plain", mappings.RuleMapping{Enabled: true}, false},
	}
	for _, tc := range cases {
		if got := isHeuristic(tc.rule); got != tc.want {
			t.Errorf("%s: isHeuristic = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A disabled rule is reported as disabled even when it is marked heuristic.
func TestBuild_DisabledHeuristicRuleIsDisabled(t *testing.T) {
	specs := []SpecRule{{ID: "X-1", Impact: "Critical"}, {ID: "X-2", Impact: "Low"}}
	mf := &mappings.MappingsFile{Rules: []mappings.RuleMapping{
		{SpecRuleID: "X-1", Heuristic: true, Status: "planned"},
		{SpecRuleID: "X-2", Enabled: true, Heuristic: true},
	}}
	s := Build(specs, mf)
	if len(s.DisabledRules) != 1 || s.DisabledRules[0] != "X-1" || len(s.EnabledRules) != 1 || s.EnabledRules[0] != "X-2" ||
		len(s.HeuristicRules) != 1 || s.HeuristicRules[0] != "X-2" {
		t.Errorf("enabled=%v disabled=%v heuristic=%v", s.EnabledRules, s.DisabledRules, s.HeuristicRules)
	}
}
