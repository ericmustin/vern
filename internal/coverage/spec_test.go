package coverage

import (
	"strings"
	"testing"

	"github.com/ericmustin/vern/spec"
)

func TestLoadSpecRulesFS_Embedded(t *testing.T) {
	rules, err := LoadSpecRulesFS(spec.FS, "rules")
	if err != nil {
		t.Fatalf("LoadSpecRulesFS: %v", err)
	}
	if len(rules) != 26 {
		t.Fatalf("got %d rules, want 26", len(rules))
	}
	if rules[0].ID != "LOG-001" {
		t.Errorf("first rule = %s, want LOG-001", rules[0].ID)
	}
	for _, r := range rules {
		if strings.Contains(r.Path, "_template") {
			t.Errorf("template not skipped: %s", r.Path)
		}
	}
}
