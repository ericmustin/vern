package evaluate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ericmustin/vern/internal/config"
	"github.com/ericmustin/vern/internal/mappings"
	"github.com/ericmustin/vern/internal/score"
	"github.com/ericmustin/vern/internal/sensitive"
)

// maxParallel bounds how many rule queries run at once.
const maxParallel = 4

// pending is a rule that passed every pre-execution check and awaits its query.
type pending struct {
	idx   int // position of the rule's placeholder in results
	rm    mappings.RuleMapping
	query string
	rows  []Row
	err   error
}

// Run loads the configured mappings file, resolves each rule's query template,
// executes the resulting queries against exec, and computes a score from the
// returned rows. Every rule ends up with a RuleResult (status plus reason);
// per-rule failures never abort the run. Only evaluated, non-advisory rules
// feed the score, and the report says when the score is partial.
func Run(ctx context.Context, cfg *config.Config, exec Executor, opts RunOptions) (*Report, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	if exec == nil {
		return nil, fmt.Errorf("nil executor")
	}

	excluded, err := excluder(cfg.Exclusions)
	if err != nil {
		return nil, err
	}

	mf := opts.Mappings
	if mf == nil {
		mf, err = mappings.Load(cfg.Mappings)
		if err != nil {
			return nil, fmt.Errorf("load mappings: %w", err)
		}
	} else {
		// Never mutate the caller's mappings: gating below writes Enabled.
		// RuleMapping holds only scalar fields, so copying the slice suffices.
		cp := *mf
		cp.Rules = append([]mappings.RuleMapping(nil), mf.Rules...)
		mf = &cp
	}

	// Same opt-in gating as generate/setup: flag on enables the rule, flag
	// off disables it (and it lands in Skipped as not_implemented).
	mappings.ApplyOptInGates(mf.Rules, cfg)

	override := mappings.SemconvCatalog()
	override.SampleMode = opts.SampleMode
	override.Fields = opts.Fields
	// Lenient: template errors are reported per rule via res.Errors.
	res, _ := mappings.ResolveWithData(mf.Rules, cfg, override)

	report := &Report{
		SchemaVersion: SchemaVersion,
		SpecVersion:   mf.SpecVersion,
		EvaluatedAt:   time.Now().UTC(),
		Backend:       opts.Backend,
		ConfigPath:    opts.ConfigPath,
		MappingsFile:  opts.MappingsFile,
		Sample:        opts.Sample,
	}

	specByID := map[string]int{}
	for i, sr := range opts.SpecRules {
		specByID[sr.ID] = i
	}
	mapped := map[string]bool{}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	var results []RuleResult
	var queue []*pending

	// newResult builds the base record; Description and Target prefer the spec rule.
	newResult := func(rm mappings.RuleMapping) RuleResult {
		impact := rm.Impact
		if c, ok := score.NormalizeImpact(rm.Impact); ok {
			impact = c
		}
		rr := RuleResult{
			RuleID:      rm.SpecRuleID,
			Impact:      impact,
			Target:      rm.Target,
			Description: rm.Description,
			Advisory:    rm.Advisory,
			Heuristic:   rm.Heuristic,
			Sensitive:   rm.Sensitive,
			Deviation:   rm.Deviation,
			Remediation: rm.Remediation,
		}
		if i, ok := specByID[rm.SpecRuleID]; ok {
			sr := opts.SpecRules[i]
			if sr.Description != "" {
				rr.Description = sr.Description
			}
			if sr.Target != "" {
				rr.Target = sr.Target
			}
		}
		return rr
	}
	skip := func(id string, agg bool) bool {
		return agg || strings.HasPrefix(id, "_")
	}
	for _, sk := range res.Skipped {
		if skip(sk.SpecRuleID, sk.Mapping.IsAggregation) {
			continue
		}
		mapped[sk.SpecRuleID] = true
		rr := newResult(sk.Mapping)
		rr.RuleID = sk.SpecRuleID
		rr.Status = StatusNotImplemented
		rr.Reason = sk.Mapping.Status
		if rr.Reason == "" && sk.Mapping.OptInFlag != "" {
			rr.Reason = "opt-in rule not enabled"
		}
		if rr.Reason == "" {
			rr.Reason = "disabled in mappings"
		}
		results = append(results, rr)
	}
	for _, er := range res.Errors {
		if skip(er.SpecRuleID, er.Mapping.IsAggregation) {
			continue
		}
		mapped[er.SpecRuleID] = true
		rr := newResult(er.Mapping)
		rr.RuleID = er.SpecRuleID
		rr.Status = StatusError
		rr.Reason = er.Reason
		if er.Mapping.Sensitive {
			// Template errors can quote template data (field names).
			rr.Reason = "query could not be built"
			logSensitiveError(logf, opts.QueryErrorDetail, rr.RuleID, rr.Reason, errors.New(er.Reason))
		}
		results = append(results, rr)
	}
	for _, rm := range res.Rules {
		if skip(rm.SpecRuleID, rm.IsAggregation) {
			continue
		}
		mapped[rm.SpecRuleID] = true
		rr := newResult(rm.RuleMapping)
		if opts.Signals != nil && !signalPresent(rm.Signal, opts.Signals) {
			rr.Status = StatusNotApplicable
			if rm.Signal == "any" {
				rr.Reason = "sample contains no telemetry"
			} else {
				rr.Reason = "sample contains no " + rm.Signal
			}
			results = append(results, rr)
			continue
		}
		if rm.Sensitive && opts.FieldsFailed {
			rr.Status = StatusError
			rr.Reason = "field discovery failed"
			results = append(results, rr)
			continue
		}
		if rm.Sensitive && !opts.FieldsDiscovered {
			// Without the run's fields the query would scan nothing and the
			// rule would read as "no matching telemetry".
			rr.Status = StatusNotAssessable
			rr.Reason = mappings.NeedsFieldDiscovery
			results = append(results, rr)
			continue
		}
		if rm.MinObservation != "" {
			d, err := time.ParseDuration(rm.MinObservation)
			if err != nil {
				rr.Status = StatusError
				rr.Reason = fmt.Sprintf("invalid min_observation %q: %v", rm.MinObservation, err)
				results = append(results, rr)
				continue
			}
			if opts.ObservedSpan > 0 && d > opts.ObservedSpan {
				rr.Status = StatusNotAssessable
				rr.Reason = fmt.Sprintf("needs %.0f days of observation; sample covers %s", d.Hours()/24, roundSpan(opts.ObservedSpan))
				results = append(results, rr)
				continue
			}
		}
		// Executed below; the placeholder keeps the metadata until rows arrive.
		results = append(results, rr)
		queue = append(queue, &pending{idx: len(results) - 1, rm: rm.RuleMapping, query: rm.ResolvedQuery})
	}

	// Execute queued rules, at most maxParallel at a time; results land by index.
	report.RulesAttempted = len(queue)
	examples := newScanBudget()
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for _, p := range queue {
		wg.Add(1)
		sem <- struct{}{}
		go func(p *pending) {
			defer wg.Done()
			defer func() { <-sem }()
			p.rows, p.err = exec.Execute(ctx, p.query)
			if p.err == nil {
				if p.rm.Sensitive {
					p.rows = redactFailing(ctx, p.rows, examples)
				} else {
					p.rows = withholdMatching(ctx, p.rows, examples)
				}
			}
		}(p)
	}
	wg.Wait()

	var scoreOutcomes []score.Outcome
	for _, p := range queue {
		rr := &results[p.idx]
		if p.err != nil {
			rr.Status = StatusError
			rr.Reason = p.err.Error()
			if p.rm.Sensitive {
				rr.Reason = queryErrorReason(p.err)
				logSensitiveError(logf, opts.QueryErrorDetail, rr.RuleID, rr.Reason, p.err)
			}
			continue
		}
		report.RulesSucceeded++
		p.rows = dropExcluded(p.rows, excluded)
		if len(p.rows) == 0 {
			rr.Status = StatusNotApplicable
			rr.Reason = "no matching telemetry"
			continue
		}
		if _, ok := score.NormalizeImpact(p.rm.Impact); !ok {
			rr.Status = StatusError
			rr.Reason = fmt.Sprintf("unknown impact %q", p.rm.Impact)
			continue
		}
		rr.Status = StatusEvaluated
		rr.Scored = !rr.Advisory
		for _, r := range p.rows {
			name := r.ServiceName
			if name == "" {
				name = score.MissingServiceName
			}
			rr.Outcomes = append(rr.Outcomes, score.Outcome{
				RuleID:      rr.RuleID,
				Impact:      rr.Impact,
				ServiceName: name,
				Passed:      r.Passed,
				Extent:      r.Extent,
				Example:     r.Example,
			})
		}
	}

	// Spec rules with no mapping.
	for _, sr := range opts.SpecRules {
		if mapped[sr.ID] {
			continue
		}
		impact := sr.Impact
		if c, ok := score.NormalizeImpact(sr.Impact); ok {
			impact = c
		}
		results = append(results, RuleResult{
			RuleID:      sr.ID,
			Impact:      impact,
			Target:      sr.Target,
			Description: sr.Description,
			Status:      StatusNotImplemented,
			Reason:      "no implementation for this rule yet",
		})
	}

	sort.SliceStable(results, func(i, j int) bool { return results[i].RuleID < results[j].RuleID })
	maskServiceNames(ctx, results, report.Sample)
	for _, rr := range results {
		if rr.Scored {
			scoreOutcomes = append(scoreOutcomes, rr.Outcomes...)
		}
		if rr.Status == StatusError {
			report.Failures = append(report.Failures, RuleFailure{RuleID: rr.RuleID, Error: rr.Reason})
		}
		if !rr.Scored {
			report.Partial = true
			report.PartialReasons = append(report.PartialReasons,
				fmt.Sprintf("%s: %s — %s", rr.RuleID, rr.Status, partialReason(rr)))
		}
	}
	report.Rules = results
	report.Score = score.Calculate(scoreOutcomes)
	return report, nil
}

