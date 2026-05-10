package cmd

import (
	"github.com/spf13/cobra"
)

var (
	version    = "0.1.0-dev"
	configPath string
)

var rootCmd = &cobra.Command{
	Use:           "vern",
	Short:         "Vern — generate Elastic Workflows YAML for OpenTelemetry instrumentation scoring",
	Version:       version,
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Vern is a vendor-agnostic CLI that maps Instrumentation Score spec rules to
ES|QL queries and emits an Elastic Workflows YAML that runs them on a schedule
in an Elastic Serverless project.`,
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "vern.yaml", "path to vern config file")
	rootCmd.AddCommand(generateCmd)
	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(agentCmd)
	rootCmd.AddCommand(reviewCmd)
	rootCmd.AddCommand(setupCmd)
}
