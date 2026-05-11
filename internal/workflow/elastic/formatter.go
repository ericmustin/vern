package elastic

import (
	"fmt"
	"strings"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/mappings"
	"gopkg.in/yaml.v3"
)

var impactWeights = map[string]int{
	"Critical":  40,
	"Important": 30,
	"Normal":    20,
	"Low":       10,
}

func Generate(resolved *mappings.ResolveResult, cfg *config.Config, summaries ...*coverage.Summary) ([]byte, error) {
	if resolved == nil {
		return nil, fmt.Errorf("nil resolved mappings")
	}
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	if len(resolved.Rules) == 0 {
		return nil, fmt.Errorf("no enabled rules to generate workflow from")
	}
	cfgCopy := *cfg
	cfgCopy.ApplyDefaults()
	cfg = &cfgCopy
	var cov *coverage.Summary
	if len(summaries) > 0 {
		cov = summaries[0]
	}

	wf := Workflow{
		Name:        "Instrumentation Score Evaluation",
		Description: "Evaluates OpenTelemetry instrumentation quality per service against the Instrumentation Score spec.",
		Tags:        []string{"observability", "instrumentation-score", "otel"},
		Triggers: []Trigger{{
			Type: "scheduled",
			With: TriggerWith{Every: cfg.ESQL.Schedule},
		}},
		Consts: map[string]string{
			"result_index": cfg.ESQL.ResultIndex,
		},
	}

	wf.Steps = append(wf.Steps, bootstrapStep())
	wf.Steps = append(wf.Steps, coverageStep(cov))

	for _, m := range resolved.Rules {
		slug := ruleSlug(m.SpecRuleID)
		wf.Steps = append(wf.Steps, evalStep(slug, m.ResolvedQuery))
		wf.Steps = append(wf.Steps, storeStep(slug, m))
	}

	if resolved.Aggregation != nil {
		wf.Steps = append(wf.Steps, Step{
			Name: "calculate_scores",
			Type: "elasticsearch.esql.query",
			With: map[string]interface{}{
				"format": "json",
				"query":  resolved.Aggregation.ResolvedQuery,
			},
		})
		wf.Steps = append(wf.Steps, storeTotalsStep())
		wf.Steps = append(wf.Steps, annotateStep(cfg.ESQL.AnnotationsIndex))
	}

	out, err := yaml.Marshal(wf)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow: %w", err)
	}
	return out, nil
}

// bootstrapStep indexes a single placeholder document containing every field
// downstream steps and dashboards reference, with each value at the type the
// dashboards expect (numbers as numbers, booleans as booleans). Elasticsearch
// dynamic mapping locks in field types from the FIRST document indexed —
// so if the first writer is a foreach using Liquid (which always emits
// strings), score, category, etc. would lock as `text`. This step writes
// them as proper types FIRST so subsequent string-form values get coerced.
//
// The row is filtered out by `rule_id == "_BOOTSTRAP"` in dashboards / score
// aggregation.
//
// NOTE: if you have an existing `instrumentation-score-results` data stream
// that was created with the wrong types, you must delete it once. Mappings
// for existing fields are immutable.
func bootstrapStep() Step {
	return Step{
		Name: "bootstrap_mapping",
		Type: "elasticsearch.index",
		With: map[string]interface{}{
			"index": "{{ consts.result_index }}",
			"document": map[string]interface{}{
				"rule_id":           "_BOOTSTRAP",
				"impact":            "Low",
				"weight":            0,
				"target":            "Bootstrap",
				"description":       "Schema bootstrap row — filtered out of score aggregation.",
				"service.name":      "_schema_init",
				"rule_passed":       true,
				"extent":            0.0,
				"example":           "",
				"evaluated_at":      "{{ 'now' | date: '%Y-%m-%dT%H:%M:%SZ' }}",
				"run_id":            "{{ execution.id }}",
				"run_started_at":    "{{ execution.startedAt }}",
				"score":             0.0,
				"category":          "Bootstrap",
				"critical_passed":   0,
				"critical_total":    0,
				"important_passed":  0,
				"important_total":   0,
				"normal_passed":     0,
				"normal_total":      0,
				"low_passed":        0,
				"low_total":         0,
				"spec_version":      "",
				"implemented_rules": []string{},
				"enabled_rules":     []string{},
				"missing_rules":     []string{},
				"partial_score":     true,
			},
		},
		OnFailure: &OnFailure{Continue: true},
	}
}

func coverageStep(cov *coverage.Summary) Step {
	specVersion := ""
	implemented := []string{}
	enabled := []string{}
	missing := []string{}
	partial := true
	if cov != nil {
		specVersion = cov.SpecVersion
		implemented = cov.ImplementedRules
		enabled = cov.EnabledRules
		missing = cov.MissingRules
		partial = cov.PartialScore
	}
	return Step{
		Name: "store_coverage",
		Type: "elasticsearch.index",
		With: map[string]interface{}{
			"index": "{{ consts.result_index }}",
			"document": map[string]interface{}{
				"rule_id":           "_COVERAGE",
				"impact":            "_coverage",
				"target":            "Coverage",
				"description":       "Rule coverage metadata for this generated Vern workflow.",
				"service.name":      "_coverage",
				"rule_passed":       true,
				"extent":            0.0,
				"example":           "",
				"evaluated_at":      "{{ 'now' | date: '%Y-%m-%dT%H:%M:%SZ' }}",
				"run_id":            "{{ execution.id }}",
				"run_started_at":    "{{ execution.startedAt }}",
				"spec_version":      specVersion,
				"implemented_rules": implemented,
				"enabled_rules":     enabled,
				"missing_rules":     missing,
				"partial_score":     partial,
			},
		},
		OnFailure: &OnFailure{Continue: true},
	}
}

