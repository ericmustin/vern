package compare

import (
	"fmt"
	"math"
	"sort"

	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/score"
)

// Compare diffs the given reports (keyed by label) and returns a structured
// comparison. labels fixes the column order for deterministic output; any
// label without a corresponding report entry is ignored.
func Compare(reports map[string]*evaluate.Report, labels []string, opts Options) *Report {
	out := &Report{
		Labels:       labels,
		ExpectedGaps: opts.ExpectedGaps,
	}

	// Per-label indexes: service -> ruleID -> outcome, and service -> score.
	ruleIdx := map[string]map[string]map[string]score.RuleOutcome{}
	scoreIdx := map[string]map[string]float64{}
	for _, label := range labels {
		r := reports[label]
		ruleIdx[label] = indexRules(r)
		scoreIdx[label] = indexScores(r)
		out.Backends = append(out.Backends, summarize(label, r))
	}

	out.Cells = buildCells(labels, ruleIdx, opts)
	out.Discrepancies = collect(labels, out.Cells, scoreIdx, opts)

	worst := Kind(0)
	for _, d := range out.Discrepancies {
		if worst == 0 || d.Kind < worst {
			worst = d.Kind
		}
	}
	out.ExitCode = int(worst)
	if worst == 0 {
		out.Verdict = "PASS"
	} else {
		out.Verdict = "FAIL"
	}
	return out
}

func indexRules(r *evaluate.Report) map[string]map[string]score.RuleOutcome {
	m := map[string]map[string]score.RuleOutcome{}
	if r == nil || r.Score == nil {
		return m
	}
	for _, svc := range r.Score.Services {
		rm := map[string]score.RuleOutcome{}
		for _, ro := range svc.Rules {
			rm[ro.RuleID] = ro
		}
		m[svc.ServiceName] = rm
	}
	return m
}

func indexScores(r *evaluate.Report) map[string]float64 {
	m := map[string]float64{}
	if r == nil || r.Score == nil {
		return m
	}
	for _, svc := range r.Score.Services {
		m[svc.ServiceName] = svc.Score
	}
	return m
}

func summarize(label string, r *evaluate.Report) BackendSummary {
	s := BackendSummary{Label: label}
	if r == nil {
		return s
	}
	s.RulesAttempted = r.RulesAttempted
	s.RulesSucceeded = r.RulesSucceeded
	s.Failures = len(r.Failures)
	if r.Score != nil {
		s.Scored = r.Score.Scored
		s.Overall = r.Score.Overall
		s.Category = r.Score.Category
		s.ServiceCount = len(r.Score.Services)
	}
	return s
}

// buildCells produces one Cell per (service, rule) pair seen in any report.
func buildCells(labels []string, ruleIdx map[string]map[string]map[string]score.RuleOutcome, opts Options) []Cell {
	type sr struct{ service, rule string }
	seen := map[sr]bool{}
	for _, rules := range ruleIdx {
		for svc, rm := range rules {
			for ruleID := range rm {
				seen[sr{svc, ruleID}] = true
			}
		}
	}

	keys := make([]sr, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].service != keys[j].service {
			return keys[i].service < keys[j].service
		}
		return keys[i].rule < keys[j].rule
	})

	cells := make([]Cell, 0, len(keys))
	for _, k := range keys {
		cell := Cell{Service: k.service, RuleID: k.rule, Outcomes: map[string]*CellOutcome{}, Agree: true}
		var present []*CellOutcome
		for _, label := range labels {
			ro, ok := ruleIdx[label][k.service][k.rule]
			if !ok {
				cell.Outcomes[label] = nil
				continue
			}
			co := &CellOutcome{Passed: ro.Passed, Extent: ro.Extent}
			cell.Outcomes[label] = co
			present = append(present, co)
		}
		for i := 1; i < len(present); i++ {
			if present[i].Passed != present[0].Passed {
				cell.Agree = false
			}
			if math.Abs(present[i].Extent-present[0].Extent) > opts.ExtentTolerance {
				cell.Agree = false
			}
		}
		cells = append(cells, cell)
	}
	return cells
}

