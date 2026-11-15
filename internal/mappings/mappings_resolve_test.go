package mappings_test

import (
	"strings"
	"testing"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/mappings"
)

// TestMappingsFilesLoadAndResolve loads each vendored mappings file and
// resolves every rule template against a default config. Catches template
// syntax mistakes (unbalanced delimiters, missing struct fields) before
// they reach a live backend.
func TestMappingsFilesLoadAndResolve(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		backend string
	}{
		{"esql", "../../configs/esql-mappings.yaml", "esql"},
		{"clickhouse", "../../configs/clickhouse-mappings.yaml", "clickhouse"},
		{"opensearch", "../../configs/opensearch-mappings.yaml", "opensearch"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mf, err := mappings.Load(tc.path)
			if err != nil {
				t.Fatalf("load %s: %v", tc.path, err)
			}
			if mf.Backend != tc.backend {
				t.Errorf("backend = %q, want %q", mf.Backend, tc.backend)
			}
			cfg := &config.Config{Backend: tc.backend}
			cfg.ApplyDefaults()

			data := mappings.SemconvCatalog()
			resolved, err := mappings.ResolveWithData(mf.Rules, cfg, data)
			if err != nil {
				t.Fatalf("resolve %s: %v", tc.path, err)
			}
			if len(resolved.Rules) == 0 {
				t.Fatalf("no resolved rules in %s", tc.path)
			}
			// The resolved query should still contain the expected output
			// projection (rule_passed appears in every backend's KEEP/FIELDS).
			for _, r := range resolved.Rules {
				if !strings.Contains(r.ResolvedQuery, "rule_passed") {
					t.Errorf("%s: resolved query missing rule_passed:\n%s", r.SpecRuleID, r.ResolvedQuery)
				}
			}
		})
	}
}

// TestESQLMappingsCoverEverySpecRule asserts every vendored spec rule has an
// ES|QL mapping with a matching impact.
func TestESQLMappingsCoverEverySpecRule(t *testing.T) {
	specRules, err := coverage.LoadSpecRules("../../spec/rules")
	if err != nil {
		t.Fatalf("LoadSpecRules: %v", err)
	}
	mf, err := mappings.Load("../../configs/esql-mappings.yaml")
	if err != nil {
		t.Fatalf("load mappings: %v", err)
	}
	byID := map[string]mappings.RuleMapping{}
	for _, r := range mf.Rules {
		byID[r.SpecRuleID] = r
	}
	for _, sr := range specRules {
		m, ok := byID[sr.ID]
		if !ok {
			t.Errorf("spec rule %s has no ES|QL mapping", sr.ID)
			continue
		}
		if !strings.EqualFold(m.Impact, sr.Impact) {
			t.Errorf("%s: impact %q != spec %q", sr.ID, m.Impact, sr.Impact)
		}
	}
}

func resolveESQLForTest(t *testing.T) (*mappings.MappingsFile, *mappings.ResolveResult) {
	t.Helper()
	mf, err := mappings.Load("../../configs/esql-mappings.yaml")
	if err != nil {
		t.Fatalf("load mappings: %v", err)
	}
	cfg := &config.Config{Backend: "esql"}
	cfg.ApplyDefaults()
	res, err := mappings.ResolveWithData(mf.Rules, cfg, mappings.SemconvCatalog())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return mf, res
}

func resolvedQueryFor(t *testing.T, res *mappings.ResolveResult, id string) string {
	t.Helper()
	for _, r := range res.Rules {
		if r.SpecRuleID == id {
			return r.ResolvedQuery
		}
	}
	t.Fatalf("rule %s not resolved", id)
	return ""
}

