package report

import (
	"strings"
	"testing"
	"time"

	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/score"
)

func TestRenderMarkdown_HappyPath(t *testing.T) {
	rep := &evaluate.Report{
		EvaluatedAt:    time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
		Backend:        "clickhouse",
		MappingsFile:   "configs/clickhouse-mappings.yaml",
		RulesAttempted: 2,
		RulesSucceeded: 2,
		Score: &score.Result{
			Scored:   true,
			Overall:  75,
			Category: "Good",
			Counts: map[string]score.Tally{
				"Critical": {Passed: 1, Total: 1},
				"Normal":   {Passed: 0, Total: 1},
			},
			Services: []score.ServiceScore{
				{
					ServiceName: "svc-a",
					Score:       75,
					Category:    "Good",
					Rules: []score.RuleOutcome{
						{RuleID: "RES-001", Impact: "Normal", Passed: false, Extent: 0.42, Example: "trace-123"},
						{RuleID: "RES-005", Impact: "Critical", Passed: true},
					},
				},
			},
		},
	}

	got := RenderMarkdown(rep)
	mustContain(t, got, "# Vern Instrumentation Score Report")
	mustContain(t, got, "- Evaluated: 2026-05-15T12:00:00Z")
	mustContain(t, got, "- Backend: clickhouse")
	mustContain(t, got, "- Score: 75.0 (Good)")
	mustContain(t, got, "- Critical: 1/1")
	mustContain(t, got, "- Normal: 0/1")
	mustContain(t, got, "### svc-a — 75.0 (Good)")
	mustContain(t, got, "| RES-001 | Normal | fail | 0.42 | trace-123 |")
	mustContain(t, got, "| RES-005 | Critical | pass | 0.00 |  |")
}

func TestRenderMarkdown_RuleFailureSection(t *testing.T) {
	rep := &evaluate.Report{
		EvaluatedAt:    time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
		Backend:        "elastic",
		RulesAttempted: 2,
		RulesSucceeded: 1,
		Failures: []evaluate.RuleFailure{
			{RuleID: "MET-001", Error: "HTTP 500: kaboom"},
		},
		Rules: []evaluate.RuleResult{
			{RuleID: "MET-001", Impact: "Normal", Status: evaluate.StatusError, Reason: "HTTP 500: kaboom"},
		},
		Score: &score.Result{Scored: true, Overall: 100, Category: "Excellent"},
	}
	got := RenderMarkdown(rep)
	mustContain(t, got, "## Not scored")
	mustContain(t, got, "- **MET-001**: error — HTTP 500: kaboom")
	if strings.Contains(got, "## Rule failures") {
		t.Error("Rule failures section should be gone")
	}
}

func TestRenderMarkdown_EscapesPipeInExample(t *testing.T) {
	rep := &evaluate.Report{
		EvaluatedAt: time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
		Backend:     "elastic",
		Rules: []evaluate.RuleResult{{RuleID: "X", Impact: "Critical", Status: evaluate.StatusEvaluated, Scored: true,
			Heuristic: true, Deviation: "a|b"}},
		Score: &score.Result{
			Services: []score.ServiceScore{{
				ServiceName: "svc",
				Score:       100,
				Category:    "Excellent",
				Rules: []score.RuleOutcome{
					{RuleID: "X", Impact: "Critical", Passed: true, Example: "a|b|c"},
				},
			}},
		},
	}
	got := RenderMarkdown(rep)
	mustContain(t, got, `a\|b\|c`)
	mustContain(t, got, `heuristic; a\|b`)
}

func sampleReport() *evaluate.Report {
	return &evaluate.Report{
		SchemaVersion:  2,
		SpecVersion:    "0.1",
		EvaluatedAt:    time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
		Backend:        "elastic",
		Partial:        true,
		PartialReasons: []string{"1 rule not implemented"},
		Rules: []evaluate.RuleResult{
			{RuleID: "RES-001", Impact: "Critical", Status: evaluate.StatusEvaluated, Scored: true,
				Remediation: "Set service.name on all resources.",
				Outcomes: []score.Outcome{
					{ServiceName: "svc-b", RuleID: "RES-001", Impact: "Critical", Passed: false},
					{ServiceName: "svc-a", RuleID: "RES-001", Impact: "Critical", Passed: false},
					{ServiceName: "svc-a", RuleID: "RES-001", Impact: "Critical", Passed: false},
					{ServiceName: "svc-c", RuleID: "RES-001", Impact: "Critical", Passed: true},
				}},
			{RuleID: "RES-005", Impact: "Normal", Status: evaluate.StatusEvaluated, Scored: true,
				Remediation: "unused because passing",
				Outcomes:    []score.Outcome{{ServiceName: "svc-a", RuleID: "RES-005", Impact: "Normal", Passed: true}}},
			{RuleID: "SPA-003", Impact: "Low", Status: evaluate.StatusEvaluated, Advisory: true},
			{RuleID: "MET-001", Impact: "Important", Status: evaluate.StatusNotImplemented, Reason: "no mapping"},
			{RuleID: "LOG-001", Impact: "Low", Status: evaluate.StatusNotApplicable},
		},
		Score: &score.Result{Scored: true, Overall: 60, Category: "Fair"},
	}
}

