// Package compare diffs multiple vern evaluation reports (one per backend)
// and reports where the per-service, per-rule outcomes disagree beyond a
// configurable tolerance. It exists to catch rule-mapping bugs: when the same
// telemetry yields different scores across backends, a rule template is wrong.
package compare

// Kind classifies a disagreement. The numeric values double as exit-code
// priorities — lower is more severe, and the process exits with the smallest
// Kind present.
type Kind int

const (
	KindRulePassedMismatch Kind = 1
	KindExtentExceeded     Kind = 2
	KindScoreExceeded      Kind = 3
	KindServiceDrift       Kind = 4
)

func (k Kind) String() string {
	switch k {
	case KindRulePassedMismatch:
		return "rule_passed mismatch"
	case KindExtentExceeded:
		return "extent tolerance exceeded"
	case KindScoreExceeded:
		return "score tolerance exceeded"
	case KindServiceDrift:
		return "service-set drift"
	default:
		return "unknown"
	}
}

// Options carry the comparison tolerances and known per-backend rule gaps.
type Options struct {
	ExtentTolerance   float64             // max |extent_a - extent_b| when rule_passed agrees
	ScoreTolerance    float64             // max per-service score spread across backends
	ServiceOverlapMin float64             // min intersection ratio (0-1) per backend vs the union
	ExpectedGaps      map[string][]string // label -> rule IDs the backend is not expected to evaluate
}

// DefaultOptions returns the default comparison tolerances.
func DefaultOptions() Options {
	return Options{
		ExtentTolerance:   0.05,
		ScoreTolerance:    10.0,
		ServiceOverlapMin: 0.80,
		ExpectedGaps:      map[string][]string{},
	}
}

// CellOutcome is one backend's result for a single (service, rule) pair.
type CellOutcome struct {
	Passed bool    `json:"rule_passed"`
	Extent float64 `json:"extent"`
}

// Cell holds every backend's outcome for one (service, rule) pair. A nil
// entry in Outcomes means that backend produced no row for the pair — the
// rule was not evaluated there (structural gap or simply no matching data).
type Cell struct {
	Service  string                  `json:"service"`
	RuleID   string                  `json:"rule_id"`
	Outcomes map[string]*CellOutcome `json:"outcomes"`
	Agree    bool                    `json:"agree"`
}

// Discrepancy is a single recorded disagreement.
type Discrepancy struct {
	Kind     Kind   `json:"kind"`
	KindName string `json:"kind_name"`
	Service  string `json:"service,omitempty"`
	RuleID   string `json:"rule_id,omitempty"`
	Detail   string `json:"detail"`
}

// BackendSummary is the headline numbers for one report.
type BackendSummary struct {
	Label          string  `json:"label"`
	Scored         bool    `json:"scored"`
	Overall        float64 `json:"overall_score"`
	Category       string  `json:"category"`
	ServiceCount   int     `json:"service_count"`
	RulesAttempted int     `json:"rules_attempted"`
	RulesSucceeded int     `json:"rules_succeeded"`
	Failures       int     `json:"failures"`
}

// Report is the full comparison result.
type Report struct {
	Labels        []string            `json:"labels"`
	ExpectedGaps  map[string][]string `json:"expected_gaps,omitempty"`
	Backends      []BackendSummary    `json:"backends"`
	Cells         []Cell              `json:"cells"`
	Discrepancies []Discrepancy       `json:"discrepancies"`
	Verdict       string              `json:"verdict"`
	ExitCode      int                 `json:"exit_code"`
}
