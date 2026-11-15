package compare

import (
	"strings"
	"testing"

	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/score"
)

func ro(ruleID string, passed bool, extent float64) score.RuleOutcome {
	return score.RuleOutcome{RuleID: ruleID, Impact: "Critical", Passed: passed, Extent: extent}
}

func svc(name string, total float64, rules ...score.RuleOutcome) score.ServiceScore {
	return score.ServiceScore{ServiceName: name, Score: total, Rules: rules}
}

func rep(services ...score.ServiceScore) *evaluate.Report {
	return &evaluate.Report{Score: &score.Result{Services: services}}
}

func TestCompare_IdenticalReportsPass(t *testing.T) {
	a := rep(svc("checkout", 100, ro("RES-005", true, 0), ro("RES-001", true, 0)))
	b := rep(svc("checkout", 100, ro("RES-005", true, 0), ro("RES-001", true, 0)))

	got := Compare(map[string]*evaluate.Report{"a": a, "b": b}, []string{"a", "b"}, DefaultOptions())
	if got.Verdict != "PASS" || got.ExitCode != 0 {
		t.Fatalf("verdict=%s exit=%d, want PASS/0", got.Verdict, got.ExitCode)
	}
	if len(got.Discrepancies) != 0 {
		t.Errorf("discrepancies = %+v", got.Discrepancies)
	}
}

func TestCompare_RulePassedMismatch(t *testing.T) {
	a := rep(svc("checkout", 100, ro("RES-005", true, 0)))
	b := rep(svc("checkout", 0, ro("RES-005", false, 1.0)))

	got := Compare(map[string]*evaluate.Report{"a": a, "b": b}, []string{"a", "b"}, DefaultOptions())
	if got.ExitCode != int(KindRulePassedMismatch) {
		t.Fatalf("exit = %d, want %d", got.ExitCode, KindRulePassedMismatch)
	}
	if got.Verdict != "FAIL" {
		t.Errorf("verdict = %s", got.Verdict)
	}
	// A boolean disagreement should not also emit an extent discrepancy for the same cell.
	for _, d := range got.Discrepancies {
		if d.Kind == KindExtentExceeded && d.RuleID == "RES-005" {
			t.Errorf("extent discrepancy should be subsumed by rule_passed mismatch")
		}
	}
}

func TestCompare_ExtentExceeded(t *testing.T) {
	// Same rule_passed, extent 0.0 vs 0.20 — over the default 0.05 tolerance.
	a := rep(svc("checkout", 80, ro("RES-005", true, 0.0)))
	b := rep(svc("checkout", 80, ro("RES-005", true, 0.20)))

	got := Compare(map[string]*evaluate.Report{"a": a, "b": b}, []string{"a", "b"}, DefaultOptions())
	if got.ExitCode != int(KindExtentExceeded) {
		t.Fatalf("exit = %d, want %d; discrepancies=%+v", got.ExitCode, KindExtentExceeded, got.Discrepancies)
	}
}

func TestCompare_ExtentWithinTolerancePasses(t *testing.T) {
	a := rep(svc("checkout", 80, ro("RES-005", true, 0.10)))
	b := rep(svc("checkout", 80, ro("RES-005", true, 0.13)))

	got := Compare(map[string]*evaluate.Report{"a": a, "b": b}, []string{"a", "b"}, DefaultOptions())
	if got.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0 (0.03 spread is within 0.05)", got.ExitCode)
	}
}

func TestCompare_ScoreExceeded(t *testing.T) {
	// rule outcomes agree, but the per-service score differs by 25 points.
	a := rep(svc("checkout", 90, ro("RES-005", true, 0.0)))
	b := rep(svc("checkout", 65, ro("RES-005", true, 0.0)))

	got := Compare(map[string]*evaluate.Report{"a": a, "b": b}, []string{"a", "b"}, DefaultOptions())
	if got.ExitCode != int(KindScoreExceeded) {
		t.Fatalf("exit = %d, want %d", got.ExitCode, KindScoreExceeded)
	}
}

