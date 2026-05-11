package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/semconv"
	"github.com/spf13/cobra"
)

var (
	semconvSyncApply bool
	semconvOutDir    string
)

var semconvCmd = &cobra.Command{
	Use:   "semconv",
	Short: "Sync the vendored OpenTelemetry semantic-convention catalog",
	Long: `Manage the vendored semantic-convention catalog under internal/semconv/.

The catalog powers two rules:
  - MET-006: metric names must not equal semconv attribute keys
  - RES-004: semconv attributes must appear at the right OTLP level

'vern semconv sync' fetches the upstream YAMLs at the pinned ref, prints a
summary, and (with --apply) regenerates attribute_keys.go and placement.go.`,
}

var semconvSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Fetch upstream semconv YAMLs and (with --apply) regenerate Go catalog",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSemconvSync(cmd.Context(), semconvSyncApply, semconvOutDir)
	},
}

func runSemconvSync(ctx context.Context, apply bool, outDir string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if cfg.Semconv.UpstreamRepo == "" || cfg.Semconv.UpstreamRef == "" {
		return errors.New("semconv.upstream_repo and semconv.upstream_ref must be set in vern.yaml")
	}

	dir := outDir
	if dir == "" {
		dir = filepath.Join("internal", "semconv")
	}

	fmt.Printf("Fetching %s @ %s ...\n", cfg.Semconv.UpstreamRepo, cfg.Semconv.UpstreamRef)
	fc := semconv.DefaultFetchClient(cfg.Semconv.UpstreamRepo, cfg.Semconv.UpstreamRef)
	docs, err := fc.FetchAll(ctx)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	fmt.Printf("Fetched %d model YAMLs.\n", len(docs))

	cat, err := semconv.Build(cfg.Semconv.UpstreamRef, docs)
	if err != nil {
		return fmt.Errorf("build catalog: %w", err)
	}

	fmt.Printf("\nCatalog summary (would be written to %s):\n", dir)
	fmt.Printf("  AttributeKeys:    %d\n", len(cat.AttributeKeys))
	fmt.Printf("  ResourceOnlyKeys: %d\n", len(cat.LevelKeys(semconv.LevelResource)))
	fmt.Printf("  SpanOnlyKeys:     %d\n", len(cat.LevelKeys(semconv.LevelSpan)))
	fmt.Printf("  LogOnlyKeys:      %d\n", len(cat.LevelKeys(semconv.LevelLog)))
	fmt.Printf("  MetricOnlyKeys:   %d\n", len(cat.LevelKeys(semconv.LevelMetric)))

	if local := semconv.ReadVersionFile(dir); local != "" {
		fmt.Printf("\nCurrent local catalog version: %s\n", local)
	} else {
		fmt.Println("\nCurrent local catalog version: (none — never synced)")
	}

	if !apply {
		fmt.Println("\nDry run — pass --apply to write generated files.")
		return nil
	}

	if err := semconv.GenerateFiles(cat, dir); err != nil {
		return fmt.Errorf("write generated files: %w", err)
	}
	fmt.Printf("\nWrote %s/attribute_keys.go, %s/placement.go, %s/VERSION.\n", dir, dir, dir)
	return nil
}

func init() {
	semconvSyncCmd.Flags().BoolVar(&semconvSyncApply, "apply", false,
		"regenerate internal/semconv/*.go (otherwise dry run)")
	semconvSyncCmd.Flags().StringVar(&semconvOutDir, "out-dir", "",
		"override output directory (default internal/semconv)")
	semconvCmd.AddCommand(semconvSyncCmd)
	rootCmd.AddCommand(semconvCmd)
}
