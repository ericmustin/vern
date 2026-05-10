package cmd

import (
	"context"
	"fmt"

	"github.com/ericmustin/vern/internal/review"
	"github.com/spf13/cobra"
)

var (
	reviewMappings       string
	reviewFormat         string
	reviewStrictCoverage bool
	reviewLiveESURL      string
)

var reviewCmd = &cobra.Command{
	Use:   "review",
	Short: "Review Vern reproducibility, generated artifact config flow, and rule coverage",
	RunE: func(cmd *cobra.Command, args []string) error {
		report, err := review.Run(context.Background(), review.Options{
			ConfigPath:     configPath,
			MappingsPath:   reviewMappings,
			Format:         reviewFormat,
			StrictCoverage: reviewStrictCoverage,
			LiveESURL:      reviewLiveESURL,
		})
		if err != nil {
			return err
		}

		switch reviewFormat {
		case "", "text":
			fmt.Print(review.Text(report))
		case "json":
			out, err := review.JSON(report)
			if err != nil {
				return err
			}
			fmt.Println(string(out))
		default:
			return fmt.Errorf("unsupported --format %q (want text or json)", reviewFormat)
		}

		if report.Counts["errors"] > 0 {
			return fmt.Errorf("review failed with %d error(s)", report.Counts["errors"])
		}
		return nil
	},
}

func init() {
	reviewCmd.Flags().StringVar(&reviewMappings, "mappings", "", "path to ES|QL mappings file (defaults to config.mappings)")
	reviewCmd.Flags().StringVar(&reviewFormat, "format", "text", "output format: text or json")
	reviewCmd.Flags().BoolVar(&reviewStrictCoverage, "strict-coverage", false, "fail when rule coverage is partial")
	reviewCmd.Flags().StringVar(&reviewLiveESURL, "live-es-url", "", "optional Elasticsearch URL for live ES|QL validation")
}
