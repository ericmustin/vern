package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Backend    string          `yaml:"backend"`
	RulesDir   string          `yaml:"rules_dir"`
	Mappings   string          `yaml:"mappings"`
	Format     string          `yaml:"format"`
	ESQL       ESQLConfig      `yaml:"esql"`
	Elastic    ElasticConfig   `yaml:"elastic"`
	Exclusions ExclusionConfig `yaml:"exclusions"`
}

type ESQLConfig struct {
	TimeWindow           string        `yaml:"time_window"`
	IndexPatterns        IndexPatterns `yaml:"index_patterns"`
	ResultIndex          string        `yaml:"result_index"`
	Schedule             string        `yaml:"schedule"`
	CardinalityThreshold int           `yaml:"cardinality_threshold"`
}

type IndexPatterns struct {
	Traces  string `yaml:"traces"`
	Metrics string `yaml:"metrics"`
	Logs    string `yaml:"logs"`
}

type ElasticConfig struct {
	KibanaURL string `yaml:"kibana_url"`
	APIKey    string `yaml:"api_key"`
}

type ExclusionConfig struct {
	Services        []string `yaml:"services"`
	ServicePatterns []string `yaml:"service_patterns"`
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

	cfg.applyDefaults()
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Backend == "" {
		c.Backend = "esql"
	}
	if c.Mappings == "" {
		c.Mappings = "esql-mappings.yaml"
	}
	if c.Format == "" {
		c.Format = "elastic"
	}
	if c.ESQL.TimeWindow == "" {
		c.ESQL.TimeWindow = "1h"
	}
	if c.ESQL.ResultIndex == "" {
		c.ESQL.ResultIndex = "instrumentation-score-results"
	}
	if c.ESQL.Schedule == "" {
		c.ESQL.Schedule = "1h"
	}
	if c.ESQL.CardinalityThreshold == 0 {
		c.ESQL.CardinalityThreshold = 10000
	}
	if c.ESQL.IndexPatterns.Traces == "" {
		c.ESQL.IndexPatterns.Traces = "traces-apm*"
	}
	if c.ESQL.IndexPatterns.Metrics == "" {
		c.ESQL.IndexPatterns.Metrics = "metrics-*.otel-*"
	}
	if c.ESQL.IndexPatterns.Logs == "" {
		c.ESQL.IndexPatterns.Logs = "logs-*.otel-*"
	}
}
