package mappings

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"text/template"

	"github.com/ericmustin/vern/internal/config"
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

	// SDKSupportMatrix is rendered ES|QL CASE-ladder text consumed by SDK-001.
	SDKSupportMatrix string
}

type ResolveResult struct {
	Rules       []ResolvedMapping
	Aggregation *ResolvedMapping
	Skipped     []SkippedRule
}

type SkippedRule struct {
	SpecRuleID string
	Reason     string
}

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
	if override.SDKSupportMatrix != "" {
		data.SDKSupportMatrix = override.SDKSupportMatrix
	}
	return resolveWith(rules, data)
}

func buildTemplateData(cfg *config.Config) TemplateData {
	return TemplateData{
		Indices:              cfg.ESQL.IndexPatterns,
		TimeWindow:           cfg.ESQL.TimeWindow,
		ScoreLookback:        cfg.ESQL.ScoreLookback,
		ResultIndex:          cfg.ESQL.ResultIndex,
		AnnotationsIndex:     cfg.ESQL.AnnotationsIndex,
		CardinalityThreshold: cfg.ESQL.CardinalityThreshold,
		ScopeFilter:          buildScopeFilter(cfg.Filters),
	}
}

func resolveWith(rules []RuleMapping, data TemplateData) (*ResolveResult, error) {
	var result ResolveResult
	var errs []string

	for _, r := range rules {
		if !r.Enabled {
			result.Skipped = append(result.Skipped, SkippedRule{r.SpecRuleID, "disabled"})
			continue
		}
		if strings.TrimSpace(r.Query) == "" {
			errs = append(errs, fmt.Sprintf("%s: empty query", r.SpecRuleID))
			continue
		}

		resolved, err := renderQuery(r.SpecRuleID, r.Query, data)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}

		rm := ResolvedMapping{RuleMapping: r, ResolvedQuery: resolved}
		if r.IsAggregation {
			result.Aggregation = &rm
		} else {
			result.Rules = append(result.Rules, rm)
		}
	}

	if len(errs) > 0 {
		return nil, errors.New("template resolution failed:\n  - " + strings.Join(errs, "\n  - "))
	}

	return &result, nil
}

func renderQuery(ruleID, tmpl string, data TemplateData) (string, error) {
	t, err := template.New(ruleID).Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("%s: parse template: %w", ruleID, err)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("%s: execute template: %w", ruleID, err)
	}

	return buf.String(), nil
}
