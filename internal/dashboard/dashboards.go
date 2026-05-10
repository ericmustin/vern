package dashboard

// markdownPanel embeds a markdown visualization inline. We use this for
// dashboard headers (small explanatory blocks) since it doesn't require
// a separate saved object.
func markdownPanel(panelIndex string, gridX, gridY, gridW, gridH int, body string) map[string]interface{} {
	return map[string]interface{}{
		"version":    "8.0.0",
		"type":       "visualization",
		"gridData":   map[string]interface{}{"x": gridX, "y": gridY, "w": gridW, "h": gridH, "i": panelIndex},
		"panelIndex": panelIndex,
		"embeddableConfig": map[string]interface{}{
			"savedVis": map[string]interface{}{
				"title":       "",
				"description": "",
				"type":        "markdown",
				"params": map[string]interface{}{
					"fontSize":          12,
					"openLinksInNewTab": true,
					"markdown":          body,
				},
				"uiState": map[string]interface{}{},
				"data": map[string]interface{}{
					"aggs": []interface{}{},
					"searchSource": map[string]interface{}{
						"query":  map[string]string{"query": "", "language": "kuery"},
						"filter": []interface{}{},
					},
				},
			},
		},
	}
}

// referencedPanel renders a saved object (search/lens) inline by reference.
// `extraEmbeddable` lets a caller add enhancements (e.g. drilldowns).
func referencedPanel(panelIndex, panelType, title string, gridX, gridY, gridW, gridH int, extraEmbeddable map[string]interface{}) map[string]interface{} {
	embed := map[string]interface{}{"enhancements": map[string]interface{}{}}
	for k, v := range extraEmbeddable {
		embed[k] = v
	}
	return map[string]interface{}{
		"version":          "8.0.0",
		"type":             panelType,
		"gridData":         map[string]interface{}{"x": gridX, "y": gridY, "w": gridW, "h": gridH, "i": panelIndex},
		"panelIndex":       panelIndex,
		"title":            title,
		"embeddableConfig": embed,
		"panelRefName":     "panel_" + panelIndex,
	}
}

func (b *builder) overviewDashboard() SavedObject {
	const drilldownEvent = "vern-drill-to-service"

	header := markdownPanel("md", 0, 0, 48, 4,
		"## Vern — Instrumentation Score\n\n"+
			"Partial score leaderboard from the latest workflow run. **Click a service.name in the table → \"Open Service Drill-down\"** to inspect that service. "+
			"Click any column header to sort.\n\nSpec: https://github.com/instrumentation-score/spec")

	avgScore := referencedPanel("metric-avg", "lens", "Average score", 0, 4, 16, 8, nil)
	svcCount := referencedPanel("metric-svc", "lens", "Services scored", 16, 4, 16, 8, nil)
	pie := referencedPanel("pie-passfail", "lens", "Rule pass / fail", 32, 4, 16, 8, nil)

	// Lens datatable with drilldown to drill-down dashboard.
	tableEmbed := map[string]interface{}{
		"enhancements": map[string]interface{}{
			"dynamicActions": map[string]interface{}{
				"events": []map[string]interface{}{{
					"eventId":  drilldownEvent,
					"triggers": []string{"FILTER_TRIGGER"},
					"action": map[string]interface{}{
						"factoryId": "DASHBOARD_TO_DASHBOARD_DRILLDOWN",
						"name":      "Open Service Drill-down",
						"config": map[string]interface{}{
							"openInNewTab":        false,
							"useCurrentDateRange": true,
							"useCurrentFilters":   true,
						},
					},
				}},
			},
		},
	}
	table := referencedPanel("table-scores", "lens", "Service scores", 0, 12, 48, 18, tableEmbed)

	// Discover-style detail row (still useful for browsing all rule rows).
	detail := referencedPanel("search-rules", "search", "All rule evidence (sortable)", 0, 30, 48, 14, nil)

	panels := []map[string]interface{}{header, avgScore, svcCount, pie, table, detail}

	return SavedObject{
		ID:   overviewID,
		Type: "dashboard",
		Attributes: map[string]interface{}{
			"version":     1,
			"title":       "Vern — Instrumentation Score Overview",
			"description": "Per-service partial score leaderboard. Click a service.name in the score table → Open Service Drill-down.",
			"timeRestore": false,
			"panelsJSON":  jsonString(panels),
			"optionsJSON": jsonString(map[string]interface{}{"useMargins": true, "hidePanelTitles": false}),
			"kibanaSavedObjectMeta": map[string]interface{}{
				"searchSourceJSON": jsonString(map[string]interface{}{
					"query":  map[string]string{"language": "kuery", "query": ""},
					"filter": []interface{}{},
				}),
			},
		},
		References: []Reference{
			{ID: lensOverviewAvgScore, Name: "metric-avg:panel_metric-avg", Type: "lens"},
			{ID: lensOverviewSvcCount, Name: "metric-svc:panel_metric-svc", Type: "lens"},
			{ID: lensOverviewPie, Name: "pie-passfail:panel_pie-passfail", Type: "lens"},
			{ID: lensOverviewTable, Name: "table-scores:panel_table-scores", Type: "lens"},
			{ID: searchRulesID, Name: "search-rules:panel_search-rules", Type: "search"},
			{ID: drilldownID, Name: "table-scores:drilldown:DASHBOARD_TO_DASHBOARD_DRILLDOWN:" + drilldownEvent + ":dashboardId", Type: "dashboard"},
		},
	}
}

