package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Backend    string           `yaml:"backend"`
	RulesDir   string           `yaml:"rules_dir"`
	Mappings   string           `yaml:"mappings"`
	Format     string           `yaml:"format"`
	ESQL       ESQLConfig       `yaml:"esql"`
	Elastic    ElasticConfig    `yaml:"elastic"`
	OpenSearch OpenSearchConfig `yaml:"opensearch"`
	ClickHouse ClickHouseConfig `yaml:"clickhouse"`
	Exclusions ExclusionConfig  `yaml:"exclusions"`
	Filters    FilterConfig     `yaml:"filters"`
	Spec       SpecConfig       `yaml:"spec"`
	Semconv    SemconvConfig    `yaml:"semconv"`
}

type ESQLConfig struct {
	TimeWindow           string        `yaml:"time_window"`
	ScoreLookback        string        `yaml:"score_lookback"`
	IndexPatterns        IndexPatterns `yaml:"index_patterns"`
	ResultIndex          string        `yaml:"result_index"`
	AnnotationsIndex     string        `yaml:"annotations_index"`
	Schedule             string        `yaml:"schedule"`
	CardinalityThreshold int           `yaml:"cardinality_threshold"`
	// UnmappedFields controls the `SET unmapped_fields="nullify";` prefix
	// added to ES|QL rule queries. "nullify" (default) or "off".
	UnmappedFields string `yaml:"unmapped_fields"`
}

type IndexPatterns struct {
	Traces  string `yaml:"traces"`
	Metrics string `yaml:"metrics"`
	Logs    string `yaml:"logs"`
}

type ElasticConfig struct {
	KibanaURL        string `yaml:"kibana_url"`
	ElasticsearchURL string `yaml:"elasticsearch_url"`
	APIKey           string `yaml:"api_key"`
}

type OpenSearchConfig struct {
	URL                  string        `yaml:"url"`
	Username             string        `yaml:"username"`
	Password             string        `yaml:"password"`
	IndexPatterns        IndexPatterns `yaml:"index_patterns"`
	TimeWindow           string        `yaml:"time_window"`
	CardinalityThreshold int           `yaml:"cardinality_threshold"`
}

type ClickHouseConfig struct {
	URL                  string           `yaml:"url"`
	Username             string           `yaml:"username"`
	Password             string           `yaml:"password"`
	Database             string           `yaml:"database"`
	Tables               ClickHouseTables `yaml:"tables"`
	TimeWindow           string           `yaml:"time_window"`
	CardinalityThreshold int              `yaml:"cardinality_threshold"`
}

type ClickHouseTables struct {
	Traces  string `yaml:"traces"`
	Logs    string `yaml:"logs"`
	Metrics string `yaml:"metrics"`
}

type ExclusionConfig struct {
	Services        []string `yaml:"services"`
	ServicePatterns []string `yaml:"service_patterns"`
}

type FilterConfig struct {
	Environments      []string `yaml:"environments"`
	ServiceNamespaces []string `yaml:"service_namespaces"`
	EnableSDKRules    bool     `yaml:"enable_sdk_rules"`
}

type SpecConfig struct {
	UpstreamRepo string `yaml:"upstream_repo"`
	UpstreamRef  string `yaml:"upstream_ref"`
}

