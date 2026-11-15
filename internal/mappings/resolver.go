package mappings

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/semconv"
	"github.com/ericmustin/vern/internal/sensitive"
)

type TemplateData struct {
	Indices              config.IndexPatterns
	TimeWindow           string
	ScoreLookback        string
	ResultIndex          string
	AnnotationsIndex     string
	CardinalityThreshold int

	// ScopeFilter is an ES|QL WHERE-clause fragment (without leading "AND ")
	// that narrows rule evaluation to user-configured environments/namespaces.
	// Empty when no filters are set.
	ScopeFilter string

	// SemconvAttributeKeys is a comma-separated, double-quoted list of all
	// semantic-convention attribute keys, suitable for IN (...) expressions.
	// e.g. `"http.request.method", "service.name", ...`
	SemconvAttributeKeys string

	// SemconvResourceOnlyKeys, SemconvSpanOnlyKeys, SemconvLogOnlyKeys,
	// SemconvMetricOnlyKeys are subsets of SemconvAttributeKeys with the same
	// quoted-CSV format, used by RES-004 to flag attribute placement violations.
	SemconvResourceOnlyKeys string
	SemconvSpanOnlyKeys     string
	SemconvLogOnlyKeys      string
	SemconvMetricOnlyKeys   string

	// MET001AttrKeys is the allowlist of high-cardinality-risk attribute keys
	// to evaluate against the per-metric cardinality threshold.
	MET001AttrKeys []string

	// MisplacedResourceKeys is the list of resource-only semconv attribute
	// keys RES-004 looks for at the span/log attribute level.
	MisplacedResourceKeys []string

	// MET006SemconvKeys is the curated double-quoted-CSV subset of semconv
	// attribute keys used by MET-006's IN-list check (ES|QL form). The full
	// registry is too large for ES|QL's IN operator parser.
	MET006SemconvKeys string

	// MET006SemconvKeysSingleQuoted is the same curated subset rendered with
	// single quotes, for ClickHouse SQL and OpenSearch PPL (both read double
	// quotes as identifiers).
	MET006SemconvKeysSingleQuoted string

	// SDKSupportMatrix is rendered ES|QL CASE-ladder text consumed by SDK-001.
	SDKSupportMatrix string

	// SampleMode is carried through ResolveWithData overrides; templates use
	// {{ if .SampleMode }} to switch to sampled evaluation.
	SampleMode bool

	// AllSignals is the comma-joined traces,logs,metrics index patterns (ES|QL
	// only; empty for other backends).
	AllSignals string

	// AdvisoryRuleIDs is the sorted, double-quoted CSV of enabled advisory
	// rule IDs (e.g. `"SPA-003"`), empty when there are none. resolveWith
	// computes it from the rules; _SCORE_AGGREGATION excludes these rows so
	// the workflow score matches vern evaluate.
	AdvisoryRuleIDs string

	// Fields is the run's discovered, scannable fields (sensitive-data
	// rules). Zero outside score runs.
	Fields Fields

	// ScanFields and ScanMetricNames are set per rule from
	// Fields.SensitiveScan(rule.ScanGroups): the capped field list a
	// sensitive-data rule's query scans and, for MET-008, the metric names.
	// Empty for other rules.
	ScanFields      []string
	ScanMetricNames []string

	// unmappedNullify is set by buildTemplateData for the esql backend when
	// esql.unmapped_fields != "off". Unexported so overrides cannot clobber it.
	unmappedNullify bool
}

const unmappedFieldsPrefix = "SET unmapped_fields=\"nullify\";\n"

type ResolveResult struct {
	Rules       []ResolvedMapping
	Aggregation *ResolvedMapping
	Skipped     []SkippedRule
	// Errors holds rules that failed to resolve (template or empty-query
	// errors); Reason is the error text.
	Errors []SkippedRule
}

type SkippedRule struct {
	SpecRuleID string
	Reason     string
	Mapping    RuleMapping
}

// Err returns nil when no rule failed to resolve, else the aggregate error.
func (r *ResolveResult) Err() error {
	if len(r.Errors) == 0 {
		return nil
	}
	msgs := make([]string, len(r.Errors))
	for i, e := range r.Errors {
		msgs[i] = e.Reason
	}
	return errors.New("template resolution failed:\n  - " + strings.Join(msgs, "\n  - "))
}

// Resolve always returns a non-nil result alongside result.Err().
func Resolve(rules []RuleMapping, cfg *config.Config) (*ResolveResult, error) {
	return resolveWith(rules, buildTemplateData(cfg))
}

