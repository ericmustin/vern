package evaluate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ericmustin/vern/configs"
	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/score"
	"github.com/ericmustin/vern/spec"
)

type fakeExec struct {
	byQuery map[string][]Row
	errFor  map[string]error
	mu      sync.Mutex
	seen    []string
}

func (f *fakeExec) Execute(_ context.Context, query string) ([]Row, error) {
	f.mu.Lock()
	f.seen = append(f.seen, query)
	f.mu.Unlock()
	// Resolved ES|QL carries the unmapped-fields prefix; fixtures key on the body.
	query = strings.TrimPrefix(query, "SET unmapped_fields=\"nullify\";\n")
	if err, ok := f.errFor[query]; ok {
		return nil, err
	}
	return f.byQuery[query], nil
}

func writeMappings(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mappings.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write mappings: %v", err)
	}
	return path
}

func baseCfg(mappingsPath string) *config.Config {
	c := &config.Config{}
	c.ApplyDefaults()
	c.Mappings = mappingsPath
	return c
}

func TestRun_HappyPath(t *testing.T) {
	mappingsPath := writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "RES-005"
    enabled: true
    description: "service.name present"
    impact: "Critical"
    target: "Resource"
    query: "FROM x"
  - spec_rule_id: "RES-001"
    enabled: true
    description: "instance id present"
    impact: "Normal"
    target: "Resource"
    query: "FROM y"
`)
	cfg := baseCfg(mappingsPath)

	exec := &fakeExec{
		byQuery: map[string][]Row{
			"FROM x": {{ServiceName: "svc-a", Passed: true}, {ServiceName: "svc-b", Passed: false, Extent: 0.5, Example: "doc-1"}},
			"FROM y": {{ServiceName: "svc-a", Passed: true}, {ServiceName: "svc-b", Passed: true}},
		},
	}

	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.RulesAttempted != 2 || rep.RulesSucceeded != 2 {
		t.Errorf("rules: attempted=%d succeeded=%d", rep.RulesAttempted, rep.RulesSucceeded)
	}
	if len(rep.Failures) != 0 {
		t.Errorf("unexpected failures: %+v", rep.Failures)
	}
	if rep.Backend != "fake" {
		t.Errorf("backend = %q, want fake", rep.Backend)
	}
	if len(rep.Score.Services) != 2 {
		t.Fatalf("services: %+v", rep.Score.Services)
	}

	// svc-a: both pass. Critical=40, Normal=20 -> 60/60 = 100
	if rep.Score.Services[0].Score != 100 {
		t.Errorf("svc-a score = %v, want 100", rep.Score.Services[0].Score)
	}
	// svc-b: Critical fails, Normal passes -> 20 / 60 = 33.33...
	if rep.Score.Services[1].Score < 33.0 || rep.Score.Services[1].Score > 34.0 {
		t.Errorf("svc-b score = %v, want ~33.3", rep.Score.Services[1].Score)
	}
}

func TestRun_PerRuleFailureSoftFails(t *testing.T) {
	mappingsPath := writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "RES-005"
    enabled: true
    description: "x"
    impact: "Critical"
    target: "Resource"
    query: "FROM good"
  - spec_rule_id: "MET-001"
    enabled: true
    description: "y"
    impact: "Important"
    target: "Metric"
    query: "FROM bad"
`)
	cfg := baseCfg(mappingsPath)

	exec := &fakeExec{
		byQuery: map[string][]Row{
			"FROM good": {{ServiceName: "svc-a", Passed: true}},
		},
		errFor: map[string]error{
			"FROM bad": errors.New("syntax: unknown column foo"),
		},
	}

	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.RulesAttempted != 2 || rep.RulesSucceeded != 1 {
		t.Errorf("rules: attempted=%d succeeded=%d", rep.RulesAttempted, rep.RulesSucceeded)
	}
	if len(rep.Failures) != 1 || rep.Failures[0].RuleID != "MET-001" {
		t.Fatalf("failures = %+v", rep.Failures)
	}
	// Score is computed from the surviving rule only.
	if len(rep.Score.Services) != 1 || rep.Score.Services[0].Score != 100 {
		t.Errorf("score: %+v", rep.Score)
	}
}

