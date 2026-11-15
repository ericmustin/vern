package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/evaluate"
	clickhouseexec "github.com/ericmustin/vern/internal/evaluate/clickhouse"
	elasticexec "github.com/ericmustin/vern/internal/evaluate/elastic"
	opensearchexec "github.com/ericmustin/vern/internal/evaluate/opensearch"
	"github.com/ericmustin/vern/internal/report"
	"github.com/ericmustin/vern/internal/score"
	"github.com/spf13/cobra"
)

var (
	evalBackend    string
	evalOutputJSON string
	evalOutputMD   string
	evalTimeout    time.Duration
)

var evaluateCmd = &cobra.Command{
	Use:   "evaluate",
	Short: "Run Instrumentation Score rules locally against a query backend and write a report",
	Long: `Evaluates Instrumentation Score rules against a live observability backend
(Elastic, ClickHouse, or OpenSearch). For each rule, vern executes the
backend-native query and pulls per-service results back, computes the score
in Go, and writes JSON + Markdown reports to disk.

Unlike 'vern generate' + 'vern sync', this path does NOT schedule anything
in the backend and does NOT write results back to the platform.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}

		backend := pickBackend(evalBackend, cfg.Backend)
		cfg.Backend = backend

		exec, mappingsPath, err := buildExecutor(backend, cfg)
		if err != nil {
			return err
		}

		// If the user has a backend-specific mappings file but the config still
		// points at the default esql-mappings.yaml, swap to the backend's file.
		if mappingsPath != "" {
			cfg.Mappings = mappingsPath
		}

		ctx, cancel := context.WithTimeout(cmd.Context(), evalTimeout)
		defer cancel()

		specRules, err := coverage.LoadSpecRules(cfg.RulesDir)
		if err != nil {
			return fmt.Errorf("load spec rules: %w", err)
		}

		rep, err := evaluate.Run(ctx, cfg, exec, evaluate.RunOptions{
			Backend:      backend,
			ConfigPath:   configPath,
			MappingsFile: cfg.Mappings,
			SpecRules:    specRules,
		})
		if err != nil {
			return err
		}

		if err := report.WriteJSON(evalOutputJSON, rep); err != nil {
			return err
		}
		if err := report.WriteMarkdown(evalOutputMD, rep); err != nil {
			return err
		}

		printSummary(cmd.OutOrStdout(), rep, evalOutputJSON, evalOutputMD)
		return nil
	},
}

func init() {
	evaluateCmd.Flags().StringVar(&evalBackend, "backend", "", "backend to query: elastic | clickhouse | opensearch (overrides config.backend)")
	evaluateCmd.Flags().StringVar(&evalOutputJSON, "output-json", "vern-report.json", "path for the JSON report")
	evaluateCmd.Flags().StringVar(&evalOutputMD, "output-md", "vern-report.md", "path for the Markdown report")
	evaluateCmd.Flags().DurationVar(&evalTimeout, "timeout", 5*time.Minute, "overall evaluation timeout")
}

// defaultMappingsFor returns the conventional mappings path for a backend
// unless the user explicitly pointed cfg.Mappings at something other than
// the esql default — in which case we trust their choice.
func defaultMappingsFor(backend, configured string) string {
	if configured != "" && configured != "./configs/esql-mappings.yaml" {
		return "" // honour the explicit override
	}
	switch backend {
	case "clickhouse":
		return "./configs/clickhouse-mappings.yaml"
	case "opensearch":
		return "./configs/opensearch-mappings.yaml"
	}
	return ""
}

func pickBackend(flag, configValue string) string {
	v := flag
	if v == "" {
		v = configValue
	}
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "elastic" {
		return "esql"
	}
	return v
}

// buildExecutor returns the configured Executor for the chosen backend along
// with the default mappings file path for that backend (empty if the config
// override should win).
func buildExecutor(backend string, cfg *config.Config) (evaluate.Executor, string, error) {
	switch backend {
	case "esql", "":
		creds, err := elasticCredentials()
		if err != nil {
			return nil, "", err
		}
		url := firstNonEmpty(cfg.Elastic.ElasticsearchURL, creds.URL, os.Getenv("ELASTICSEARCH_URL"))
		apiKey := firstNonEmpty(cfg.Elastic.APIKey, creds.APIKey, os.Getenv("ELASTIC_API_KEY"), os.Getenv("KIBANA_API_KEY"))
		if url == "" {
			return nil, "", errors.New("elastic backend needs elasticsearch_url (config) or VERN_ELASTICSEARCH_URL env var")
		}
		ex := elasticexec.New(url, apiKey)
		if apiKey == "" {
			ex.Username = creds.Username
			ex.Password = creds.Password
		}
		return ex, "", nil
	case "clickhouse":
		url := firstNonEmpty(cfg.ClickHouse.URL, os.Getenv("VERN_CLICKHOUSE_URL"))
		user := firstNonEmpty(cfg.ClickHouse.Username, os.Getenv("VERN_CLICKHOUSE_USERNAME"))
		pass := firstNonEmpty(cfg.ClickHouse.Password, os.Getenv("VERN_CLICKHOUSE_PASSWORD"))
		if url == "" {
			return nil, "", errors.New("clickhouse backend needs clickhouse.url (config) or VERN_CLICKHOUSE_URL env var")
		}
		return clickhouseexec.New(url, user, pass, cfg.ClickHouse.Database), defaultMappingsFor("clickhouse", cfg.Mappings), nil
	case "opensearch":
		url := firstNonEmpty(cfg.OpenSearch.URL, os.Getenv("VERN_OPENSEARCH_URL"))
		user := firstNonEmpty(cfg.OpenSearch.Username, os.Getenv("VERN_OPENSEARCH_USERNAME"))
		pass := firstNonEmpty(cfg.OpenSearch.Password, os.Getenv("VERN_OPENSEARCH_PASSWORD"))
		if url == "" {
			return nil, "", errors.New("opensearch backend needs opensearch.url (config) or VERN_OPENSEARCH_URL env var")
		}
		return opensearchexec.New(url, user, pass), defaultMappingsFor("opensearch", cfg.Mappings), nil
	default:
		return nil, "", fmt.Errorf("unknown backend %q (supported: elastic, clickhouse, opensearch)", backend)
	}
}

func printSummary(w io.Writer, rep *evaluate.Report, jsonPath, mdPath string) {
	mf := rep.MappingsFile
	if mf == "" {
		mf = "embedded"
	}
	fmt.Fprintf(w, "Vern evaluation — backend=%s mappings=%s\n", rep.Backend, mf)
	scored := 0
	for _, r := range rep.Rules {
		if r.Scored {
			scored++
		}
	}
	fmt.Fprintf(w, "  rules:    %d scored, %d not scored\n", scored, len(rep.Rules)-scored)
	for _, reason := range rep.PartialReasons {
		fmt.Fprintf(w, "    - %s\n", truncForLog(reason, 120))
	}
	if rep.Score != nil {
		if !rep.Score.Scored {
			fmt.Fprintf(w, "  overall:  not scored — no rule could be evaluated\n")
		} else {
			fmt.Fprintf(w, "  overall:  %s (%s)\n", score.Format(rep.Score.Overall), rep.Score.Category)
		}
		fmt.Fprintf(w, "  services: %d\n", len(rep.Score.Services))
	}
	if rep.Sample != nil {
		if summary, total, atLeast := report.DroppedSummary(rep.Sample.Dropped); total > 0 {
			lead := ""
			if atLeast {
				lead = "at least "
			}
			fmt.Fprintf(w, "  dropped:  %s%d items not indexed (%s), not reflected in the score\n", lead, total, summary)
		}
	}
	if len(rep.Failures) > 0 {
		fmt.Fprintf(w, "  failures: %d\n", len(rep.Failures))
		for _, f := range rep.Failures {
			fmt.Fprintf(w, "    - %s: %s\n", f.RuleID, truncForLog(f.Error, 120))
		}
	}
	if jsonPath != "" {
		fmt.Fprintf(w, "  json:     %s\n", jsonPath)
	}
	if mdPath != "" {
		fmt.Fprintf(w, "  md:       %s\n", mdPath)
	}
}

func truncForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// esCredentials are the connection settings read from VERN_* env vars only.
type esCredentials struct {
	URL, APIKey, Username, Password string
}

// elasticCredentials reads VERN_ELASTICSEARCH_URL, VERN_API_KEY (falling back
// to the trimmed contents of VERN_API_KEY_FILE), and the username/password
// pair. It deliberately ignores ELASTIC_* and KIBANA_* variables so callers
// that must not pick up a developer's cluster key (vern score) stay safe;
// evaluate layers its legacy fallbacks on top.
func elasticCredentials() (esCredentials, error) {
	c := esCredentials{
		URL:      os.Getenv("VERN_ELASTICSEARCH_URL"),
		APIKey:   os.Getenv("VERN_API_KEY"),
		Username: os.Getenv("VERN_ELASTICSEARCH_USERNAME"),
		Password: os.Getenv("VERN_ELASTICSEARCH_PASSWORD"),
	}
	if c.APIKey == "" {
		if f := os.Getenv("VERN_API_KEY_FILE"); f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				return c, fmt.Errorf("read VERN_API_KEY_FILE: %w", err)
			}
			c.APIKey = strings.TrimSpace(string(b))
		}
	}
	return c, nil
}
