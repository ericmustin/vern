package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/engine/elastic"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/otlpin"
	"github.com/ericmustin/vern/internal/otlpout"
	"github.com/ericmustin/vern/internal/report"
	"github.com/ericmustin/vern/internal/samplescore"
	"github.com/ericmustin/vern/internal/score"
	"github.com/spf13/cobra"
)

var (
	scoreOutputJSON string
	scoreOutputMD   string
	scoreKeep       bool
	scoreTimeout    time.Duration
	scoreMappings   string
	scoreRulesDir   string
	scoreSetupTmpl  bool
	scoreDebug      bool

	scoreMinScore     optionalFloat
	scoreOutputsFile  string
	scoreReadyTimeout time.Duration
	scoreOTLPEndpoint string
	scoreOTLPHeaders  []string
)

const (
	// scoreSweepAge is how old a leftover run (from an interrupted or crashed
	// invocation) must be before vern score deletes it at startup.
	scoreSweepAge = time.Hour
	// scoreMaxTimeout keeps every live run well inside scoreSweepAge, so a
	// concurrent invocation's startup sweep never deletes a run in progress.
	scoreMaxTimeout = 50 * time.Minute
	// otlpExportTimeout bounds the OTLP export, independent of --timeout.
	otlpExportTimeout = 15 * time.Second
)

// optionalFloat is a float flag that remembers whether it was set.
type optionalFloat struct {
	set bool
	v   float64
}

func (o *optionalFloat) String() string {
	if !o.set {
		return ""
	}
	return strconv.FormatFloat(o.v, 'f', -1, 64)
}

func (o *optionalFloat) Set(s string) error {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}
	o.set, o.v = true, v
	return nil
}

func (o *optionalFloat) Type() string { return "float" }

var scoreCmd = &cobra.Command{
	Use:   "score <file>...",
	Short: "Score OTLP JSON captures by loading them into a self-managed Elasticsearch 9.5+ cluster",
	Long: `Decodes OTLP JSON capture files (use - for stdin), loads them into a
self-managed Elasticsearch 9.5+ cluster through its native OTLP endpoint,
runs the Instrumentation Score rules against that run's data only, writes
JSON and Markdown reports, and deletes the data.

Exit codes: 0 ok; 1 error (decode, ingest, query, a rule that failed with an
error, or no item indexed at all); 2 score below --min-score, or not scored
while --min-score is set. When both 1 and 2 apply, vern exits 1. Reports and
the --outputs-file are written before the exit code is decided.

Connection settings come from VERN_ELASTICSEARCH_URL, VERN_API_KEY or
VERN_API_KEY_FILE, VERN_ELASTICSEARCH_USERNAME and VERN_ELASTICSEARCH_PASSWORD.
From the config file (--config), only these settings affect scoring:
exclusions (services, service_patterns), filters (environments,
service_namespaces, enable_sdk_rules), esql.unmapped_fields and esql.cardinality_threshold. The backend, index
patterns and time window are set per run.

Leftover runs older than 1h (from interrupted invocations) are deleted at
startup. Pass --setup-template on single-node or ephemeral clusters to let
vern create the metrics-otel@custom component template.`,
	Args: cobra.MinimumNArgs(1),
	RunE: runScore,
}

func init() {
	f := scoreCmd.Flags()
	f.StringVar(&scoreOutputJSON, "output-json", "vern-report.json", "path for the JSON report (empty: do not write)")
	f.StringVar(&scoreOutputMD, "output-md", "vern-report.md", "path for the Markdown report (empty: do not write)")
	f.BoolVar(&scoreKeep, "keep", false, "do not delete the loaded data afterwards (debugging)")
	f.DurationVar(&scoreTimeout, "timeout", 5*time.Minute, "overall scoring timeout (at most 50m)")
	f.DurationVar(&scoreReadyTimeout, "ready-timeout", 90*time.Second, "how long to wait for Elasticsearch to answer before giving up")
	f.Var(&scoreMinScore, "min-score", "exit 2 when the overall score is below this value (0-100) or the run is not scored; unset means no gate")
	f.StringVar(&scoreOutputsFile, "outputs-file", "", "append key=value results (score, category, scored, rules_scored, rules_total, report_json, report_md) to this file, e.g. $GITHUB_OUTPUT")
	f.StringVar(&scoreOTLPEndpoint, "otlp-endpoint", "", "export the score as OTLP/HTTP protobuf metrics to this base endpoint (/v1/metrics is appended); overrides OTEL_EXPORTER_OTLP_[METRICS_]ENDPOINT")
	f.StringArrayVar(&scoreOTLPHeaders, "otlp-header", nil, "OTLP export header as key=value (repeatable; overrides OTEL_EXPORTER_OTLP_HEADERS; values are URL-decoded)")
	f.StringVar(&scoreMappings, "mappings", "", "ES|QL mappings file (default: embedded)")
	f.StringVar(&scoreRulesDir, "rules-dir", "", "spec rules directory (default: embedded)")
	f.BoolVar(&scoreDebug, "debug", false, "log the full error of a failed sensitive-data rule query; it can quote field and metric names from the sample, so keep it off where stderr is shared, such as a public CI log (env VERN_DEBUG)")
	f.BoolVar(&scoreSetupTmpl, "setup-template", false, "create the metrics-otel@custom component template (7d look-back, 0 replicas) if absent; this changes a cluster-wide template, so use it only on single-node or ephemeral clusters")
}