// scanBudgetBytes bounds the text the Go-side detectors scan in one run,
// once for rule examples and once for service names. The scan is linear
// but costs about 0.5 s per MiB that matches nothing (eleven detectors),
// and a crafted sample can return a 1 MiB example per service and rule, so
// 4 MiB caps a run's redaction work at a few seconds. Real examples and
// service names are far smaller. Past the budget an example degrades to a
// fixed text (pass/fail and extent are decided in ES|QL and stay correct)
// and a service name is withheld unchecked. A var only so tests can shrink
// it.
var scanBudgetBytes int64 = 4 << 20

// scanBudget is a run-wide byte allowance shared by the query goroutines.
type scanBudget struct{ left atomic.Int64 }

func newScanBudget() *scanBudget {
	b := &scanBudget{}
	b.left.Store(scanBudgetBytes)
	return b
}

// take spends n bytes and reports whether they were available. Once a take
// fails, every later one fails too.
func (b *scanBudget) take(n int) bool { return b.left.Add(-int64(n)) >= 0 }

// redactFailing returns rows with the raw example of every failing row of
// a sensitive rule, which carries the matched value, replaced by its
// redacted form (sensitive.RedactExampleContext). It runs right after
// Execute, so the raw text never reaches an outcome, a report, an error or
// a log. A passing row's example is the query's generated text ("No
// sensitive data matched N checks") and is kept. Once ctx is done or
// the run's budget is spent, a failing example becomes
// sensitive.UnknownExample without being scanned. The executor's slice is
// not modified: it may be shared.
func redactFailing(ctx context.Context, rows []Row, budget *scanBudget) []Row {
	out := make([]Row, len(rows))
	for i, r := range rows {
		if !r.Passed {
			if ctx.Err() != nil || !budget.take(len(r.Example)) {
				r.Example = sensitive.UnknownExample
			} else {
				r.Example = sensitive.RedactExampleContext(ctx, r.Example)
			}
		}
		out[i] = r
	}
	return out
}