func TestESQLMappingsMetadata(t *testing.T) {
	mf, res := resolveESQLForTest(t)
	for _, r := range mf.Rules {
		if strings.Contains(r.Query, "METADATA _id") {
			t.Errorf("%s: query still uses METADATA _id", r.SpecRuleID)
		}
		if r.IsAggregation || !r.Enabled {
			continue
		}
		switch r.Signal {
		case "traces", "logs", "metrics", "any":
		default:
			t.Errorf("%s: signal = %q, want traces|logs|metrics|any", r.SpecRuleID, r.Signal)
		}
		if strings.TrimSpace(r.Remediation) == "" {
			t.Errorf("%s: missing remediation", r.SpecRuleID)
		}
	}
	for _, r := range res.Rules {
		if strings.Contains(r.ResolvedQuery, "METADATA _id") {
			t.Errorf("%s: resolved query contains METADATA _id", r.SpecRuleID)
		}
		if strings.Contains(r.ResolvedQuery, `$"`) {
			t.Errorf("%s: resolved query contains a literal $ before closing quote", r.SpecRuleID)
		}
	}
}

func TestESQLMappingsMET005Pattern(t *testing.T) {
	_, res := resolveESQLForTest(t)
	q := resolvedQueryFor(t, res, "MET-005")
	if !strings.Contains(q, `([._].*)?"`) {
		t.Errorf("MET-005 should allow a trailing suffix:\n%s", q)
	}
	if strings.Contains(q, `)$"`) {
		t.Errorf("MET-005 must not end with a literal $:\n%s", q)
	}
}

func TestESQLMappingsResourceRulesReadAllSignals(t *testing.T) {
	_, res := resolveESQLForTest(t)
	for _, id := range []string{"RES-001", "RES-002", "RES-005"} {
		q := resolvedQueryFor(t, res, id)
		for _, want := range []string{"logs-*.otel-*", "metrics-*.otel-*"} {
			if !strings.Contains(q, want) {
				t.Errorf("%s: resolved query missing %s", id, want)
			}
		}
	}
}

func resolveESQLWithSampleMode(t *testing.T, sample bool) *mappings.ResolveResult {
	t.Helper()
	mf, err := mappings.Load("../../configs/esql-mappings.yaml")
	if err != nil {
		t.Fatalf("load mappings: %v", err)
	}
	cfg := &config.Config{Backend: "esql"}
	cfg.ApplyDefaults()
	data := mappings.SemconvCatalog()
	data.SampleMode = sample
	res, err := mappings.ResolveWithData(mf.Rules, cfg, data)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return res
}

func TestSPA002SampleModeExcludesServerSpans(t *testing.T) {
	const want = `NOT kind IN ("Server"`
	if q := resolvedQueryFor(t, resolveESQLWithSampleMode(t, true), "SPA-002"); !strings.Contains(q, want) {
		t.Errorf("SPA-002 in sample mode should contain %q:\n%s", want, q)
	}
	if q := resolvedQueryFor(t, resolveESQLWithSampleMode(t, false), "SPA-002"); strings.Contains(q, want) {
		t.Errorf("SPA-002 outside sample mode must not contain %q:\n%s", want, q)
	}
}

func TestRES004CoversResourceOnlyKeys(t *testing.T) {
	_, res := resolveESQLForTest(t)
	q := resolvedQueryFor(t, res, "RES-004")
	for _, want := range []string{
		"`attributes.service.version` IS NOT NULL",
		"`attributes.host.name` IS NOT NULL",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("RES-004 should contain %q:\n%s", want, q)
		}
	}
}

func TestLOG001HasMinObservation(t *testing.T) {
	mf, err := mappings.Load("../../configs/esql-mappings.yaml")
	if err != nil {
		t.Fatalf("load mappings: %v", err)
	}
	for _, r := range mf.Rules {
		if r.SpecRuleID == "LOG-001" {
			if r.MinObservation != "336h" {
				t.Errorf("LOG-001 MinObservation = %q, want 336h", r.MinObservation)
			}
			return
		}
	}
	t.Fatal("LOG-001 mapping not found")
}