func runScore(cmd *cobra.Command, args []string) error {
	if err := validateScoreFlags(); err != nil {
		return err
	}
	creds, err := elasticCredentials()
	if err != nil {
		return err
	}
	if creds.URL == "" {
		return errors.New("vern score needs VERN_ELASTICSEARCH_URL (an Elasticsearch 9.5+ cluster; see deploy/compose.yaml)")
	}
	if err := checkESURL(creds.URL); err != nil {
		return err
	}

	cfg := config.Default()
	if _, statErr := os.Stat(configPath); statErr == nil {
		if cfg, err = config.Load(configPath); err != nil {
			return err
		}
	}

	debug, err := debugEnabled(scoreDebug, os.Getenv)
	if err != nil {
		return err
	}
	logf := func(format string, a ...any) { fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", a...) }
	// stderr can be a public CI log: a failed sensitive-rule query's full
	// error (it can quote the sample's field and metric names) is logged
	// only when debugging is switched on.
	opts := samplescore.Options{Config: cfg, Files: args, Keep: scoreKeep, Sweep: scoreSweepAge, Logf: logf, MappingsFile: scoreMappings, QueryErrorDetail: debug}
	if scoreMappings != "" {
		if opts.Mappings, err = mappings.Load(scoreMappings); err != nil {
			return fmt.Errorf("load mappings: %w", err)
		}
	}
	if scoreRulesDir != "" {
		if opts.SpecRules, err = coverage.LoadSpecRules(scoreRulesDir); err != nil {
			return fmt.Errorf("load spec rules: %w", err)
		}
	}

	var inputs []otlpin.Input
	for _, a := range args {
		if a == "-" {
			inputs = append(inputs, otlpin.Input{Name: "stdin", R: cmd.InOrStdin()})
			continue
		}
		f, err := os.Open(a)
		if err != nil {
			return fmt.Errorf("open %s: %w", a, err)
		}
		defer f.Close()
		inputs = append(inputs, otlpin.Input{Name: a, R: io.Reader(f)})
	}
	sample, err := otlpin.Decode(inputs, otlpin.DefaultLimits)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	eng := elastic.New(elastic.Config{
		URL: creds.URL, APIKey: creds.APIKey, Username: creds.Username, Password: creds.Password,
		Logf:           logf,
		EnsureTemplate: scoreSetupTmpl,
		ReadyTimeout:   scoreReadyTimeout,
	})

	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	// Ctrl-C / SIGTERM cancel the run; samplescore still purges with a fresh
	// context. Once cancelled, signal handling is released so a second Ctrl-C
	// terminates immediately.
	sigCtx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { <-sigCtx.Done(); stop() }()
	ctx, cancel := context.WithTimeout(sigCtx, scoreTimeout)
	defer cancel()
	rep, err := samplescore.Score(ctx, eng, sample, opts)
	if err != nil {
		return err
	}

	// Order matters for CI: reports, then the outputs file, then export, and
	// only then the exit-code decision, so a failing gate still leaves every
	// artefact behind.
	if scoreOutputJSON != "" {
		if err := report.WriteJSON(scoreOutputJSON, rep); err != nil {
			return err
		}
	}
	if scoreOutputMD != "" {
		if err := report.WriteMarkdown(scoreOutputMD, rep); err != nil {
			return err
		}
	}
	printSummary(cmd.OutOrStdout(), rep, scoreOutputJSON, scoreOutputMD)
	if scoreOutputsFile != "" {
		if err := writeOutputsFile(scoreOutputsFile, rep, scoreOutputJSON, scoreOutputMD); err != nil {
			return err
		}
	}
	// Export failure is an exit-1 condition passed to scoreOutcome alongside
	// the others, so reports and outputs are already on disk.
	var exportErr error
	oc := otlpout.ConfigFromEnv(os.Getenv).WithEndpoint(scoreOTLPEndpoint).WithHeaders(scoreOTLPHeaders)
	oc.ServiceVersion = version
	for _, w := range oc.Warnings {
		logf("warning: %s", w)
	}
	if !oc.Enabled() && len(oc.Headers) > 0 {
		logf("warning: otlp headers set but no endpoint; export disabled")
	}
	if oc.Enabled() {
		// Export gets its own deadline so a long scoring run cannot starve it;
		// it still derives from sigCtx so Ctrl-C cancels it.
		exportCtx, exportCancel := context.WithTimeout(sigCtx, otlpExportTimeout)
		defer exportCancel()
		// A failed export is returned through scoreOutcome and printed once
		// by Execute; only success is logged here.
		if exportErr = otlpout.Export(exportCtx, rep, oc); exportErr == nil {
			logf("exported score metrics via OTLP")
		}
	}
	return scoreOutcome(rep, scoreMinScore, exportErr)
}

// debugEnabled reports whether --debug or VERN_DEBUG (a boolean; empty
// means off) switches debugging on.
func debugEnabled(flag bool, getenv func(string) string) (bool, error) {
	if flag {
		return true, nil
	}
	v := strings.TrimSpace(getenv("VERN_DEBUG"))
	if v == "" {
		return false, nil
	}
	on, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("VERN_DEBUG: invalid boolean %q", v)
	}
	return on, nil
}

