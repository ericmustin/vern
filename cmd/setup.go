package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/ericmustin/vern/internal/agent"
	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/dashboard"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/review"
	syncc "github.com/ericmustin/vern/internal/sync"
	"github.com/ericmustin/vern/internal/workflow/elastic"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	setupMappings       string
	setupWorkflow       string
	setupDashboards     string
	setupKibanaURL      string
	setupAPIKey         string
	setupReplace        bool
	setupDryRun         bool
	setupSkipDashboards bool
	setupSkipAgent      bool
	setupStrictCoverage bool
)

type setupArtifacts struct {
	Config         *config.Config
	Mappings       *mappings.MappingsFile
	Coverage       coverage.Summary
	Resolved       *mappings.ResolveResult
	Workflow       []byte
	Dashboards     []byte
	WorkflowPath   string
	DashboardsPath string
	MappingsPath   string
}

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Review, generate, sync, and set up Vern in Kibana",
	Long: `Runs the happy-path Vern setup flow:
review config and rule coverage, generate the workflow and dashboards,
upload the workflow, import dashboards, and create/update the Agent Builder
agent and skill.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSetup(context.Background())
	},
}

func runSetup(ctx context.Context) error {
	kibanaURL := firstNonEmpty(setupKibanaURL, os.Getenv("KIBANA_URL"))
	apiKey := firstNonEmpty(setupAPIKey, os.Getenv("KIBANA_API_KEY"), os.Getenv("VERN_API_KEY"))

	if !setupDryRun {
		if kibanaURL == "" {
			return fmt.Errorf("missing --kibana-url or KIBANA_URL env var")
		}
		if apiKey == "" {
			return fmt.Errorf("missing --api-key or KIBANA_API_KEY env var")
		}
	}

	fmt.Println("Vern setup")
	fmt.Printf("  config: %s\n", configPath)
	if kibanaURL != "" {
		fmt.Printf("  kibana: %s\n", sanitizeSetupURL(kibanaURL))
	}
	if setupDryRun {
		fmt.Println("  mode:   dry-run (no files written, no remote changes)")
	}
	fmt.Println()

	fmt.Println("[1/6] Reviewing configuration and rule coverage")
	report, err := review.Run(ctx, review.Options{
		ConfigPath:     configPath,
		MappingsPath:   setupMappings,
		StrictCoverage: setupStrictCoverage,
	})
	if err != nil {
		return err
	}
	if report.Counts["errors"] > 0 {
		fmt.Print(indent(review.Text(report), "      "))
		return fmt.Errorf("review failed with %d error(s)", report.Counts["errors"])
	}
	fmt.Printf("      ok: %d enabled, %d heuristic, %d disabled, %d missing\n",
		report.Counts["enabled_rules"],
		report.Counts["heuristic_rules"],
		report.Counts["disabled_rules"],
		report.Counts["missing_rules"],
	)
	for _, issue := range report.Issues {
		if issue.Severity == "warning" {
			fmt.Printf("      warning: %s\n", issue.Message)
		}
	}

	fmt.Println("[2/6] Generating workflow and dashboards")
	artifacts, err := buildSetupArtifacts()
	if err != nil {
		return err
	}
	if setupDryRun {
		fmt.Printf("      would write workflow:  %s (%d bytes)\n", artifacts.WorkflowPath, len(artifacts.Workflow))
		if !setupSkipDashboards {
			fmt.Printf("      would write dashboards: %s (%d bytes)\n", artifacts.DashboardsPath, len(artifacts.Dashboards))
		}
	} else {
		if err := os.WriteFile(artifacts.WorkflowPath, artifacts.Workflow, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", artifacts.WorkflowPath, err)
		}
		fmt.Printf("      wrote workflow:  %s (%d bytes)\n", artifacts.WorkflowPath, len(artifacts.Workflow))
		if !setupSkipDashboards {
			if err := os.WriteFile(artifacts.DashboardsPath, artifacts.Dashboards, 0o644); err != nil {
				return fmt.Errorf("write %s: %w", artifacts.DashboardsPath, err)
			}
			fmt.Printf("      wrote dashboards: %s (%d bytes)\n", artifacts.DashboardsPath, len(artifacts.Dashboards))
		}
	}
	fmt.Printf("      coverage: partial=%t, spec=%s\n", artifacts.Coverage.PartialScore, artifacts.Coverage.SpecVersion)

	client := syncc.NewClient(kibanaURL, apiKey)

	fmt.Println("[3/6] Uploading workflow")
	var uploadResult *syncc.UploadResult
	if setupDryRun {
		fmt.Printf("      would POST workflow to %s/api/workflows\n", strings.TrimRight(kibanaURL, "/"))
	} else {
		if setupReplace {
			name := workflowName(artifacts.Workflow)
			if name != "" {
				existing, err := client.FindByName(name)
				if err != nil {
					return fmt.Errorf("look up existing workflows: %w", err)
				}
				if len(existing) > 0 {
					fmt.Printf("      replacing %d existing workflow(s) named %q\n", len(existing), name)
					if err := client.DeleteByIDs(existing); err != nil {
						return fmt.Errorf("delete existing workflows: %w", err)
					}
				} else {
					fmt.Printf("      no existing workflow named %q\n", name)
				}
			}
		}
		uploadResult, err = client.Upload(artifacts.Workflow, false)
		if err != nil {
			if uploadResult != nil && uploadResult.Body != "" {
				fmt.Fprintf(os.Stderr, "response: %s\n", uploadResult.Body)
			}
			return err
		}
		fmt.Printf("      uploaded: HTTP %d\n", uploadResult.StatusCode)
		if uploadResult.WorkflowID != "" {
			fmt.Printf("      workflow id: %s\n", uploadResult.WorkflowID)
			fmt.Printf("      valid: %t\n", uploadResult.Valid)
		}
	}

	fmt.Println("[4/6] Importing dashboards")
	if setupSkipDashboards {
		fmt.Println("      skipped by --skip-dashboards")
	} else if setupDryRun {
		fmt.Printf("      would import %s into Kibana saved objects\n", artifacts.DashboardsPath)
	} else {
		result, err := client.ImportSavedObjects(artifacts.Dashboards)
		if err != nil {
			return err
		}
		fmt.Printf("      imported saved objects: %d\n", result.SuccessCount)
		for _, item := range result.Imported {
			fmt.Printf("      - %s\n", item)
		}
		for _, item := range result.Errors {
			fmt.Printf("      error: %s\n", item)
		}
	}

	fmt.Println("[5/6] Setting up Agent Builder")
	if setupSkipAgent {
		fmt.Println("      skipped by --skip-agent")
	} else if setupDryRun {
		fmt.Printf("      would upsert skill: %s\n", agent.SkillID)
		fmt.Printf("      would upsert agent: %s\n", agent.AgentID)
	} else {
		skillID, agentID, err := upsertSetupAgent(client, artifacts.Config, &artifacts.Coverage)
		if err != nil {
			return err
		}
		fmt.Printf("      skill: %s\n", skillID)
		fmt.Printf("      agent: %s\n", agentID)
	}

	fmt.Println("[6/6] Setup complete")
	printSetupLinks(kibanaURL, uploadResult)
	return nil
}

func buildSetupArtifacts() (*setupArtifacts, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	mappingsPath := setupMappings
	if mappingsPath == "" {
		mappingsPath = cfg.Mappings
	}
	mf, err := mappings.Load(mappingsPath)
	if err != nil {
		return nil, err
	}
	specRules, err := coverage.LoadSpecRules(cfg.RulesDir)
	if err != nil {
		return nil, err
	}
	cov := coverage.Build(specRules, mf)
	resolved, err := mappings.Resolve(mf.Rules, cfg)
	if err != nil {
		return nil, err
	}
	workflowBytes, err := elastic.Generate(resolved, cfg, &cov)
	if err != nil {
		return nil, err
	}
	dashboardsBytes, err := dashboard.Generate(cfg, &cov)
	if err != nil {
		return nil, err
	}
	dashboardsPath := setupDashboards
	if dashboardsPath == "" {
		dashboardsPath = defaultDashboardsPath(setupWorkflow)
	}
	return &setupArtifacts{
		Config:         cfg,
		Mappings:       mf,
		Coverage:       cov,
		Resolved:       resolved,
		Workflow:       workflowBytes,
		Dashboards:     dashboardsBytes,
		WorkflowPath:   setupWorkflow,
		DashboardsPath: dashboardsPath,
		MappingsPath:   mappingsPath,
	}, nil
}

func upsertSetupAgent(client *syncc.Client, cfg *config.Config, cov *coverage.Summary) (string, string, error) {
	s := agent.BuildSkill(agent.Context{Config: cfg, Coverage: cov})
	skillNoID, err := json.Marshal(struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Content     string   `json:"content"`
		ToolIDs     []string `json:"tool_ids"`
	}{s.Name, s.Description, s.Content, s.ToolIDs})
	if err != nil {
		return "", "", fmt.Errorf("encode skill (PUT): %w", err)
	}
	skillWithID, err := json.Marshal(s)
	if err != nil {
		return "", "", fmt.Errorf("encode skill (POST): %w", err)
	}
	skillID, err := client.UpsertSkill(s.ID, skillNoID, skillWithID)
	if err != nil {
		return "", "", fmt.Errorf("upsert skill: %w", err)
	}

	a := agent.BuildAgent()
	bodyNoID, err := json.Marshal(struct {
		Name          string              `json:"name"`
		Description   string              `json:"description"`
		Configuration agent.Configuration `json:"configuration"`
	}{a.Name, a.Description, a.Configuration})
	if err != nil {
		return "", "", fmt.Errorf("encode agent (PUT): %w", err)
	}
	bodyWithID, err := json.Marshal(a)
	if err != nil {
		return "", "", fmt.Errorf("encode agent (POST): %w", err)
	}
	agentID, err := client.UpsertAgent(a.ID, bodyNoID, bodyWithID)
	if err != nil {
		return "", "", fmt.Errorf("upsert agent: %w", err)
	}
	return skillID, agentID, nil
}

func workflowName(yamlBytes []byte) string {
	var probe struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(yamlBytes, &probe); err != nil {
		return ""
	}
	return probe.Name
}

func printSetupLinks(kibanaURL string, uploadResult *syncc.UploadResult) {
	if kibanaURL == "" {
		fmt.Println("      links: provide --kibana-url to print clickable Kibana links")
		return
	}
	base := strings.TrimRight(kibanaURL, "/")
	fmt.Println()
	fmt.Println("Open:")
	fmt.Printf("  Workflows:     %s/app/workflows\n", base)
	if uploadResult != nil && uploadResult.WorkflowID != "" {
		fmt.Printf("  Workflow ID:   %s\n", uploadResult.WorkflowID)
	}
	fmt.Printf("  Overview:      %s/app/dashboards#/view/vern-overview\n", base)
	fmt.Printf("  Drill-down:    %s/app/dashboards#/view/vern-drilldown\n", base)
	fmt.Printf("  Agent Builder: %s/app/agent_builder\n", base)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func sanitizeSetupURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n") + "\n"
}

func init() {
	setupCmd.Flags().StringVar(&setupMappings, "mappings", "", "path to ES|QL mappings file (defaults to config.mappings)")
	setupCmd.Flags().StringVarP(&setupWorkflow, "workflow", "w", "workflows.yaml", "workflow YAML output path")
	setupCmd.Flags().StringVar(&setupDashboards, "dashboards", "", "dashboards NDJSON output path (default: dashboards.ndjson next to --workflow)")
	setupCmd.Flags().StringVar(&setupKibanaURL, "kibana-url", "", "Kibana base URL (env: KIBANA_URL)")
	setupCmd.Flags().StringVar(&setupAPIKey, "api-key", "", "Kibana API key (env: KIBANA_API_KEY)")
	setupCmd.Flags().BoolVar(&setupReplace, "replace", false, "delete any existing workflow(s) with the same name before uploading")
	setupCmd.Flags().BoolVar(&setupDryRun, "dry-run", false, "render and print planned steps without writing files or making remote changes")
	setupCmd.Flags().BoolVar(&setupSkipDashboards, "skip-dashboards", false, "skip writing and importing dashboards")
	setupCmd.Flags().BoolVar(&setupSkipAgent, "skip-agent", false, "skip Agent Builder skill and agent setup")
	setupCmd.Flags().BoolVar(&setupStrictCoverage, "strict-coverage", false, "fail when rule coverage is partial")
}
