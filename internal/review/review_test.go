package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_DetectsMissingRulesDir(t *testing.T) {
	dir := t.TempDir()
	mappingsPath := filepath.Join(dir, "mappings.yaml")
	writeFile(t, mappingsPath, minimalMappings("Critical", "Resource"))
	configPath := filepath.Join(dir, "vern.yaml")
	writeFile(t, configPath, minimalConfig(filepath.Join(dir, "missing"), mappingsPath, "custom-score-results"))

	report, err := Run(context.Background(), Options{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Counts["errors"] == 0 {
		t.Fatalf("expected missing rules_dir error, got report: %+v", report)
	}
}

func TestRun_DetectsSpecMismatch(t *testing.T) {
	dir := t.TempDir()
	rulesDir := filepath.Join(dir, "rules")
	if err := os.Mkdir(rulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(rulesDir, "RES-005.md"), specRule("RES-005", "Critical", "Resource"))
	mappingsPath := filepath.Join(dir, "mappings.yaml")
	writeFile(t, mappingsPath, minimalMappings("Normal", "Resource"))
	configPath := filepath.Join(dir, "vern.yaml")
	writeFile(t, configPath, minimalConfig(rulesDir, mappingsPath, "custom-score-results"))

	report, err := Run(context.Background(), Options{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Counts["errors"] == 0 {
		t.Fatalf("expected mismatch error")
	}
	if !hasIssue(report, "impact mismatch") {
		t.Fatalf("expected impact mismatch issue, got %+v", report.Issues)
	}
}

func TestRun_CustomResultIndexArtifactsPass(t *testing.T) {
	dir := t.TempDir()
	rulesDir := filepath.Join(dir, "rules")
	if err := os.Mkdir(rulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(rulesDir, "RES-005.md"), specRule("RES-005", "Critical", "Resource"))
	writeFile(t, filepath.Join(rulesDir, "SDK-001.md"), specRule("SDK-001", "Low", "SDK"))
	mappingsPath := filepath.Join(dir, "mappings.yaml")
	writeFile(t, mappingsPath, minimalMappings("Critical", "Resource"))
	configPath := filepath.Join(dir, "vern.yaml")
	writeFile(t, configPath, minimalConfig(rulesDir, mappingsPath, "custom-score-results"))

	report, err := Run(context.Background(), Options{ConfigPath: configPath})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Counts["errors"] != 0 {
		t.Fatalf("expected no errors, got %+v", report.Issues)
	}
	if report.Counts["warnings"] == 0 {
		t.Fatalf("expected partial coverage warning")
	}
	if !report.Coverage.PartialScore || len(report.Coverage.MissingRules) != 1 || report.Coverage.MissingRules[0] != "SDK-001" {
		t.Fatalf("unexpected coverage: %+v", report.Coverage)
	}
	for _, artifact := range report.Artifacts {
		if !artifact.Passed {
			t.Fatalf("expected artifact check to pass: %+v", artifact)
		}
	}
}

func TestRun_StrictCoverageFailsPartialScore(t *testing.T) {
	dir := t.TempDir()
	rulesDir := filepath.Join(dir, "rules")
	if err := os.Mkdir(rulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(rulesDir, "RES-005.md"), specRule("RES-005", "Critical", "Resource"))
	writeFile(t, filepath.Join(rulesDir, "SDK-001.md"), specRule("SDK-001", "Low", "SDK"))
	mappingsPath := filepath.Join(dir, "mappings.yaml")
	writeFile(t, mappingsPath, minimalMappings("Critical", "Resource"))
	configPath := filepath.Join(dir, "vern.yaml")
	writeFile(t, configPath, minimalConfig(rulesDir, mappingsPath, "custom-score-results"))

	report, err := Run(context.Background(), Options{ConfigPath: configPath, StrictCoverage: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Counts["errors"] == 0 {
		t.Fatalf("expected strict coverage error")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func specRule(id, impact, target string) string {
	return "**Rule ID:** " + id + "\n\n" +
		"**Description:** test rule\n\n" +
		"**Target:** " + target + "\n\n" +
		"**Impact:** " + impact + "\n"
}

func minimalConfig(rulesDir, mappingsPath, resultIndex string) string {
	return "backend: esql\n" +
		"rules_dir: " + rulesDir + "\n" +
		"mappings: " + mappingsPath + "\n" +
		"format: elastic\n" +
		"esql:\n" +
		"  time_window: 1h\n" +
		"  score_lookback: 2h\n" +
		"  result_index: " + resultIndex + "\n" +
		"  annotations_index: custom-annotations\n" +
		"  schedule: 1h\n" +
		"  index_patterns:\n" +
		"    traces: custom-traces-*\n" +
		"    logs: custom-logs-*\n" +
		"    metrics: custom-metrics-*\n"
}

func minimalMappings(impact, target string) string {
	return "backend: esql\n" +
		"version: \"test\"\n" +
		"spec_version: \"0.1\"\n" +
		"rules:\n" +
		"  - spec_rule_id: RES-005\n" +
		"    enabled: true\n" +
		"    description: service.name is present\n" +
		"    impact: " + impact + "\n" +
		"    target: " + target + "\n" +
		"    query: |\n" +
		"      FROM {{ .Indices.Traces }}\n" +
		"      | KEEP rule_passed = true, `service.name`, example = \"\", extent = 0.0\n"
}

func hasIssue(report *Report, needle string) bool {
	for _, issue := range report.Issues {
		if strings.Contains(issue.Message, needle) {
			return true
		}
	}
	return false
}
