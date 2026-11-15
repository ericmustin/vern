package score

import (
	"math"
	"testing"
)

func TestCalculate_PerfectScore(t *testing.T) {
	outs := []Outcome{
		{RuleID: "RES-005", Impact: "Critical", ServiceName: "svc-a", Passed: true},
		{RuleID: "MET-001", Impact: "Important", ServiceName: "svc-a", Passed: true},
		{RuleID: "RES-001", Impact: "Normal", ServiceName: "svc-a", Passed: true},
		{RuleID: "MET-006", Impact: "Low", ServiceName: "svc-a", Passed: true},
	}
	got := Calculate(outs)
	if len(got.Services) != 1 {
		t.Fatalf("want 1 service, got %d", len(got.Services))
	}
	if got.Services[0].Score != 100 {
		t.Errorf("svc-a score = %v, want 100", got.Services[0].Score)
	}
	if got.Services[0].Category != "Excellent" {
		t.Errorf("svc-a category = %q, want Excellent", got.Services[0].Category)
	}
	if got.Overall != 100 {
		t.Errorf("overall = %v, want 100", got.Overall)
	}
}

func TestCalculate_WeightedFormula(t *testing.T) {
	// Critical fails, everything else passes:
	// numerator   = 0*40 + 1*30 + 1*20 + 1*10 = 60
	// denominator = 1*40 + 1*30 + 1*20 + 1*10 = 100
	// score = 60.0
	outs := []Outcome{
		{RuleID: "RES-005", Impact: "Critical", ServiceName: "svc-a", Passed: false, Extent: 0.5},
		{RuleID: "MET-001", Impact: "Important", ServiceName: "svc-a", Passed: true},
		{RuleID: "RES-001", Impact: "Normal", ServiceName: "svc-a", Passed: true},
		{RuleID: "MET-006", Impact: "Low", ServiceName: "svc-a", Passed: true},
	}
	got := Calculate(outs)
	if math.Abs(got.Services[0].Score-60) > 0.0001 {
		t.Errorf("svc-a score = %v, want 60", got.Services[0].Score)
	}
	if got.Services[0].Category != "Needs Improvement" {
		t.Errorf("svc-a category = %q, want Needs Improvement", got.Services[0].Category)
	}
}

func TestCalculate_MultipleServicesFleetRollup(t *testing.T) {
	// svc-a: all 4 pass  -> score 100
	// svc-b: only critical passes -> score = 40/100 = 40
	// fleet: passed-weighted = 1*40 + 1*30 + 1*20 + 1*10 (a) + 1*40 (b) = 140
	//        total-weighted  = 40 + 30 + 20 + 10        (a) + 40+30+20+10 (b) = 200
	//        overall = 140/200 * 100 = 70
	outs := []Outcome{
		{RuleID: "RES-005", Impact: "Critical", ServiceName: "svc-a", Passed: true},
		{RuleID: "MET-001", Impact: "Important", ServiceName: "svc-a", Passed: true},
		{RuleID: "RES-001", Impact: "Normal", ServiceName: "svc-a", Passed: true},
		{RuleID: "MET-006", Impact: "Low", ServiceName: "svc-a", Passed: true},

		{RuleID: "RES-005", Impact: "Critical", ServiceName: "svc-b", Passed: true},
		{RuleID: "MET-001", Impact: "Important", ServiceName: "svc-b", Passed: false},
		{RuleID: "RES-001", Impact: "Normal", ServiceName: "svc-b", Passed: false},
		{RuleID: "MET-006", Impact: "Low", ServiceName: "svc-b", Passed: false},
	}
	got := Calculate(outs)
	if len(got.Services) != 2 {
		t.Fatalf("want 2 services, got %d", len(got.Services))
	}
	if got.Services[0].ServiceName != "svc-a" || got.Services[1].ServiceName != "svc-b" {
		t.Errorf("services not sorted: %+v", []string{got.Services[0].ServiceName, got.Services[1].ServiceName})
	}
	if math.Abs(got.Services[1].Score-40) > 0.0001 {
		t.Errorf("svc-b score = %v, want 40", got.Services[1].Score)
	}
	if math.Abs(got.Overall-70) > 0.0001 {
		t.Errorf("overall = %v, want 70", got.Overall)
	}
	if got.Category != "Needs Improvement" {
		t.Errorf("overall category = %q, want Needs Improvement (70 < 75)", got.Category)
	}
}

func TestCalculate_CategoryBoundaries(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{100, "Excellent"},
		{90, "Excellent"},
		{89.99, "Good"},
		{75, "Good"},
		{74.99, "Needs Improvement"},
		{50, "Needs Improvement"},
		{49.99, "Poor"},
		{0, "Poor"},
	}
	for _, c := range cases {
		if got := Categorize(c.score); got != c.want {
			t.Errorf("Categorize(%v) = %q, want %q", c.score, got, c.want)
		}
	}
}

