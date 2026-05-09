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
}

type ResolvedMapping struct {
	RuleMapping
	ResolvedQuery string
}
