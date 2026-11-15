// Package evaluate orchestrates local execution of Instrumentation Score rules
// against a query backend. The runner loads the configured mappings file,
// resolves rule templates, dispatches each rule's query to a backend-specific
// Executor, attaches rule identity, and feeds the collected outcomes to the
// score calculator.
package evaluate

import (
	"context"
	"time"

	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/score"
)

// SchemaVersion is the version of the JSON report layout.
const SchemaVersion = 2

// RuleStatus says what happened to a rule in a run.
type RuleStatus string

const (
	StatusEvaluated      RuleStatus = "evaluated"
	StatusNotApplicable  RuleStatus = "not_applicable"
	StatusNotAssessable  RuleStatus = "not_assessable"
	StatusNotImplemented RuleStatus = "not_implemented"
	StatusError          RuleStatus = "error"
)

// RuleResult is the per-rule record in a report: every spec rule gets one.
type RuleResult struct {
	RuleID      string     `json:"rule_id"`
	Impact      string     `json:"impact"`
	Target      string     `json:"target"`
	Description string     `json:"description"`
	Status      RuleStatus `json:"status"`
	Scored      bool       `json:"scored"` // Status==evaluated && !Advisory
	Reason      string     `json:"reason,omitempty"`
	Advisory    bool       `json:"advisory,omitempty"`
	Heuristic   bool       `json:"heuristic,omitempty"`
	// Sensitive marks the sensitive-data rules: their failing examples are
	// redacted, and the sample's blind spots qualify their passes.
	Sensitive   bool            `json:"sensitive,omitempty"`
	Deviation   string          `json:"deviation,omitempty"`
	Remediation string          `json:"remediation,omitempty"`
	Outcomes    []score.Outcome `json:"outcomes,omitempty"`
}

// Row is what a backend Executor returns for a single rule query — one row
// per service. RuleID and Impact are attached by the runner from the rule
// definition, so the executor only needs to populate ServiceName/Passed/
// Extent/Example.
type Row struct {
	ServiceName string
	Passed      bool
	Extent      float64
	Example     string
}

// Executor runs a single rendered query against a backend and returns one
// Row per service. Implementations are expected to handle HTTP transport,
// auth, payload framing, and response parsing. The context governs the
// per-query timeout.
type Executor interface {
	Execute(ctx context.Context, query string) ([]Row, error)
}

// RunOptions carry metadata that the executor cannot know about itself.
type RunOptions struct {
	Backend      string // surfaces in the report (e.g. "elastic", "clickhouse")
	ConfigPath   string
	MappingsFile string

	// SpecRules, when set, makes the report cover every spec rule: those
	// without a mapping become not_implemented.
	SpecRules []coverage.SpecRule
	// Signals lists the signals present in the data; nil means live mode
	// with no signal gating.
	Signals map[string]bool
	// SampleMode is forwarded to query templates.
	SampleMode bool
	// ObservedSpan is how long the data covers; 0 means unknown, so no
	// min_observation gating.
	ObservedSpan time.Duration
	// Mappings, when non-nil, is used instead of loading cfg.Mappings from disk.
	Mappings *mappings.MappingsFile
	// Sample describes the loaded OTLP sample; copied to Report.Sample.
	Sample *SampleInfo
	// Fields are the run's discovered scannable fields, forwarded to query
	// templates (sensitive-data rules).
	Fields mappings.Fields
	// FieldsFailed means field discovery failed: rules marked Sensitive end
	// as errors ("field discovery failed"); other rules are unaffected.
	FieldsFailed bool
	// FieldsDiscovered means Fields come from a per-run field discovery
	// (vern score, vern serve). When false, as under vern evaluate, rules
	// marked Sensitive are not run: they end not_assessable with
	// mappings.NeedsFieldDiscovery as the reason.
	FieldsDiscovered bool
	// Logf receives the detail behind a sensitive rule's fixed error reason
	// (nil: discarded).
	Logf func(format string, args ...any)
	// QueryErrorDetail lets Logf receive a sensitive rule's full error text.
	// Elasticsearch errors can echo the query, which names the run's fields
	// and matched metric names, so set it only when the log is the user's
	// own (vern score's terminal), never for a shared server log; otherwise
	// Logf gets the classification only.
	QueryErrorDetail bool
	// FixedErrors limits every failed rule's reason to a fixed classification
	// (as for sensitive rules), for reports that are public API responses.
	FixedErrors bool
}

// SampleInfo describes the OTLP sample a score run evaluated.
type SampleInfo struct {
	Files         []string       `json:"files,omitempty"`
	Spans         int            `json:"spans"`
	SpanEvents    int            `json:"span_events"`
	LogRecords    int            `json:"log_records"`
	DataPoints    int            `json:"data_points"`
	Services      []string       `json:"services"`
	Signals       []string       `json:"signals"` // sorted
	OriginalStart time.Time      `json:"original_start"`
	OriginalEnd   time.Time      `json:"original_end"`
	Dropped       map[string]int `json:"dropped,omitempty"` // signal -> items not indexed
	Notes         []string       `json:"notes,omitempty"`
	// BlindSpots lists, as plain sentences, what the sensitive-data rules
	// could not scan in this sample; a pass says nothing about those parts.
	BlindSpots []string `json:"blind_spots,omitempty"`
	RunID      string   `json:"run_id,omitempty"` // engine run identifier (data stream namespace)
}

// RuleFailure records an executor or template-resolution error for a single
// rule. Failures do not abort the run — the remaining rules still execute
// and the score is computed from the rules that succeeded.
type RuleFailure struct {
	RuleID string `json:"rule_id"`
	Error  string `json:"error"`
}

// Report bundles a score result with the run metadata callers need to
// understand the context of a particular evaluation.
type Report struct {
	SchemaVersion  int           `json:"schema_version"`
	SpecVersion    string        `json:"spec_version,omitempty"`
	EvaluatedAt    time.Time     `json:"evaluated_at"`
	Backend        string        `json:"backend"`
	ConfigPath     string        `json:"config_path,omitempty"`
	MappingsFile   string        `json:"mappings_file,omitempty"`
	RulesAttempted int           `json:"rules_attempted"`
	RulesSucceeded int           `json:"rules_succeeded"`
	Failures       []RuleFailure `json:"failures,omitempty"`
	Rules          []RuleResult  `json:"rules"`
	Partial        bool          `json:"partial"`
	PartialReasons []string      `json:"partial_reasons,omitempty"`
	Score          *score.Result `json:"score"`
	Sample         *SampleInfo   `json:"sample,omitempty"`
	// Problems lists reasons the run cannot be trusted (see Problems); the
	// CLI exits 1 and the server flags the run when it is non-empty.
	Problems []string `json:"problems,omitempty"`
}
