package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ericmustin/vern/internal/agent"
	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/dashboard"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/sdksupport"
	"github.com/ericmustin/vern/internal/semconv"
	"github.com/ericmustin/vern/internal/workflow/elastic"
)

const (
	defaultResultIndex      = "instrumentation-score-results"
	defaultAnnotationsIndex = "observability-annotations"
)

type Options struct {
	ConfigPath     string
	MappingsPath   string
	Format         string
	StrictCoverage bool
	LiveESURL      string
}

type Issue struct {
	Severity string `json:"severity"`
	Check    string `json:"check"`
	Message  string `json:"message"`
}

type ArtifactCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type LiveCheck struct {
	RuleID string `json:"rule_id"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

type Report struct {
	ConfigPath   string                 `json:"config_path"`
	MappingsPath string                 `json:"mappings_path"`
	LiveESURL    string                 `json:"live_es_url,omitempty"`
	Coverage     coverage.Summary       `json:"coverage"`
	Issues       []Issue                `json:"issues"`
	Artifacts    []ArtifactCheck        `json:"artifacts"`
	LiveChecks   []LiveCheck            `json:"live_checks,omitempty"`
	Counts       map[string]int         `json:"counts"`
	Config       *config.Config         `json:"-"`
	Mappings     *mappings.MappingsFile `json:"-"`
}

func Run(ctx context.Context, opts Options) (*Report, error) {
	if opts.ConfigPath == "" {
		opts.ConfigPath = "vern.yaml"
	}
	report := &Report{
		ConfigPath: opts.ConfigPath,
		LiveESURL:  opts.LiveESURL,
		Counts:     map[string]int{},
	}

	if _, err := os.Stat(opts.ConfigPath); err != nil {
		report.addError("config", "config path is not readable: "+err.Error())
		return finalize(report, opts), nil
	}

	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		report.addError("config", err.Error())
		return finalize(report, opts), nil
	}
	report.Config = cfg

	mappingsPath := opts.MappingsPath
	if mappingsPath == "" {
		mappingsPath = cfg.Mappings
	}
	report.MappingsPath = mappingsPath

	if _, err := os.Stat(cfg.RulesDir); err != nil {
		report.addError("rules_dir", "rules_dir is not readable: "+err.Error())
	}
	if _, err := os.Stat(mappingsPath); err != nil {
		report.addError("mappings", "mappings path is not readable: "+err.Error())
		return finalize(report, opts), nil
	}

	mf, err := mappings.Load(mappingsPath)
	if err != nil {
		report.addError("mappings", err.Error())
		return finalize(report, opts), nil
	}
	report.Mappings = mf

	specRules, err := coverage.LoadSpecRules(cfg.RulesDir)
	if err != nil {
		report.addError("spec", err.Error())
		return finalize(report, opts), nil
	}
	report.Coverage = coverage.Build(specRules, mf)
	report.compareMappingsToSpec(specRules, mf.Rules)

	resolved, err := mappings.ResolveWithData(mf.Rules, cfg, mappings.TemplateData{
		SemconvAttributeKeys:    semconv.QuotedCSV(semconv.AttributeKeys),
		SemconvResourceOnlyKeys: semconv.QuotedCSV(semconv.ResourceOnlyKeys),
		SemconvSpanOnlyKeys:     semconv.QuotedCSV(semconv.SpanOnlyKeys),
		SemconvLogOnlyKeys:      semconv.QuotedCSV(semconv.LogOnlyKeys),
		SemconvMetricOnlyKeys:   semconv.QuotedCSV(semconv.MetricOnlyKeys),
		MET001AttrKeys:          semconv.MET001AttrAllowlist(),
		MET006SemconvKeys:       semconv.QuotedCSV(semconv.MET006CuratedKeys()),
		SDKSupportMatrix:        sdksupport.RenderESQLCase("resource.attributes.telemetry.sdk.language", "resource.attributes.telemetry.sdk.version"),
	})
	if err != nil {
		report.addError("resolve", err.Error())
		return finalize(report, opts), nil
	}

	report.checkArtifacts(resolved, cfg, &report.Coverage)
	if opts.LiveESURL != "" {
		report.LiveChecks = validateLiveESQL(ctx, opts.LiveESURL, resolved.Rules)
		for _, check := range report.LiveChecks {
			if !check.Passed {
				report.addError("live-esql", fmt.Sprintf("%s: %s", check.RuleID, check.Detail))
			}
		}
	}

	return finalize(report, opts), nil
}

func finalize(report *Report, opts Options) *Report {
	if report.Coverage.PartialScore {
		report.addWarning("coverage", "score is partial; missing, disabled, or heuristic rules exist")
		if opts.StrictCoverage {
			report.addError("coverage", "strict coverage is enabled and the score is partial")
		}
	}
	report.Counts["errors"] = report.countSeverity("error")
	report.Counts["warnings"] = report.countSeverity("warning")
	report.Counts["enabled_rules"] = len(report.Coverage.EnabledRules)
	report.Counts["disabled_rules"] = len(report.Coverage.DisabledRules)
	report.Counts["missing_rules"] = len(report.Coverage.MissingRules)
	report.Counts["heuristic_rules"] = len(report.Coverage.HeuristicRules)
	return report
}

func (r *Report) compareMappingsToSpec(specRules []coverage.SpecRule, rules []mappings.RuleMapping) {
	byID := map[string]coverage.SpecRule{}
	for _, rule := range specRules {
		byID[rule.ID] = rule
	}
	for _, rule := range rules {
		if strings.HasPrefix(rule.SpecRuleID, "_") {
			continue
		}
		spec, ok := byID[rule.SpecRuleID]
		if !ok {
			r.addError("coverage", fmt.Sprintf("%s exists in mappings but not vendored spec rules", rule.SpecRuleID))
			continue
		}
		if rule.Impact != "" && !strings.EqualFold(rule.Impact, spec.Impact) {
			r.addError("coverage", fmt.Sprintf("%s impact mismatch: mapping=%s spec=%s", rule.SpecRuleID, rule.Impact, spec.Impact))
		}
		if rule.Target != "" && coverage.NormalizeTarget(rule.Target) != coverage.NormalizeTarget(spec.Target) {
			r.addError("coverage", fmt.Sprintf("%s target mismatch: mapping=%s spec=%s", rule.SpecRuleID, rule.Target, spec.Target))
		}
	}
}

func (r *Report) checkArtifacts(resolved *mappings.ResolveResult, cfg *config.Config, cov *coverage.Summary) {
	workflow, err := elastic.Generate(resolved, cfg, cov)
	if err != nil {
		r.addError("workflow", "generate workflow: "+err.Error())
		return
	}
	dashboards, err := dashboard.Generate(cfg, cov)
	if err != nil {
		r.addError("dashboards", "generate dashboards: "+err.Error())
		return
	}
	skill := agent.BuildSkill(agent.Context{Config: cfg, Coverage: cov})

	r.addArtifact("workflow", artifactContains(string(workflow), cfg.ESQL.ResultIndex), "workflow references configured result index")
	if resolved.Aggregation != nil {
		r.addArtifact("workflow", artifactContains(string(workflow), cfg.ESQL.AnnotationsIndex), "workflow references configured annotations index")
	}
	r.addArtifact("dashboards", artifactContains(string(dashboards), cfg.ESQL.ResultIndex), "dashboards reference configured result index")
	r.addArtifact("agent-skill", artifactContains(skill.Content, cfg.ESQL.ResultIndex), "agent skill references configured result index")
	r.addArtifact("agent-skill", artifactContains(skill.Content, cfg.ESQL.IndexPatterns.Traces), "agent skill references configured trace index pattern")
	r.addArtifact("agent-skill", artifactContains(skill.Content, cfg.ESQL.IndexPatterns.Logs), "agent skill references configured log index pattern")
	r.addArtifact("agent-skill", artifactContains(skill.Content, cfg.ESQL.IndexPatterns.Metrics), "agent skill references configured metric index pattern")

	if cfg.ESQL.ResultIndex != defaultResultIndex {
		r.addArtifact("workflow", !strings.Contains(string(workflow), defaultResultIndex), "workflow has no default result-index hardcoding")
		r.addArtifact("dashboards", !strings.Contains(string(dashboards), defaultResultIndex), "dashboards have no default result-index hardcoding")
		r.addArtifact("agent-skill", !strings.Contains(skill.Content, defaultResultIndex), "agent skill has no default result-index hardcoding")
	}
	if resolved.Aggregation != nil && cfg.ESQL.AnnotationsIndex != defaultAnnotationsIndex {
		r.addArtifact("workflow", !strings.Contains(string(workflow), defaultAnnotationsIndex), "workflow has no default annotations-index hardcoding")
	}
}

func artifactContains(body, value string) bool {
	return value == "" || strings.Contains(body, value)
}

func (r *Report) addArtifact(name string, passed bool, detail string) {
	r.Artifacts = append(r.Artifacts, ArtifactCheck{Name: name, Passed: passed, Detail: detail})
	if !passed {
		r.addError(name, detail)
	}
}

func validateLiveESQL(ctx context.Context, base string, rules []mappings.ResolvedMapping) []LiveCheck {
	base = strings.TrimRight(base, "/")
	apiKey := os.Getenv("ELASTIC_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("KIBANA_API_KEY")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	var checks []LiveCheck
	for _, rule := range rules {
		if rule.IsAggregation {
			continue
		}
		check := LiveCheck{RuleID: rule.SpecRuleID}
		payload, _ := json.Marshal(map[string]string{"query": rule.ResolvedQuery})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/_query?format=json", bytes.NewReader(payload))
		if err != nil {
			check.Detail = err.Error()
			checks = append(checks, check)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "ApiKey "+apiKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			check.Detail = err.Error()
			checks = append(checks, check)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			check.Detail = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 500))
			checks = append(checks, check)
			continue
		}
		check.Passed = true
		check.Detail = "query planned and executed"
		checks = append(checks, check)
	}
	return checks
}

func Text(report *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Vern reproducibility review\n")
	fmt.Fprintf(&b, "  config:   %s\n", report.ConfigPath)
	fmt.Fprintf(&b, "  mappings: %s\n", report.MappingsPath)
	if report.LiveESURL != "" {
		fmt.Fprintf(&b, "  live ES:  %s\n", sanitizeURL(report.LiveESURL))
	}
	fmt.Fprintf(&b, "\nCoverage\n")
	fmt.Fprintf(&b, "  partial score: %t\n", report.Coverage.PartialScore)
	fmt.Fprintf(&b, "  enabled:   %s\n", coverage.Join(report.Coverage.EnabledRules))
	fmt.Fprintf(&b, "  heuristic: %s\n", coverage.Join(report.Coverage.HeuristicRules))
	fmt.Fprintf(&b, "  disabled:  %s\n", coverage.Join(report.Coverage.DisabledRules))
	fmt.Fprintf(&b, "  missing:   %s\n", coverage.Join(report.Coverage.MissingRules))

	if details := coverageDetails(report.Coverage.Rules); len(details) > 0 {
		fmt.Fprintf(&b, "\nCoverage details\n")
		for _, detail := range details {
			fmt.Fprintf(&b, "  %s\n", detail)
		}
	}

	if len(report.Artifacts) > 0 {
		fmt.Fprintf(&b, "\nGenerated artifact checks\n")
		for _, check := range report.Artifacts {
			status := "ok"
			if !check.Passed {
				status = "fail"
			}
			fmt.Fprintf(&b, "  [%s] %s: %s\n", status, check.Name, check.Detail)
		}
	}

	if len(report.LiveChecks) > 0 {
		fmt.Fprintf(&b, "\nLive ES|QL checks\n")
		for _, check := range report.LiveChecks {
			status := "ok"
			if !check.Passed {
				status = "fail"
			}
			fmt.Fprintf(&b, "  [%s] %s: %s\n", status, check.RuleID, check.Detail)
		}
	}

	if len(report.Issues) > 0 {
		fmt.Fprintf(&b, "\nIssues\n")
		issues := append([]Issue(nil), report.Issues...)
		sort.SliceStable(issues, func(i, j int) bool {
			if issues[i].Severity == issues[j].Severity {
				return issues[i].Check < issues[j].Check
			}
			return issues[i].Severity < issues[j].Severity
		})
		for _, issue := range issues {
			fmt.Fprintf(&b, "  [%s] %s: %s\n", issue.Severity, issue.Check, issue.Message)
		}
	}

	fmt.Fprintf(&b, "\nSummary: %d error(s), %d warning(s)\n", report.Counts["errors"], report.Counts["warnings"])
	return b.String()
}

func JSON(report *Report) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}

func coverageDetails(rules []coverage.RuleCoverage) []string {
	var details []string
	for _, rule := range rules {
		if rule.Status == "enabled" {
			continue
		}
		detail := fmt.Sprintf("[%s] %s (%s, %s)", rule.Status, rule.ID, rule.Impact, rule.Target)
		if rule.Reason != "" {
			detail += ": " + rule.Reason
		}
		details = append(details, detail)
	}
	return details
}

func (r *Report) addError(check, msg string) {
	r.Issues = append(r.Issues, Issue{Severity: "error", Check: check, Message: msg})
}

func (r *Report) addWarning(check, msg string) {
	for _, issue := range r.Issues {
		if issue.Severity == "warning" && issue.Check == check && issue.Message == msg {
			return
		}
	}
	r.Issues = append(r.Issues, Issue{Severity: "warning", Check: check, Message: msg})
}

func (r *Report) countSeverity(severity string) int {
	n := 0
	for _, issue := range r.Issues {
		if issue.Severity == severity {
			n++
		}
	}
	return n
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func sanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
}