func TestRun_DisabledRulesAreSkipped(t *testing.T) {
	mappingsPath := writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "RES-005"
    enabled: false
    description: "off"
    impact: "Critical"
    target: "Resource"
    query: "FROM x"
`)
	cfg := baseCfg(mappingsPath)
	exec := &fakeExec{byQuery: map[string][]Row{}}

	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.RulesAttempted != 0 {
		t.Errorf("attempted = %d, want 0", rep.RulesAttempted)
	}
}

// optInMappings mirrors the shipped SDK-001 shape: disabled in YAML, enabled
// only through its opt_in_flag.
const optInMappings = `
backend: esql
rules:
  - spec_rule_id: "SDK-001"
    enabled: false
    opt_in_flag: "filters.enable_sdk_rules"
    status: "opt-in: enable via filters.enable_sdk_rules"
    description: "sdk support"
    impact: "Low"
    target: "SDK"
    query: "FROM sdk"
`

func TestRun_OptInRuleRunsWhenFlagOn(t *testing.T) {
	cfg := baseCfg(writeMappings(t, optInMappings))
	cfg.Filters.EnableSDKRules = true

	exec := &fakeExec{byQuery: map[string][]Row{"FROM sdk": {{ServiceName: "svc", Passed: true}}}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.RulesAttempted != 1 {
		t.Errorf("opt-in rule should run when flag on; attempted=%d", rep.RulesAttempted)
	}
	rr := ruleByID(t, rep, "SDK-001")
	if rr.Status != StatusEvaluated || !rr.Scored {
		t.Errorf("rule: %+v", rr)
	}
}

func TestRun_OptInRuleNotImplementedWhenFlagOff(t *testing.T) {
	cfg := baseCfg(writeMappings(t, optInMappings))
	cfg.Filters.EnableSDKRules = false

	exec := &fakeExec{byQuery: map[string][]Row{"FROM sdk": {{ServiceName: "svc", Passed: true}}}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.RulesAttempted != 0 || len(exec.seen) != 0 {
		t.Errorf("opt-in rule should not run when flag off; attempted=%d seen=%v", rep.RulesAttempted, exec.seen)
	}
	rr := ruleByID(t, rep, "SDK-001")
	if rr.Status != StatusNotImplemented || rr.Reason != "opt-in: enable via filters.enable_sdk_rules" {
		t.Errorf("rule: %+v", rr)
	}
}

func TestRun_UnknownImpactIsError(t *testing.T) {
	cfg := baseCfg(writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "RES-005"
    enabled: true
    description: "d"
    impact: "Urgent"
    target: "Resource"
    query: "FROM x"
  - spec_rule_id: "RES-001"
    enabled: true
    description: "d"
    impact: "Normal"
    target: "Resource"
    query: "FROM y"
`))
	exec := &fakeExec{byQuery: map[string][]Row{
		"FROM x": {{ServiceName: "a", Passed: false}},
		"FROM y": {{ServiceName: "a", Passed: true}},
	}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	rr := ruleByID(t, rep, "RES-005")
	if rr.Status != StatusError || rr.Reason != `unknown impact "Urgent"` || rr.Scored || len(rr.Outcomes) != 0 {
		t.Errorf("rule: %+v", rr)
	}
	found := false
	for _, f := range rep.Failures {
		if f.RuleID == "RES-005" && f.Error == `unknown impact "Urgent"` {
			found = true
		}
	}
	if !found {
		t.Errorf("failures: %+v", rep.Failures)
	}
	// Only RES-001 (passing) is scored, so the failing Urgent rule cannot pull the score down.
	if rep.Score.Overall != 100 {
		t.Errorf("overall = %v, want 100", rep.Score.Overall)
	}
}

func ruleByID(t *testing.T, rep *Report, id string) RuleResult {
	t.Helper()
	for _, r := range rep.Rules {
		if r.RuleID == id {
			return r
		}
	}
	t.Fatalf("no RuleResult for %s in %+v", id, rep.Rules)
	return RuleResult{}
}

