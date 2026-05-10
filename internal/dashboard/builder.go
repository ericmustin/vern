package dashboard

import (
	"encoding/json"
	"fmt"

	"github.com/ericmustin/vern/internal/coverage"
)

// builder holds parameters used by every saved object factory.
type builder struct {
	resultIndex  string
	indexPattern string
	specBaseURL  string
	coverage     *coverage.Summary
}

// jsonString serializes v into a JSON string. Several Kibana fields
// (kibanaSavedObjectMeta.searchSourceJSON, panelsJSON, controlGroupInput.panelsJSON,
// fieldFormatMap, etc.) hold *stringified* JSON, not nested objects.
func jsonString(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Errorf("dashboard: marshal stringified JSON: %w", err))
	}
	return string(b)
}

// scorePalette is shared between the score column in the table and the
// score metric panel. Discrete stops at 50/75/90/100 mirror the spec's
// Poor/Needs Improvement/Good/Excellent thresholds.
var scorePalette = map[string]interface{}{
	"name": "custom",
	"type": "palette",
	"params": map[string]interface{}{
		"name":  "custom",
		"steps": 5,
		"stops": []map[string]interface{}{
			{"color": "#bd271e", "stop": 50},  // red    — Poor (<50)
			{"color": "#f5a700", "stop": 75},  // amber  — Needs Improvement (50-74)
			{"color": "#79aad9", "stop": 90},  // blue   — Good (75-89)
			{"color": "#017d73", "stop": 100}, // green  — Excellent (≥90)
		},
		"rangeMin":   0,
		"rangeMax":   100,
		"continuity": "all",
		"rangeType":  "number",
	},
}

// (passFailPalette removed; pie charts use Lens's default categorical palette
// to avoid color-mapping schema mismatches across Kibana minor versions. The
// palette can be customised in Kibana → Lens after first render.)

func (b *builder) buildAll() []SavedObject {
	return []SavedObject{
		b.dataView(),
		b.savedSearchTotals(),
		b.savedSearchRules(),
		b.lensOverviewTable(),
		b.lensPiePassFail(lensOverviewPie, "Vern: Pass / Fail counts (all services)", ""),
		b.lensAvgScore(),
		b.lensSvcCount(),
		b.lensDrilldownScore(),
		b.lensPiePassFail(lensDrilldownPie, "Vern: Pass / Fail (selected service)", ""),
		b.overviewDashboard(),
		b.drilldownDashboard(),
	}
}

func (b *builder) dataView() SavedObject {
	formatMap := map[string]interface{}{
		"rule_id": map[string]interface{}{
			"id": "url",
			"params": map[string]interface{}{
				"urlTemplate":          b.specBaseURL + "/{{value}}.md",
				"labelTemplate":        "{{value}}",
				"openLinkInCurrentTab": false,
			},
		},
		"score":  map[string]interface{}{"id": "number", "params": map[string]string{"pattern": "0.[00]"}},
		"extent": map[string]interface{}{"id": "percent", "params": map[string]string{"pattern": "0.[00]%"}},
	}
	return SavedObject{
		ID:   dataViewID,
		Type: "index-pattern",
		Attributes: map[string]interface{}{
			"title":          b.indexPattern,
			"name":           "Vern: Instrumentation Score Results",
			"timeFieldName":  "evaluated_at",
			"fieldFormatMap": jsonString(formatMap),
		},
		References: []Reference{},
	}
}

func (b *builder) savedSearch(id, title, description, query string, columns []string, sortField, sortDir string) SavedObject {
	searchSource := map[string]interface{}{
		"query":        map[string]string{"language": "kuery", "query": query},
		"filter":       []interface{}{},
		"indexRefName": "kibanaSavedObjectMeta.searchSourceJSON.index",
	}
	return SavedObject{
		ID:   id,
		Type: "search",
		Attributes: map[string]interface{}{
			"title":       title,
			"description": description,
			"columns":     columns,
			"sort":        [][]string{{sortField, sortDir}},
			"kibanaSavedObjectMeta": map[string]interface{}{
				"searchSourceJSON": jsonString(searchSource),
			},
		},
		References: []Reference{
			{ID: dataViewID, Name: "kibanaSavedObjectMeta.searchSourceJSON.index", Type: "index-pattern"},
		},
	}
}

func (b *builder) savedSearchTotals() SavedObject {
	return b.savedSearch(searchTotalsID,
		"Vern: Service Scores",
		"Per-service total scores from the latest workflow run.",
		`rule_id:"_TOTAL"`,
		[]string{"service.name", "score", "category", "critical_passed", "critical_total", "important_passed", "important_total", "normal_passed", "normal_total", "low_passed", "low_total", "evaluated_at"},
		"score", "desc")
}

func (b *builder) savedSearchRules() SavedObject {
	return b.savedSearch(searchRulesID,
		"Vern: Per-Rule Breakdown",
		"Per-rule pass/fail evidence per service. rule_id links to the upstream spec.",
		`NOT rule_id:_TOTAL AND NOT rule_id:_BOOTSTRAP AND NOT rule_id:_COVERAGE`,
		[]string{"rule_id", "impact", "target", "rule_passed", "extent", "example", "description", "evaluated_at"},
		"evaluated_at", "desc")
}

