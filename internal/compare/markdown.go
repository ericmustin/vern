package compare

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// WriteJSON writes the comparison report to path as indented JSON.
func WriteJSON(path string, rep *Report) error {
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal comparison: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// WriteMarkdown writes the human-readable comparison to path.
func WriteMarkdown(path string, rep *Report) error {
	if err := os.WriteFile(path, []byte(RenderMarkdown(rep)), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// RenderMarkdown returns the Markdown comparison body.
func RenderMarkdown(rep *Report) string {
	var b strings.Builder

	fmt.Fprintln(&b, "# Cross-Backend Comparison")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Backends: %s\n", strings.Join(rep.Labels, ", "))
	if gaps := formatGaps(rep.ExpectedGaps); gaps != "" {
		fmt.Fprintf(&b, "Expected gaps: %s\n", gaps)
	}
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "## Verdict: %s\n", rep.Verdict)
	fmt.Fprintln(&b)

	writeBackendTable(&b, rep)
	writeCoverageTable(&b, rep)
	writeGrid(&b, rep)
	writeDiscrepancies(&b, rep)

	return b.String()
}

func writeBackendTable(b *strings.Builder, rep *Report) {
	fmt.Fprintln(b, "## Per-backend overall")
	fmt.Fprintln(b)
	fmt.Fprintln(b, "| Backend | Score | Category | Services | Rules attempted | Rules succeeded | Failures |")
	fmt.Fprintln(b, "|---|---|---|---|---|---|---|")
	for _, s := range rep.Backends {
		scoreCol, category := fmt.Sprintf("%.1f", s.Overall), s.Category
		if !s.Scored {
			scoreCol, category = "not scored", ""
		}
		fmt.Fprintf(b, "| %s | %s | %s | %d | %d | %d | %d |\n",
			s.Label, scoreCol, category, s.ServiceCount, s.RulesAttempted, s.RulesSucceeded, s.Failures)
	}
	fmt.Fprintln(b)
}

// writeCoverageTable shows which rules each backend actually evaluated.
func writeCoverageTable(b *strings.Builder, rep *Report) {
	rules := rulesFromCells(rep.Cells)
	if len(rules) == 0 {
		return
	}
	// evaluated[rule][label] = at least one non-nil outcome.
	evaluated := map[string]map[string]bool{}
	for _, c := range rep.Cells {
		if evaluated[c.RuleID] == nil {
			evaluated[c.RuleID] = map[string]bool{}
		}
		for label, co := range c.Outcomes {
			if co != nil {
				evaluated[c.RuleID][label] = true
			}
		}
	}

	fmt.Fprintln(b, "## Coverage")
	fmt.Fprintln(b)
	fmt.Fprintln(b, "Whether each backend produced at least one outcome for the rule.")
	fmt.Fprintln(b)
	fmt.Fprintf(b, "| Rule | %s |\n", strings.Join(rep.Labels, " | "))
	fmt.Fprintln(b, "|"+strings.Repeat("---|", len(rep.Labels)+1))
	for _, rule := range rules {
		cells := []string{rule}
		for _, label := range rep.Labels {
			switch {
			case evaluated[rule][label]:
				cells = append(cells, "yes")
			case isExpectedGap(rep.ExpectedGaps, label, rule):
				cells = append(cells, "gap (expected)")
			default:
				cells = append(cells, "**GAP**")
			}
		}
		fmt.Fprintf(b, "| %s |\n", strings.Join(cells, " | "))
	}
	fmt.Fprintln(b)
}

// writeGrid shows the (service, rule) cells that disagree.
func writeGrid(b *strings.Builder, rep *Report) {
	var disagreeing []Cell
	for _, c := range rep.Cells {
		if !c.Agree {
			disagreeing = append(disagreeing, c)
		}
	}

	fmt.Fprintln(b, "## Rule × service grid")
	fmt.Fprintln(b)
	if len(disagreeing) == 0 {
		fmt.Fprintf(b, "All %d evaluated (service, rule) cells agree within tolerance.\n\n", len(rep.Cells))
		return
	}
	fmt.Fprintln(b, "Cells with a disagreement. `P`=pass, `F`=fail, `—`=not evaluated; extent in parens.")
	fmt.Fprintln(b)
	fmt.Fprintf(b, "| Service | Rule | %s |\n", strings.Join(rep.Labels, " | "))
	fmt.Fprintln(b, "|---|---|"+strings.Repeat("---|", len(rep.Labels)))
	for _, c := range disagreeing {
		cells := []string{c.Service, c.RuleID}
		for _, label := range rep.Labels {
			cells = append(cells, formatOutcome(c.Outcomes[label]))
		}
		fmt.Fprintf(b, "| %s |\n", strings.Join(cells, " | "))
	}
	fmt.Fprintln(b)
}

func writeDiscrepancies(b *strings.Builder, rep *Report) {
	fmt.Fprintln(b, "## Discrepancies")
	fmt.Fprintln(b)
	if len(rep.Discrepancies) == 0 {
		fmt.Fprintln(b, "(none)")
		fmt.Fprintln(b)
		return
	}
	// Most severe first (lowest Kind value).
	ds := append([]Discrepancy(nil), rep.Discrepancies...)
	sort.SliceStable(ds, func(i, j int) bool {
		if ds[i].Kind != ds[j].Kind {
			return ds[i].Kind < ds[j].Kind
		}
		if ds[i].Service != ds[j].Service {
			return ds[i].Service < ds[j].Service
		}
		return ds[i].RuleID < ds[j].RuleID
	})
	for _, d := range ds {
		loc := ""
		switch {
		case d.Service != "" && d.RuleID != "":
			loc = fmt.Sprintf(" [%s / %s]", d.Service, d.RuleID)
		case d.Service != "":
			loc = fmt.Sprintf(" [%s]", d.Service)
		}
		fmt.Fprintf(b, "- **%s**%s: %s\n", d.KindName, loc, d.Detail)
	}
	fmt.Fprintln(b)
}

func formatOutcome(co *CellOutcome) string {
	if co == nil {
		return "—"
	}
	mark := "F"
	if co.Passed {
		mark = "P"
	}
	return fmt.Sprintf("%s (%.2f)", mark, co.Extent)
}

func rulesFromCells(cells []Cell) []string {
	set := map[string]bool{}
	for _, c := range cells {
		set[c.RuleID] = true
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

func isExpectedGap(gaps map[string][]string, label, rule string) bool {
	for _, r := range gaps[label] {
		if r == rule {
			return true
		}
	}
	return false
}

func formatGaps(gaps map[string][]string) string {
	var parts []string
	labels := make([]string, 0, len(gaps))
	for label := range gaps {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		rules := append([]string(nil), gaps[label]...)
		sort.Strings(rules)
		for _, r := range rules {
			parts = append(parts, label+":"+r)
		}
	}
	return strings.Join(parts, ", ")
}