// validateScoreFlags rejects flag values before any network call.
func validateScoreFlags() error {
	var errs []error
	if scoreTimeout <= 0 {
		errs = append(errs, fmt.Errorf("--timeout must be positive, got %s", scoreTimeout))
	}
	if scoreTimeout > scoreMaxTimeout {
		errs = append(errs, fmt.Errorf("--timeout %s exceeds the 50m maximum: runs older than %s are swept at startup, so a run must finish well within that", scoreTimeout, scoreSweepAge))
	}
	if scoreReadyTimeout <= 0 {
		errs = append(errs, fmt.Errorf("--ready-timeout must be positive, got %s", scoreReadyTimeout))
	}
	if scoreMinScore.set && (scoreMinScore.v < 0 || scoreMinScore.v > 100 || math.IsNaN(scoreMinScore.v)) {
		errs = append(errs, fmt.Errorf("--min-score must be between 0 and 100, got %s", scoreMinScore.String()))
	}
	return errors.Join(errs...)
}

// checkESURL requires an absolute http(s) URL with a host. Its errors never
// echo the value, which may carry credentials in its userinfo.
func checkESURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("VERN_ELASTICSEARCH_URL must be an absolute http(s) URL with a host")
	}
	return nil
}

// scoreOutcome decides the exit status once every artefact is written.
// Exit 1 (errors, including extra ones such as a failed export) wins over
// exit 2 (the --min-score gate).
func scoreOutcome(rep *evaluate.Report, min optionalFloat, extra ...error) error {
	var errs []error
	for _, e := range extra {
		if e != nil {
			errs = append(errs, e)
		}
	}
	for _, p := range rep.Problems {
		errs = append(errs, errors.New(p))
	}
	if len(errs) > 0 {
		return &exitError{code: 1, err: errors.Join(errs...)}
	}
	if !min.set {
		return nil
	}
	if rep.Score == nil || !rep.Score.Scored {
		return &exitError{code: 2, err: fmt.Errorf("not scored: no rule could be evaluated (--min-score %s)", min.String())}
	}
	// Gate on the one-decimal score users see in the summary, reports and outputs.
	shown := score.Format(rep.Score.Overall)
	if got, _ := strconv.ParseFloat(shown, 64); got < min.v {
		return &exitError{code: 2, err: fmt.Errorf("score %s is below --min-score %s", shown, min.String())}
	}
	return nil
}

// writeOutputsFile appends GitHub $GITHUB_OUTPUT style key=value lines. It
// creates the file when missing and never truncates it.
func writeOutputsFile(path string, rep *evaluate.Report, jsonPath, mdPath string) error {
	scoreVal, category, scored := "", "", false
	if rep.Score != nil && rep.Score.Scored {
		scored = true
		scoreVal = score.Format(rep.Score.Overall)
		category = rep.Score.Category
	}
	n := 0
	for _, r := range rep.Rules {
		if r.Scored {
			n++
		}
	}
	kv := [][2]string{
		{"score", scoreVal},
		{"category", category},
		{"scored", strconv.FormatBool(scored)},
		{"rules_scored", strconv.Itoa(n)},
		{"rules_total", strconv.Itoa(len(rep.Rules))},
		{"report_json", jsonPath},
		{"report_md", mdPath},
	}
	var b strings.Builder
	for _, p := range kv {
		if strings.ContainsAny(p[1], "\r\n") {
			return fmt.Errorf("outputs file: value for %s contains a newline", p[0])
		}
		fmt.Fprintf(&b, "%s=%s\n", p[0], p[1])
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("outputs file: %w", err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		return fmt.Errorf("outputs file %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("outputs file %s: %w", path, err)
	}
	return nil
}