func TestRun_MissingServiceNameIsScored(t *testing.T) {
	cfg := baseCfg(writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "RES-005"
    enabled: true
    description: "d"
    impact: "Critical"
    target: "Resource"
    query: "FROM x"
`))
	exec := &fakeExec{byQuery: map[string][]Row{"FROM x": {{ServiceName: "", Passed: false}}}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Score.Services) != 1 || rep.Score.Services[0].ServiceName != score.MissingServiceName {
		t.Fatalf("services: %+v", rep.Score.Services)
	}
	if rep.Score.Services[0].Score != 0 {
		t.Errorf("score = %v, want 0", rep.Score.Services[0].Score)
	}
	if rr := ruleByID(t, rep, "RES-005"); rr.Status != StatusEvaluated || !rr.Scored {
		t.Errorf("rule: %+v", rr)
	}
}

func TestRun_TemplateErrorFailsOnlyThatRule(t *testing.T) {
	cfg := baseCfg(writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "BAD-001"
    enabled: true
    description: "d"
    impact: "Normal"
    target: "Resource"
    query: "FROM {{ .Nope }}"
  - spec_rule_id: "RES-005"
    enabled: true
    description: "d"
    impact: "Critical"
    target: "Resource"
    query: "FROM x"
`))
	exec := &fakeExec{byQuery: map[string][]Row{"FROM x": {{ServiceName: "a", Passed: true}}}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if rr := ruleByID(t, rep, "BAD-001"); rr.Status != StatusError || rr.Reason == "" {
		t.Errorf("bad rule: %+v", rr)
	}
	if rr := ruleByID(t, rep, "RES-005"); rr.Status != StatusEvaluated {
		t.Errorf("good rule: %+v", rr)
	}
	found := false
	for _, f := range rep.Failures {
		if f.RuleID == "BAD-001" {
			found = true
		}
	}
	if !found {
		t.Errorf("failures: %+v", rep.Failures)
	}
}

func TestRun_DisabledRuleIsNotImplemented(t *testing.T) {
	cfg := baseCfg(writeMappings(t, `
backend: esql
spec_version: "0.9"
rules:
  - spec_rule_id: "RES-005"
    enabled: false
    status: "needs a catalog"
    description: "d"
    impact: "Critical"
    target: "Resource"
    query: "FROM x"
`))
	rep, err := Run(context.Background(), cfg, &fakeExec{}, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	rr := ruleByID(t, rep, "RES-005")
	if rr.Status != StatusNotImplemented || rr.Reason != "needs a catalog" {
		t.Errorf("rule: %+v", rr)
	}
	if rep.SpecVersion != "0.9" || rep.SchemaVersion != SchemaVersion {
		t.Errorf("versions: %q %d", rep.SpecVersion, rep.SchemaVersion)
	}
}

func TestRun_UnmappedSpecRuleIsNotImplemented(t *testing.T) {
	cfg := baseCfg(writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "RES-005"
    enabled: true
    description: "d"
    impact: "Critical"
    target: "Resource"
    query: "FROM x"
`))
	exec := &fakeExec{byQuery: map[string][]Row{"FROM x": {{ServiceName: "a", Passed: true}}}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{
		Backend: "fake",
		SpecRules: []coverage.SpecRule{
			{ID: "RES-005", Impact: "Critical", Target: "Resource", Description: "spec desc"},
			{ID: "LOG-003", Impact: "Normal", Target: "Log", Description: "log thing"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rr := ruleByID(t, rep, "LOG-003")
	if rr.Status != StatusNotImplemented || rr.Reason != "no implementation for this rule yet" {
		t.Errorf("rule: %+v", rr)
	}
	if !rep.Partial {
		t.Error("want Partial")
	}
	joined := strings.Join(rep.PartialReasons, "\n")
	if !strings.Contains(joined, "LOG-003") {
		t.Errorf("reasons: %v", rep.PartialReasons)
	}
	if got := ruleByID(t, rep, "RES-005").Description; got != "spec desc" {
		t.Errorf("description = %q, want spec's", got)
	}
}

func TestRun_SignalGating(t *testing.T) {
	cfg := baseCfg(writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "MET-001"
    enabled: true
    signal: metrics
    description: "d"
    impact: "Normal"
    target: "Metric"
    query: "FROM m"
`))
	exec := &fakeExec{}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake", Signals: map[string]bool{"traces": true}})
	if err != nil {
		t.Fatal(err)
	}
	rr := ruleByID(t, rep, "MET-001")
	if rr.Status != StatusNotApplicable || rr.Reason != "sample contains no metrics" {
		t.Errorf("rule: %+v", rr)
	}
	if len(exec.seen) != 0 {
		t.Errorf("query executed: %v", exec.seen)
	}
}

