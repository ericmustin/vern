// Package conformance proves that each rule's ES|QL query agrees with the
// Instrumentation Score spec. Fixtures live in testdata/conformance (see its
// README.md); the tests that score them against Elasticsearch carry the
// `integration` build tag. The tests in this file need no cluster: they check
// that the fixtures are well formed, so a broken fixture fails `go test ./...`.
package conformance

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"gopkg.in/yaml.v3"

	"github.com/ericmustin/vern/configs"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/otlpin"
	"github.com/ericmustin/vern/internal/score"
	"github.com/ericmustin/vern/internal/semconv"
)

// fixtureRoot is testdata/conformance relative to this package.
const fixtureRoot = "../../testdata/conformance"

// maxCaseSpan bounds the time span of a case with metric data points. Score
// shifts the newest item to now-1m, and Elasticsearch rejects metric points
// older than about 2h (the metrics data streams are time series indices).
const maxCaseSpan = time.Hour

// maxNoMetricCaseSpan bounds a case with no metric data points. Traces and
// logs data streams accept old documents, so such a case may be long, which
// rules with a min_observation need (LOG-001: 14 days). The longest
// look-back any query uses is LOG-001's NOW() - 30d. Score puts the newest
// item at now-1m, so a case spanning a full 30 days would push its oldest
// item past that window; one hour less keeps every item inside it.
const maxNoMetricCaseSpan = 30*24*time.Hour - time.Hour

// expectFile is testdata/conformance/<RULE>/expect.yaml.
type expectFile struct {
	Rule  string                `yaml:"rule"`
	Cases map[string]expectCase `yaml:"cases"`
}

// expectCase is one case: <key>.jsonl (or <key>.jsonl.gz) scored on its own.
type expectCase struct {
	Status   evaluate.RuleStatus `yaml:"status"`
	Services map[string]bool     `yaml:"services"` // service.name -> rule_passed; evaluated only
	Scored   *bool               `yaml:"scored"`   // optional; evaluated only: the report's scored flag
	// BlindSpotsContains lists substrings: each must appear in one of the
	// report's sample blind spots (sensitive-data rules).
	BlindSpotsContains []string `yaml:"blind_spots_contains"`
	// MinChecks (sensitive-data pass cases) requires every service's pass
	// message, "No sensitive data matched N checks", to report N >= MinChecks,
	// so a field-discovery failure cannot pass as a clean result.
	MinChecks int `yaml:"min_checks"`
}

// ruleFixture is a loaded rule directory.
type ruleFixture struct {
	Dir    string
	Expect expectFile
}

// caseNames returns the case keys in a stable order.
func (f ruleFixture) caseNames() []string {
	names := make([]string, 0, len(f.Expect.Cases))
	for k := range f.Expect.Cases {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// caseExts are the case file suffixes, preferred first.
var caseExts = []string{".jsonl.gz", ".jsonl"}

// casePath returns <key>.jsonl.gz when it exists, else <key>.jsonl.
func (f ruleFixture) casePath(name string) string {
	for _, ext := range caseExts {
		p := filepath.Join(f.Dir, name+ext)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(f.Dir, name+".jsonl")
}

// caseKey returns the case key of a case file name, or false if it is not one.
func caseKey(base string) (string, bool) {
	for _, ext := range caseExts {
		if strings.HasSuffix(base, ext) {
			return strings.TrimSuffix(base, ext), true
		}
	}
	return "", false
}

// loadFixtures reads every testdata/conformance/<RULE>/expect.yaml.
func loadFixtures(t *testing.T) []ruleFixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(fixtureRoot, "*", "expect.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no expect.yaml under %s", fixtureRoot)
	}
	sort.Strings(paths)
	var out []ruleFixture
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var ef expectFile
		dec := yaml.NewDecoder(bytes.NewReader(b))
		dec.KnownFields(true)
		if err := dec.Decode(&ef); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out = append(out, ruleFixture{Dir: filepath.Dir(p), Expect: ef})
	}
	return out
}

// decodeFile decodes one fixture file with the CLI's limits.
func decodeFile(path string) (*otlpin.Sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, err := otlpin.Decode([]otlpin.Input{{Name: filepath.Base(path), R: f}}, otlpin.DefaultLimits)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return s, nil
}

// resourceServiceNames returns the service.name of every resource in s; a
// resource without one contributes "".
func resourceServiceNames(s *otlpin.Sample) []string {
	var out []string
	add := func(m pcommon.Map) {
		v, ok := m.Get("service.name")
		if !ok {
			out = append(out, "")
			return
		}
		out = append(out, v.AsString())
	}
	for i := 0; i < s.Traces.ResourceSpans().Len(); i++ {
		add(s.Traces.ResourceSpans().At(i).Resource().Attributes())
	}
	for i := 0; i < s.Logs.ResourceLogs().Len(); i++ {
		add(s.Logs.ResourceLogs().At(i).Resource().Attributes())
	}
	for i := 0; i < s.Metrics.ResourceMetrics().Len(); i++ {
		add(s.Metrics.ResourceMetrics().At(i).Resource().Attributes())
	}
	return out
}

func enabledRuleIDs(t *testing.T) map[string]bool {
	t.Helper()
	ids, _ := ruleIDs(t)
	return ids
}

// ruleIDs returns the enabled rules, and among them those that are on by
// default (no opt-in flag), from the embedded ES|QL mappings.
func ruleIDs(t *testing.T) (enabled, byDefault map[string]bool) {
	t.Helper()
	mf, err := mappings.LoadBytes(configs.ESQLMappings, "embedded esql-mappings.yaml")
	if err != nil {
		t.Fatal(err)
	}
	enabled, byDefault = map[string]bool{}, map[string]bool{}
	for _, r := range mf.Rules {
		if r.Enabled && !r.IsAggregation {
			enabled[r.SpecRuleID] = true
			if r.OptInFlag == "" {
				byDefault[r.SpecRuleID] = true
			}
		}
	}
	return enabled, byDefault
}

