package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/dashboard"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/workflow/elastic"
	"github.com/spf13/cobra"
)

var (
	generateMappings   string
	generateOutput     string
	generateDashboards string
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

		resolved, err := mappings.Resolve(mf.Rules, cfg)
		if err != nil {
			return err
		}

		out, err := elastic.Generate(resolved, cfg)
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
		ndjson, err := dashboard.Generate(cfg)
		if err != nil {
			return fmt.Errorf("generate dashboards: %w", err)
		}
		if err := os.WriteFile(dashboardsPath, ndjson, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dashboardsPath, err)
		}
		fmt.Printf("Generated %s (Kibana saved objects)\n", dashboardsPath)
		return nil
	},
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

func init() {
	generateCmd.Flags().StringVar(&generateMappings, "mappings", "", "path to ES|QL mappings file (defaults to config.mappings)")
	generateCmd.Flags().StringVarP(&generateOutput, "output", "o", "workflows.yaml", "output workflow YAML path")
	generateCmd.Flags().StringVar(&generateDashboards, "dashboards", "", "output Kibana saved-objects NDJSON path (default: dashboards.ndjson next to --output)")
}