func TestRun_MinObservation(t *testing.T) {
	cfg := baseCfg(writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "TRC-001"
    enabled: true
    min_observation: "336h"
    description: "d"
    impact: "Normal"
    target: "Span"
    query: "FROM t"
`))
	exec := &fakeExec{}
	span := 90*time.Second + 314304792*time.Nanosecond
	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake", ObservedSpan: span})
	if err != nil {
		t.Fatal(err)
	}
	rr := ruleByID(t, rep, "TRC-001")
	if rr.Status != StatusNotAssessable || rr.Reason != "needs 14 days of observation; sample covers 1m30s" {
		t.Errorf("rule: %+v", rr)
	}
	if len(exec.seen) != 0 {
		t.Errorf("query executed: %v", exec.seen)
	}
}

func TestRoundSpan(t *testing.T) {
	for in, want := range map[time.Duration]string{
		90*time.Second + 314304792*time.Nanosecond: "1m30s",
		1500 * time.Microsecond * 100:              "150ms",
		26*time.Hour + 499*time.Millisecond:        "26h0m0s",
	} {
		if got := roundSpan(in).String(); got != want {
			t.Errorf("roundSpan(%v) = %s, want %s", in, got, want)
		}
	}
}

func TestRun_ZeroRowsIsNotApplicable(t *testing.T) {
	cfg := baseCfg(writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "RES-005"
    enabled: true
    description: "d"
    impact: "Critical"
    target: "Resource"
    query: "FROM x"
`))
	rep, err := Run(context.Background(), cfg, &fakeExec{}, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	rr := ruleByID(t, rep, "RES-005")
	if rr.Status != StatusNotApplicable || rr.Reason != "no matching telemetry" || rr.Scored {
		t.Errorf("rule: %+v", rr)
	}
	if rep.Score.Scored {
		t.Error("score should not be Scored")
	}
}

func TestRun_AdvisoryNotScored(t *testing.T) {
	cfg := baseCfg(writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "ADV-001"
    enabled: true
    advisory: true
    description: "d"
    impact: "Critical"
    target: "Span"
    query: "FROM adv"
  - spec_rule_id: "RES-005"
    enabled: true
    description: "d"
    impact: "Critical"
    target: "Resource"
    query: "FROM x"
`))
	exec := &fakeExec{byQuery: map[string][]Row{
		"FROM adv": {{ServiceName: "a", Passed: false}},
		"FROM x":   {{ServiceName: "a", Passed: true}},
	}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Score.Overall != 100 {
		t.Errorf("overall = %v, want 100", rep.Score.Overall)
	}
	rr := ruleByID(t, rep, "ADV-001")
	if rr.Scored || rr.Status != StatusEvaluated || len(rr.Outcomes) != 1 || rr.Outcomes[0].Passed {
		t.Errorf("advisory: %+v", rr)
	}
	if !rep.Partial {
		t.Error("advisory rule not scored should make report partial")
	}
}

func TestRun_ImpactCaseInsensitive(t *testing.T) {
	cfg := baseCfg(writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "RES-005"
    enabled: true
    description: "d"
    impact: "critical"
    target: "Resource"
    query: "FROM x"
  - spec_rule_id: "RES-001"
    enabled: true
    description: "d"
    impact: "Low"
    target: "Resource"
    query: "FROM y"
`))
	exec := &fakeExec{byQuery: map[string][]Row{
		"FROM x": {{ServiceName: "a", Passed: false}},
		"FROM y": {{ServiceName: "a", Passed: true}},
	}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	// 10 / (40+10) = 20
	if rep.Score.Overall != 20 {
		t.Errorf("overall = %v, want 20", rep.Score.Overall)
	}
	if rr := ruleByID(t, rep, "RES-005"); rr.Impact != "Critical" || !rr.Scored {
		t.Errorf("rule: %+v", rr)
	}
}

