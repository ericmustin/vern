package coverage

import (
	"testing"

	"github.com/ericmustin/vern/internal/mappings"
)

func TestDefaultMappingsHaveNoMissingVendoredSpecRules(t *testing.T) {
	specRules, err := LoadSpecRules("../../spec/rules")
	if err != nil {
		t.Fatalf("LoadSpecRules: %v", err)
	}
	mf, err := mappings.Load("../../configs/esql-mappings.yaml")
	if err != nil {
		t.Fatalf("Load mappings: %v", err)
	}

	summary := Build(specRules, mf)
	if len(summary.MissingRules) != 0 {
		t.Fatalf("expected every vendored spec rule to have a mapping, missing: %v", summary.MissingRules)
	}
	for _, rule := range summary.Rules {
		if rule.Status == "disabled" && rule.Reason == "" {
			t.Fatalf("disabled rule %s must document a reason", rule.ID)
		}
	}
}
