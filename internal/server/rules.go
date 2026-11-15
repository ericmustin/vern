package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/ericmustin/vern/configs"
	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/coverage"
	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/samplescore"
	"github.com/ericmustin/vern/internal/score"
	"github.com/ericmustin/vern/spec"
)

const (
	specRuleURL       = "https://github.com/instrumentation-score/spec/blob/main/rules/%s.md"
	implementationURL = "https://github.com/ericmustin/vern/blob/main/configs/esql-mappings.yaml"

	// statusImplemented marks a rule that runs on every upload; what it
	// yields per run (evaluated, not_applicable, ...) depends on the data.
	statusImplemented = "implemented"
)

// methodologyRule is one rule in GET /api/v1/rules.
type methodologyRule struct {
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

type methodology struct {
	SpecVersion string            `json:"spec_version"`
	Rules       []methodologyRule `json:"rules"`
}

var impactOrder = map[string]int{"Critical": 0, "Important": 1, "Normal": 2, "Low": 3}

// buildRules renders the methodology body from the same mappings, spec rules
// and config a run uses (embedded defaults unless opts overrides them). The
// status follows evaluate.Run's pre-execution classification: rules the
// resolver skips are not_implemented with the same reasons, template errors
// are error, and resolvable rules are "implemented".
func buildRules(opts samplescore.Options) ([]byte, error) {
	cfg := opts.Config
	if cfg == nil {
		cfg = config.Default()
	}
	mf := opts.Mappings
	if mf == nil {
		var err error
		if mf, err = mappings.LoadBytes(configs.ESQLMappings, "embedded esql-mappings.yaml"); err != nil {
			return nil, fmt.Errorf("load embedded mappings: %w", err)
		}
	}
	specRules := opts.SpecRules
	if specRules == nil {
		var err error
		if specRules, err = coverage.LoadSpecRulesFS(spec.FS, "rules"); err != nil {
			return nil, fmt.Errorf("load embedded spec rules: %w", err)
		}
	}

	// Same gating and resolution as evaluate.Run, on a copy.
	rules := append([]mappings.RuleMapping(nil), mf.Rules...)
	mappings.ApplyOptInGates(rules, cfg)
	override := mappings.SemconvCatalog()
	override.SampleMode = true
	res, _ := mappings.ResolveWithData(rules, cfg, override)

	type outcome struct {
		rm             mappings.RuleMapping
		status, reason string
	}
	byID := map[string]outcome{}
	for _, sk := range res.Skipped {
		reason := sk.Mapping.Status
		if reason == "" && sk.Mapping.OptInFlag != "" {
			reason = "opt-in rule not enabled"
		}
		if reason == "" {
			reason = "disabled in mappings"
		}
		byID[sk.SpecRuleID] = outcome{sk.Mapping, string(evaluate.StatusNotImplemented), reason}
	}
	for _, er := range res.Errors {
		byID[er.SpecRuleID] = outcome{er.Mapping, string(evaluate.StatusError), er.Reason}
	}
	for _, rm := range res.Rules {
		byID[rm.SpecRuleID] = outcome{rm.RuleMapping, statusImplemented, ""}
	}

	out := methodology{SpecVersion: mf.SpecVersion}
	for _, sr := range specRules {
		md, err := fs.ReadFile(spec.FS, "rules/"+sr.ID+".md")
		if err != nil {
			return nil, fmt.Errorf("read spec text for %s: %w", sr.ID, err)
		}
		r := methodologyRule{
			ID:           sr.ID,
			Impact:       sr.Impact,
			Target:       sr.Target,
			Description:  sr.Description,
			SpecMarkdown: string(md),
			SpecURL:      fmt.Sprintf(specRuleURL, sr.ID),
		}
		if o, ok := byID[sr.ID]; ok {
			r.ImplementationURL = implementationURL
			r.Status, r.Reason = o.status, o.reason
			r.Advisory, r.Heuristic = o.rm.Advisory, o.rm.Heuristic
			r.Deviation, r.Remediation = o.rm.Deviation, o.rm.Remediation
			if r.Description == "" {
				r.Description = o.rm.Description
			}
		} else {
			r.Status = string(evaluate.StatusNotImplemented)
			r.Reason = "no implementation for this rule yet"
		}
		if c, ok := score.NormalizeImpact(r.Impact); ok {
			r.Impact = c
		}
		out.Rules = append(out.Rules, r)
	}
	sort.SliceStable(out.Rules, func(i, j int) bool {
		a, b := out.Rules[i], out.Rules[j]
		if oa, ob := rank(a.Impact), rank(b.Impact); oa != ob {
			return oa < ob
		}
		return strings.Compare(a.ID, b.ID) < 0
	})
	return json.Marshal(out)
}

func rank(impact string) int {
	if o, ok := impactOrder[impact]; ok {
		return o
	}
	return len(impactOrder)
}