// detectors is the sensitive-data catalog, for checking ordinary examples.
var detectors = sensitive.Detectors()

// exampleNotChecked replaces an ordinary example the detectors could not
// check (the run was cancelled or its scan budget is spent).
const exampleNotChecked = "example withheld: it could not be checked for sensitive data"

// matchLabel returns the label of the first detector that matches s. It
// gives up once ctx is done (checked reports false).
func matchLabel(ctx context.Context, s string) (label string, matched, checked bool) {
	for _, d := range detectors {
		if ctx.Err() != nil {
			return "", false, false
		}
		if d.Match(s) {
			return d.Label, true, true
		}
	}
	return "", false, ctx.Err() == nil
}

// withholdMatching returns rows with every ordinary rule example that a
// sensitive-data detector matches replaced by "example withheld: it matches
// <label>": no matched value may reach a report through any rule.
// An example that cannot be checked (ctx done, budget spent) is withheld
// too. Other examples pass verbatim. The executor's slice is not modified.
func withholdMatching(ctx context.Context, rows []Row, budget *scanBudget) []Row {
	out := make([]Row, len(rows))
	for i, r := range rows {
		if r.Example != "" {
			if !budget.take(len(r.Example)) {
				r.Example = exampleNotChecked
			} else if label, matched, checked := matchLabel(ctx, r.Example); !checked {
				r.Example = exampleNotChecked
			} else if matched {
				r.Example = "example withheld: it matches " + label
			}
		}
		out[i] = r
	}
	return out
}

