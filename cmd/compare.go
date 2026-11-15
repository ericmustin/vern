package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ericmustin/vern/internal/compare"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/spf13/cobra"
)

var (
	compareLabels       []string
	compareExpectedGaps []string
	compareExtentTol    float64
	compareScoreTol     float64
	compareOverlapMin   float64
	compareOutput       string
)

var compareCmd = &cobra.Command{
	Use:   "compare",
	Short: "Diff vern evaluation reports across backends and flag score disagreements",
	Long: `Compares two or more JSON reports produced by 'vern evaluate' (one per
backend) and reports where per-service, per-rule outcomes disagree beyond a
configurable tolerance.

Each --label is name=path. The comparison writes a Markdown summary and a
JSON sibling, and exits non-zero when a disagreement exceeds tolerance:
  1  rule_passed mismatch
  2  extent tolerance exceeded
  3  score tolerance exceeded
  4  service-set drift`,
	Example: `  vern compare \
    --label elastic=reports/elastic.json \
    --label clickhouse=reports/clickhouse.json \
    --label opensearch=reports/opensearch.json \
    --expected-gap opensearch:MET-001 \
    --expected-gap opensearch:MET-006 \
    --output reports/compare.md`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(compareLabels) < 2 {
			return fmt.Errorf("need at least two --label name=path entries")
		}

		labels := make([]string, 0, len(compareLabels))
		reports := map[string]*evaluate.Report{}
		for _, entry := range compareLabels {
			name, path, ok := strings.Cut(entry, "=")
			if !ok || name == "" || path == "" {
				return fmt.Errorf("invalid --label %q (want name=path)", entry)
			}
			if _, dup := reports[name]; dup {
				return fmt.Errorf("duplicate --label name %q", name)
			}
			rep, err := loadReport(path)
			if err != nil {
				return err
			}
			labels = append(labels, name)
			reports[name] = rep
		}

		opts := compare.DefaultOptions()
		opts.ExtentTolerance = compareExtentTol
		opts.ScoreTolerance = compareScoreTol
		opts.ServiceOverlapMin = compareOverlapMin

		gaps, err := parseExpectedGaps(compareExpectedGaps, reports)
		if err != nil {
			return err
		}
		opts.ExpectedGaps = gaps

		result := compare.Compare(reports, labels, opts)

		if err := compare.WriteMarkdown(compareOutput, result); err != nil {
			return err
		}
		jsonPath := strings.TrimSuffix(compareOutput, ".md") + ".json"
		if err := compare.WriteJSON(jsonPath, result); err != nil {
			return err
		}

		printCompareSummary(result, compareOutput, jsonPath)

		// The reports are written; signal the verdict via the exit code.
		if result.ExitCode != 0 {
			os.Exit(result.ExitCode)
		}
		return nil
	},
}

func init() {
	compareCmd.Flags().StringArrayVar(&compareLabels, "label", nil, "report as name=path (repeatable, min 2)")
	compareCmd.Flags().StringArrayVar(&compareExpectedGaps, "expected-gap", nil, "label:rule_id the backend is not expected to evaluate (repeatable)")
	compareCmd.Flags().Float64Var(&compareExtentTol, "extent-tolerance", 0.05, "max allowed extent spread when rule_passed agrees")
	compareCmd.Flags().Float64Var(&compareScoreTol, "score-tolerance", 10.0, "max allowed per-service score spread")
	compareCmd.Flags().Float64Var(&compareOverlapMin, "service-overlap-min", 0.80, "min per-backend service coverage ratio (0-1)")
	compareCmd.Flags().StringVar(&compareOutput, "output", "vern-compare.md", "path for the Markdown comparison (JSON written alongside)")
}

func loadReport(path string) (*evaluate.Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read report %s: %w", path, err)
	}
	var rep evaluate.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, fmt.Errorf("parse report %s: %w", path, err)
	}
	return &rep, nil
}

func parseExpectedGaps(entries []string, reports map[string]*evaluate.Report) (map[string][]string, error) {
	gaps := map[string][]string{}
	for _, e := range entries {
		label, rule, ok := strings.Cut(e, ":")
		if !ok || label == "" || rule == "" {
			return nil, fmt.Errorf("invalid --expected-gap %q (want label:rule_id)", e)
		}
		if _, known := reports[label]; !known {
			return nil, fmt.Errorf("--expected-gap %q references unknown label %q", e, label)
		}
		gaps[label] = append(gaps[label], rule)
	}
	return gaps, nil
}

func printCompareSummary(r *compare.Report, mdPath, jsonPath string) {
	fmt.Printf("Cross-backend comparison — %s\n", r.Verdict)
	for _, b := range r.Backends {
		fmt.Printf("  %-12s overall=%.1f services=%d rules=%d/%d\n",
			b.Label, b.Overall, b.ServiceCount, b.RulesSucceeded, b.RulesAttempted)
	}
	if len(r.Discrepancies) > 0 {
		fmt.Printf("  discrepancies: %d\n", len(r.Discrepancies))
		for _, d := range r.Discrepancies {
			loc := ""
			if d.Service != "" {
				loc = " [" + d.Service
				if d.RuleID != "" {
					loc += "/" + d.RuleID
				}
				loc += "]"
			}
			fmt.Printf("    - %s%s: %s\n", d.KindName, loc, d.Detail)
		}
	}
	fmt.Printf("  md:   %s\n", mdPath)
	fmt.Printf("  json: %s\n", jsonPath)
}