func TestRun_RulesSortedUnderParallelism(t *testing.T) {
	var b strings.Builder
	b.WriteString("backend: esql\nrules:\n")
	byQuery := map[string][]Row{}
	for i := 9; i >= 0; i-- {
		fmt.Fprintf(&b, "  - spec_rule_id: \"RES-%03d\"\n    enabled: true\n    description: d\n    impact: Normal\n    target: Resource\n    query: \"FROM q%d\"\n", i, i)
		byQuery[fmt.Sprintf("FROM q%d", i)] = []Row{{ServiceName: fmt.Sprintf("svc-RES-%03d", i), Passed: true}}
	}
	cfg := baseCfg(writeMappings(t, b.String()))
	rep, err := Run(context.Background(), cfg, &fakeExec{byQuery: byQuery}, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rules) != 10 {
		t.Fatalf("rules = %d", len(rep.Rules))
	}
	for i := 1; i < len(rep.Rules); i++ {
		if rep.Rules[i-1].RuleID >= rep.Rules[i].RuleID {
			t.Fatalf("not sorted: %v", rep.Rules)
		}
	}
	for _, rr := range rep.Rules {
		if len(rr.Outcomes) != 1 || rr.Outcomes[0].ServiceName != "svc-"+rr.RuleID {
			t.Errorf("%s has outcomes of another rule: %+v", rr.RuleID, rr.Outcomes)
		}
	}
	if rep.RulesAttempted != 10 || rep.RulesSucceeded != 10 {
		t.Errorf("attempted=%d succeeded=%d", rep.RulesAttempted, rep.RulesSucceeded)
	}
}

