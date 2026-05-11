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
}

type ResolvedMapping struct {
	RuleMapping
	ResolvedQuery string
}