func TestCompare_ServiceDrift(t *testing.T) {
	// Backend b only covers 1 of 4 services -> 25% < 80% threshold.
	a := rep(
		svc("s1", 100, ro("RES-005", true, 0)),
		svc("s2", 100, ro("RES-005", true, 0)),
		svc("s3", 100, ro("RES-005", true, 0)),
		svc("s4", 100, ro("RES-005", true, 0)),
	)
	b := rep(svc("s1", 100, ro("RES-005", true, 0)))

	got := Compare(map[string]*evaluate.Report{"a": a, "b": b}, []string{"a", "b"}, DefaultOptions())
	if got.ExitCode != int(KindServiceDrift) {
		t.Fatalf("exit = %d, want %d; discrepancies=%+v", got.ExitCode, KindServiceDrift, got.Discrepancies)
	}
}

func TestCompare_ExitCodePrecedence(t *testing.T) {
	// A rule_passed mismatch AND a score gap — exit must be the most severe (1).
	a := rep(svc("checkout", 100, ro("RES-005", true, 0)))
	b := rep(svc("checkout", 50, ro("RES-005", false, 1.0)))

	got := Compare(map[string]*evaluate.Report{"a": a, "b": b}, []string{"a", "b"}, DefaultOptions())
	if got.ExitCode != int(KindRulePassedMismatch) {
		t.Fatalf("exit = %d, want %d (rule_passed beats score)", got.ExitCode, KindRulePassedMismatch)
	}
}

func TestCompare_ExpectedGapNotAFailure(t *testing.T) {
	// opensearch has no MET-001 outcome; that's a declared expected gap.
	withMetric := rep(svc("checkout", 100, ro("RES-005", true, 0), ro("MET-001", true, 0)))
	noMetric := rep(svc("checkout", 100, ro("RES-005", true, 0)))

	opts := DefaultOptions()
	opts.ExpectedGaps = map[string][]string{"opensearch": {"MET-001"}}

	got := Compare(
		map[string]*evaluate.Report{"elastic": withMetric, "opensearch": noMetric},
		[]string{"elastic", "opensearch"},
		opts,
	)
	if got.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0 — a missing rule is 'not evaluated', never a failure", got.ExitCode)
	}
	// The coverage section should still surface the gap as expected.
	md := RenderMarkdown(got)
	if !strings.Contains(md, "gap (expected)") {
		t.Errorf("coverage table should mark MET-001 on opensearch as an expected gap:\n%s", md)
	}
}

func TestCompare_MissingRuleIsNotEvaluatedNotMismatch(t *testing.T) {
	// One backend has the rule, the other simply produced no row. Not a mismatch.
	a := rep(svc("checkout", 100, ro("RES-005", true, 0), ro("SPA-003", true, 0)))
	b := rep(svc("checkout", 100, ro("RES-005", true, 0)))

	got := Compare(map[string]*evaluate.Report{"a": a, "b": b}, []string{"a", "b"}, DefaultOptions())
	if got.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", got.ExitCode)
	}
}

func TestRenderMarkdown_GridShownOnlyForDisagreements(t *testing.T) {
	a := rep(svc("checkout", 100, ro("RES-005", true, 0)))
	b := rep(svc("checkout", 0, ro("RES-005", false, 1.0)))
	got := Compare(map[string]*evaluate.Report{"a": a, "b": b}, []string{"a", "b"}, DefaultOptions())

	md := RenderMarkdown(got)
	if !strings.Contains(md, "## Rule × service grid") {
		t.Error("missing grid header")
	}
	if !strings.Contains(md, "RES-005") {
		t.Error("disagreeing cell should appear in grid")
	}
}

func TestRenderMarkdown_NotScoredBackendRow(t *testing.T) {
	rep := &Report{
		Labels: []string{"a", "b"},
		Backends: []BackendSummary{
			{Label: "a", Scored: true, Overall: 80, Category: "Good"},
			{Label: "b", Scored: false},
		},
	}
	got := RenderMarkdown(rep)
	if !strings.Contains(got, "| a | 80.0 | Good |") {
		t.Errorf("scored row wrong:\n%s", got)
	}
	if !strings.Contains(got, "| b | not scored |  |") {
		t.Errorf("not-scored row wrong:\n%s", got)
	}
}
