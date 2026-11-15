package evaluate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/score"
	"github.com/ericmustin/vern/internal/sensitive"
)

const (
	canaryEmail = "alice@example.com"
	canaryKey   = "sk-proj-canaryCANARY1234567890"
)

// sensitiveMappingsYAML has one sensitive rule (query "SENS") and one
// ordinary rule (query "PLAIN").
const sensitiveMappingsYAML = `
backend: esql
rules:
  - spec_rule_id: "SPA-006"
    enabled: true
    sensitive: true
    scan_groups: [Span]
    impact: "Critical"
    target: "Span"
    query: "SENS"
  - spec_rule_id: "RES-005"
    enabled: true
    impact: "Critical"
    target: "Resource"
    query: "PLAIN"
`

// rawExample is a query example as Elasticsearch returns it: segments of
// <field>U+001F<value> joined by U+001E, each preceded by U+001E.
func rawExample(pairs ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		b.WriteString(sensitive.SegmentSep + sensitive.FormatExample(pairs[i], pairs[i+1]))
	}
	return b.String()
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestRun_RedactsSensitiveExamples: a failing outcome of a sensitive rule
// carries only the redacted example; a passing one keeps its generated text.
// The matched value appears nowhere in the report.
func TestRun_RedactsSensitiveExamples(t *testing.T) {
	cfg := baseCfg(writeMappings(t, sensitiveMappingsYAML))
	exec := &fakeExec{byQuery: map[string][]Row{
		"SENS": {
			{ServiceName: "leaky", Passed: false, Extent: 1, Example: rawExample(
				"name", "GET /login", "attributes.user.email", canaryEmail, "attributes.note", "key "+canaryKey)},
			{ServiceName: "clean", Passed: true, Example: "No sensitive data matched 22 checks"},
		},
		"PLAIN": {{ServiceName: "leaky", Passed: true}},
	}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{FieldsDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	rr := ruleByID(t, rep, "SPA-006")
	if !rr.Sensitive {
		t.Error("SPA-006 result not marked sensitive")
	}
	got := map[string]string{}
	for _, o := range rr.Outcomes {
		got[o.ServiceName] = o.Example
	}
	// user.email is a sensitive key, which Locate checks before the
	// value detectors.
	if want := "attributes.user.email: sensitive key name (al…om (17 chars))"; got["leaky"] != want {
		t.Errorf("failing example = %q, want %q", got["leaky"], want)
	}
	if want := "No sensitive data matched 22 checks"; got["clean"] != want {
		t.Errorf("passing example = %q, want %q verbatim", got["clean"], want)
	}
	js := mustJSON(t, rep)
	for _, c := range []string{canaryEmail, canaryKey, sensitive.FieldSep, sensitive.SegmentSep, `\u001e`, `\u001f`} {
		if strings.Contains(js, c) {
			t.Errorf("report JSON contains %q", c)
		}
	}
}

// TestRun_RedactsUnlocatedExample: a failing example Go cannot locate
// becomes the fixed placeholder, never the raw text.
func TestRun_RedactsUnlocatedExample(t *testing.T) {
	cfg := baseCfg(writeMappings(t, sensitiveMappingsYAML))
	exec := &fakeExec{byQuery: map[string][]Row{
		"SENS": {{ServiceName: "svc", Passed: false, Extent: 1, Example: "no segments here, just " + canaryEmail[:5]}},
	}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{FieldsDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	if ex := ruleByID(t, rep, "SPA-006").Outcomes[0].Example; ex != "<field unknown>: <detector unknown> (redacted)" {
		t.Errorf("example = %q", ex)
	}
}

// TestRun_RedactsHugeExample: an example near ES|QL's 1 MiB limit comes
// back as one short line.
func TestRun_RedactsHugeExample(t *testing.T) {
	cfg := baseCfg(writeMappings(t, sensitiveMappingsYAML))
	big := rawExample("body.text", strings.Repeat("lorem ipsum ", 1<<20/12)+canaryEmail)
	exec := &fakeExec{byQuery: map[string][]Row{"SENS": {{ServiceName: "svc", Passed: false, Extent: 1, Example: big}}}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{FieldsDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	ex := ruleByID(t, rep, "SPA-006").Outcomes[0].Example
	if !strings.HasPrefix(ex, "body.text: email address (lo…om (") || len(ex) > 100 {
		t.Errorf("example (%d bytes) = %.120q", len(ex), ex)
	}
}

// TestRun_OrdinaryExamples: an ordinary rule's example is evidence
// the user needs (span, scope and metric names) and passes verbatim, unless
// a detector matches it: then it is withheld, since no matched value may
// reach a report through any rule.
func TestRun_OrdinaryExamples(t *testing.T) {
	cases := map[string]struct{ raw, want string }{
		"clean stays untouched": {
			raw:  "span GET /users/{id} lacks http.route (3f2b8c1e-9a4d-4e6f-8b2a-1c3d5e7f9a0b)",
			want: "span GET /users/{id} lacks http.route (3f2b8c1e-9a4d-4e6f-8b2a-1c3d5e7f9a0b)",
		},
		"canary is withheld": {
			raw:  "orders." + canaryEmail + ".count",
			want: "example withheld: it matches email address",
		},
		"api key is withheld": {
			raw:  "traces from lib " + canaryKey,
			want: "example withheld: it matches API key",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := baseCfg(writeMappings(t, sensitiveMappingsYAML))
			exec := &fakeExec{byQuery: map[string][]Row{
				"PLAIN": {
					{ServiceName: "fail", Passed: false, Extent: 0.5, Example: tc.raw},
					{ServiceName: "pass", Passed: true, Example: tc.raw},
				},
			}}
			rep, err := Run(context.Background(), cfg, exec, RunOptions{FieldsDiscovered: true})
			if err != nil {
				t.Fatal(err)
			}
			rr := ruleByID(t, rep, "RES-005")
			if rr.Sensitive {
				t.Error("RES-005 marked sensitive")
			}
			for _, o := range rr.Outcomes {
				if o.Example != tc.want {
					t.Errorf("%s: example = %q, want %q", o.ServiceName, o.Example, tc.want)
				}
			}
			for _, c := range []string{canaryEmail, canaryKey} {
				if strings.Contains(mustJSON(t, rep), c) {
					t.Errorf("report JSON contains %q", c)
				}
			}
		})
	}
}

type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) logf(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format+"\n", a...)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestRun_SensitiveQueryErrorReasonIsFixed: Elasticsearch can echo
// query text, which carries field and metric names, so a sensitive rule's
// error reason is a fixed classification. The log gets the full error only
// when the caller says its log is private (QueryErrorDetail).
func TestRun_SensitiveQueryErrorReasonIsFixed(t *testing.T) {
	echo := errors.New(`HTTP 400: {"error":{"reason":"Unknown column [metrics.orders.` + canaryEmail + `.count]"}}`)
	cases := map[string]struct {
		err        error
		detail     bool
		wantReason string
		wantLog    []string
		leakInLog  bool
	}{
		"http error":         {err: echo, wantReason: "query failed (HTTP 400)", wantLog: []string{"SPA-006", "query failed (HTTP 400)", "detail withheld"}},
		"http error, detail": {err: echo, detail: true, wantReason: "query failed (HTTP 400)", wantLog: []string{"SPA-006", canaryEmail}, leakInLog: true},
		"deadline":           {err: fmt.Errorf("post query: %w", context.DeadlineExceeded), wantReason: "query timed out"},
		"canceled":           {err: fmt.Errorf("post query: %w", context.Canceled), wantReason: "query canceled"},
		"other":              {err: errors.New("parse response: " + canaryEmail), wantReason: "query failed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := baseCfg(writeMappings(t, sensitiveMappingsYAML))
			exec := &fakeExec{errFor: map[string]error{"SENS": tc.err, "PLAIN": errors.New("plain failure")}}
			logs := &logBuf{}
			rep, err := Run(context.Background(), cfg, exec, RunOptions{Logf: logs.logf, QueryErrorDetail: tc.detail, FieldsDiscovered: true})
			if err != nil {
				t.Fatal(err)
			}
			rr := ruleByID(t, rep, "SPA-006")
			if rr.Status != StatusError || rr.Reason != tc.wantReason {
				t.Errorf("SPA-006 = %s %q, want error %q", rr.Status, rr.Reason, tc.wantReason)
			}
			// Ordinary rules keep their full error.
			if r := ruleByID(t, rep, "RES-005"); r.Reason != "plain failure" {
				t.Errorf("RES-005 reason = %q", r.Reason)
			}
			if js := mustJSON(t, rep); strings.Contains(js, canaryEmail) {
				t.Errorf("report JSON contains the canary: %s", js)
			}
			out := logs.String()
			for _, w := range tc.wantLog {
				if !strings.Contains(out, w) {
					t.Errorf("log lacks %q:\n%s", w, out)
				}
			}
			if !tc.leakInLog && strings.Contains(out, canaryEmail) {
				t.Errorf("log contains the canary without QueryErrorDetail:\n%s", out)
			}
		})
	}
}

// TestRun_SensitiveTemplateErrorReasonIsFixed: a sensitive rule whose query
// cannot be built gets a fixed reason too; the detail goes to the log.
func TestRun_SensitiveTemplateErrorReasonIsFixed(t *testing.T) {
	cfg := baseCfg(writeMappings(t, `
backend: esql
rules:
  - spec_rule_id: "SPA-006"
    enabled: true
    sensitive: true
    scan_groups: [Span]
    impact: "Critical"
    query: "{{ .NoSuchField }}"
`))
	logs := &logBuf{}
	rep, err := Run(context.Background(), cfg, &fakeExec{}, RunOptions{Logf: logs.logf, QueryErrorDetail: true})
	if err != nil {
		t.Fatal(err)
	}
	rr := ruleByID(t, rep, "SPA-006")
	if rr.Status != StatusError || rr.Reason != "query could not be built" {
		t.Errorf("SPA-006 = %s %q", rr.Status, rr.Reason)
	}
	if !strings.Contains(logs.String(), "NoSuchField") {
		t.Errorf("log lacks the template error:\n%s", logs.String())
	}
}

// TestRun_SensitiveRulesNeedFieldDiscovery: without a per-run field
// discovery (vern evaluate), a sensitive rule is not run and ends
// not_assessable with a reason that names vern score and vern serve, not
// "no matching telemetry". Ordinary rules are unaffected.
func TestRun_SensitiveRulesNeedFieldDiscovery(t *testing.T) {
	cfg := baseCfg(writeMappings(t, sensitiveMappingsYAML))
	exec := &fakeExec{byQuery: map[string][]Row{
		"SENS":  {{ServiceName: "svc", Passed: true, Example: "No sensitive data matched 1 checks"}},
		"PLAIN": {{ServiceName: "svc", Passed: true}},
	}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rr := ruleByID(t, rep, "SPA-006")
	if rr.Status != StatusNotAssessable || rr.Reason != mappings.NeedsFieldDiscovery || rr.Scored {
		t.Errorf("SPA-006: status %s, reason %q, scored %v", rr.Status, rr.Reason, rr.Scored)
	}
	if rr.Reason != "needs per-run field discovery: run vern score or vern serve" {
		t.Errorf("reason = %q", rr.Reason)
	}
	for _, q := range exec.seen {
		if strings.HasSuffix(q, "SENS") {
			t.Error("the sensitive query ran without field discovery")
		}
	}
	if got := ruleByID(t, rep, "RES-005").Status; got != StatusEvaluated {
		t.Errorf("RES-005: status %s, want evaluated", got)
	}
	if !rep.Partial || !strings.Contains(strings.Join(rep.PartialReasons, "\n"), "SPA-006: not_assessable — needs per-run field discovery") {
		t.Errorf("partial reasons = %q", rep.PartialReasons)
	}
}

// TestRun_MasksMatchingServiceNames: a service name a detector
// matches becomes "service-N (name withheld: matches <label>)" in every
// outcome, the score's services and Sample.Services; N follows the sorted
// withheld names. A clean name is left untouched.
func TestRun_MasksMatchingServiceNames(t *testing.T) {
	cfg := baseCfg(writeMappings(t, sensitiveMappingsYAML))
	exec := &fakeExec{byQuery: map[string][]Row{
		"SENS": {
			{ServiceName: canaryEmail, Passed: false, Extent: 1, Example: rawExample("resource.attributes.service.name", canaryEmail)},
			{ServiceName: canaryKey, Passed: true, Example: "No sensitive data matched 1 checks"},
			{ServiceName: "checkout", Passed: true, Example: "No sensitive data matched 1 checks"},
		},
		"PLAIN": {
			{ServiceName: canaryKey, Passed: false, Extent: 1, Example: "span GET /"},
			{ServiceName: "checkout", Passed: true},
		},
	}}
	services := []string{"checkout", canaryKey, canaryEmail}
	sample := &SampleInfo{Services: services}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{FieldsDiscovered: true, Sample: sample})
	if err != nil {
		t.Fatal(err)
	}
	const (
		emailPseudo = "service-1 (name withheld: matches email address)"
		keyPseudo   = "service-2 (name withheld: matches API key)"
	)
	if got, want := strings.Join(rep.Sample.Services, "|"), "checkout|"+keyPseudo+"|"+emailPseudo; got != want {
		t.Errorf("Sample.Services = %q, want %q", got, want)
	}
	if services[1] != canaryKey {
		t.Error("the caller's Services backing array was modified")
	}
	var scoreNames []string
	for _, s := range rep.Score.Services {
		scoreNames = append(scoreNames, s.ServiceName)
	}
	if got, want := strings.Join(scoreNames, "|"), "checkout|"+emailPseudo+"|"+keyPseudo; got != want {
		t.Errorf("score services = %q, want %q", got, want)
	}
	outcomes := map[string]bool{}
	for _, rr := range rep.Rules {
		for _, o := range rr.Outcomes {
			outcomes[o.ServiceName] = true
		}
	}
	for _, n := range []string{"checkout", emailPseudo, keyPseudo} {
		if !outcomes[n] {
			t.Errorf("no outcome for %q (have %v)", n, outcomes)
		}
	}
	js := mustJSON(t, rep)
	for _, c := range []string{canaryEmail, canaryKey} {
		if strings.Contains(js, c) {
			t.Errorf("report JSON contains %q", c)
		}
	}
}

// TestRun_CleanServiceNamesUntouched: with no matching name nothing is
// renamed, and the Sample keeps its own slice.
func TestRun_CleanServiceNamesUntouched(t *testing.T) {
	cfg := baseCfg(writeMappings(t, sensitiveMappingsYAML))
	exec := &fakeExec{byQuery: map[string][]Row{
		"PLAIN": {{ServiceName: "checkout", Passed: true}, {ServiceName: "user-service-2", Passed: false, Extent: 1}},
	}}
	sample := &SampleInfo{Services: []string{"checkout", "user-service-2"}}
	rep, err := Run(context.Background(), cfg, exec, RunOptions{FieldsDiscovered: true, Sample: sample})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rep.Sample.Services, "|"); got != "checkout|user-service-2" {
		t.Errorf("Sample.Services = %q", got)
	}
	for _, s := range rep.Score.Services {
		if s.ServiceName != "checkout" && s.ServiceName != "user-service-2" {
			t.Errorf("renamed service %q", s.ServiceName)
		}
	}
}

// TestRun_CancelledContextStopsRedaction: once the run's context is done, a
// failing sensitive example is the fixed unknown example without a scan.
func TestRun_CancelledContextStopsRedaction(t *testing.T) {
	cfg := baseCfg(writeMappings(t, sensitiveMappingsYAML))
	exec := &fakeExec{byQuery: map[string][]Row{
		"SENS": {{ServiceName: "svc", Passed: false, Extent: 1, Example: rawExample("attributes.msg", canaryEmail)}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := Run(ctx, cfg, exec, RunOptions{FieldsDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	if ex := ruleByID(t, rep, "SPA-006").Outcomes[0].Example; ex != sensitive.UnknownExample {
		t.Errorf("example = %q, want %q", ex, sensitive.UnknownExample)
	}
}

// TestRedaction_ContextAndBudget: redactFailing, withholdMatching and
// maskServiceNames stop scanning once ctx is done or the run's byte budget
// is spent, and fall back to fixed text that carries nothing of the input.
func TestRedaction_ContextAndBudget(t *testing.T) {
	failing := []Row{{ServiceName: "a", Example: rawExample("attributes.msg", canaryEmail)}}
	ordinary := []Row{{ServiceName: "a", Example: "span GET /"}}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	if got := redactFailing(cancelled, failing, newScanBudget())[0].Example; got != sensitive.UnknownExample {
		t.Errorf("redactFailing cancelled: %q", got)
	}
	if got := withholdMatching(cancelled, ordinary, newScanBudget())[0].Example; got != exampleNotChecked {
		t.Errorf("withholdMatching cancelled: %q", got)
	}
	results := []RuleResult{{Outcomes: []score.Outcome{{ServiceName: "checkout"}}}}
	maskServiceNames(cancelled, results, nil)
	if got := results[0].Outcomes[0].ServiceName; got != "service-1 (name withheld: not checked)" {
		t.Errorf("maskServiceNames cancelled: %q", got)
	}

	// A budget smaller than the second example: the first is redacted, the
	// second degrades.
	small := &scanBudget{}
	small.left.Store(int64(len(failing[0].Example)) + 5)
	two := append(append([]Row(nil), failing...), failing...)
	out := redactFailing(context.Background(), two, small)
	if out[0].Example == sensitive.UnknownExample || out[1].Example != sensitive.UnknownExample {
		t.Errorf("budget: got %q, %q", out[0].Example, out[1].Example)
	}
	if got := withholdMatching(context.Background(), ordinary, small)[0].Example; got != exampleNotChecked {
		t.Errorf("withholdMatching over budget: %q", got)
	}
	if two[1].Example != failing[0].Example {
		t.Error("redactFailing modified its input")
	}
}

// TestRun_ScanBudgetBoundsRedaction: many large failing examples (a crafted
// upload) cost at most the run's budget, not one scan each; the rest get the
// fixed unknown example.
func TestRun_ScanBudgetBoundsRedaction(t *testing.T) {
	cfg := baseCfg(writeMappings(t, sensitiveMappingsYAML))
	big := rawExample("body.text", strings.Repeat("a ", 32<<10)+canaryEmail)
	// The real budget is 4 MiB (about 2 s of scanning, far more under
	// -race); a 200 KiB one exercises the same wiring quickly.
	saved := scanBudgetBytes
	scanBudgetBytes = 200 << 10
	t.Cleanup(func() { scanBudgetBytes = saved })
	var rows []Row
	for i := 0; i < 32; i++ {
		rows = append(rows, Row{ServiceName: fmt.Sprintf("svc-%02d", i), Passed: false, Extent: 1, Example: big})
	}
	exec := &fakeExec{byQuery: map[string][]Row{"SENS": rows}}
	start := time.Now()
	rep, err := Run(context.Background(), cfg, exec, RunOptions{FieldsDiscovered: true})
	if err != nil {
		t.Fatal(err)
	}
	unknown := 0
	for _, o := range ruleByID(t, rep, "SPA-006").Outcomes {
		if o.Example == sensitive.UnknownExample {
			unknown++
		}
	}
	allowed := int(scanBudgetBytes) / len(big)
	if scanned := 32 - unknown; scanned != allowed {
		t.Errorf("%d examples scanned, budget allows %d", scanned, allowed)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("redaction took %s", d)
	}
}