func (b *builder) drilldownDashboard() SavedObject {
	const ctlID = "svc-control"

	control := map[string]interface{}{
		ctlID: map[string]interface{}{
			"explicitInput": map[string]interface{}{
				"dataViewId":   dataViewID,
				"enhancements": map[string]interface{}{},
				"fieldName":    "service.name.keyword",
				"id":           ctlID,
				"title":        "Service",
				"singleSelect": true,
			},
			"grow":  true,
			"order": 0,
			"type":  "optionsListControl",
			"width": "medium",
		},
	}
	controlGroup := map[string]interface{}{
		"chainingSystem":           "HIERARCHICAL",
		"controlStyle":             "oneLine",
		"ignoreParentSettingsJSON": jsonString(map[string]bool{"ignoreFilters": false, "ignoreQuery": false, "ignoreTimerange": false, "ignoreValidations": false}),
		"panelsJSON":               jsonString(control),
	}

	header := markdownPanel("md", 0, 0, 48, 4,
		"### Service drill-down\n\n"+
			"Pick a service in the **Service** control above. The partial score gauge, pass/fail pie, and per-rule breakdown table all filter together. "+
			"Click any **rule_id** in the breakdown to open the upstream spec.")

	score := referencedPanel("metric-score", "lens", "Score", 0, 4, 24, 8, nil)
	pie := referencedPanel("pie-passfail", "lens", "Pass / fail", 24, 4, 24, 8, nil)

	// Per-rule breakdown stays a saved search (sortable Discover, rule_id URL formatter).
	rules := referencedPanel("search-rules", "search", "Per-rule breakdown", 0, 12, 48, 18, nil)

	panels := []map[string]interface{}{header, score, pie, rules}

	return SavedObject{
		ID:   drilldownID,
		Type: "dashboard",
		Attributes: map[string]interface{}{
			"version":           1,
			"title":             "Vern — Service Drill-down",
			"description":       "Select a service to see its partial instrumentation score and per-rule pass/fail evidence.",
			"timeRestore":       false,
			"controlGroupInput": controlGroup,
			"panelsJSON":        jsonString(panels),
			"optionsJSON":       jsonString(map[string]interface{}{"useMargins": true, "hidePanelTitles": false}),
			"kibanaSavedObjectMeta": map[string]interface{}{
				"searchSourceJSON": jsonString(map[string]interface{}{
					"query":  map[string]string{"language": "kuery", "query": ""},
					"filter": []interface{}{},
				}),
			},
		},
		References: []Reference{
			{ID: dataViewID, Name: "controlGroup_" + ctlID + ":optionsListDataView", Type: "index-pattern"},
			{ID: lensDrilldownScore, Name: "metric-score:panel_metric-score", Type: "lens"},
			{ID: lensDrilldownPie, Name: "pie-passfail:panel_pie-passfail", Type: "lens"},
			{ID: searchRulesID, Name: "search-rules:panel_search-rules", Type: "search"},
		},
	}
}

// generate calls Generate with the configured builder.
func (g *builder) ndjson() ([]byte, error) {
	objects := g.buildAll()
	var buf []byte
	for i, o := range objects {
		b, err := jsonMarshal(o)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			buf = append(buf, '\n')
		}
		buf = append(buf, b...)
	}
	buf = append(buf, '\n')
	return buf, nil
}

// jsonMarshal is a tiny indirection so tests can verify roundtrip.
func jsonMarshal(v interface{}) ([]byte, error) {
	return jsonMarshalNoEscape(v)
}

// (jsonMarshalNoEscape is defined in generator.go.)