func TestCalculate_UnknownImpactDropped(t *testing.T) {
	// A rule with an unrecognised impact should be silently ignored so it
	// cannot skew the score with weight 0.
	outs := []Outcome{
		{RuleID: "WEIRD-1", Impact: "Mystery", ServiceName: "svc-a", Passed: false},
		{RuleID: "RES-005", Impact: "Critical", ServiceName: "svc-a", Passed: true},
	}
	got := Calculate(outs)
	if got.Services[0].Score != 100 {
		t.Errorf("score = %v, want 100 (mystery impact should be dropped)", got.Services[0].Score)
	}
}

func TestCalculate_EmptyServiceNameDropped(t *testing.T) {
	outs := []Outcome{
		{RuleID: "RES-005", Impact: "Critical", ServiceName: "", Passed: false},
	}
	got := Calculate(outs)
	if len(got.Services) != 0 {
		t.Errorf("want 0 services, got %d", len(got.Services))
	}
}

func TestCalculate_ZeroDenominatorIsZeroNotNaN(t *testing.T) {
	got := Calculate(nil)
	if got.Overall != 0 {
		t.Errorf("overall = %v, want 0", got.Overall)
	}
}

func TestNormalizeImpact(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"critical", "Critical", true},
		{" Important ", "Important", true},
		{"LOW", "Low", true},
		{"urgent", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeImpact(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeImpact(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCalculate_LowercaseImpactCounts(t *testing.T) {
	got := Calculate([]Outcome{
		{RuleID: "R1", Impact: "critical", Passed: false, ServiceName: "a"},
		{RuleID: "R2", Impact: "Normal", Passed: true, ServiceName: "a"},
	})
	want := 20.0 / 60.0 * 100
	if math.Abs(got.Overall-want) > 0.0001 {
		t.Errorf("overall = %v, want %v", got.Overall, want)
	}
	if _, ok := got.Counts["critical"]; ok {
		t.Errorf("fleet counts has non-canonical key: %v", got.Counts)
	}
	if _, ok := got.Services[0].Counts["Critical"]; !ok {
		t.Errorf("service counts missing canonical Critical: %v", got.Services[0].Counts)
	}
	for _, r := range got.Services[0].Rules {
		if r.RuleID == "R1" && r.Impact != "Critical" {
			t.Errorf("rule impact = %q, want Critical", r.Impact)
		}
	}
}

func TestCalculate_NoOutcomesIsNotScored(t *testing.T) {
	got := Calculate(nil)
	if got.Scored || got.Category != "" || got.Overall != 0 {
		t.Errorf("got Scored=%v Category=%q Overall=%v, want false, \"\", 0", got.Scored, got.Category, got.Overall)
	}
	if !Calculate([]Outcome{{RuleID: "R", Impact: "Low", ServiceName: "a", Passed: true}}).Scored {
		t.Error("non-empty run should be Scored")
	}
}

func TestFormat(t *testing.T) {
	cases := map[float64]string{0: "0.0", 100: "100.0", 6.25: "6.2", 6.35: "6.3", 72.45: "72.5", 49.96: "50.0", 33.333333: "33.3"}
	for v, want := range cases {
		if got := Format(v); got != want {
			t.Errorf("Format(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestCalculate_DisplayAndCategoryAgree(t *testing.T) {
	// 89.96 -> passes 8996 of 10000 weight units (Critical=40 -> total 250 * 40).
	mk := func(passed, total int) *Result {
		var outs []Outcome
		for i := 0; i < total; i++ {
			outs = append(outs, Outcome{RuleID: "R", Impact: "Low", ServiceName: "s", Passed: i < passed})
		}
		return Calculate(outs)
	}
	r := mk(2249, 2500) // 89.96
	if r.Overall < 89.95 || r.Overall >= 90 {
		t.Fatalf("setup: overall = %v", r.Overall)
	}
	if r.OverallDisplay != "90.0" || r.Category != "Excellent" {
		t.Errorf("89.96 -> %q %q", r.OverallDisplay, r.Category)
	}
	if got := r.Services[0].ScoreDisplay; got != "90.0" || r.Services[0].Category != "Excellent" {
		t.Errorf("service 89.96 -> %q %q", got, r.Services[0].Category)
	}
	r = mk(1, 16) // 6.25
	if r.OverallDisplay != "6.2" {
		t.Errorf("6.25 -> %q", r.OverallDisplay)
	}
	if r.Services[0].ScoreDisplay != "6.2" {
		t.Errorf("service 6.25 -> %q", r.Services[0].ScoreDisplay)
	}
}
