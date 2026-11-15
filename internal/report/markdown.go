package report

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ericmustin/vern/internal/evaluate"
	"github.com/ericmustin/vern/internal/score"
)

// WriteMarkdown writes a human-readable Markdown summary of the evaluation
// report to path.
func WriteMarkdown(path string, rep *evaluate.Report) error {
	if rep == nil {
		return fmt.Errorf("nil report")
	}
	body := RenderMarkdown(rep)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// RenderMarkdown returns the Markdown body without writing it. Kept separate
// from WriteMarkdown so golden-file tests can compare strings directly.
func RenderMarkdown(rep *evaluate.Report) string {
	var b strings.Builder

	fmt.Fprintln(&b, "# Vern Instrumentation Score Report")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- Evaluated: %s\n", rep.EvaluatedAt.Format("2006-01-02T15:04:05Z"))
	fmt.Fprintf(&b, "- Backend: %s\n", rep.Backend)
	if rep.MappingsFile != "" {
		fmt.Fprintf(&b, "- Mappings: %s\n", escapeMD(rep.MappingsFile))
	}
	if rep.ConfigPath != "" {
		fmt.Fprintf(&b, "- Config: %s\n", escapeMD(rep.ConfigPath))
	}
	fmt.Fprintln(&b)

	if rep.Sample != nil {
		if banner := droppedBanner(rep.Sample.Dropped); banner != "" {
			fmt.Fprintf(&b, "%s\n\n", banner)
		}
	}

	fmt.Fprintln(&b, "## Overall")
	fmt.Fprintln(&b)
	if rep.Score == nil || !rep.Score.Scored {
		fmt.Fprintln(&b, "- Score: not scored — no rule could be evaluated")
	} else {
		fmt.Fprintf(&b, "- Score: %s (%s)\n", score.Format(rep.Score.Overall), rep.Score.Category)
	}
	if rep.Score != nil {
		for _, impact := range impactOrder {
			if t, ok := rep.Score.Counts[impact]; ok {
				fmt.Fprintf(&b, "- %s: %d/%d\n", impact, t.Passed, t.Total)
			}
		}
	}
	fmt.Fprintln(&b)

	if rep.Partial {
		scored := 0
		for _, r := range rep.Rules {
			if r.Scored {
				scored++
			}
		}
		fmt.Fprintf(&b, "> **Partial score:** %d of %d rules scored. See *Not scored* below.\n\n", scored, len(rep.Rules))
	}

	if rep.Sample != nil {
		writeSampleSection(&b, rep.Sample)
	}

	if len(rep.Rules) > 0 {
		fmt.Fprintln(&b, "## Rules")
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "| Rule | Impact | Status | Failing services | Notes |")
		fmt.Fprintln(&b, "|---|---|---|---|---|")
		for _, r := range rep.Rules {
			var notes []string
			if r.Advisory {
				notes = append(notes, "advisory")
			}
			if r.Heuristic {
				notes = append(notes, "heuristic")
			}
			if r.Deviation != "" {
				notes = append(notes, r.Deviation)
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", r.RuleID, r.Impact, r.Status,
				escapeAll(failingServices(r), ", "), escapeAll(notes, "; "))
		}
		fmt.Fprintln(&b)
	}

	if rep.Score != nil && len(rep.Score.Services) > 0 {
		fmt.Fprintln(&b, "## Services")
		fmt.Fprintln(&b)
		for _, s := range rep.Score.Services {
			writeServiceSection(&b, s)
		}
	}

	var fixes []string
	for _, r := range rep.Rules {
		if r.Remediation != "" && len(failingServices(r)) > 0 {
			fixes = append(fixes, fmt.Sprintf("- **%s**: %s\n", r.RuleID, r.Remediation))
		}
	}
	if len(fixes) > 0 {
		fmt.Fprintln(&b, "## How to fix")
		fmt.Fprintln(&b)
		for _, f := range fixes {
			fmt.Fprint(&b, f)
		}
		fmt.Fprintln(&b)
	}

	var unscored []string
	for _, r := range rep.Rules {
		if r.Scored {
			continue
		}
		reason := r.Reason
		if reason == "" && r.Advisory {
			unscored = append(unscored, fmt.Sprintf("- **%s**: advisory — shown for information, not scored\n", r.RuleID))
			continue
		}
		if reason == "" {
			unscored = append(unscored, fmt.Sprintf("- **%s**: %s\n", r.RuleID, r.Status))
		} else {
			unscored = append(unscored, fmt.Sprintf("- **%s**: %s — %s\n", r.RuleID, r.Status, escapeMD(reason)))
		}
	}
	if len(unscored) > 0 {
		fmt.Fprintln(&b, "## Not scored")
		fmt.Fprintln(&b)
		for _, u := range unscored {
			fmt.Fprint(&b, u)
		}
		fmt.Fprintln(&b)
	}

	return b.String()
}

