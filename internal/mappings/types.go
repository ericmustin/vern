package mappings

type MappingsFile struct {
	Backend     string        `yaml:"backend"`
	Version     string        `yaml:"version"`
	SpecVersion string        `yaml:"spec_version"`
	Rules       []RuleMapping `yaml:"rules"`
}

type RuleMapping struct {
	SpecRuleID    string `yaml:"spec_rule_id"`
	Enabled       bool   `yaml:"enabled"`
	Description   string `yaml:"description"`
	Impact        string `yaml:"impact"`
	Target        string `yaml:"target"`
	Status        string `yaml:"status"`
	IsAggregation bool   `yaml:"is_aggregation"`
	Notes         string `yaml:"notes"`
	Query         string `yaml:"query"`
	// OptInFlag is the name of a Config flag that must be true for this rule
	// to participate in scoring. Currently used by SDK-001 (gated behind
	// `filters.enable_sdk_rules`) because the support matrix is vendored
	// metadata that drifts. Empty string means the rule is unconditionally
	// enabled when Enabled is true.
	OptInFlag string `yaml:"opt_in_flag"`

	// Signal is "traces", "logs", "metrics" or "any".
	Signal string `yaml:"signal"`
	// MinObservation is a Go duration string (e.g. "336h").
	MinObservation string `yaml:"min_observation"`
	Advisory       bool   `yaml:"advisory"`
	Heuristic      bool   `yaml:"heuristic"`
	Deviation      string `yaml:"deviation"`
	Remediation    string `yaml:"remediation"`
	// Sensitive marks the sensitive-data rules (RES-010, SPA-006, LOG-003,
	// MET-008). They scan the run's discovered fields, so a failed field
	// discovery turns them into errors without affecting other rules.
	Sensitive bool `yaml:"sensitive"`
	// ScanGroups names the Fields groups a sensitive-data rule's query scans
	// (GroupResource, GroupScope, ..., GroupMetricNames for metric names).
	// Fields.SensitiveScan(ScanGroups) gives the capped field list (the
	// template's .ScanFields), its overflow, the metric names
	// (.ScanMetricNames) and the per-value character budget.
	ScanGroups []string `yaml:"scan_groups"`
}

type ResolvedMapping struct {
	RuleMapping
	ResolvedQuery string
}