// maskServiceNames replaces every service name that a sensitive-data
// detector matches with a stable pseudonym, "service-N (name
// withheld: matches <label>)", in every outcome and in sample.Services, so
// the score, Markdown, API, UI and OTLP export never carry it. N follows the
// sorted order of the withheld names, so it is deterministic. A name that
// cannot be checked (ctx done, budget spent) is withheld as "service-N (name
// withheld: not checked)". It runs after exclusions, which match real names.
// sample.Services gets a new slice (the caller's backing array is untouched),
// but the SampleInfo itself is updated in place: it is the report's.
func maskServiceNames(ctx context.Context, results []RuleResult, sample *SampleInfo) {
	seen := map[string]bool{}
	var names []string
	add := func(n string) {
		if n != "" && n != score.MissingServiceName && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, rr := range results {
		for _, o := range rr.Outcomes {
			add(o.ServiceName)
		}
	}
	if sample != nil {
		for _, n := range sample.Services {
			add(n)
		}
	}
	sort.Strings(names)

	budget := newScanBudget()
	why := map[string]string{}
	var withheld []string
	for _, n := range names {
		label, matched, checked := "", false, false
		if budget.take(len(n)) {
			label, matched, checked = matchLabel(ctx, n)
		}
		switch {
		case !checked:
			why[n] = "not checked"
		case matched:
			why[n] = "matches " + label
		default:
			continue
		}
		withheld = append(withheld, n)
	}
	if len(withheld) == 0 {
		return
	}
	pseudo := make(map[string]string, len(withheld))
	for i, n := range withheld { // names is sorted, so withheld is too
		pseudo[n] = fmt.Sprintf("service-%d (name withheld: %s)", i+1, why[n])
	}
	for i := range results {
		for j, o := range results[i].Outcomes {
			if p, ok := pseudo[o.ServiceName]; ok {
				results[i].Outcomes[j].ServiceName = p
			}
		}
	}
	if sample != nil && len(sample.Services) > 0 {
		svcs := make([]string, len(sample.Services))
		for i, n := range sample.Services {
			if p, ok := pseudo[n]; ok {
				n = p
			}
			svcs[i] = n
		}
		sample.Services = svcs
	}
}

var httpStatusRe = regexp.MustCompile(`\bHTTP (\d{3})\b`)

// queryErrorReason is a sensitive rule's error reason: a fixed
// classification plus at most an HTTP status code, never error text.
// Elasticsearch errors can echo the query, and a sensitive rule's query
// names the run's fields and matched metric names.
func queryErrorReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "query timed out"
	case errors.Is(err, context.Canceled):
		return "query canceled"
	}
	if m := httpStatusRe.FindStringSubmatch(err.Error()); m != nil {
		return "query failed (HTTP " + m[1] + ")"
	}
	return "query failed"
}

// logSensitiveError logs the detail behind a sensitive rule's fixed reason:
// the full error only when the caller's log is private (detail), else the
// reason alone.
func logSensitiveError(logf func(string, ...any), detail bool, ruleID, reason string, err error) {
	if detail {
		logf("%s: %s: %v", ruleID, reason, err)
		return
	}
	logf("%s: %s (detail withheld: the error can quote field and metric names from the sample)", ruleID, reason)
}

// excluder compiles the configured exclusions into a service-name matcher.
func excluder(ex config.ExclusionConfig) (func(string) bool, error) {
	names := map[string]bool{}
	for _, n := range ex.Services {
		names[n] = true
	}
	var pats []*regexp.Regexp
	for _, p := range ex.ServicePatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid exclusions.service_patterns entry %q: %w", p, err)
		}
		pats = append(pats, re)
	}
	return func(name string) bool {
		if names[name] {
			return true
		}
		for _, re := range pats {
			if re.MatchString(name) {
				return true
			}
		}
		return false
	}, nil
}

// dropExcluded removes rows of excluded services, before scoring.
func dropExcluded(rows []Row, excluded func(string) bool) []Row {
	kept := rows[:0:0]
	for _, r := range rows {
		if !excluded(r.ServiceName) {
			kept = append(kept, r)
		}
	}
	return kept
}

func partialReason(rr RuleResult) string {
	if rr.Reason == "" && rr.Advisory {
		return "advisory, not scored"
	}
	return rr.Reason
}

// signalPresent reports whether a rule's signal appears in the data. An empty
// signal is never gated; "any" needs at least one signal present.
func signalPresent(signal string, present map[string]bool) bool {
	switch signal {
	case "":
		return true
	case "any":
		for _, ok := range present {
			if ok {
				return true
			}
		}
		return false
	}
	return present[signal]
}

// roundSpan rounds an observed sample span for display: whole seconds, or
// milliseconds when the span is shorter than a second.
func roundSpan(d time.Duration) time.Duration {
	if d < time.Second {
		return d.Round(time.Millisecond)
	}
	return d.Round(time.Second)
}
