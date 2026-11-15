package cmd

import (
	"slices"
	"strings"
	"testing"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/workflow/elastic"
)

func TestDefaultAgentSkillPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "default workflows name", in: "workflows.yaml", want: "agent-skill.md"},
		{name: "singular workflow name", in: "workflow.yml", want: "agent-skill.md"},
		{name: "custom output name", in: "build/review.yaml", want: "build/review-agent-skill.md"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultAgentSkillPath(tt.in); got != tt.want {
				t.Fatalf("defaultAgentSkillPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func loadRepoForResolve(t *testing.T) (*mappings.MappingsFile, *config.Config) {
	t.Helper()
	cfg, err := config.Load("../vern.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	mf, err := mappings.Load("../configs/esql-mappings.yaml")
	if err != nil {
		t.Fatalf("load mappings: %v", err)
	}
	return mf, cfg
}

func TestResolveForWorkflow_UsesSemconvCatalog(t *testing.T) {
	mf, cfg := loadRepoForResolve(t)
	res, err := resolveForWorkflow(mf, cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	found := false
	for _, r := range res.Rules {
		if r.SpecRuleID == "MET-006" {
			found = true
			if !strings.Contains(r.ResolvedQuery, `"service.version"`) {
				t.Errorf("MET-006 missing semconv keys:\n%s", r.ResolvedQuery)
			}
		}
	}
	if !found {
		t.Fatal("MET-006 not resolved")
	}
}

func TestResolveForWorkflow_SDKRuleEnabledByFlag(t *testing.T) {
	mf, cfg := loadRepoForResolve(t)
	cfg.Filters.EnableSDKRules = true
	res, err := resolveForWorkflow(mf, cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, r := range res.Rules {
		if r.SpecRuleID == "SDK-001" {
			return
		}
	}
	t.Fatal("SDK-001 not resolved although filters.enable_sdk_rules is true")
}

func TestResolveForWorkflow_SDKRuleGatedOff(t *testing.T) {
	mf, cfg := loadRepoForResolve(t)
	cfg.Filters.EnableSDKRules = false
	res, err := resolveForWorkflow(mf, cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, r := range res.Rules {
		if r.SpecRuleID == "SDK-001" {
			t.Fatal("SDK-001 resolved although filters.enable_sdk_rules is false")
		}
	}
}

// TestGenerate_SensitiveRulesAbsentFromWorkflow: generate's path
// (workflow gates, coverage, resolution, formatter) leaves the four
// sensitive-data rules out of the workflow and reports them disabled, with
// the per-run field discovery reason, in the coverage the _COVERAGE
// document and the agent skill are built from.
func TestGenerate_SensitiveRulesAbsentFromWorkflow(t *testing.T) {
	mf, cfg := loadRepoForResolve(t)
	mappings.ApplyWorkflowGates(mf.Rules, cfg)
	specRules, err := coverage.LoadSpecRules("../spec/rules")
	if err != nil {
		t.Fatal(err)
	}
	cov := coverage.Build(specRules, mf)
	res, err := resolveForWorkflow(mf, cfg)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	out, err := elastic.Generate(res, cfg, &cov)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	wf := string(out)
	if !strings.Contains(wf, "name: eval_res_005") {
		t.Fatal("ordinary rule RES-005 missing from the workflow")
	}
	reasons := map[string]string{}
	for _, r := range cov.Rules {
		reasons[r.ID] = r.Status + ": " + r.Reason
	}
	for _, id := range []string{"RES-010", "SPA-006", "LOG-003", "MET-008"} {
		slug := strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		if strings.Contains(wf, "eval_"+slug) || strings.Contains(wf, "rule_id: "+id) {
			t.Errorf("%s has a workflow step", id)
		}
		if slices.Contains(cov.EnabledRules, id) || !slices.Contains(cov.DisabledRules, id) {
			t.Errorf("%s: coverage lists it enabled", id)
		}
		if want := "disabled: " + mappings.NeedsFieldDiscovery; reasons[id] != want {
			t.Errorf("%s coverage = %q, want %q", id, reasons[id], want)
		}
	}
}
