package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestDefault_MatchesApplyDefaults(t *testing.T) {
	c := Default()
	if c.Mappings != "./configs/esql-mappings.yaml" {
		t.Errorf("Mappings = %q", c.Mappings)
	}
	if c.ESQL.UnmappedFields != "nullify" {
		t.Errorf("UnmappedFields = %q", c.ESQL.UnmappedFields)
	}
}

func TestLoad_BadExclusionPattern(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vern.yaml")
	if err := os.WriteFile(path, []byte("exclusions:\n  service_patterns: [\"^ok-\", \"(\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), `"("`) {
		t.Fatalf("err = %v, want it to name the pattern", err)
	}
}

func TestLoad_GoodExclusionPattern(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vern.yaml")
	if err := os.WriteFile(path, []byte("exclusions:\n  services: [a]\n  service_patterns: [\"^test-\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}
