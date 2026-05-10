package dashboard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ericmustin/vern/internal/config"
)

func TestGenerate_AllObjectsValidJSON(t *testing.T) {
	cfg := &config.Config{}
	cfg.ESQL.ResultIndex = "instrumentation-score-results"

	out, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) < 7 {
		t.Fatalf("want at least 7 NDJSON lines, got %d", len(lines))
	}

	typeCounts := map[string]int{}
	for i, line := range lines {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d not valid JSON: %v\n%s", i, err, line)
		}
		typeCounts[obj["type"].(string)]++
	}

	if typeCounts["index-pattern"] != 1 {
		t.Errorf("want 1 index-pattern, got %d", typeCounts["index-pattern"])
	}
	if typeCounts["search"] < 2 {
		t.Errorf("want at least 2 saved searches, got %d", typeCounts["search"])
	}
	if typeCounts["lens"] < 4 {
		t.Errorf("want at least 4 lens objects, got %d", typeCounts["lens"])
	}
	if typeCounts["dashboard"] != 2 {
		t.Errorf("want 2 dashboards, got %d", typeCounts["dashboard"])
	}
}

func TestGenerate_SubstitutesIndexPattern(t *testing.T) {
	cfg := &config.Config{}
	cfg.ESQL.ResultIndex = "my-custom-index"

	out, err := Generate(cfg)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"title":"my-custom-index*"`) {
		t.Errorf("expected my-custom-index* in data view title")
	}
	if !strings.Contains(s, `FROM my-custom-index`) {
		t.Errorf("expected Lens ES|QL to use my-custom-index")
	}
	if strings.Contains(s, "instrumentation-score-results") {
		t.Errorf("expected no hardcoded default result index in custom dashboard output")
	}
}

func TestGenerate_PreservesURLFormatterTemplate(t *testing.T) {
	cfg := &config.Config{}
	cfg.ESQL.ResultIndex = "instrumentation-score-results"

	out, _ := Generate(cfg)
	s := string(out)
	if !strings.Contains(s, `{{value}}.md`) {
		t.Error("expected {{value}}.md placeholder in field formatter URL")
	}
	if !strings.Contains(s, "https://github.com/instrumentation-score/spec/blob/main/rules") {
		t.Error("expected spec base URL in field formatter")
	}
}

func TestGenerate_DrillDownDashboardHasServiceControl(t *testing.T) {
	cfg := &config.Config{}
	cfg.ESQL.ResultIndex = "instrumentation-score-results"

	out, _ := Generate(cfg)
	s := string(out)
	// control field uses .keyword sub-field for aggregatable values
	if !strings.Contains(s, `service.name.keyword`) {
		t.Error("expected service.name.keyword field in drill-down control")
	}
	if !strings.Contains(s, "controlGroupInput") {
		t.Error("expected controlGroupInput on drill-down dashboard")
	}
}

func TestGenerate_OverviewHasDrilldownToService(t *testing.T) {
	cfg := &config.Config{}
	cfg.ESQL.ResultIndex = "instrumentation-score-results"

	out, _ := Generate(cfg)
	s := string(out)
	if !strings.Contains(s, "DASHBOARD_TO_DASHBOARD_DRILLDOWN") {
		t.Error("expected drilldown action on overview")
	}
	if !strings.Contains(s, "vern-drill-to-service") {
		t.Error("expected drilldown event id")
	}
}

func TestGenerate_LensObjectsHaveMigrationVersions(t *testing.T) {
	cfg := &config.Config{}
	cfg.ESQL.ResultIndex = "instrumentation-score-results"

	out, _ := Generate(cfg)
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		var obj map[string]interface{}
		json.Unmarshal([]byte(line), &obj)
		if obj["type"] != "lens" {
			continue
		}
		if obj["typeMigrationVersion"] == nil {
			t.Errorf("lens %s missing typeMigrationVersion", obj["id"])
		}
		if obj["coreMigrationVersion"] == nil {
			t.Errorf("lens %s missing coreMigrationVersion", obj["id"])
		}
	}
}
