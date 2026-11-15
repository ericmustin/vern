package evaluate

import (
	"fmt"
	"strings"
)

// Problems returns human-readable reasons the run cannot be trusted, in a
// fixed order: rules that failed with an error, then "nothing was indexed".
// A clean run returns nil. It is a pure function of the report, so the CLI
// and the server flag the same runs.
func Problems(rep *Report) []string {
	if rep == nil {
		return nil
	}
	var out []string
	var failed []string
	for _, r := range rep.Rules {
		if r.Status == StatusError {
			failed = append(failed, r.RuleID)
		}
	}
	if len(failed) > 0 {
		out = append(out, fmt.Sprintf("%d rule(s) failed with an error: %s (see the report)", len(failed), strings.Join(failed, ", ")))
	}
	if nothingIndexed(rep.Sample) {
		out = append(out, "no items were indexed: the scoring backend dropped every item sent (see the report's dropped counts)")
	}
	return out
}

// nothingIndexed reports whether items were sent but every one was dropped,
// judged from the report alone: for each signal present in the sample, the
// dropped count reaches the number of items sent (logs = log records plus
// span events, as loaded). Metrics drops are a lower bound, so a total metrics
// loss can go undetected; the check then errs towards "something indexed".
func nothingIndexed(s *SampleInfo) bool {
	if s == nil {
		return false
	}
	sent := map[string]int{"traces": s.Spans, "logs": s.LogRecords + s.SpanEvents, "metrics": s.DataPoints}
	total := 0
	for sig, n := range sent {
		if n == 0 {
			continue
		}
		total += n
		if s.Dropped[sig] < n {
			return false
		}
	}
	return total > 0
}
