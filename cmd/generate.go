package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ericmustin/vern/internal/agent"
	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/dashboard"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/sdksupport"
	"github.com/ericmustin/vern/internal/semconv"
	"github.com/ericmustin/vern/internal/workflow/elastic"
	"github.com/spf13/cobra"
)

var (
	generateMappings   string
	generateOutput     string
	generateDashboards string
	generateAgentSkill string
)

var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate an Elastic Workflows YAML from rule mappings",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}

		mappingsPath := generateMappings
		if mappingsPath == "" {
			mappingsPath = cfg.Mappings
		}

		mf, err := mappings.Load(mappingsPath)
		if err != nil {
			return err
		}

		applyOptInGates(mf, cfg)

		specRules, err := coverage.LoadSpecRules(cfg.RulesDir)
		if err != nil {
			return err
		}
		cov := coverage.Build(specRules, mf)

		resolved, err := mappings.ResolveWithData(mf.Rules, cfg, mappings.TemplateData{
			SemconvAttributeKeys:    semconv.QuotedCSV(semconv.AttributeKeys),
			SemconvResourceOnlyKeys: semconv.QuotedCSV(semconv.ResourceOnlyKeys),
			SemconvSpanOnlyKeys:     semconv.QuotedCSV(semconv.SpanOnlyKeys),
			SemconvLogOnlyKeys:      semconv.QuotedCSV(semconv.LogOnlyKeys),
			SemconvMetricOnlyKeys:   semconv.QuotedCSV(semconv.MetricOnlyKeys),
			MET001AttrKeys:          semconv.MET001AttrAllowlist(),
			SDKSupportMatrix:        sdksupport.RenderESQLCase("resource.attributes.telemetry.sdk.language", "resource.attributes.telemetry.sdk.version"),
		})
		if err != nil {
			return err
		}

		out, err := elastic.Generate(resolved, cfg, &cov)
		if err != nil {
			return err
		}

		if err := os.WriteFile(generateOutput, out, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", generateOutput, err)
		}

		fmt.Printf("Generated %s\n", generateOutput)
		fmt.Printf("  rules: %d enabled", len(resolved.Rules))
		if resolved.Aggregation != nil {
			fmt.Printf(" + score aggregation")
		}
		if len(resolved.Skipped) > 0 {
			fmt.Printf(" (%d skipped)", len(resolved.Skipped))
		}
		fmt.Println()
		for _, s := range resolved.Skipped {
			fmt.Printf("  skipped: %s (%s)\n", s.SpecRuleID, s.Reason)
		}

		dashboardsPath := generateDashboards
		if dashboardsPath == "" {
			dashboardsPath = defaultDashboardsPath(generateOutput)
		}
		ndjson, err := dashboard.Generate(cfg, &cov)
		if err != nil {
			return fmt.Errorf("generate dashboards: %w", err)
		}
		if err := os.WriteFile(dashboardsPath, ndjson, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dashboardsPath, err)
		}
		fmt.Printf("Generated %s (Kibana saved objects)\n", dashboardsPath)

		agentSkillPath := generateAgentSkill
		if agentSkillPath == "" {
			agentSkillPath = defaultAgentSkillPath(generateOutput)
		}
		skillMarkdown := agent.RenderSkillMarkdown(agent.Context{Config: cfg, Coverage: &cov})
		if err := os.WriteFile(agentSkillPath, []byte(skillMarkdown), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", agentSkillPath, err)
		}
		fmt.Printf("Generated %s (Agent Builder skill markdown)\n", agentSkillPath)
		return nil
	},
}

// applyOptInGates flips Enabled on rules whose OptInFlag is satisfied by
// the active config (and turns off any rule whose flag is not satisfied,
// in case the YAML accidentally had it enabled). Today only SDK-001 uses
// this — its opt_in_flag is "filters.enable_sdk_rules".
func applyOptInGates(mf *mappings.MappingsFile, cfg *config.Config) {
	flagSet := func(name string) bool {
		switch name {
		case "":
			return true // no gate
		case "filters.enable_sdk_rules":
			return cfg.Filters.EnableSDKRules
		default:
			return false
		}
	}
	for i := range mf.Rules {
		if mf.Rules[i].OptInFlag == "" {
			continue
		}
		mf.Rules[i].Enabled = flagSet(mf.Rules[i].OptInFlag)
	}
}

// defaultDashboardsPath puts dashboards.ndjson next to the workflow file.
func defaultDashboardsPath(workflowOutput string) string {
	dir := filepath.Dir(workflowOutput)
	base := filepath.Base(workflowOutput)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	if name == "workflows" || name == "workflow" {
		return filepath.Join(dir, "dashboards.ndjson")
	}
	return filepath.Join(dir, name+"-dashboards.ndjson")
}

// defaultAgentSkillPath puts agent-skill.md next to the workflow file.
func defaultAgentSkillPath(workflowOutput string) string {
	dir := filepath.Dir(workflowOutput)
	base := filepath.Base(workflowOutput)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	if name == "workflows" || name == "workflow" {
		return filepath.Join(dir, "agent-skill.md")
	}
	return filepath.Join(dir, name+"-agent-skill.md")
}

func init() {
	generateCmd.Flags().StringVar(&generateMappings, "mappings", "", "path to ES|QL mappings file (defaults to config.mappings)")
	generateCmd.Flags().StringVarP(&generateOutput, "output", "o", "workflows.yaml", "output workflow YAML path")
	generateCmd.Flags().StringVar(&generateDashboards, "dashboards", "", "output Kibana saved-objects NDJSON path (default: dashboards.ndjson next to --output)")
	generateCmd.Flags().StringVar(&generateAgentSkill, "agent-skill", "", "output Agent Builder skill markdown path (default: agent-skill.md next to --output)")
}