// TestFixtures_EveryRuleCovered requires a fixture directory for every rule
// that is enabled by default. Opt-in rules (SDK-001) are off unless a config
// flag turns them on, so they are linted by the schema lint instead.
func TestFixtures_EveryRuleCovered(t *testing.T) {
	_, byDefault := ruleIDs(t)
	have := map[string]bool{}
	for _, f := range loadFixtures(t) {
		have[f.Expect.Rule] = true
	}
	var missing []string
	for id := range byDefault {
		if !have[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("enabled rules without a testdata/conformance/<RULE> directory: %v", missing)
	}
}

var validStatuses = []evaluate.RuleStatus{
	evaluate.StatusEvaluated,
	evaluate.StatusNotApplicable,
	evaluate.StatusNotAssessable,
	evaluate.StatusNotImplemented,
	evaluate.StatusError,
}

// TestFixtures_WellFormed checks every rule directory without a cluster.
func TestFixtures_WellFormed(t *testing.T) {
	enabled := enabledRuleIDs(t)
	for _, f := range loadFixtures(t) {
		dirName := filepath.Base(f.Dir)
		t.Run(dirName, func(t *testing.T) {
			var errs []string
			fail := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

			if f.Expect.Rule != dirName {
				fail("rule %q does not match directory %q", f.Expect.Rule, dirName)
			}
			if !enabled[f.Expect.Rule] {
				fail("rule %q is not an enabled rule in configs/esql-mappings.yaml", f.Expect.Rule)
			}
			for _, need := range []string{"pass", "fail"} {
				if _, ok := f.Expect.Cases[need]; !ok {
					fail("missing required case %q", need)
				}
			}
			want := "conf-" + strings.ToLower(f.Expect.Rule)
			okName := func(svc string) bool {
				return svc == want || (strings.HasPrefix(svc, want+"-") && len(svc) > len(want)+1)
			}
			for _, name := range f.caseNames() {
				c := f.Expect.Cases[name]
				if !slices.Contains(validStatuses, c.Status) {
					fail("case %s: unknown status %q", name, c.Status)
				}
				if c.Status == evaluate.StatusEvaluated && len(c.Services) == 0 {
					fail("case %s: status evaluated needs services", name)
				}
				if c.Status != evaluate.StatusEvaluated && len(c.Services) != 0 {
					fail("case %s: services are only checked for status evaluated", name)
				}
				if c.Status != evaluate.StatusEvaluated && c.Scored != nil {
					fail("case %s: scored is only checked for status evaluated", name)
				}
				for svc := range c.Services {
					if !okName(svc) && svc != score.MissingServiceName {
						fail("case %s: service %q, want %q, %q or %q", name, svc, want, want+"-<suffix>", score.MissingServiceName)
					}
				}
				s, err := decodeFile(f.casePath(name))
				if err != nil {
					fail("case %s: %v", name, err)
					continue
				}
				sum := otlpin.Summarize(s)
				if sum.Items() == 0 {
					fail("case %s: no telemetry", name)
				}
				limit := maxCaseSpan
				if sum.DataPoints == 0 {
					limit = maxNoMetricCaseSpan
				}
				if sum.Span() > limit {
					fail("case %s: spans %s, max %s (%d metric data points)", name, sum.Span(), limit, sum.DataPoints)
				}
				for _, svc := range resourceServiceNames(s) {
					if !okName(svc) && svc != "" {
						fail("case %s: resource service.name %q, want %q or %q (or absent/empty)", name, svc, want, want+"-<suffix>")
					}
				}
			}
			// Every .jsonl or .jsonl.gz in the directory is a case, and a
			// key has one file.
			entries, _ := os.ReadDir(f.Dir)
			seen := map[string]string{}
			for _, e := range entries {
				key, ok := caseKey(e.Name())
				if !ok || e.IsDir() {
					continue
				}
				if prev, dup := seen[key]; dup {
					fail("case %s has two files: %s and %s", key, prev, e.Name())
				}
				seen[key] = e.Name()
				if _, ok := f.Expect.Cases[key]; !ok {
					fail("%s has no case in expect.yaml", e.Name())
				}
			}
			if len(errs) > 0 {
				t.Fatalf("%d problem(s):\n  %s", len(errs), strings.Join(errs, "\n  "))
			}
		})
	}
}

// TestFixtures_SchemaCoversResourceOnlyKeys keeps _schema/full.jsonl in step
// with the semconv catalog: RES-004 references attributes.<key> for every
// resource-only key, so the schema lint needs each one on a span.
func TestFixtures_SchemaCoversResourceOnlyKeys(t *testing.T) {
	s, err := decodeFile(filepath.Join(fixtureRoot, "_schema", "full.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	rss := s.Traces.ResourceSpans()
	for i := 0; i < rss.Len(); i++ {
		sss := rss.At(i).ScopeSpans()
		for j := 0; j < sss.Len(); j++ {
			spans := sss.At(j).Spans()
			for k := 0; k < spans.Len(); k++ {
				for key := range spans.At(k).Attributes().All() {
					have[key] = true
				}
			}
		}
	}
	var missing []string
	for _, k := range semconv.ResourceOnlyKeys {
		if !have[k] {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("_schema/full.jsonl lacks span attributes for %d resource-only key(s) (add them to the \"schema step\" span): %v", len(missing), missing)
	}
}
