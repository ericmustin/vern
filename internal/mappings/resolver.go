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
	ResultIndex          string
	CardinalityThreshold int
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
	data := TemplateData{
		Indices:              cfg.ESQL.IndexPatterns,
		TimeWindow:           cfg.ESQL.TimeWindow,
		ResultIndex:          cfg.ESQL.ResultIndex,
		CardinalityThreshold: cfg.ESQL.CardinalityThreshold,
	}

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