type SemconvConfig struct {
	UpstreamRepo string `yaml:"upstream_repo"`
	UpstreamRef  string `yaml:"upstream_ref"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	for _, p := range cfg.Exclusions.ServicePatterns {
		if _, err := regexp.Compile(p); err != nil {
			return nil, fmt.Errorf("config %s: invalid exclusions.service_patterns entry %q: %w", path, p, err)
		}
	}

	cfg.ApplyDefaults()
	return &cfg, nil
}

func (c *Config) ApplyDefaults() {
	if c.Backend == "" {
		c.Backend = "esql"
	}
	if c.RulesDir == "" {
		c.RulesDir = "./spec/rules"
	}
	if c.Mappings == "" {
		c.Mappings = "./configs/esql-mappings.yaml"
	}
	if c.Format == "" {
		c.Format = "elastic"
	}
	if c.ESQL.TimeWindow == "" {
		c.ESQL.TimeWindow = "1h"
	}
	if c.ESQL.ScoreLookback == "" {
		c.ESQL.ScoreLookback = "2h"
	}
	if c.ESQL.ResultIndex == "" {
		c.ESQL.ResultIndex = "instrumentation-score-results"
	}
	if c.ESQL.AnnotationsIndex == "" {
		c.ESQL.AnnotationsIndex = "observability-annotations"
	}
	if c.ESQL.UnmappedFields == "" {
		c.ESQL.UnmappedFields = "nullify"
	}
	if c.ESQL.Schedule == "" {
		c.ESQL.Schedule = "1h"
	}
	if c.ESQL.CardinalityThreshold == 0 {
		c.ESQL.CardinalityThreshold = 10000
	}
	if c.ESQL.IndexPatterns.Traces == "" {
		c.ESQL.IndexPatterns.Traces = "traces-*.otel-*"
	}
	if c.ESQL.IndexPatterns.Metrics == "" {
		c.ESQL.IndexPatterns.Metrics = "metrics-*.otel-*"
	}
	if c.ESQL.IndexPatterns.Logs == "" {
		c.ESQL.IndexPatterns.Logs = "logs-*.otel-*"
	}
	if c.OpenSearch.TimeWindow == "" {
		// PPL INTERVAL clause; e.g. "INTERVAL 30 DAY".
		c.OpenSearch.TimeWindow = "30 DAY"
	}
	if c.OpenSearch.CardinalityThreshold == 0 {
		c.OpenSearch.CardinalityThreshold = 10000
	}
	// Defaults follow the SS4O (Simple Schema for Observability) index naming
	// that the OpenTelemetry Collector's opensearch exporter writes — that is
	// the golden path for OTel ingest. Data Prepper pipelines use different
	// names (e.g. otel-v1-apm-span-*); override index_patterns for those.
	if c.OpenSearch.IndexPatterns.Traces == "" {
		c.OpenSearch.IndexPatterns.Traces = "ss4o_traces-*-*"
	}
	if c.OpenSearch.IndexPatterns.Metrics == "" {
		c.OpenSearch.IndexPatterns.Metrics = "ss4o_metrics-*-*"
	}
	if c.OpenSearch.IndexPatterns.Logs == "" {
		c.OpenSearch.IndexPatterns.Logs = "ss4o_logs-*-*"
	}
	if c.ClickHouse.Database == "" {
		c.ClickHouse.Database = "otel"
	}
	if c.ClickHouse.TimeWindow == "" {
		c.ClickHouse.TimeWindow = "30 DAY"
	}
	if c.ClickHouse.CardinalityThreshold == 0 {
		c.ClickHouse.CardinalityThreshold = 10000
	}
	if c.ClickHouse.Tables.Traces == "" {
		c.ClickHouse.Tables.Traces = "otel_traces"
	}
	if c.ClickHouse.Tables.Logs == "" {
		c.ClickHouse.Tables.Logs = "otel_logs"
	}
	if c.ClickHouse.Tables.Metrics == "" {
		c.ClickHouse.Tables.Metrics = "otel_metrics_sum"
	}
	if c.Spec.UpstreamRepo == "" {
		c.Spec.UpstreamRepo = "instrumentation-score/spec"
	}
	if c.Spec.UpstreamRef == "" {
		c.Spec.UpstreamRef = "main"
	}
	if c.Semconv.UpstreamRepo == "" {
		c.Semconv.UpstreamRepo = "open-telemetry/semantic-conventions"
	}
	if c.Semconv.UpstreamRef == "" {
		c.Semconv.UpstreamRef = "v1.37.0"
	}
}

// Default returns a Config with ApplyDefaults applied, for runs with no
// vern.yaml on disk.
func Default() *Config {
	c := &Config{}
	c.ApplyDefaults()
	return c
}
