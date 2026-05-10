package dashboard

// SavedObject is the wrapper Kibana expects in NDJSON imports.
type SavedObject struct {
	ID                   string                 `json:"id"`
	Type                 string                 `json:"type"`
	CoreMigrationVersion string                 `json:"coreMigrationVersion,omitempty"`
	TypeMigrationVersion string                 `json:"typeMigrationVersion,omitempty"`
	Managed              bool                   `json:"managed"`
	Attributes           map[string]interface{} `json:"attributes"`
	References           []Reference            `json:"references"`
}

type Reference struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// Migration versions discovered by exporting an existing Lens object from
// the cluster and confirming roundtrip through /api/saved_objects/_import.
// If Kibana version diverges these may need to bump.
const (
	coreMigrationVersion     = "8.8.0"
	lensTypeMigrationVersion = "10.1.0"
)

// Saved object IDs that the dashboards reference. Kept stable so re-imports
// overwrite the same objects (idempotent).
const (
	dataViewID     = "vern-instrumentation-score"
	overviewID     = "vern-overview"
	drilldownID    = "vern-drilldown"
	searchTotals   = "vern-search-rules" // typo-safe constants
	searchTotalsID = "vern-search-totals"
	searchRulesID  = "vern-search-rules"

	lensOverviewTable    = "vern-lens-overview-table"
	lensOverviewPie      = "vern-lens-overview-pie"
	lensOverviewAvgScore = "vern-lens-overview-avg-score"
	lensOverviewSvcCount = "vern-lens-overview-svc-count"
	lensDrilldownScore   = "vern-lens-drilldown-score"
	lensDrilldownPie     = "vern-lens-drilldown-pie"
)