// TestESQLMappingsLive955Fixes pins query fixes found by live validation on
// Elasticsearch 9.5.5 with the OTel collector's elasticsearch exporter
// (mapping mode otel).
func TestESQLMappingsLive955Fixes(t *testing.T) {
	_, res := resolveESQLForTest(t)
	// TS_INFO's unit column comes from mapping meta: null with the
	// collector's exporter, and set from the first document by the native
	// OTLP endpoint, so it is not per series. The per-series unit is the
	// `unit` dimension, and the column must not be a fallback.
	for _, id := range []string{"MET-002", "MET-003"} {
		q := resolvedQueryFor(t, res, id)
		if want := `GROK dimensions "\"unit\":\"%{DATA:dim_unit}\""`; !strings.Contains(q, want) {
			t.Errorf("%s: missing %q:\n%s", id, want, q)
		}
		if strings.Contains(q, "COALESCE(dim_unit, unit)") {
			t.Errorf("%s: must not fall back to TS_INFO's unit column:\n%s", id, q)
		}
	}
	// MET-003 compares units across services.
	if q := resolvedQueryFor(t, res, "MET-003"); !strings.Contains(q, "svcs = VALUES(svc) BY metric_name") || !strings.Contains(q, "| MV_EXPAND svcs") {
		t.Errorf("MET-003 should group units by metric name across services:\n%s", q)
	}
	// TS_INFO's metric_name is the field path `metrics.<name>`.
	if q := resolvedQueryFor(t, res, "MET-006"); !strings.Contains(q, `SUBSTRING(metric_name, 9)`) || !strings.Contains(q, "otel_name IN (") {
		t.Errorf("MET-006 should compare the name without the metrics. prefix:\n%s", q)
	}
	// TRIM/TO_LOWER return null (with a warning) on multi-valued input.
	if q := resolvedQueryFor(t, res, "RES-006"); !strings.Contains(q, "| MV_EXPAND c") {
		t.Errorf("RES-006 should expand multi-valued service.criticality:\n%s", q)
	}
}

func TestRES003PreconditionIsPodScoped(t *testing.T) {
	_, res := resolveESQLForTest(t)
	q := resolvedQueryFor(t, res, "RES-003")
	for _, want := range []string{"resource.attributes.k8s.pod.name", "resource.attributes.k8s.container.name"} {
		if !strings.Contains(q, want) {
			t.Errorf("RES-003 precondition should contain %q:\n%s", want, q)
		}
	}
	for _, bad := range []string{"k8s.deployment.name", "k8s.statefulset.name", "k8s.node.name"} {
		if strings.Contains(q, bad) {
			t.Errorf("RES-003 must not trigger on %q (not pod telemetry):\n%s", bad, q)
		}
	}
}

func TestSPA002ScopeFilterDoesNotShrinkSpanIDs(t *testing.T) {
	mf, err := mappings.Load("../../configs/esql-mappings.yaml")
	if err != nil {
		t.Fatalf("load mappings: %v", err)
	}
	cfg := &config.Config{Backend: "esql"}
	cfg.ApplyDefaults()
	cfg.Filters.ServiceNamespaces = []string{"a"}
	res, err := mappings.ResolveWithData(mf.Rules, cfg, mappings.SemconvCatalog())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	q := resolvedQueryFor(t, res, "SPA-002")
	const frag = `resource.attributes.service.namespace IN ("a")`

	from := strings.Index(q, "FROM ")
	firstEval := strings.Index(q, "| EVAL")
	if from < 0 || firstEval < from {
		t.Fatalf("SPA-002: cannot find FROM ... | EVAL:\n%s", q)
	}
	if strings.Contains(q[from:firstEval], frag) {
		t.Errorf("SPA-002: scope filter must not narrow the spans feeding ids:\n%s", q)
	}
	np := strings.Index(q, "needs_parent =")
	if np < 0 || !strings.Contains(q[np:], frag) {
		t.Errorf("SPA-002: scope filter should apply in needs_parent:\n%s", q)
	}
}