func evalStep(slug, query string) Step {
	return Step{
		Name: "eval_" + slug,
		Type: "elasticsearch.esql.query",
		With: map[string]interface{}{
			"format": "json",
			"query":  query,
		},
	}
}

// storeStep wraps an elasticsearch.index step in a foreach so we write one
// document per service-row from the eval step's output. The eval query
// guarantees a fixed column order: rule_passed, service.name, example, extent.
// Loop variable in Elastic Workflows Liquid is `foreach.item`.
func storeStep(slug string, m mappings.ResolvedMapping) Step {
	return Step{
		Name:    "store_" + slug,
		Type:    "foreach",
		Foreach: "{{ steps.eval_" + slug + ".output.values }}",
		Steps: []Step{{
			Name: "index_" + slug,
			Type: "elasticsearch.index",
			With: map[string]interface{}{
				"index": "{{ consts.result_index }}",
				"document": map[string]interface{}{
					"rule_id":        m.SpecRuleID,
					"impact":         m.Impact,
					"weight":         impactWeights[m.Impact],
					"target":         m.Target,
					"description":    m.Description,
					"evaluated_at":   "{{ 'now' | date: '%Y-%m-%dT%H:%M:%SZ' }}",
					"run_id":         "{{ execution.id }}",
					"run_started_at": "{{ execution.startedAt }}",
					"rule_passed":    "{{ foreach.item[0] }}",
					"service.name":   "{{ foreach.item[1] }}",
					"example":        "{{ foreach.item[2] }}",
					"extent":         "{{ foreach.item[3] }}",
				},
			},
			OnFailure: &OnFailure{Continue: true},
		}},
	}
}

// storeTotalsStep persists per-service score totals from the calculate_scores
// query into the result index with rule_id="_TOTAL". Dashboards read these
// rows to render the overview leaderboard and per-service score metric
// without re-running rule queries.
//
// Column order from calculate_scores ESQL output:
//
//	[0] service.name      [1] score          [2] category
//	[3] critical_passed   [4] critical_total
//	[5] important_passed  [6] important_total
//	[7] normal_passed     [8] normal_total
//	[9] low_passed        [10] low_total
func storeTotalsStep() Step {
	return Step{
		Name:    "store_totals",
		Type:    "foreach",
		Foreach: "{{ steps.calculate_scores.output.values }}",
		Steps: []Step{{
			Name: "index_total",
			Type: "elasticsearch.index",
			With: map[string]interface{}{
				"index": "{{ consts.result_index }}",
				"document": map[string]interface{}{
					"rule_id":          "_TOTAL",
					"impact":           "_total",
					"target":           "Score",
					"description":      "Per-service instrumentation score — aggregated from rule results.",
					"evaluated_at":     "{{ 'now' | date: '%Y-%m-%dT%H:%M:%SZ' }}",
					"run_id":           "{{ execution.id }}",
					"run_started_at":   "{{ execution.startedAt }}",
					"service.name":     "{{ foreach.item[0] }}",
					"score":            "{{ foreach.item[1] }}",
					"category":         "{{ foreach.item[2] }}",
					"critical_passed":  "{{ foreach.item[3] }}",
					"critical_total":   "{{ foreach.item[4] }}",
					"important_passed": "{{ foreach.item[5] }}",
					"important_total":  "{{ foreach.item[6] }}",
					"normal_passed":    "{{ foreach.item[7] }}",
					"normal_total":     "{{ foreach.item[8] }}",
					"low_passed":       "{{ foreach.item[9] }}",
					"low_total":        "{{ foreach.item[10] }}",
				},
			},
			OnFailure: &OnFailure{Continue: true},
		}},
	}
}

// annotateStep writes one APM "deployment" annotation per service to the
// configured annotations index that Elastic APM views read from. The
// annotation appears as a marker on the service detail page, labeled with
// the service's instrumentation score and category.
//
// Writes directly to the index (not the apm/services/{name}/annotation API)
// so the workflow doesn't need a Kibana API key. Schema mirrors what the
// API would produce.
func annotateStep(index string) Step {
	return Step{
		Name:    "annotate_apm",
		Type:    "foreach",
		Foreach: "{{ steps.calculate_scores.output.values }}",
		Steps: []Step{{
			Name: "post_annotation",
			Type: "elasticsearch.index",
			With: map[string]interface{}{
				"index": index,
				"document": map[string]interface{}{
					"@timestamp": "{{ 'now' | date: '%Y-%m-%dT%H:%M:%SZ' }}",
					"service": map[string]interface{}{
						"name":    "{{ foreach.item[0] }}",
						"version": "vern-score-{{ foreach.item[1] }}",
					},
					"annotation": map[string]interface{}{
						"type":  "deployment",
						"title": "Vern Instrumentation Score: {{ foreach.item[1] }} ({{ foreach.item[2] }})",
					},
					"message": "Vern Instrumentation Score: {{ foreach.item[1] }} ({{ foreach.item[2] }})",
					"tags":    []string{"apm", "vern", "instrumentation-score"},
					"event": map[string]interface{}{
						"created": "{{ 'now' | date: '%Y-%m-%dT%H:%M:%SZ' }}",
					},
				},
			},
			OnFailure: &OnFailure{Continue: true},
		}},
	}
}

func ruleSlug(ruleID string) string {
	s := strings.ToLower(ruleID)
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.TrimPrefix(s, "_")
	return s
}
