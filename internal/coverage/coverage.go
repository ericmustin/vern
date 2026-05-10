package coverage

import (
	"sort"
	"strings"

	"github.com/ericmustin/vern/internal/mappings"
)

type RuleCoverage struct {
	ID          string `json:"id"`
	Impact      string `json:"impact"`
	Target      string `json:"target"`
	Status      string `json:"status"`
	Description string `json:"description,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type Summary struct {
	SpecVersion      string         `json:"spec_version"`
	PartialScore     bool           `json:"partial_score"`
	Rules            []RuleCoverage `json:"rules"`
	ImplementedRules []string       `json:"implemented_rules"`
	EnabledRules     []string       `json:"enabled_rules"`
	DisabledRules    []string       `json:"disabled_rules"`
	MissingRules     []string       `json:"missing_rules"`
	HeuristicRules   []string       `json:"heuristic_rules"`
}

func Build(specRules []SpecRule, mf *mappings.MappingsFile) Summary {
	specVersion := ""
	if mf != nil {
		specVersion = mf.SpecVersion
	}
	byID := map[string]mappings.RuleMapping{}
	if mf != nil {
		for _, rule := range mf.Rules {
			if strings.HasPrefix(rule.SpecRuleID, "_") {
				continue
			}
			byID[rule.SpecRuleID] = rule
		}
	}

	var summary Summary
	summary.SpecVersion = specVersion
	for _, spec := range specRules {
		rule := RuleCoverage{
			ID:          spec.ID,
			Impact:      spec.Impact,
			Target:      spec.Target,
			Description: spec.Description,
		}
		if mapping, ok := byID[spec.ID]; ok {
			summary.ImplementedRules = append(summary.ImplementedRules, spec.ID)
			switch {
			case isHeuristic(mapping):
				rule.Status = "heuristic"
				rule.Reason = strings.TrimSpace(mapping.Status)
				summary.EnabledRules = append(summary.EnabledRules, spec.ID)
				summary.HeuristicRules = append(summary.HeuristicRules, spec.ID)
			case mapping.Enabled:
				rule.Status = "enabled"
				summary.EnabledRules = append(summary.EnabledRules, spec.ID)
			default:
				rule.Status = "disabled"
				if mapping.Status != "" {
					rule.Reason = mapping.Status
				}
				summary.DisabledRules = append(summary.DisabledRules, spec.ID)
			}
		} else {
			rule.Status = "missing"
			summary.MissingRules = append(summary.MissingRules, spec.ID)
		}
		summary.Rules = append(summary.Rules, rule)
	}

	sortStrings(summary.ImplementedRules)
	sortStrings(summary.EnabledRules)
	sortStrings(summary.DisabledRules)
	sortStrings(summary.MissingRules)
	sortStrings(summary.HeuristicRules)
	summary.PartialScore = len(summary.DisabledRules) > 0 || len(summary.MissingRules) > 0 || len(summary.HeuristicRules) > 0
	return summary
}

func isHeuristic(rule mappings.RuleMapping) bool {
	status := strings.ToLower(strings.TrimSpace(rule.Status))
	if status == "heuristic" {
		return true
	}
	return rule.Enabled && strings.Contains(strings.ToLower(rule.Notes), "spec criteria is todo")
}

func sortStrings(values []string) {
	sort.Strings(values)
}

func Join(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}
