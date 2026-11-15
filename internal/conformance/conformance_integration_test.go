//go:build integration

// Conformance against Elasticsearch 9.5.5 on the free Basic license. These
// tests score each fixture in testdata/conformance through samplescore.Score,
// the same path `vern score` uses, against a local container started with
// `make es-test-up` (VERN_TEST_ES_URL, default http://localhost:9200).
package conformance

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ericmustin/vern/configs"
	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/engine/elastic"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/samplescore"
)

// scoreSlots caps concurrent Score calls so a 1g-heap test cluster keeps up.
// With the four sensitive-data rules enabled, two or more concurrent runs
// trip the 1g node's parent circuit breaker (HTTP 429), so cases run one at
// a time.
var scoreSlots = make(chan struct{}, 1)

func esURL() string {
	if u := os.Getenv("VERN_TEST_ES_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://localhost:9200"
}

func newEngine(t *testing.T) *elastic.Engine {
	t.Helper()
	e := elastic.New(elastic.Config{URL: esURL(), Logf: t.Logf, EnsureTemplate: true})
	if err := e.Ready(t.Context()); err != nil {
		t.Fatalf("ready: %v", err)
	}
	return e
}

// scoreFile decodes path and scores it in its own run.
func scoreFile(t *testing.T, e *elastic.Engine, path string, opts samplescore.Options) *evaluate.Report {
	t.Helper()
	s, err := decodeFile(path)
	if err != nil {
		t.Fatal(err)
	}
	scoreSlots <- struct{}{}
	defer func() { <-scoreSlots }()
	opts.Files = []string{filepath.Base(path)}
	rep, err := samplescore.Score(t.Context(), e, s, opts)
	if err != nil {
		t.Fatalf("score %s: %v", path, err)
	}
	if d := rep.Sample.Dropped; len(d) != 0 {
		t.Fatalf("%s: items not indexed: %v", path, d)
	}
	return rep
}

// errorRules lists the rules with status error, skipping skip.
func errorRules(rep *evaluate.Report, skip string) []string {
	var out []string
	for _, r := range rep.Rules {
		if r.Status == evaluate.StatusError && r.RuleID != skip {
			out = append(out, fmt.Sprintf("%s: %s", r.RuleID, r.Reason))
		}
	}
	return out
}

func findRule(rep *evaluate.Report, id string) (evaluate.RuleResult, bool) {
	for _, r := range rep.Rules {
		if r.RuleID == id {
			return r, true
		}
	}
	return evaluate.RuleResult{}, false
}

func TestConformance_LicenseIsBasic(t *testing.T) {
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(esURL() + "/_license")
	if err != nil {
		t.Fatalf("GET _license: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET _license: %d %s", resp.StatusCode, body)
	}
	var lic struct {
		License struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"license"`
	}
	if err := json.Unmarshal(body, &lic); err != nil {
		t.Fatalf("decode _license: %v: %s", err, body)
	}
	if lic.License.Type != "basic" {
		t.Fatalf("license.type = %q, want basic (status %q)", lic.License.Type, lic.License.Status)
	}
}

func TestConformance_Rules(t *testing.T) {
	e := newEngine(t)
	for _, f := range loadFixtures(t) {
		for _, name := range f.caseNames() {
			f, name, want := f, name, f.Expect.Cases[name]
			t.Run(f.Expect.Rule+"/"+name, func(t *testing.T) {
				t.Parallel()
				rep := scoreFile(t, e, f.casePath(name), samplescore.Options{})
				rr, ok := findRule(rep, f.Expect.Rule)
				if !ok {
					t.Fatalf("rule %s missing from report", f.Expect.Rule)
				}
				if rr.Status != want.Status {
					t.Errorf("status = %s (%s), want %s", rr.Status, rr.Reason, want.Status)
				}
				if want.Scored != nil && rr.Scored != *want.Scored {
					t.Errorf("scored = %v, want %v (advisory %v)", rr.Scored, *want.Scored, rr.Advisory)
				}
				if want.Status == evaluate.StatusEvaluated {
					got := map[string]bool{}
					var problems []string
					for _, o := range rr.Outcomes {
						if prev, dup := got[o.ServiceName]; dup && prev != o.Passed {
							problems = append(problems, fmt.Sprintf("service %q has both passed=%v and passed=%v", o.ServiceName, prev, o.Passed))
						}
						got[o.ServiceName] = o.Passed
					}
					if !equalServices(got, want.Services) {
						problems = append(problems, fmt.Sprintf("services = %v, want %v (outcomes %+v)", got, want.Services, rr.Outcomes))
					}
					for _, p := range problems {
						t.Error(p)
					}
				}
				if want.MinChecks > 0 {
					for _, o := range rr.Outcomes {
						var n int
						if _, err := fmt.Sscanf(o.Example, "No sensitive data matched %d checks", &n); err != nil || n < want.MinChecks {
							t.Errorf("%s: example %q, want \"No sensitive data matched N checks\" with N >= %d", o.ServiceName, o.Example, want.MinChecks)
						}
					}
				}
				for _, sub := range want.BlindSpotsContains {
					if !slices.ContainsFunc(rep.Sample.BlindSpots, func(b string) bool { return strings.Contains(b, sub) }) {
						t.Errorf("blind spots %q lack %q", rep.Sample.BlindSpots, sub)
					}
				}
				if errs := errorRules(rep, f.Expect.Rule); len(errs) > 0 {
					t.Errorf("other rules errored:\n  %s", strings.Join(errs, "\n  "))
				}
			})
		}
	}
}

func equalServices(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// schemaLintAllowed lists the rules the schema lint may leave unplanned, each
// with the reason. Every other rule in the report must come back evaluated,
// so a rule skipped for any reason (signal gating, min_observation, an empty
// result, a disabled mapping) cannot hide a field typo from the lint. An
// entry whose rule does get evaluated fails the test, so the list cannot go
// stale.
var schemaLintAllowed = map[string]string{
	"MET-004": "disabled: histogram bucket boundaries are not exposed to ES|QL; only a placeholder ROW query",
}

// lintMappings returns the embedded ES|QL mappings with every min_observation
// cleared, so the runner plans those rules instead of marking them
// not_assessable on a short sample. It also returns the rules it changed.
func lintMappings(t *testing.T) (*mappings.MappingsFile, []string) {
	t.Helper()
	mf, err := mappings.LoadBytes(configs.ESQLMappings, "embedded esql-mappings.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cp := *mf
	cp.Rules = slices.Clone(mf.Rules)
	var cleared []string
	for i := range cp.Rules {
		if cp.Rules[i].MinObservation != "" {
			cleared = append(cleared, cp.Rules[i].SpecRuleID)
			cp.Rules[i].MinObservation = ""
		}
	}
	return &cp, cleared
}

// TestConformance_SchemaLint plans every rule with unmapped_fields off against
// a sample that sets every referenced field: a misspelled field is then an
// "Unknown column" error instead of a silent null under NULLIFY. SDK-001 (opt
// in) and the scope filters are switched on so their fields are linted too,
// and min_observation is cleared so rules that need a long sample are planned.
func TestConformance_SchemaLint(t *testing.T) {
	e := newEngine(t)
	cfg := config.Default()
	cfg.ESQL.UnmappedFields = "off"
	cfg.Filters.EnableSDKRules = true
	cfg.Filters.Environments = []string{"production"}
	cfg.Filters.ServiceNamespaces = []string{"conf"}
	mf, cleared := lintMappings(t)
	t.Logf("min_observation cleared for: %s", strings.Join(cleared, " "))

	rep := scoreFile(t, e, filepath.Join(fixtureRoot, "_schema", "full.jsonl"), samplescore.Options{Config: cfg, Mappings: mf})

	var planned, problems []string
	for _, r := range rep.Rules {
		why, allowed := schemaLintAllowed[r.RuleID]
		switch {
		case r.Status == evaluate.StatusError:
			problems = append(problems, fmt.Sprintf("%s: error: %s", r.RuleID, r.Reason))
		case r.Status == evaluate.StatusEvaluated:
			planned = append(planned, r.RuleID)
			if allowed {
				problems = append(problems, fmt.Sprintf("%s: evaluated but allowlisted (%s); remove it from schemaLintAllowed", r.RuleID, why))
			}
		case !allowed:
			problems = append(problems, fmt.Sprintf("%s: %s (%s); not planned, so not linted", r.RuleID, r.Status, r.Reason))
		}
	}
	sort.Strings(planned)
	t.Logf("planned and linted (%d): %s", len(planned), strings.Join(planned, " "))
	if len(problems) > 0 {
		t.Fatalf("schema lint with unmapped_fields off:\n  %s", strings.Join(problems, "\n  "))
	}
	if !slices.Contains(planned, "SDK-001") || !slices.Contains(planned, "LOG-001") {
		t.Errorf("SDK-001 and LOG-001 must be linted; planned: %v", planned)
	}
}

func TestConformance_DemoSampleScores(t *testing.T) {
	e := newEngine(t)
	rep := scoreFile(t, e, "../../web/examples/demo.jsonl.gz", samplescore.Options{})
	if rep.Score == nil || !rep.Score.Scored {
		t.Fatalf("demo sample not scored: %+v", rep.Score)
	}
	if errs := errorRules(rep, ""); len(errs) > 0 {
		t.Fatalf("rules errored on the demo sample:\n  %s", strings.Join(errs, "\n  "))
	}
}