func TestRenderMarkdown_PartialBannerAndHowToFix(t *testing.T) {
	got := RenderMarkdown(sampleReport())
	mustContain(t, got, "> **Partial score:** 2 of 5 rules scored. See *Not scored* below.")
	mustContain(t, got, "| RES-001 | Critical | evaluated | svc-a, svc-b |  |")
	mustContain(t, got, "- **RES-001**: Set service.name on all resources.")
	if strings.Contains(got, "unused because passing") {
		t.Error("remediation shown for passing rule")
	}
	mustContain(t, got, "- **SPA-003**: advisory — shown for information, not scored")
	mustContain(t, got, "- **MET-001**: not_implemented — no mapping")
	mustContain(t, got, "- **LOG-001**: not_applicable\n")
	t.Log("\n" + got)
}

func TestRenderMarkdown_NotScored(t *testing.T) {
	rep := &evaluate.Report{
		EvaluatedAt: time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
		Backend:     "elastic",
		Score:       &score.Result{},
	}
	got := RenderMarkdown(rep)
	mustContain(t, got, "- Score: not scored — no rule could be evaluated")
	if strings.Contains(got, "(Poor)") || strings.Contains(got, "0.0") {
		t.Errorf("must not show a numeric score:\n%s", got)
	}
}

func mustContain(t *testing.T, body, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Errorf("body does not contain %q\n--- body ---\n%s", want, body)
	}
}

func TestRenderMarkdown_SampleSectionAndDroppedBanner(t *testing.T) {
	rep := &evaluate.Report{
		EvaluatedAt: time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
		Backend:     "esql",
		Score:       &score.Result{Scored: true, Overall: 80, Category: "Good"},
		Sample: &evaluate.SampleInfo{
			Files: []string{"a.json", "b.json"}, Spans: 3, SpanEvents: 1, LogRecords: 2, DataPoints: 4,
			Services: []string{"checkout", "cart"}, Signals: []string{"logs", "traces"},
			OriginalStart: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			OriginalEnd:   time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC),
			Dropped:       map[string]int{"metrics": 5, "traces": 2},
			Notes:         []string{"skipped a thing"},
		},
	}
	out := RenderMarkdown(rep)
	banner := "> **At least 7 items were not indexed and are not reflected in this score:** traces 2, metrics at least 5."
	if !strings.Contains(out, banner) {
		t.Fatalf("missing banner:\n%s", out)
	}
	if strings.Index(out, banner) > strings.Index(out, "## Overall") {
		t.Fatal("banner must precede the score")
	}
	for _, want := range []string{"## Sample", "a.json, b.json", "2026-01-01T00:00:00Z", "2026-01-01T00:05:00Z",
		"Spans: 3", "Span events: 1", "Log records: 2", "Data points: 4", "checkout, cart", "logs, traces", "- skipped a thing"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Index(out, "## Sample") < strings.Index(out, "## Overall") {
		t.Fatal("sample section must follow the score")
	}
	rep.Sample.Dropped = map[string]int{"traces": 2, "logs": 1}
	if want := "> **3 items were not indexed and are not reflected in this score:** traces 2, logs 1."; !strings.Contains(RenderMarkdown(rep), want) {
		t.Fatalf("exact banner without metrics: want %q in\n%s", want, RenderMarkdown(rep))
	}
	rep.Sample.Dropped = nil
	if strings.Contains(RenderMarkdown(rep), "were not indexed") {
		t.Fatal("banner without drops")
	}
}