func resolveESQLAggregation(t *testing.T, mutate func([]mappings.RuleMapping)) string {
	t.Helper()
	mf, err := mappings.Load("../../configs/esql-mappings.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(mf.Rules)
	}
	cfg := &config.Config{}
	cfg.ApplyDefaults()
	res, err := mappings.ResolveWithData(mf.Rules, cfg, mappings.SemconvCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if res.Aggregation == nil {
		t.Fatal("no _SCORE_AGGREGATION")
	}
	return res.Aggregation.ResolvedQuery
}

// TestScoreAggregationExcludesAdvisoryRules: the workflow score must leave
// advisory rules out, as vern evaluate does.
func TestScoreAggregationExcludesAdvisoryRules(t *testing.T) {
	q := resolveESQLAggregation(t, nil)
	if !strings.Contains(q, `NOT (rule_id IN ("_BOOTSTRAP", "_TOTAL", "_COVERAGE", "SPA-003"))`) {
		t.Errorf("aggregation does not exclude SPA-003:\n%s", q)
	}
}

func TestScoreAggregationWithoutAdvisoryRulesIsValid(t *testing.T) {
	q := resolveESQLAggregation(t, func(rules []mappings.RuleMapping) {
		for i := range rules {
			rules[i].Advisory = false
		}
	})
	if !strings.Contains(q, `NOT (rule_id IN ("_BOOTSTRAP", "_TOTAL", "_COVERAGE"))`) {
		t.Errorf("aggregation exclusion list changed:\n%s", q)
	}
	for _, bad := range []string{"IN ()", ", )", ",)", "SPA-003"} {
		if strings.Contains(q, bad) {
			t.Errorf("aggregation contains %q:\n%s", bad, q)
		}
	}
}

// TestMappingsPolicyParity: advisory and heuristic are shared rule policy, so
// a rule mapped by several backends must carry the same flags in each file;
// otherwise vern compare reports drift that is policy, not data.
func TestMappingsPolicyParity(t *testing.T) {
	type policy struct {
		file                string
		advisory, heuristic bool
	}
	byRule := map[string][]policy{}
	for _, path := range []string{
		"../../configs/esql-mappings.yaml",
		"../../configs/clickhouse-mappings.yaml",
		"../../configs/opensearch-mappings.yaml",
	} {
		mf, err := mappings.Load(path)
		if err != nil {
			t.Fatalf("load %s: %v", path, err)
		}
		for _, r := range mf.Rules {
			byRule[r.SpecRuleID] = append(byRule[r.SpecRuleID], policy{path, r.Advisory, r.Heuristic})
		}
	}
	for id, ps := range byRule {
		for _, p := range ps[1:] {
			if p.advisory != ps[0].advisory || p.heuristic != ps[0].heuristic {
				t.Errorf("%s: %s has advisory=%v heuristic=%v, %s has advisory=%v heuristic=%v",
					id, ps[0].file, ps[0].advisory, ps[0].heuristic, p.file, p.advisory, p.heuristic)
			}
		}
	}
}

// TestESQLMappingsUnitAndDebugRules pins the unit rules (dimensionless "1",
// MET-003 compared across services) and the LOG-001 hour threshold.
func TestESQLMappingsUnitAndDebugRules(t *testing.T) {
	mf, res := resolveESQLForTest(t)
	// "1" is UCUM dimensionless and a valid, non-default unit.
	if q := resolvedQueryFor(t, res, "MET-002"); strings.Contains(q, `unit == "1"`) {
		t.Errorf("MET-002 must not fail unit \"1\":\n%s", q)
	}
	// DATE_DIFF("day") truncates: 14 d 23 h would pass. Compare hours.
	q := resolvedQueryFor(t, res, "LOG-001")
	for _, want := range []string{`DATE_DIFF("hour", first_debug, last_debug)`, "336"} {
		if !strings.Contains(q, want) {
			t.Errorf("LOG-001 missing %q:\n%s", want, q)
		}
	}
	dev := map[string]string{}
	for _, r := range mf.Rules {
		dev[r.SpecRuleID] = r.Deviation
	}
	if dev["MET-003"] != "" {
		t.Errorf("MET-003 checks units across services; remove its deviation %q", dev["MET-003"])
	}
	for id, want := range map[string]string{
		"MET-002": "UCUM validity is not checked; any non-empty unit passes.",
		"LOG-001": "In live mode (no sample) the 14-day observation requirement is not enforced",
	} {
		if !strings.Contains(dev[id], want) {
			t.Errorf("%s deviation = %q, want it to contain %q", id, dev[id], want)
		}
	}
}

// TestSDK001UsesESQLMultivalueFunctions: ES|QL has no [i] indexing; the
// major version must come from MV_FIRST(SPLIT(...)).
func TestSDK001UsesESQLMultivalueFunctions(t *testing.T) {
	mf, err := mappings.Load("../../configs/esql-mappings.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.ApplyDefaults()
	cfg.Filters.EnableSDKRules = true
	mappings.ApplyOptInGates(mf.Rules, cfg)
	res, err := mappings.ResolveWithData(mf.Rules, cfg, mappings.SemconvCatalog())
	if err != nil {
		t.Fatal(err)
	}
	q := resolvedQueryFor(t, res, "SDK-001")
	if strings.Contains(q, ")[0]") || !strings.Contains(q, "TO_INTEGER(MV_FIRST(SPLIT(") {
		t.Errorf("SDK-001 must take the major version with MV_FIRST(SPLIT(...)):\n%s", q)
	}
}

// sensitiveFields is a small discovered field set for the four sensitive-data
// rules; attributes.shared occurs in traces and logs.
func sensitiveFields() mappings.Fields {
	return mappings.Fields{
		Resource:    []string{"resource.attributes.service.name", "resource.attributes.process.command_args"},
		Scope:       []string{"scope.name", "scope.attributes.lib.token"},
		Span:        []string{"name", "status.message", "links.attributes.l", "attributes.shared", "attributes.http.request.header.authorization"},
		SpanEvent:   []string{"event_name", "attributes.shared", "attributes.exception.message"},
		Log:         []string{"body.text", "body.structured", "event_name", "attributes.shared", "attributes.exception.message"},
		Metric:      []string{"unit", "attributes.dp"},
		MetricNames: []string{"http.server.duration", "orders.alice@example.com.count"},
	}
}

// TestResolve_SensitiveRulesUseFields renders the four rules with
// discovered fields through the resolver.
func TestResolve_SensitiveRulesUseFields(t *testing.T) {
	mf, err := mappings.Load("../../configs/esql-mappings.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{"RES-010": true, "SPA-006": true, "LOG-003": true, "MET-008": true}
	var rules []mappings.RuleMapping
	for _, r := range mf.Rules {
		if ids[r.SpecRuleID] {
			if !r.Enabled || r.Status != "" {
				t.Errorf("%s: enabled=%v status=%q, want enabled with no status", r.SpecRuleID, r.Enabled, r.Status)
			}
			if !r.Sensitive || !r.Heuristic || r.Impact != "Critical" {
				t.Errorf("%s: sensitive=%v heuristic=%v impact=%s", r.SpecRuleID, r.Sensitive, r.Heuristic, r.Impact)
			}
			for _, want := range []string{"Best-effort pattern detection; the spec's list is open-ended and no detector set is complete.", "Luhn"} {
				if !strings.Contains(r.Deviation, want) {
					t.Errorf("%s: deviation lacks %q", r.SpecRuleID, want)
				}
			}
			if strings.TrimSpace(r.Remediation) == "" {
				t.Errorf("%s: missing remediation", r.SpecRuleID)
			}
			rules = append(rules, r)
		}
	}
	if len(rules) != 4 {
		t.Fatalf("found %d of the four rules", len(rules))
	}
	cfg := &config.Config{Backend: "esql"}
	cfg.ApplyDefaults()
	cfg.ESQL.IndexPatterns = config.IndexPatterns{Traces: "traces-*.otel-r1", Logs: "logs-*.otel-r1", Metrics: "metrics-*.otel-r1"}
	override := mappings.SemconvCatalog()
	override.Fields = sensitiveFields()
	res, err := mappings.ResolveWithData(rules, cfg, override)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	seg := func(f string) string { return "REPLACE(LEFT(MV_CONCAT(TO_STRING(`" + f + "`), \"; \"), " }
	cases := []struct {
		id, from, where string
		in, out         []string
	}{
		{"RES-010", "FROM traces-*.otel-r1,logs-*.otel-r1,metrics-*.otel-r1\n", "",
			[]string{"resource.attributes.service.name", "resource.attributes.process.command_args"},
			[]string{"scope.name", "name", "body.text"}},
		{"SPA-006", "FROM traces-*.otel-r1,logs-*.otel-r1\n", `(data_stream.type == "traces" OR observed_timestamp IS NULL)`,
			[]string{"name", "status.message", "links.attributes.l", "attributes.shared", "event_name", "attributes.exception.message", "scope.name", "scope.attributes.lib.token"},
			[]string{"body.text", "body.structured", "resource.attributes.service.name"}},
		{"LOG-003", "FROM logs-*.otel-r1\n", "observed_timestamp IS NOT NULL",
			[]string{"body.text", "body.structured", "event_name", "attributes.shared", "scope.name"},
			[]string{"status.message", "resource.attributes.service.name"}},
		{"MET-008", "FROM metrics-*.otel-r1\n", "",
			[]string{"unit", "attributes.dp", "scope.name"},
			[]string{"name", "body.text", "resource.attributes.service.name"}},
	}
	for _, c := range cases {
		q := resolvedQueryFor(t, res, c.id)
		if !strings.HasPrefix(q, "SET unmapped_fields=\"nullify\";\n"+c.from) {
			t.Errorf("%s: want prefix %q:\n%.300s", c.id, c.from, q)
		}
		if !strings.Contains(q, "@timestamp > NOW() - ") || !strings.Contains(q, c.where) {
			t.Errorf("%s: missing time window or %q", c.id, c.where)
		}
		for _, f := range c.in {
			if strings.Count(q, seg(f)) != 1 {
				t.Errorf("%s: field %s scanned %d times, want once", c.id, f, strings.Count(q, seg(f)))
			}
		}
		for _, f := range c.out {
			if strings.Contains(q, seg(f)) {
				t.Errorf("%s: field %s must not be scanned", c.id, f)
			}
		}
		if !strings.HasSuffix(strings.TrimSpace(q), "| KEEP rule_passed, service.name, example, extent") {
			t.Errorf("%s: wrong output shape", c.id)
		}
	}
	met := resolvedQueryFor(t, res, "MET-008")
	if !strings.Contains(met, "`metrics.orders.alice@example.com.count` IS NOT NULL") || strings.Contains(met, "http.server.duration") {
		t.Errorf("MET-008: metric-name segment wrong:\n%s", met)
	}
	spa := resolvedQueryFor(t, res, "SPA-006")
	if !strings.Contains(spa, "TO_STRING(`attributes.http.request.header.authorization`), \"; \")) NOT RLIKE") ||
		!strings.Contains(spa, "TO_STRING(`scope.attributes.lib.token`), \"; \")) NOT RLIKE") {
		t.Errorf("SPA-006: sensitive-key fields must be key-checked")
	}

	// With no discovered fields (generate, review) the rules still render, to
	// a query that yields no rows.
	override.Fields = mappings.Fields{}
	res, err = mappings.ResolveWithData(rules, cfg, override)
	if err != nil {
		t.Fatalf("resolve without fields: %v", err)
	}
	for id := range ids {
		if q := resolvedQueryFor(t, res, id); !strings.Contains(q, "| WHERE false") {
			t.Errorf("%s without fields should yield no rows:\n%s", id, q)
		}
	}
}
