package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/samplescore"
)

type rulesResponse struct {
	SpecVersion string            `json:"spec_version"`
	Rules       []json.RawMessage `json:"rules"`
}

type ruleJSON struct {
	ID                string `json:"id"`
	Impact            string `json:"impact"`
	Target            string `json:"target"`
	Description       string `json:"description"`
	SpecMarkdown      string `json:"spec_markdown"`
	SpecURL           string `json:"spec_url"`
	ImplementationURL string `json:"implementation_url"`
	Status            string `json:"status"`
	Reason            string `json:"reason"`
	Advisory          bool   `json:"advisory"`
	Heuristic         bool   `json:"heuristic"`
	Deviation         string `json:"deviation"`
	Remediation       string `json:"remediation"`
}

func TestRules_All26(t *testing.T) {
	s := newTestServer(t, Config{Engine: &fakeEngine{}})
	rec := serve(s, httptest.NewRequest(http.MethodGet, "/api/v1/rules", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var resp rulesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SpecVersion == "" {
		t.Error("spec_version is empty")
	}
	if len(resp.Rules) != 26 {
		t.Fatalf("got %d rules, want 26", len(resp.Rules))
	}
	keys := []string{"id", "impact", "target", "description", "spec_markdown", "spec_url", "implementation_url", "status", "reason", "advisory", "heuristic", "deviation", "remediation"}
	order := map[string]int{"Critical": 0, "Important": 1, "Normal": 2, "Low": 3}
	var prev ruleJSON
	byID := map[string]ruleJSON{}
	for i, raw := range resp.Rules {
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			if _, ok := fields[k]; !ok {
				t.Errorf("rule %d lacks key %q", i, k)
			}
		}
		var r ruleJSON
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatal(err)
		}
		byID[r.ID] = r
		if !strings.HasPrefix(r.SpecMarkdown, "**Rule ID:** "+r.ID+"\n") {
			t.Errorf("%s: spec_markdown does not look like the rule's spec text", r.ID)
		}
		if want := "https://github.com/instrumentation-score/spec/blob/main/rules/" + r.ID + ".md"; r.SpecURL != want {
			t.Errorf("%s: spec_url = %q, want %q", r.ID, r.SpecURL, want)
		}
		if r.ImplementationURL != "https://github.com/ericmustin/vern/blob/main/configs/esql-mappings.yaml" {
			t.Errorf("%s: implementation_url = %q", r.ID, r.ImplementationURL)
		}
		if _, ok := order[r.Impact]; !ok {
			t.Errorf("%s: impact %q is not a canonical tier", r.ID, r.Impact)
		}
		if r.Description == "" || r.Target == "" || r.Status == "" {
			t.Errorf("%s: empty description, target or status: %+v", r.ID, r)
		}
		if r.Status == "not_implemented" && r.Reason == "" {
			t.Errorf("%s: not_implemented without a reason", r.ID)
		}
		if i > 0 {
			po, ro := order[prev.Impact], order[r.Impact]
			if po > ro || (po == ro && prev.ID >= r.ID) {
				t.Errorf("rules out of order: %s (%s) before %s (%s)", prev.ID, prev.Impact, r.ID, r.Impact)
			}
		}
		prev = r
	}
	met4 := byID["MET-004"]
	if met4.Status != "not_implemented" || met4.Reason == "" {
		t.Errorf("MET-004 = %q %q, want not_implemented with a reason", met4.Status, met4.Reason)
	}
	if sdk := byID["SDK-001"]; sdk.Status != "not_implemented" || sdk.Reason == "" {
		t.Errorf("SDK-001 = %q %q, want not_implemented (opt-in off) with a reason", sdk.Status, sdk.Reason)
	}
	if res1 := byID["RES-001"]; res1.Status != "implemented" || res1.Remediation == "" {
		t.Errorf("RES-001 = %+v, want implemented with a remediation", res1)
	}
	if met1 := byID["MET-001"]; !met1.Heuristic || met1.Deviation == "" {
		t.Errorf("MET-001 = %+v, want heuristic with a deviation", met1)
	}
}

// A rule without embedded spec text fails the methodology build; New cannot
// return an error, so it logs it and /api/v1/rules answers 500.
func TestRules_MissingSpecText500(t *testing.T) {
	opts := samplescore.Options{SpecRules: []coverage.SpecRule{{ID: "NOPE-999", Impact: "Low", Target: "Span"}}}
	if _, err := buildRules(opts); err == nil || !strings.Contains(err.Error(), "NOPE-999") {
		t.Fatalf("buildRules err = %v, want a missing spec text error", err)
	}
	logs := &logCapture{}
	s := newTestServer(t, Config{Engine: &fakeEngine{}, Options: opts, Logf: logs.logf})
	rec := serve(s, httptest.NewRequest(http.MethodGet, "/api/v1/rules", nil))
	if rec.Code != http.StatusInternalServerError || errorBody(t, rec) != "internal error" {
		t.Fatalf("rules = %d %s, want 500 internal error", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "NOPE-999") {
		t.Fatalf("build error not logged:\n%s", logs)
	}
}
