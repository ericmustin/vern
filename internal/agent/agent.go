// Package agent builds the Elastic Agent Builder skill + agent that vern
// uploads via `vern agent setup`.
package agent

import (
	"fmt"
	"strings"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
)

// Stable identifiers so re-running `vern agent setup` upserts in place.
const (
	SkillID          = "vern-instrumentation-score-governance"
	SkillName        = "vern-instrumentation-score-governance"
	SkillDescription = "Answer governance questions about OpenTelemetry instrumentation quality scores produced by the Vern workflow. Use when the user mentions instrumentation score, OTel quality, service-level scoring, a service's score, best/worst services, services below a compliance threshold, failing rules for a service, services failing a specific spec rule (RES-/SPA-/LOG-/MET-/SDK-*), comparing service scores, or pulling example violating documents."

	AgentID          = "vern-instrumentation-score"
	AgentName        = "Vern Instrumentation Score"
	AgentDescription = "Answer questions about OpenTelemetry instrumentation quality. Backed by the vern-instrumentation-score-governance skill — knows the per-service score schema, can find best/worst services, look up failing rules, and pull example evidence documents."
	// Agent-level instructions are intentionally minimal; the skill carries
	// the data-specific knowledge so it can be reused across agents.
	AgentInstructions = "You answer questions about OpenTelemetry instrumentation quality scores. " +
		"Use the **" + SkillID + "** skill for any service-level quality, scoring, " +
		"compliance, or rule-evidence question — it knows the data schema and the " +
		"correct query patterns. Cite upstream spec URLs when discussing rules and " +
		"include dashboard links when a visual follow-up would help."
)

