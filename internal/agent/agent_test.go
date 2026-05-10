package agent

import (
	"strings"
	"testing"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
)

func TestBuildSkill_UsesConfiguredIndexesAndCoverage(t *testing.T) {
	cfg := &config.Config{}
	cfg.ESQL.ResultIndex = "custom-score-results"
	cfg.ESQL.IndexPatterns.Traces = "custom-traces-*"
	cfg.ESQL.IndexPatterns.Logs = "custom-logs-*"
	cfg.ESQL.IndexPatterns.Metrics = "custom-metrics-*"
	cfg.ApplyDefaults()
	cov := &coverage.Summary{
		SpecVersion:    "0.1",
		PartialScore:   true,
		EnabledRules:   []string{"RES-005"},
		MissingRules:   []string{"SDK-001"},
		HeuristicRules: []string{"SPA-003"},
	}

	skill := BuildSkill(Context{Config: cfg, Coverage: cov})
	rendered := RenderSkillMarkdown(Context{Config: cfg, Coverage: cov})
	if skill.Content != rendered {
		t.Fatalf("BuildSkill content differs from rendered markdown")
	}

	for _, want := range []string{
		"custom-score-results",
		"custom-traces-*",
		"custom-logs-*",
		"custom-metrics-*",
		"partial Instrumentation Score",
		"SDK-001",
		"SPA-003",
	} {
		if !strings.Contains(skill.Content, want) {
			t.Fatalf("skill content missing %q:\n%s", want, skill.Content)
		}
	}
	if strings.Contains(skill.Content, "instrumentation-score-results") {
		t.Fatalf("skill content contains default result index despite custom config:\n%s", skill.Content)
	}
}
