package config

import "testing"

func TestApplyDefaults_ReproduciblePathsAndNativeOTel(t *testing.T) {
	cfg := &Config{}
	cfg.ApplyDefaults()

	if cfg.RulesDir != "./spec/rules" {
		t.Fatalf("RulesDir = %q", cfg.RulesDir)
	}
	if cfg.Mappings != "./configs/esql-mappings.yaml" {
		t.Fatalf("Mappings = %q", cfg.Mappings)
	}
	if cfg.ESQL.IndexPatterns.Traces != "traces-*.otel-*" {
		t.Fatalf("Traces = %q", cfg.ESQL.IndexPatterns.Traces)
	}
	if cfg.ESQL.ScoreLookback != "2h" {
		t.Fatalf("ScoreLookback = %q", cfg.ESQL.ScoreLookback)
	}
	if cfg.ESQL.AnnotationsIndex != "observability-annotations" {
		t.Fatalf("AnnotationsIndex = %q", cfg.ESQL.AnnotationsIndex)
	}
}
