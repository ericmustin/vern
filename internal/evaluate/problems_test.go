package evaluate

import (
	"strings"
	"testing"
)

func TestProblems_RuleErrors(t *testing.T) {
	rep := &Report{Rules: []RuleResult{
		{RuleID: "RES-005", Status: StatusError},
		{RuleID: "SPA-001", Status: StatusEvaluated},
		{RuleID: "LOG-002", Status: StatusError},
	}}
	got := Problems(rep)
	if len(got) != 1 {
		t.Fatalf("problems = %q, want 1 sentence", got)
	}
	for _, id := range []string{"RES-005", "LOG-002", "2 rule(s) failed with an error"} {
		if !strings.Contains(got[0], id) {
			t.Errorf("%q missing %q", got[0], id)
		}
	}
}

func TestProblems_NothingIndexed(t *testing.T) {
	cases := []struct {
		name string
		s    *SampleInfo
		want bool
	}{
		{"nil sample", nil, false},
		{"empty sample", &SampleInfo{}, false},
		{"nothing dropped", &SampleInfo{Spans: 5}, false},
		{"all spans dropped", &SampleInfo{Spans: 5, Dropped: map[string]int{"traces": 5}}, true},
		{"some spans dropped", &SampleInfo{Spans: 5, Dropped: map[string]int{"traces": 4}}, false},
		{"traces dropped, logs kept", &SampleInfo{Spans: 5, LogRecords: 2, Dropped: map[string]int{"traces": 5}}, false},
		{"everything dropped", &SampleInfo{Spans: 5, SpanEvents: 1, LogRecords: 2, DataPoints: 3,
			Dropped: map[string]int{"traces": 5, "logs": 3, "metrics": 3}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Problems(&Report{Sample: c.s})
			if (len(got) == 1 && strings.Contains(got[0], "no items were indexed")) != c.want {
				t.Errorf("problems = %q, want nothing-indexed=%v", got, c.want)
			}
		})
	}
}

func TestProblems_Order(t *testing.T) {
	rep := &Report{
		Rules:  []RuleResult{{RuleID: "RES-005", Status: StatusError}},
		Sample: &SampleInfo{Spans: 1, Dropped: map[string]int{"traces": 1}},
	}
	got := Problems(rep)
	if len(got) != 2 || !strings.Contains(got[0], "RES-005") || !strings.Contains(got[1], "no items were indexed") {
		t.Errorf("problems = %q", got)
	}
}

func TestProblems_CleanIsNil(t *testing.T) {
	rep := &Report{Rules: []RuleResult{{RuleID: "RES-005", Status: StatusEvaluated}}, Sample: &SampleInfo{Spans: 3}}
	if got := Problems(rep); got != nil {
		t.Errorf("problems = %q, want nil", got)
	}
	if got := Problems(nil); got != nil {
		t.Errorf("nil report: %q", got)
	}
}
