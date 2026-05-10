package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/ericmustin/vern/internal/agent"
	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/sync"
	"github.com/spf13/cobra"
)

var (
	agentKibanaURL string
	agentAPIKey    string
	agentMappings  string
)

var agentCmd = &cobra.Command{
	Use:   "agent",
	Short: "Manage the Vern Agent Builder agent in Kibana",
	Long: `Vern can ship an optional Elastic Agent Builder agent that knows the
configured Vern result-index schema and how to query it. Use it to ask
"what's the score for service X?", "show me failing rules for X", or
"best/worst services" in conversational form, with citations to the
upstream spec.`,
}

var agentSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Create or update the Vern agent in Kibana → Agent Builder",
	RunE: func(cmd *cobra.Command, args []string) error {
		kibanaURL := agentKibanaURL
		if kibanaURL == "" {
			kibanaURL = os.Getenv("KIBANA_URL")
		}
		apiKey := agentAPIKey
		if apiKey == "" {
			apiKey = os.Getenv("KIBANA_API_KEY")
			if apiKey == "" {
				apiKey = os.Getenv("VERN_API_KEY")
			}
		}
		if kibanaURL == "" {
			return fmt.Errorf("missing --kibana-url or KIBANA_URL env var")
		}
		if apiKey == "" {
			return fmt.Errorf("missing --api-key or KIBANA_API_KEY env var")
		}

		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		mappingsPath := agentMappings
		if mappingsPath == "" {
			mappingsPath = cfg.Mappings
		}
		mf, err := mappings.Load(mappingsPath)
		if err != nil {
			return err
		}
		specRules, err := coverage.LoadSpecRules(cfg.RulesDir)
		if err != nil {
			return err
		}
		cov := coverage.Build(specRules, mf)

		client := sync.NewClient(kibanaURL, apiKey)

		// 1) Upsert the governance skill first — the agent references it.
		s := agent.BuildSkill(agent.Context{Config: cfg, Coverage: &cov})
		skillNoID, err := json.Marshal(struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Content     string   `json:"content"`
			ToolIDs     []string `json:"tool_ids"`
		}{s.Name, s.Description, s.Content, s.ToolIDs})
		if err != nil {
			return fmt.Errorf("encode skill (PUT): %w", err)
		}
		skillWithID, err := json.Marshal(s)
		if err != nil {
			return fmt.Errorf("encode skill (POST): %w", err)
		}
		skillID, err := client.UpsertSkill(s.ID, skillNoID, skillWithID)
		if err != nil {
			return fmt.Errorf("upsert skill: %w", err)
		}

		// 2) Upsert the agent that references the skill.
		a := agent.BuildAgent()
		bodyNoID, err := json.Marshal(struct {
			Name          string              `json:"name"`
			Description   string              `json:"description"`
			Configuration agent.Configuration `json:"configuration"`
		}{a.Name, a.Description, a.Configuration})
		if err != nil {
			return fmt.Errorf("encode agent (PUT): %w", err)
		}
		bodyWithID, err := json.Marshal(a)
		if err != nil {
			return fmt.Errorf("encode agent (POST): %w", err)
		}

		id, err := client.UpsertAgent(a.ID, bodyNoID, bodyWithID)
		if err != nil {
			return err
		}

		fmt.Printf("Vern agent + skill ready in Kibana → Agent Builder\n")
		fmt.Printf("  skill: %s\n", skillID)
		fmt.Printf("  agent: %s (%s)\n", id, a.Name)
		fmt.Printf("\nOpen %s/app/agent_builder to chat. Try:\n", kibanaURL)
		fmt.Println("  - \"what's the instrumentation score for shipping?\"")
		fmt.Println("  - \"which 5 services have the worst scores?\"")
		fmt.Println("  - \"show me failing rules for cart with example doc ids\"")
		fmt.Println("  - \"what does the SPA-004 rule check?\"")
		return nil
	},
}

func init() {
	agentCmd.AddCommand(agentSetupCmd)
	agentCmd.PersistentFlags().StringVar(&agentKibanaURL, "kibana-url", "", "Kibana base URL (env: KIBANA_URL)")
	agentCmd.PersistentFlags().StringVar(&agentAPIKey, "api-key", "", "Kibana API key (env: KIBANA_API_KEY)")
	agentCmd.PersistentFlags().StringVar(&agentMappings, "mappings", "", "path to ES|QL mappings file (defaults to config.mappings)")
}