func TestEscapeMD(t *testing.T) {
	cases := []struct{ in, want string }{
		{"svc-a", "svc-a"},
		{"a|b", `a\|b`},
		{"`code`", "\\`code\\`"},
		{"[x](http://evil)", `\[x\](http:\/\/evil)`},
		{"![img](http://evil/p.png)", `!\[img\](http:\/\/evil/p.png)`},
		{"<img src=x onerror=alert(1)>", "&lt;img src=x onerror=alert(1)&gt;"},
		{"*bold* _it_", `\*bold\* \_it\_`},
		{"line1\nline2\r\nline3\rend", "line1 line2 line3 end"},
		{`back\slash`, `back\\slash`},
		{`\|`, `\\\|`},
		{"see http://evil.example/x", `see http:\/\/evil.example/x`},
		{"https://e.co", `https:\/\/e.co`},
		{"www.evil.example", `www\.evil.example`},
		{"go to www.evil.example", `go to www\.evil.example`},
		{"# Owned", `\# Owned`},
		{"## Owned", `\## Owned`},
		{"- item", `\- item`},
		{"+ item", `\+ item`},
		{"> quote", "&gt; quote"},
		{"1. first", `1\. first`},
		{"12) first", `12\) first`},
		{"  # indented", `  \# indented`},
		{"a # b - c + d 1. e", "a # b - c + d 1. e"},
		{"svc-1.2", "svc-1.2"},
		{"", ""},
	}
	for _, c := range cases {
		if got := escapeMD(c.in); got != c.want {
			t.Errorf("escapeMD(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRenderMarkdown_EscapesUserStrings feeds hostile service names, file
// names, notes, examples and reasons through the renderer: none may break a
// table row or render as a link, image or HTML.
func TestRenderMarkdown_EscapesUserStrings(t *testing.T) {
	evil := "x|y\n## Owned [link](http://e) ![i](http://e/i.png) <b>*_`"
	rep := &evaluate.Report{
		EvaluatedAt:  time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC),
		Backend:      "esql",
		MappingsFile: "m_[1].yaml",
		Rules: []evaluate.RuleResult{
			{RuleID: "RES-001", Impact: "Critical", Status: evaluate.StatusEvaluated, Scored: true,
				Outcomes: []score.Outcome{{ServiceName: evil, RuleID: "RES-001", Impact: "Critical", Passed: false}}},
			{RuleID: "MET-001", Impact: "Normal", Status: evaluate.StatusError, Reason: evil},
		},
		Score: &score.Result{Scored: true, Overall: 0, Category: "Poor", Services: []score.ServiceScore{{
			ServiceName: evil, Category: "Poor",
			Rules: []score.RuleOutcome{{RuleID: "RES-001", Impact: "Critical", Example: evil}},
		}}},
		Sample: &evaluate.SampleInfo{Files: []string{evil}, Services: []string{evil}, Notes: []string{evil}},
	}
	out := RenderMarkdown(rep)
	const esc = `x\|y ## Owned \[link\](http:\/\/e) !\[i\](http:\/\/e/i.png) &lt;b&gt;\*\_` + "\\`"
	for _, want := range []string{
		"| RES-001 | Critical | evaluated | " + esc + " |  |",
		"### " + esc + " — 0.0 (Poor)",
		"| RES-001 | Critical | fail | 0.00 | " + esc + " |",
		"- **MET-001**: error — " + esc,
		"- Files: " + esc,
		"- Services: " + esc,
		"- " + esc + "\n",
		`- Mappings: m\_\[1\].yaml`,
	} {
		mustContain(t, out, want)
	}
	for _, bad := range []string{"\n## Owned", "[link](", "[i](", "<b>", "x|y", "http://"} {
		if strings.Contains(out, bad) {
			t.Errorf("unescaped %q in\n%s", bad, out)
		}
	}
}

// TestMarkdown_BlindSpots: the sample's blind spots get their own
// subsection under Sample, escaped like every other value.
func TestMarkdown_BlindSpots(t *testing.T) {
	rep := &evaluate.Report{
		Score: &score.Result{Scored: true, Overall: 90, Category: "Excellent"},
		Sample: &evaluate.SampleInfo{BlindSpots: []string{
			"12 values longer than 1024 characters couldn't be scanned for sensitive data",
			"SPA-006: values longer than 260 characters were scanned only up to the first 260 characters",
			"<b>*x*</b>",
		}},
	}
	out := RenderMarkdown(rep)
	sample, blind := strings.Index(out, "## Sample"), strings.Index(out, "### Blind spots")
	if sample < 0 || blind < sample {
		t.Fatalf("no Blind spots subsection under Sample:\n%s", out)
	}
	for _, want := range []string{
		"### Blind spots\n\nThe sensitive-data rules could not scan these parts of the sample; a pass says nothing about them.\n\n",
		"- 12 values longer than 1024 characters couldn't be scanned for sensitive data\n",
		"- SPA-006: values longer than 260 characters were scanned only up to the first 260 characters\n",
		`- &lt;b&gt;\*x\*&lt;/b&gt;` + "\n",
	} {
		mustContain(t, out, want)
	}
	rep.Sample.BlindSpots = nil
	if strings.Contains(RenderMarkdown(rep), "Blind spots") {
		t.Error("Blind spots subsection without blind spots")
	}
}