// Skill is the request body for PUT/POST /api/agent_builder/skills.
type Skill struct {
	ID          string   `json:"id,omitempty"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Content     string   `json:"content"`
	ToolIDs     []string `json:"tool_ids"`
}

// Agent is the request body for PUT/POST /api/agent_builder/agents/{id}.
type Agent struct {
	ID            string        `json:"id,omitempty"`
	Name          string        `json:"name"`
	Description   string        `json:"description"`
	Configuration Configuration `json:"configuration"`
}

type Configuration struct {
	Instructions string   `json:"instructions"`
	Tools        []Tools  `json:"tools"`
	SkillIDs     []string `json:"skill_ids"`
}

type Tools struct {
	ToolIDs []string `json:"tool_ids"`
}

type Context struct {
	Config   *config.Config
	Coverage *coverage.Summary
}

// vernTools is the minimum set the skill (and agent) need:
//
//	platform.core.search             — query the configured Vern result index
//	platform.core.list_indices       — discover related signal indices
//	platform.core.get_index_mapping  — resolve field types if asked
//	platform.core.get_document_by_id — fetch the original violating doc from
//	                                   traces-*/logs-* using the `example`
//	                                   field on per-rule rows
var vernTools = []string{
	"platform.core.search",
	"platform.core.list_indices",
	"platform.core.get_index_mapping",
	"platform.core.get_document_by_id",
}

// BuildSkill returns the governance skill definition.
func BuildSkill(ctx Context) Skill {
	return Skill{
		ID:          SkillID,
		Name:        SkillName,
		Description: SkillDescription,
		Content:     RenderSkillMarkdown(ctx),
		ToolIDs:     vernTools,
	}
}

// BuildAgent returns the agent definition that references the skill.
func BuildAgent() Agent {
	return Agent{
		ID:          AgentID,
		Name:        AgentName,
		Description: AgentDescription,
		Configuration: Configuration{
			Instructions: AgentInstructions,
			Tools:        []Tools{{ToolIDs: vernTools}},
			SkillIDs:     []string{SkillID},
		},
	}
}

// RenderSkillMarkdown returns the exact markdown content uploaded to Agent Builder.
// It is exported so CLI commands can also persist a human-reviewable artifact.
func RenderSkillMarkdown(ctx Context) string {
	cfg := ctx.Config
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfgCopy := *cfg
	cfgCopy.ApplyDefaults()
	cfg = &cfgCopy

	cov := ctx.Coverage
	var enabled, disabled, missing, heuristic string
	partial := false
	specVersion := ""
	if cov != nil {
		enabled = coverage.Join(cov.EnabledRules)
		disabled = coverage.Join(cov.DisabledRules)
		missing = coverage.Join(cov.MissingRules)
		heuristic = coverage.Join(cov.HeuristicRules)
		partial = cov.PartialScore
		specVersion = cov.SpecVersion
	} else {
		enabled = "unknown"
		disabled = "unknown"
		missing = "unknown"
		heuristic = "unknown"
		partial = true
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You are running the **Vern Instrumentation Score Governance** skill. Use this skill any time the user asks about OpenTelemetry instrumentation quality scores produced by the Vern workflow — service-level quality lookups, ranking, spec compliance, failing rules, evidence pulls, or comparisons.\n\n")
	fmt.Fprintf(&b, "## Score status\n\n")
	fmt.Fprintf(&b, "Vern reports a **partial Instrumentation Score**: `%t`. The score is calculated only from rules implemented and enabled in this Vern configuration. Always mention that coverage status when presenting a score.\n\n", partial)
	if specVersion != "" {
		fmt.Fprintf(&b, "- Spec version: `%s`\n", specVersion)
	}
	fmt.Fprintf(&b, "- Enabled rules: `%s`\n", enabled)
	fmt.Fprintf(&b, "- Heuristic rules: `%s`\n", heuristic)
	fmt.Fprintf(&b, "- Disabled rules: `%s`\n", disabled)
	fmt.Fprintf(&b, "- Missing rules: `%s`\n\n", missing)

	fmt.Fprintf(&b, "## Where the data lives\n\n")
	fmt.Fprintf(&b, "Always query the data stream **`%s`** for Vern data. Never re-run rule queries against `%s`, `%s`, or `%s` unless the user explicitly asks to debug rule execution.\n\n", cfg.ESQL.ResultIndex, cfg.ESQL.IndexPatterns.Traces, cfg.ESQL.IndexPatterns.Logs, cfg.ESQL.IndexPatterns.Metrics)
	fmt.Fprintf(&b, "Signal index patterns for evidence lookup:\n\n")
	fmt.Fprintf(&b, "- Traces: `%s`\n", cfg.ESQL.IndexPatterns.Traces)
	fmt.Fprintf(&b, "- Logs: `%s`\n", cfg.ESQL.IndexPatterns.Logs)
	fmt.Fprintf(&b, "- Metrics: `%s`\n\n", cfg.ESQL.IndexPatterns.Metrics)

	fmt.Fprintf(&b, "## Document schema\n\n")
	fmt.Fprintf(&b, "Four row types live in `%s`. Filter by `rule_id.keyword`:\n\n", cfg.ESQL.ResultIndex)
	fmt.Fprintf(&b, "| `rule_id.keyword` | Row meaning | Key fields |\n")
	fmt.Fprintf(&b, "|---|---|---|\n")
	fmt.Fprintf(&b, "| `_TOTAL` | Per-service partial score, one per service per workflow run | `service.name`, `score`, `category`, impact passed/total fields, `evaluated_at` |\n")
	fmt.Fprintf(&b, "| `_COVERAGE` | Coverage metadata for the current generated workflow | `spec_version`, `implemented_rules`, `enabled_rules`, `missing_rules`, `partial_score`, `evaluated_at` |\n")
	fmt.Fprintf(&b, "| `_BOOTSTRAP` | Schema placeholder. Always exclude. | - |\n")
	fmt.Fprintf(&b, "| any other rule id | Per-rule, per-service evidence row | `service.name`, `rule_passed`, `extent`, `example`, `impact`, `target`, `description`, `evaluated_at` |\n\n")
	fmt.Fprintf(&b, "Fields:\n")
	fmt.Fprintf(&b, "- `score` (0-100): for `_TOTAL` rows. Cast with `score::double` in ES|QL when sorting/aggregating.\n")
	fmt.Fprintf(&b, "- `category`: `Excellent` (>=90), `Good` (>=75), `Needs Improvement` (>=50), `Poor` (<50).\n")
	fmt.Fprintf(&b, "- `extent` (0.0-1.0): proportion of evidence violating the rule. 0 = clean, 1 = all bad.\n")
	fmt.Fprintf(&b, "- `example`: doc `_id` of a violating document in the underlying signal index.\n")
	fmt.Fprintf(&b, "- `service.name` is commonly `text` with a `service.name.keyword` sub-field. Use `service.name.keyword` in `term` filters / sort when available.\n\n")

	fmt.Fprintf(&b, "## Score formula\n\n")
	fmt.Fprintf(&b, "The partial score uses the spec formula over implemented enabled rules only:\n\n")
	fmt.Fprintf(&b, "```text\nscore = sum(P_i * W_i) / sum(T_i * W_i) * 100\n```\n\n")
	fmt.Fprintf(&b, "Weights: Critical=40, Important=30, Normal=20, Low=10.\n\n")
	fmt.Fprintf(&b, "## Spec lookup\n\n")
	fmt.Fprintf(&b, "Each rule_id maps to `https://github.com/instrumentation-score/spec/blob/main/rules/<RULE_ID>.md`. Link that URL when a rule comes up.\n\n")
	fmt.Fprintf(&b, "## Dashboards\n\n")
	fmt.Fprintf(&b, "- Overview: `/app/dashboards#/view/vern-overview`\n")
	fmt.Fprintf(&b, "- Per-service drill-down: `/app/dashboards#/view/vern-drilldown?_a=(filters:!((meta:(key:service.name,params:(query:'<svc>')),query:(match_phrase:(service.name:'<svc>')))))`\n\n")

	fmt.Fprintf(&b, "## Query patterns\n\n")
	fmt.Fprintf(&b, "Use `platform.core.search` with `index: \"%s\"`. Always sort by `evaluated_at` desc and take the freshest row because older runs accumulate.\n\n", cfg.ESQL.ResultIndex)
	fmt.Fprintf(&b, "Score for one service:\n\n")
	fmt.Fprintf(&b, "```json\n{\"size\":1,\"query\":{\"bool\":{\"must\":[{\"term\":{\"rule_id.keyword\":\"_TOTAL\"}},{\"term\":{\"service.name.keyword\":\"<SVC>\"}}]}},\"sort\":[{\"evaluated_at\":{\"order\":\"desc\"}}]}\n```\n\n")
	fmt.Fprintf(&b, "Worst services:\n\n")
	fmt.Fprintf(&b, "```json\n{\"size\":5,\"query\":{\"term\":{\"rule_id.keyword\":\"_TOTAL\"}},\"sort\":[{\"score\":{\"order\":\"asc\"}}]}\n```\n\n")
	fmt.Fprintf(&b, "Failing rules for one service:\n\n")
	fmt.Fprintf(&b, "```json\n{\"size\":50,\"query\":{\"bool\":{\"must\":[{\"term\":{\"service.name.keyword\":\"<SVC>\"}},{\"term\":{\"rule_passed\":false}}],\"must_not\":[{\"terms\":{\"rule_id.keyword\":[\"_TOTAL\",\"_BOOTSTRAP\",\"_COVERAGE\"]}}]}},\"sort\":[{\"evaluated_at\":{\"order\":\"desc\"}}]}\n```\n\n")
	fmt.Fprintf(&b, "Coverage metadata:\n\n")
	fmt.Fprintf(&b, "```json\n{\"size\":1,\"query\":{\"term\":{\"rule_id.keyword\":\"_COVERAGE\"}},\"sort\":[{\"evaluated_at\":{\"order\":\"desc\"}}]}\n```\n\n")

	fmt.Fprintf(&b, "## Response style\n\n")
	fmt.Fprintf(&b, "- Lead with the answer and state that the score is partial.\n")
	fmt.Fprintf(&b, "- Include service names, rule IDs, `extent`, and `score` values.\n")
	fmt.Fprintf(&b, "- Cite the upstream spec URL when discussing a rule.\n")
	fmt.Fprintf(&b, "- If a query returns zero rows, say so explicitly. Never invent data.\n")
	fmt.Fprintf(&b, "- If `_TOTAL` rows are missing, direct the user to run the Vern workflow first.\n")
	return b.String()
}
