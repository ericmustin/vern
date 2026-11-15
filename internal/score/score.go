// Package score computes Instrumentation Score values from per-rule evaluation
// outcomes. The formula matches the Instrumentation Score spec exactly:
//
//	score = Σ(passed_i × weight_i) / Σ(total_i × weight_i) × 100
//
// where each (rule, service) pair contributes one "total" and contributes a
// "passed" of 1 if rule_passed is true. Weights are per impact tier.
package score

import (
	"sort"
	"strconv"
	"strings"
)

// Format renders a score with one decimal place. Every place that shows or
// gates on a score (summary, reports, outputs file, --min-score) uses it, so
// they agree on ties such as 6.25 -> "6.2".
func Format(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

// categorizeDisplayed categorizes the value as shown (one decimal), so a score
// of 89.96 reads "90.0" and "Excellent" rather than "90.0" and "Good".
func categorizeDisplayed(v float64) string {
	shown, _ := strconv.ParseFloat(Format(v), 64)
	return Categorize(shown)
}

// ImpactWeights are the weights from the spec, keyed by impact tier.
var ImpactWeights = map[string]int{
	"Critical":  40,
	"Important": 30,
	"Normal":    20,
	"Low":       10,
}

// MissingServiceName is the sentinel callers use for rows with an empty
// service.name, so they are scored rather than dropped by Calculate.
const MissingServiceName = "(missing service.name)"

// NormalizeImpact returns the canonical impact tier for s, matching
// case-insensitively and ignoring surrounding whitespace. ok is false for
// anything that is not a known tier.
func NormalizeImpact(s string) (string, bool) {
	for tier := range ImpactWeights {
		if strings.EqualFold(strings.TrimSpace(s), tier) {
			return tier, true
		}
	}
	return "", false
}

// Outcome is a single (rule, service) evaluation result, produced by an
// executor and fed to Calculate.
type Outcome struct {
	RuleID      string  `json:"rule_id"`
	Impact      string  `json:"impact"`
	ServiceName string  `json:"service_name"`
	Passed      bool    `json:"rule_passed"`
	Extent      float64 `json:"extent"`
	Example     string  `json:"example,omitempty"`
}

// RuleOutcome is the per-rule view inside a ServiceScore (omits ServiceName
// because it's redundant with the enclosing service).
type RuleOutcome struct {
	RuleID  string  `json:"rule_id"`
	Impact  string  `json:"impact"`
	Passed  bool    `json:"rule_passed"`
	Extent  float64 `json:"extent"`
	Example string  `json:"example,omitempty"`
}

// Tally counts passed-vs-total for one impact tier.
type Tally struct {
	Passed int `json:"passed"`
	Total  int `json:"total"`
}

// ServiceScore is the per-service rollup with its rules and tallies.
type ServiceScore struct {
	ServiceName  string           `json:"service_name"`
	Score        float64          `json:"score"`
	ScoreDisplay string           `json:"score_display"` // Format(Score)
	Category     string           `json:"category"`      // of the displayed value
	Rules        []RuleOutcome    `json:"rules"`
	Counts       map[string]Tally `json:"counts"`
}

// Result is the fleet-level aggregate.
type Result struct {
	Services []ServiceScore `json:"services"`
	Overall  float64        `json:"overall_score"`
	// OverallDisplay is Format(Overall); Category is derived from that value.
	OverallDisplay string `json:"overall_display"`
	Category       string `json:"overall_category"`
	// Scored is false when the fleet has no weighted outcomes; Overall is then
	// 0 and Category is empty, and reports should say "not scored".
	Scored bool             `json:"scored"`
	Counts map[string]Tally `json:"counts"`
}

// Calculate groups outcomes by service, computes per-service weighted scores,
// and rolls up a fleet-level overall score. Services are returned in stable
// alphabetical order. Impact is normalized with NormalizeImpact and the
// canonical form is used everywhere downstream; outcomes whose Impact is not a
// known tier, or whose ServiceName is empty (map it to MissingServiceName
// first), are dropped silently. When nothing is scorable, Scored is false,
// Overall is 0 and Category is empty.
func Calculate(outcomes []Outcome) *Result {
	byService := map[string][]Outcome{}
	for _, o := range outcomes {
		if o.ServiceName == "" {
			continue
		}
		impact, ok := NormalizeImpact(o.Impact)
		if !ok {
			continue
		}
		o.Impact = impact
		byService[o.ServiceName] = append(byService[o.ServiceName], o)
	}

	fleet := map[string]Tally{}
	services := make([]ServiceScore, 0, len(byService))
	for name, list := range byService {
		ss := buildServiceScore(name, list)
		services = append(services, ss)
		for impact, t := range ss.Counts {
			f := fleet[impact]
			f.Passed += t.Passed
			f.Total += t.Total
			fleet[impact] = f
		}
	}
	sort.Slice(services, func(i, j int) bool { return services[i].ServiceName < services[j].ServiceName })

	overall := weightedScore(fleet)
	res := &Result{
		Services:       services,
		Overall:        overall,
		OverallDisplay: Format(overall),
		Counts:         fleet,
	}
	if weightedTotal(fleet) > 0 {
		res.Scored = true
		res.Category = categorizeDisplayed(overall)
	}
	return res
}

func buildServiceScore(name string, outcomes []Outcome) ServiceScore {
	counts := map[string]Tally{}
	rules := make([]RuleOutcome, 0, len(outcomes))
	for _, o := range outcomes {
		t := counts[o.Impact]
		t.Total++
		if o.Passed {
			t.Passed++
		}
		counts[o.Impact] = t
		rules = append(rules, RuleOutcome{
			RuleID:  o.RuleID,
			Impact:  o.Impact,
			Passed:  o.Passed,
			Extent:  o.Extent,
			Example: o.Example,
		})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].RuleID < rules[j].RuleID })
	s := weightedScore(counts)
	return ServiceScore{
		ServiceName:  name,
		Score:        s,
		ScoreDisplay: Format(s),
		Category:     categorizeDisplayed(s),
		Rules:        rules,
		Counts:       counts,
	}
}

func weightedTotal(counts map[string]Tally) int {
	var den int
	for impact, t := range counts {
		den += t.Total * ImpactWeights[impact]
	}
	return den
}

func weightedScore(counts map[string]Tally) float64 {
	var num, den int
	for impact, t := range counts {
		w := ImpactWeights[impact]
		num += t.Passed * w
		den += t.Total * w
	}
	if den == 0 {
		return 0
	}
	return float64(num) / float64(den) * 100
}

// Categorize maps a numeric score to the spec's category labels.
func Categorize(s float64) string {
	switch {
	case s >= 90:
		return "Excellent"
	case s >= 75:
		return "Good"
	case s >= 50:
		return "Needs Improvement"
	default:
		return "Poor"
	}
}