// lensFromESQL constructs a Lens saved object using a textBased (ES|QL) layer.
// `cols` defines columns visible to Lens — each is {columnId, fieldName, meta}.
// `viz` is the visualizationType-specific block written under state.visualization.
func (b *builder) lensFromESQL(id, title, vizType, esql string, cols []map[string]interface{}, viz map[string]interface{}) SavedObject {
	const layerID = "l1"
	return SavedObject{
		ID:                   id,
		Type:                 "lens",
		CoreMigrationVersion: coreMigrationVersion,
		TypeMigrationVersion: lensTypeMigrationVersion,
		Attributes: map[string]interface{}{
			"title":             title,
			"visualizationType": vizType,
			"state": map[string]interface{}{
				"datasourceStates": map[string]interface{}{
					"textBased": map[string]interface{}{
						"layers": map[string]interface{}{
							layerID: map[string]interface{}{
								"columns":   cols,
								"index":     dataViewID,
								"query":     map[string]string{"esql": esql},
								"timeField": "evaluated_at",
							},
						},
					},
				},
				"filters":       []interface{}{},
				"query":         map[string]string{"language": "kuery", "query": ""},
				"visualization": viz,
			},
		},
		References: []Reference{
			{ID: dataViewID, Name: "indexpattern-datasource-layer-" + layerID, Type: "index-pattern"},
		},
	}
}

// col is a small helper for lens columns.
func col(id, field, metaType string) map[string]interface{} {
	return map[string]interface{}{
		"columnId":  id,
		"fieldName": field,
		"meta":      map[string]string{"type": metaType},
	}
}

func (b *builder) lensOverviewTable() SavedObject {
	cols := []map[string]interface{}{
		col("service.name", "service.name", "string"),
		col("score", "score", "number"),
		col("category", "category", "string"),
	}
	viz := map[string]interface{}{
		"columns": []map[string]interface{}{
			{"columnId": "service.name", "isTransposed": false},
			{"columnId": "score", "alignment": "right", "colorMode": "cell", "palette": scorePalette},
			{"columnId": "category", "isTransposed": false},
		},
		"layerId":        "l1",
		"layerType":      "data",
		"rowHeight":      "single",
		"rowHeightLines": 1,
	}
	// score::double cast handles both proper-typed and text-typed fields
	// (older data streams created before the type-clean bootstrap may have
	// score indexed as text). category likewise via TO_STRING for safety.
	return b.lensFromESQL(lensOverviewTable, "Vern: Service Scores",
		"lnsDatatable",
		fmt.Sprintf(`FROM %s | WHERE rule_id == "_TOTAL" | EVAL score = score::double | KEEP service.name, score, category | SORT score DESC | LIMIT 100`, b.resultIndex),
		cols, viz)
}

func (b *builder) lensPiePassFail(id, title, extraWhere string) SavedObject {
	where := `WHERE NOT (rule_id IN ("_BOOTSTRAP", "_TOTAL", "_COVERAGE"))`
	if extraWhere != "" {
		where += " AND " + extraWhere
	}
	// Convert rule_passed to a stable string label so the pie has nice slice
	// names (and is robust to the field being either boolean or coerced from
	// a string). Pass = green-ish, Fail = red-ish via Lens default palette
	// once the user customizes; default categorical colors are applied
	// automatically.
	cols := []map[string]interface{}{
		col("status", "status", "string"),
		col("n", "n", "number"),
	}
	viz := map[string]interface{}{
		"shape": "pie",
		"layers": []map[string]interface{}{{
			"layerId":         "l1",
			"layerType":       "data",
			"primaryGroups":   []string{"status"},
			"metrics":         []string{"n"},
			"categoryDisplay": "default",
			"legendDisplay":   "default",
			"numberDisplay":   "value",
			"nestedLegend":    false,
		}},
	}
	return b.lensFromESQL(id, title, "lnsPie",
		fmt.Sprintf(`FROM %s | %s | EVAL status = CASE(rule_passed == true, "Pass", "Fail") | STATS n = COUNT(*) BY status`, b.resultIndex, where),
		cols, viz)
}

func (b *builder) lensAvgScore() SavedObject {
	cols := []map[string]interface{}{
		col("avg_score", "avg_score", "number"),
	}
	viz := map[string]interface{}{
		"layerId":        "l1",
		"layerType":      "data",
		"metricAccessor": "avg_score",
		"color":          "#017d73",
		"palette":        scorePalette,
		"subtitle":       "Mean across all services",
	}
	return b.lensFromESQL(lensOverviewAvgScore, "Vern: Average score",
		"lnsMetric",
		fmt.Sprintf(`FROM %s | WHERE rule_id == "_TOTAL" | EVAL s = score::double | STATS avg_score = ROUND(AVG(s), 1)`, b.resultIndex),
		cols, viz)
}

func (b *builder) lensSvcCount() SavedObject {
	cols := []map[string]interface{}{
		col("services", "services", "number"),
	}
	viz := map[string]interface{}{
		"layerId":        "l1",
		"layerType":      "data",
		"metricAccessor": "services",
		"subtitle":       "Services scored",
		"color":          "#0077cc",
	}
	return b.lensFromESQL(lensOverviewSvcCount, "Vern: Services scored",
		"lnsMetric",
		fmt.Sprintf(`FROM %s | WHERE rule_id == "_TOTAL" | STATS services = COUNT(*)`, b.resultIndex),
		cols, viz)
}

func (b *builder) lensDrilldownScore() SavedObject {
	cols := []map[string]interface{}{
		col("score", "score", "number"),
	}
	viz := map[string]interface{}{
		"layerId":        "l1",
		"layerType":      "data",
		"metricAccessor": "score",
		"color":          "#017d73",
		"palette":        scorePalette,
		"subtitle":       "Selected service",
	}
	return b.lensFromESQL(lensDrilldownScore, "Vern: Selected service score",
		"lnsMetric",
		fmt.Sprintf(`FROM %s | WHERE rule_id == "_TOTAL" | EVAL score = score::double | KEEP score | LIMIT 1`, b.resultIndex),
		cols, viz)
}
