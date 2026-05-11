package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/specsync"
	"github.com/spf13/cobra"
)

var (
	specSyncApply bool
)

var specCmd = &cobra.Command{
	Use:   "spec",
	Short: "Inspect and sync the vendored Instrumentation Score spec",
	Long: `Manage the vendored Instrumentation Score spec under ./spec/.

Use 'vern spec status' to see drift against the upstream pinned ref,
and 'vern spec sync --apply' to overwrite local files with upstream.`,
}

var specStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show drift between local ./spec/ and the configured upstream ref",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSpecCompare(cmd.Context(), false)
	},
}

var specSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Compare local spec with upstream; --apply to overwrite local files",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSpecCompare(cmd.Context(), specSyncApply)
	},
}

func runSpecCompare(ctx context.Context, apply bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.Spec.UpstreamRepo == "" || cfg.Spec.UpstreamRef == "" {
		return errors.New("spec.upstream_repo and spec.upstream_ref must be set in vern.yaml")
	}

	specDir := filepath.Clean(filepath.Dir(cfg.RulesDir)) // ./spec/rules → ./spec
	if specDir == "." || specDir == "" {
		specDir = "./spec"
	}

	client := specsync.DefaultClient(cfg.Spec.UpstreamRepo, cfg.Spec.UpstreamRef)
	rep, err := client.Compare(ctx, specDir)
	if err != nil {
		return fmt.Errorf("compare spec: %w", err)
	}

	printSpecReport(rep)

	if !apply {
		return nil
	}

	if rep.NumOutOfSync() == 0 {
		fmt.Println("\nNo drift — nothing to apply.")
		return nil
	}

	fmt.Printf("\nApplying %d upstream changes to %s ...\n", rep.NumOutOfSync(), specDir)
	if err := client.Apply(ctx, specDir, rep); err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	fmt.Printf("Local spec now matches %s @ %s.\n", cfg.Spec.UpstreamRepo, cfg.Spec.UpstreamRef)
	fmt.Println("(Pinned ref is tracked in vern.yaml's spec.upstream_ref.)")
	return nil
}

func printSpecReport(rep *specsync.Report) {
	fmt.Printf("Upstream pinned: %s @ %s\n", rep.Repo, rep.Ref)
	fmt.Printf("Files compared:  %d (%d out of sync)\n\n", len(rep.Files), rep.NumOutOfSync())

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STATUS\tFILE")
	for _, f := range rep.Files {
		if f.Status() == "in-sync" {
			continue
		}
		fmt.Fprintf(w, "%s\t%s\n", f.Status(), f.Path)
	}
	if rep.NumOutOfSync() == 0 {
		fmt.Fprintln(w, "in-sync\t(all files match upstream)")
	}
	_ = w.Flush()
}

func init() {
	specSyncCmd.Flags().BoolVar(&specSyncApply, "apply", false,
		"overwrite local files with upstream content (also updates spec/VERSION)")
	specCmd.AddCommand(specStatusCmd)
	specCmd.AddCommand(specSyncCmd)
	rootCmd.AddCommand(specCmd)
}