func TestRun_DuplicateRuleIDsEachGetAStatus(t *testing.T) {
	cfg := baseCfg(writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "RES-005"
    enabled: true
    description: "first"
    impact: "Critical"
    target: "Resource"
    query: "FROM one"
  - spec_rule_id: "RES-005"
    enabled: true
    description: "second"
    impact: "Critical"
    target: "Resource"
    query: "FROM two"
`))
	exec := &fakeExec{byQuery: map[string][]Row{
		"FROM one": {{ServiceName: "svc-one", Passed: true}},
		"FROM two": {{ServiceName: "svc-two", Passed: false}},
	}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rules) != 2 {
		t.Fatalf("rules = %+v", rep.Rules)
	}
	seen := map[string]bool{}
	for _, rr := range rep.Rules {
		if rr.Status != StatusEvaluated || len(rr.Outcomes) != 1 {
			t.Errorf("rule: %+v", rr)
			continue
		}
		seen[rr.Outcomes[0].ServiceName] = true
	}
	if !seen["svc-one"] || !seen["svc-two"] {
		t.Errorf("outcomes lost or duplicated: %+v", rep.Rules)
	}
	if rep.RulesAttempted != 2 || rep.RulesSucceeded != 2 {
		t.Errorf("attempted=%d succeeded=%d", rep.RulesAttempted, rep.RulesSucceeded)
	}
}

func TestRun_UsesPreloadedMappings(t *testing.T) {
	mf, err := mappings.LoadBytes(configs.ESQLMappings, "embedded")
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	specRules, err := coverage.LoadSpecRulesFS(spec.FS, "rules")
	if err != nil {
		t.Fatalf("LoadSpecRulesFS: %v", err)
	}
	cfg := baseCfg(filepath.Join(t.TempDir(), "missing.yaml"))
	rep, err := Run(context.Background(), cfg, &fakeExec{}, RunOptions{Mappings: mf, SpecRules: specRules})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Rules) != 26 {
		t.Errorf("got %d rules, want 26", len(rep.Rules))
	}
}

func TestRun_CopiesSampleInfo(t *testing.T) {
	path := writeMappings(t, "backend: esql\nrules: []\n")
	si := &SampleInfo{Spans: 3, Services: []string{"a"}, Signals: []string{"traces"}}
	rep, err := Run(context.Background(), baseCfg(path), &fakeExec{}, RunOptions{Sample: si})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !reflect.DeepEqual(rep.Sample, si) {
		t.Errorf("Sample = %+v, want %+v", rep.Sample, si)
	}
}

func TestRun_DoesNotMutatePreloadedMappings(t *testing.T) {
	mf, err := mappings.LoadBytes(configs.ESQLMappings, "embedded")
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	specRules, err := coverage.LoadSpecRulesFS(spec.FS, "rules")
	if err != nil {
		t.Fatalf("LoadSpecRulesFS: %v", err)
	}
	sdkEnabled := func() bool {
		for _, r := range mf.Rules {
			if r.SpecRuleID == "SDK-001" {
				return r.Enabled
			}
		}
		t.Fatal("SDK-001 not in mappings")
		return false
	}
	if sdkEnabled() {
		t.Fatal("precondition: SDK-001 should start disabled")
	}

	run := func(enable bool) *Report {
		cfg := baseCfg("missing.yaml")
		cfg.Filters.EnableSDKRules = enable
		rep, err := Run(context.Background(), cfg, &fakeExec{}, RunOptions{Mappings: mf, SpecRules: specRules})
		if err != nil {
			t.Errorf("Run(enable=%v): %v", enable, err)
			return nil
		}
		return rep
	}

	var wg sync.WaitGroup
	var on, off *Report
	wg.Add(2)
	go func() { defer wg.Done(); on = run(true) }()
	go func() { defer wg.Done(); off = run(false) }()
	wg.Wait()
	if on == nil || off == nil {
		return
	}

	if sdkEnabled() {
		t.Error("Run mutated the preloaded mappings: SDK-001 Enabled became true")
	}
	if got := ruleStatus(off, "SDK-001"); got != StatusNotImplemented {
		t.Errorf("run with SDK rules off: SDK-001 status = %q, want %q", got, StatusNotImplemented)
	}
}

func ruleStatus(rep *Report, id string) RuleStatus {
	for _, r := range rep.Rules {
		if r.RuleID == id {
			return r.Status
		}
	}
	return ""
}

func TestRun_ExclusionsDropServices(t *testing.T) {
	mappingsPath := writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "RES-005"
    enabled: true
    description: "service.name present"
    impact: "Critical"
    target: "Resource"
    query: "FROM x"
`)
	cfg := baseCfg(mappingsPath)
	cfg.Exclusions = config.ExclusionConfig{Services: []string{"svc-a"}, ServicePatterns: []string{"^test-"}}
	exec := &fakeExec{byQuery: map[string][]Row{
		"FROM x": {{ServiceName: "svc-a", Passed: true}, {ServiceName: "test-x", Passed: true}, {ServiceName: "svc-b", Passed: false}},
	}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{Backend: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Score.Services) != 1 || rep.Score.Services[0].ServiceName != "svc-b" {
		t.Fatalf("services = %+v", rep.Score.Services)
	}
	if rep.Score.Overall != 0 {
		t.Errorf("overall = %v, want 0 (only failing svc-b left)", rep.Score.Overall)
	}
	if got := ruleByID(t, rep, "RES-005").Outcomes; len(got) != 1 || got[0].ServiceName != "svc-b" {
		t.Errorf("rule outcomes = %+v", got)
	}
}

func TestRun_BadExclusionPatternIsError(t *testing.T) {
	cfg := baseCfg(writeMappings(t, "backend: esql\nrules: []\n"))
	cfg.Exclusions.ServicePatterns = []string{"("}
	_, err := Run(context.Background(), cfg, &fakeExec{}, RunOptions{})
	if err == nil || !strings.Contains(err.Error(), `"("`) {
		t.Fatalf("err = %v", err)
	}
}

// TestRun_SensitiveRulesScanDiscoveredFields: Run hands the discovered fields
// to the four sensitive-data rules (enabled in the embedded mappings) and
// MET-008's metric-name check reaches its query.
func TestRun_SensitiveRulesScanDiscoveredFields(t *testing.T) {
	mf, err := mappings.LoadBytes(configs.ESQLMappings, "embedded")
	if err != nil {
		t.Fatal(err)
	}
	var kept []mappings.RuleMapping
	for _, r := range mf.Rules {
		if r.Sensitive {
			if !r.Enabled {
				t.Errorf("%s ships disabled", r.SpecRuleID)
			}
			kept = append(kept, r)
		}
	}
	if len(kept) != 4 {
		t.Fatalf("sensitive rules = %d, want 4", len(kept))
	}
	mf.Rules = kept
	exec := &fakeExec{}
	rep, err := Run(context.Background(), baseCfg(""), exec, RunOptions{
		Mappings:         mf,
		FieldsDiscovered: true,
		Fields: mappings.Fields{
			Span: []string{"attributes.db.query.text"}, Log: []string{"body.text"}, Resource: []string{"resource.attributes.custom.config"},
			Metric: []string{"unit"}, MetricNames: []string{"http.server.duration", "orders.alice@example.com.count"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(exec.seen, "\n----\n")
	for _, want := range []string{
		"TO_STRING(`attributes.db.query.text`)", "TO_STRING(`body.text`)", "TO_STRING(`resource.attributes.custom.config`)",
		"`metrics.orders.alice@example.com.count` IS NOT NULL",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("no query contains %s", want)
		}
	}
	// No rows (as for an empty field list): not applicable.
	for _, rr := range rep.Rules {
		if rr.Status != StatusNotApplicable {
			t.Errorf("%s: status %s, want not_applicable for no rows", rr.RuleID, rr.Status)
		}
	}
}