func collect(
	labels []string,
	cells []Cell,
	scoreIdx map[string]map[string]float64,
	opts Options,
) []Discrepancy {
	var ds []Discrepancy

	// Rule-level: rule_passed mismatch and extent divergence.
	for _, cell := range cells {
		var present []struct {
			label string
			out   *CellOutcome
		}
		for _, label := range labels {
			if co := cell.Outcomes[label]; co != nil {
				present = append(present, struct {
					label string
					out   *CellOutcome
				}{label, co})
			}
		}
		if len(present) < 2 {
			continue // nothing to compare
		}

		passedMismatch := false
		for i := 1; i < len(present); i++ {
			if present[i].out.Passed != present[0].out.Passed {
				passedMismatch = true
			}
		}
		if passedMismatch {
			ds = append(ds, Discrepancy{
				Kind:     KindRulePassedMismatch,
				KindName: KindRulePassedMismatch.String(),
				Service:  cell.Service,
				RuleID:   cell.RuleID,
				Detail:   fmt.Sprintf("rule_passed disagrees: %s", passedSummary(labels, cell)),
			})
			continue // a boolean disagreement subsumes any extent gap
		}

		minE, maxE := present[0].out.Extent, present[0].out.Extent
		for _, p := range present[1:] {
			minE = math.Min(minE, p.out.Extent)
			maxE = math.Max(maxE, p.out.Extent)
		}
		if maxE-minE > opts.ExtentTolerance {
			ds = append(ds, Discrepancy{
				Kind:     KindExtentExceeded,
				KindName: KindExtentExceeded.String(),
				Service:  cell.Service,
				RuleID:   cell.RuleID,
				Detail:   fmt.Sprintf("extent spread %.3f exceeds tolerance %.3f: %s", maxE-minE, opts.ExtentTolerance, extentSummary(labels, cell)),
			})
		}
	}

	// Per-service score spread.
	for _, svc := range allServices(labels, scoreIdx) {
		var vals []float64
		var parts []string
		for _, label := range labels {
			if v, ok := scoreIdx[label][svc]; ok {
				vals = append(vals, v)
				parts = append(parts, fmt.Sprintf("%s=%.1f", label, v))
			}
		}
		if len(vals) < 2 {
			continue
		}
		minV, maxV := vals[0], vals[0]
		for _, v := range vals[1:] {
			minV = math.Min(minV, v)
			maxV = math.Max(maxV, v)
		}
		if maxV-minV > opts.ScoreTolerance {
			ds = append(ds, Discrepancy{
				Kind:     KindScoreExceeded,
				KindName: KindScoreExceeded.String(),
				Service:  svc,
				Detail:   fmt.Sprintf("score spread %.1f exceeds tolerance %.1f: %s", maxV-minV, opts.ScoreTolerance, joinStrings(parts)),
			})
		}
	}

	// Service-set drift.
	union := allServices(labels, scoreIdx)
	if len(union) > 0 {
		for _, label := range labels {
			have := 0
			for _, svc := range union {
				if _, ok := scoreIdx[label][svc]; ok {
					have++
				}
			}
			ratio := float64(have) / float64(len(union))
			if ratio < opts.ServiceOverlapMin {
				ds = append(ds, Discrepancy{
					Kind:     KindServiceDrift,
					KindName: KindServiceDrift.String(),
					Detail: fmt.Sprintf("backend %q covers %d/%d services (%.0f%%, below %.0f%% threshold)",
						label, have, len(union), ratio*100, opts.ServiceOverlapMin*100),
				})
			}
		}
	}

	return ds
}

// allServices returns the sorted union of service names across all labels.
func allServices(labels []string, scoreIdx map[string]map[string]float64) []string {
	set := map[string]bool{}
	for _, label := range labels {
		for svc := range scoreIdx[label] {
			set[svc] = true
		}
	}
	out := make([]string, 0, len(set))
	for svc := range set {
		out = append(out, svc)
	}
	sort.Strings(out)
	return out
}

func passedSummary(labels []string, cell Cell) string {
	var parts []string
	for _, label := range labels {
		co := cell.Outcomes[label]
		switch {
		case co == nil:
			parts = append(parts, label+"=—")
		case co.Passed:
			parts = append(parts, label+"=pass")
		default:
			parts = append(parts, label+"=fail")
		}
	}
	return joinStrings(parts)
}

func extentSummary(labels []string, cell Cell) string {
	var parts []string
	for _, label := range labels {
		if co := cell.Outcomes[label]; co != nil {
			parts = append(parts, fmt.Sprintf("%s=%.3f", label, co.Extent))
		}
	}
	return joinStrings(parts)
}

func joinStrings(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