// ResolveWithData lets the caller inject catalog data (semconv keys, SDK
// matrix) that the resolver itself does not own. Callers that don't need
// catalogs should use Resolve.
func ResolveWithData(rules []RuleMapping, cfg *config.Config, override TemplateData) (*ResolveResult, error) {
	data := buildTemplateData(cfg)
	if override.SemconvAttributeKeys != "" {
		data.SemconvAttributeKeys = override.SemconvAttributeKeys
	}
	if override.SemconvResourceOnlyKeys != "" {
		data.SemconvResourceOnlyKeys = override.SemconvResourceOnlyKeys
	}
	if override.SemconvSpanOnlyKeys != "" {
		data.SemconvSpanOnlyKeys = override.SemconvSpanOnlyKeys
	}
	if override.SemconvLogOnlyKeys != "" {
		data.SemconvLogOnlyKeys = override.SemconvLogOnlyKeys
	}
	if override.SemconvMetricOnlyKeys != "" {
		data.SemconvMetricOnlyKeys = override.SemconvMetricOnlyKeys
	}
	if len(override.MET001AttrKeys) > 0 {
		data.MET001AttrKeys = override.MET001AttrKeys
	}
	if len(override.MisplacedResourceKeys) > 0 {
		data.MisplacedResourceKeys = override.MisplacedResourceKeys
	}
	if override.MET006SemconvKeys != "" {
		data.MET006SemconvKeys = override.MET006SemconvKeys
	}
	if override.MET006SemconvKeysSingleQuoted != "" {
		data.MET006SemconvKeysSingleQuoted = override.MET006SemconvKeysSingleQuoted
	}
	if override.SDKSupportMatrix != "" {
		data.SDKSupportMatrix = override.SDKSupportMatrix
	}
	if override.SampleMode {
		data.SampleMode = true
	}
	data.Fields = override.Fields
	return resolveWith(rules, data)
}

func buildTemplateData(cfg *config.Config) TemplateData {
	switch cfg.Backend {
	case "clickhouse":
		return TemplateData{
			Indices: config.IndexPatterns{
				Traces:  cfg.ClickHouse.Tables.Traces,
				Metrics: cfg.ClickHouse.Tables.Metrics,
				Logs:    cfg.ClickHouse.Tables.Logs,
			},
			TimeWindow:           cfg.ClickHouse.TimeWindow,
			CardinalityThreshold: cfg.ClickHouse.CardinalityThreshold,
		}
	case "opensearch":
		return TemplateData{
			Indices:              cfg.OpenSearch.IndexPatterns,
			TimeWindow:           cfg.OpenSearch.TimeWindow,
			CardinalityThreshold: cfg.OpenSearch.CardinalityThreshold,
		}
	default: // "esql", "elastic", or empty — original behaviour
		return TemplateData{
			AllSignals:           cfg.ESQL.IndexPatterns.Traces + "," + cfg.ESQL.IndexPatterns.Logs + "," + cfg.ESQL.IndexPatterns.Metrics,
			unmappedNullify:      cfg.ESQL.UnmappedFields != "off",
			Indices:              cfg.ESQL.IndexPatterns,
			TimeWindow:           cfg.ESQL.TimeWindow,
			ScoreLookback:        cfg.ESQL.ScoreLookback,
			ResultIndex:          cfg.ESQL.ResultIndex,
			AnnotationsIndex:     cfg.ESQL.AnnotationsIndex,
			CardinalityThreshold: cfg.ESQL.CardinalityThreshold,
			ScopeFilter:          buildScopeFilter(cfg.Filters),
		}
	}
}

func resolveWith(rules []RuleMapping, data TemplateData) (*ResolveResult, error) {
	var result ResolveResult

	fail := func(r RuleMapping, reason string) {
		result.Errors = append(result.Errors, SkippedRule{SpecRuleID: r.SpecRuleID, Reason: reason, Mapping: r})
	}

	var advisory []string
	for _, r := range rules {
		if r.Enabled && r.Advisory && !r.IsAggregation {
			advisory = append(advisory, r.SpecRuleID)
		}
	}
	data.AdvisoryRuleIDs = semconv.QuotedCSV(advisory)

	for _, r := range rules {
		if !r.Enabled {
			result.Skipped = append(result.Skipped, SkippedRule{SpecRuleID: r.SpecRuleID, Reason: "disabled", Mapping: r})
			continue
		}
		if strings.TrimSpace(r.Query) == "" {
			fail(r, fmt.Sprintf("%s: empty query", r.SpecRuleID))
			continue
		}

		rd := data
		if len(r.ScanGroups) > 0 {
			scan, err := data.Fields.SensitiveScan(r.ScanGroups)
			if err != nil {
				fail(r, fmt.Sprintf("%s: scan_groups: %v", r.SpecRuleID, err))
				continue
			}
			rd.ScanFields, rd.ScanMetricNames = scan.Fields, scan.MetricNames
		}
		resolved, err := renderQuery(r.SpecRuleID, r.Query, rd)
		if err != nil {
			fail(r, err.Error())
			continue
		}
		if data.unmappedNullify && !r.IsAggregation {
			resolved = unmappedFieldsPrefix + resolved
		}

		rm := ResolvedMapping{RuleMapping: r, ResolvedQuery: resolved}
		if r.IsAggregation {
			result.Aggregation = &rm
		} else {
			result.Rules = append(result.Rules, rm)
		}
	}

	return &result, result.Err()
}

// templateFuncs are the functions rule templates may call:
// sensitiveQuery / sensitiveMetricQuery render the sensitive-data scan that
// follows a rule's FROM and WHERE (sensitive.Query, sensitive.MetricQuery).
var templateFuncs = template.FuncMap{
	"sensitiveQuery":       sensitive.Query,
	"sensitiveMetricQuery": sensitive.MetricQuery,
}

func renderQuery(ruleID, tmpl string, data TemplateData) (string, error) {
	t, err := template.New(ruleID).Option("missingkey=error").Funcs(templateFuncs).Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("%s: parse template: %w", ruleID, err)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("%s: execute template: %w", ruleID, err)
	}

	return buf.String(), nil
}