// mdEscaper neutralises Markdown in user-controlled strings (service names,
// file names, notes, examples, error text) so they cannot break a table row
// or render as a link, image, emphasis, code or HTML when the report lands in
// a GitHub job summary. Backslash goes first so input cannot cancel an escape.
// "://" and "www." are broken up so GFM extended autolinks do not fire.
var mdEscaper = strings.NewReplacer(
	"\r\n", " ", "\n", " ", "\r", " ",
	"://", `:\/\/`, "www.", `www\.`,
	`\`, `\\`,
	"|", `\|`,
	"`", "\\`",
	"[", `\[`, "]", `\]`,
	"*", `\*`, "_", `\_`,
	"<", "&lt;", ">", "&gt;",
)

// escapeMD escapes s for inline use. A leading block marker (#, -, +, or
// digits followed by . or ) — > is already an entity) is escaped too, since
// user strings can open a line or a list item (sample notes).
func escapeMD(s string) string {
	s = mdEscaper.Replace(s)
	i := len(s) - len(strings.TrimLeft(s, " \t"))
	if i == len(s) {
		return s
	}
	switch c := s[i]; {
	case c == '#' || c == '-' || c == '+':
		return s[:i] + `\` + s[i:]
	case c >= '0' && c <= '9':
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j < len(s) && (s[j] == '.' || s[j] == ')') {
			return s[:j] + `\` + s[j:]
		}
	}
	return s
}

// escapeAll escapes each element, then joins them.
func escapeAll(items []string, sep string) string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = escapeMD(s)
	}
	return strings.Join(out, sep)
}

// failingServices returns the sorted, de-duplicated services with a failed outcome.
func failingServices(r evaluate.RuleResult) []string {
	set := map[string]bool{}
	for _, o := range r.Outcomes {
		if !o.Passed {
			set[o.ServiceName] = true
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

var impactOrder = []string{"Critical", "Important", "Normal", "Low"}

func writeServiceSection(b *strings.Builder, s score.ServiceScore) {
	fmt.Fprintf(b, "### %s — %s (%s)\n", escapeMD(s.ServiceName), score.Format(s.Score), s.Category)
	fmt.Fprintln(b)
	fmt.Fprintln(b, "| Rule | Impact | Result | Extent | Example |")
	fmt.Fprintln(b, "|---|---|---|---|---|")
	for _, r := range s.Rules {
		result := "fail"
		if r.Passed {
			result = "pass"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %.2f | %s |\n", r.RuleID, r.Impact, result, r.Extent, escapeMD(r.Example))
	}
	fmt.Fprintln(b)
}

// DroppedSummary describes items that never reached the index, e.g.
// "traces 2, metrics at least 5", with their total. at least is true when the
// total is a lower bound (metrics drops are). total is 0 when nothing dropped.
func DroppedSummary(dropped map[string]int) (summary string, total int, atLeast bool) {
	var parts []string
	for _, sig := range []string{"traces", "logs", "metrics"} {
		n := dropped[sig]
		if n <= 0 {
			continue
		}
		total += n
		if sig == "metrics" {
			atLeast = true
			parts = append(parts, fmt.Sprintf("metrics at least %d", n))
		} else {
			parts = append(parts, fmt.Sprintf("%s %d", sig, n))
		}
	}
	return strings.Join(parts, ", "), total, atLeast
}

// droppedBanner returns the blockquote warning for items that never reached
// the index, or "" when nothing was dropped. Metrics drops are a lower bound.
func droppedBanner(dropped map[string]int) string {
	summary, total, atLeast := DroppedSummary(dropped)
	if total == 0 {
		return ""
	}
	lead := ""
	if atLeast {
		lead = "At least "
	}
	return fmt.Sprintf("> **%s%d items were not indexed and are not reflected in this score:** %s.", lead, total, summary)
}

func writeSampleSection(b *strings.Builder, s *evaluate.SampleInfo) {
	fmt.Fprintln(b, "## Sample")
	fmt.Fprintln(b)
	if len(s.Files) > 0 {
		fmt.Fprintf(b, "- Files: %s\n", escapeAll(s.Files, ", "))
	}
	if !s.OriginalStart.IsZero() || !s.OriginalEnd.IsZero() {
		fmt.Fprintf(b, "- Original time range: %s to %s\n",
			s.OriginalStart.UTC().Format(time.RFC3339), s.OriginalEnd.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(b, "- Spans: %d\n", s.Spans)
	fmt.Fprintf(b, "- Span events: %d\n", s.SpanEvents)
	fmt.Fprintf(b, "- Log records: %d\n", s.LogRecords)
	fmt.Fprintf(b, "- Data points: %d\n", s.DataPoints)
	fmt.Fprintf(b, "- Services: %s\n", escapeAll(s.Services, ", "))
	fmt.Fprintf(b, "- Signals: %s\n", strings.Join(s.Signals, ", "))
	if len(s.Notes) > 0 {
		fmt.Fprintln(b)
		fmt.Fprintln(b, "Notes:")
		fmt.Fprintln(b)
		for _, n := range s.Notes {
			fmt.Fprintf(b, "- %s\n", escapeMD(n))
		}
	}
	if len(s.BlindSpots) > 0 {
		fmt.Fprintln(b)
		fmt.Fprintln(b, "### Blind spots")
		fmt.Fprintln(b)
		fmt.Fprintln(b, "The sensitive-data rules could not scan these parts of the sample; a pass says nothing about them.")
		fmt.Fprintln(b)
		for _, n := range s.BlindSpots {
			fmt.Fprintf(b, "- %s\n", escapeMD(n))
		}
	}
	fmt.Fprintln(b)
}
