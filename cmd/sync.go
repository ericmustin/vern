package cmd

import (
	"fmt"
	"os"

	"github.com/ericmustin/vern/internal/sync"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	syncWorkflow   string
	syncDashboards string
	syncKibanaURL  string
	syncAPIKey     string
	syncDryRun     bool
	syncReplace    bool
	syncSkipDash   bool
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Upload a workflow YAML to an Elastic Serverless project via the Kibana API",
	RunE: func(cmd *cobra.Command, args []string) error {
		yamlBytes, err := os.ReadFile(syncWorkflow)
		if err != nil {
			return fmt.Errorf("read workflow %s: %w", syncWorkflow, err)
		}

		var probe interface{}
		if err := yaml.Unmarshal(yamlBytes, &probe); err != nil {
			return fmt.Errorf("invalid YAML in %s: %w", syncWorkflow, err)
		}

		kibanaURL := syncKibanaURL
		if kibanaURL == "" {
			kibanaURL = os.Getenv("KIBANA_URL")
		}
		apiKey := syncAPIKey
		if apiKey == "" {
			apiKey = os.Getenv("KIBANA_API_KEY")
			if apiKey == "" {
				apiKey = os.Getenv("VERN_API_KEY")
			}
		}

		if !syncDryRun {
			if kibanaURL == "" {
				return fmt.Errorf("missing --kibana-url or KIBANA_URL env var")
			}
			if apiKey == "" {
				return fmt.Errorf("missing --api-key or KIBANA_API_KEY env var")
			}
		}

		client := sync.NewClient(kibanaURL, apiKey)

		if syncReplace && !syncDryRun {
			var probeWf struct {
				Name string `yaml:"name"`
			}
			if err := yaml.Unmarshal(yamlBytes, &probeWf); err == nil && probeWf.Name != "" {
				existing, err := client.FindByName(probeWf.Name)
				if err != nil {
					return fmt.Errorf("look up existing workflows: %w", err)
				}
				if len(existing) > 0 {
					fmt.Printf("Replacing %d existing workflow(s) named %q: %v\n", len(existing), probeWf.Name, existing)
					if err := client.DeleteByIDs(existing); err != nil {
						return fmt.Errorf("delete existing workflows: %w", err)
					}
				}
			}
		}

		result, err := client.Upload(yamlBytes, syncDryRun)
		if err != nil {
			if result != nil && result.Body != "" {
				fmt.Fprintf(os.Stderr, "response: %s\n", result.Body)
			}
			return err
		}

		if syncDryRun {
			fmt.Println("Dry run successful — nothing uploaded.")
			return nil
		}

		fmt.Printf("Uploaded workflow (HTTP %d)\n", result.StatusCode)
		if result.WorkflowID != "" {
			fmt.Printf("  workflow id: %s\n", result.WorkflowID)
			if result.Name != "" {
				fmt.Printf("  name:        %s\n", result.Name)
			}
			fmt.Printf("  valid:       %t\n", result.Valid)
			if !result.Valid {
				fmt.Println("  (Kibana parsed the YAML but flagged it invalid — open the workflow in Kibana to see specific errors)")
			}
		} else if result.Body != "" {
			fmt.Printf("  response: %s\n", result.Body)
		}

		if !syncSkipDash {
			dashPath := syncDashboards
			if dashPath == "" {
				dashPath = "dashboards.ndjson"
			}
			ndjson, err := os.ReadFile(dashPath)
			if err != nil {
				if os.IsNotExist(err) {
					fmt.Printf("\nSkipping dashboards: %s not found (run `vern generate` first or pass --skip-dashboards)\n", dashPath)
					return nil
				}
				return fmt.Errorf("read %s: %w", dashPath, err)
			}
			fmt.Printf("\nImporting Kibana saved objects from %s\n", dashPath)
			doRes, err := client.ImportSavedObjects(ndjson)
			if err != nil {
				return err
			}
			fmt.Printf("  successCount: %d\n", doRes.SuccessCount)
			for _, item := range doRes.Imported {
				fmt.Printf("    %s\n", item)
			}
			for _, e := range doRes.Errors {
				fmt.Printf("    error: %s\n", e)
			}
		}
		return nil
	},
}

func init() {
	syncCmd.Flags().StringVarP(&syncWorkflow, "workflow", "w", "workflows.yaml", "path to workflow YAML file")
	syncCmd.Flags().StringVar(&syncKibanaURL, "kibana-url", "", "Kibana base URL (env: KIBANA_URL)")
	syncCmd.Flags().StringVar(&syncAPIKey, "api-key", "", "Kibana API key (env: KIBANA_API_KEY)")
	syncCmd.Flags().BoolVar(&syncDryRun, "dry-run", false, "validate and print payload size without uploading")
	syncCmd.Flags().BoolVar(&syncReplace, "replace", false, "delete any existing workflow(s) with the same name before uploading (idempotent updates)")
	syncCmd.Flags().StringVar(&syncDashboards, "dashboards", "", "path to dashboards NDJSON (default: dashboards.ndjson)")
	syncCmd.Flags().BoolVar(&syncSkipDash, "skip-dashboards", false, "skip the saved-objects import step")
}
