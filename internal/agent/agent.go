// Package agent builds the Elastic Agent Builder skill + agent that vern
// uploads via `vern agent setup`. The bulk of the data-schema knowledge
// lives in a Kibana **Skill** (id: vern-instrumentation-score-governance):
// a reusable, discoverable bundle of `description` + `content` + tool_ids
// that other agents can also reference. The agent itself is a thin
// pointer to the skill.
package agent

import (
	_ "embed"
)

//go:embed skill_content.md
var skillContent string

// Stable identifiers so re-running `vern agent setup` upserts in place.
const (
	SkillID          = "vern-instrumentation-score-governance"
	SkillName        = "vern-instrumentation-score-governance"
	SkillDescription = "Answer governance questions about OpenTelemetry instrumentation quality scores produced by the Vern workflow. Use when the user mentions instrumentation score, OTel quality, service-level scoring, the instrumentation-score-results index, or asks for: a service's score, best/worst services, services below a compliance threshold, failing rules for a service, services failing a specific spec rule (RES-/SPA-/LOG-/MET-/SDK-*), comparing service scores, or pulling example violating documents."

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
	Instructions string  `json:"instructions"`
	Tools        []Tools `json:"tools"`
	SkillIDs     []string `json:"skill_ids"`
}

type Tools struct {
	ToolIDs []string `json:"tool_ids"`
}

// vernTools is the minimum set the skill (and agent) need:
//
//	platform.core.search             — query instrumentation-score-results
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
func BuildSkill() Skill {
	return Skill{
		ID:          SkillID,
		Name:        SkillName,
		Description: SkillDescription,
		Content:     skillContent,
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
