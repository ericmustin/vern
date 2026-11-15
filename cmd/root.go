package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

var (
	version    = "0.1.0-dev"
	configPath string
)

var rootCmd = &cobra.Command{
	Use:           "vern",
	Short:         "Vern — score OpenTelemetry instrumentation quality (OTLP captures, Elastic Workflows)",
	Version:       version,
	SilenceUsage:  true,
	SilenceErrors: true,
	Long: `Vern is a vendor-agnostic CLI that maps Instrumentation Score spec rules to
backend queries. It scores OTLP captures on a self-managed Elasticsearch
(vern score), evaluates live backends (vern evaluate), and emits an Elastic
Workflows YAML that runs the rules on a schedule in an Elastic Serverless
project (vern generate, vern setup).`,
}

// exitError carries the process exit code for an error. Errors that are not
// exitErrors exit 1.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

// exitCode maps an error returned by a command to the process exit code:
// 0 for nil, the code of the first exitError in the chain, otherwise 1.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 1
}

// Execute runs the root command, prints any error once to stderr (cobra's
// own error and usage printing are silenced), and exits with its code.
func Execute() {
	err := rootCmd.Execute()
	if err == nil {
		return
	}
	os.Exit(printError(os.Stderr, err))
}

// printError writes err to w once and returns its exit code. A failed
// --min-score gate (exit 2) is not an error, so it gets a "gate:" prefix;
// everything else gets "error:".
func printError(w io.Writer, err error) int {
	code := exitCode(err)
	prefix := "error:"
	if code == 2 {
		prefix = "gate:"
	}
	fmt.Fprintln(w, prefix, err)
	return code
}

func init() {
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "vern.yaml", "path to vern config file")
	rootCmd.AddCommand(generateCmd)
	rootCmd.AddCommand(syncCmd)
	rootCmd.AddCommand(agentCmd)
	rootCmd.AddCommand(reviewCmd)
	rootCmd.AddCommand(setupCmd)
	rootCmd.AddCommand(evaluateCmd)
	rootCmd.AddCommand(compareCmd)
	rootCmd.AddCommand(scoreCmd)
}
