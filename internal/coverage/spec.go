package coverage

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type SpecRule struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	Target      string `json:"target"`
	Impact      string `json:"impact"`
	Path        string `json:"path"`
}

// LoadSpecRules reads spec rule markdown files from a directory on disk.
// SpecRule.Path is the on-disk path (dir joined with the file name).
func LoadSpecRules(dir string) ([]SpecRule, error) {
	return loadSpecRules(os.DirFS(dir), ".", func(name string) string {
		return filepath.Join(dir, name)
	})
}

// LoadSpecRulesFS reads spec rule markdown files from dir inside fsys (for
// example the embedded spec.FS). Files starting with "_" are skipped and the
// result is sorted by rule ID. SpecRule.Path is the fs path.
func LoadSpecRulesFS(fsys fs.FS, dir string) ([]SpecRule, error) {
	return loadSpecRules(fsys, dir, func(name string) string {
		return path.Join(dir, name)
	})
}

func loadSpecRules(fsys fs.FS, dir string, display func(name string) string) ([]SpecRule, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read rules dir %s: %w", dir, err)
	}

	var rules []SpecRule
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".md" || strings.HasPrefix(name, "_") {
			continue
		}
		rule, err := parseSpecRule(fsys, path.Join(dir, name), display(name))
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}

	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	return rules, nil
}

func parseSpecRule(fsys fs.FS, fsPath, display string) (SpecRule, error) {
	data, err := fs.ReadFile(fsys, fsPath)
	if err != nil {
		return SpecRule{}, fmt.Errorf("read spec rule %s: %w", display, err)
	}
	body := string(data)
	rule := SpecRule{
		ID:          specField(body, "Rule ID"),
		Description: specField(body, "Description"),
		Target:      specField(body, "Target"),
		Impact:      specField(body, "Impact"),
		Path:        display,
	}
	if rule.ID == "" {
		return SpecRule{}, fmt.Errorf("parse spec rule %s: missing Rule ID", display)
	}
	if rule.Target == "" {
		return SpecRule{}, fmt.Errorf("parse spec rule %s: missing Target", display)
	}
	if rule.Impact == "" {
		return SpecRule{}, fmt.Errorf("parse spec rule %s: missing Impact", display)
	}
	rule.Target = normalizeWhitespace(rule.Target)
	rule.Impact = normalizeWhitespace(rule.Impact)
	return rule, nil
}

func specField(body, name string) string {
	pattern := fmt.Sprintf(`(?m)^\*\*%s:\*\*\s*(.+?)\s*$`, regexp.QuoteMeta(name))
	matches := regexp.MustCompile(pattern).FindStringSubmatch(body)
	if len(matches) < 2 {
		return ""
	}
	value := strings.TrimSpace(matches[1])
	if strings.HasPrefix(value, "`") && strings.HasSuffix(value, "`") {
		value = strings.Trim(value, "`")
	}
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(value, ".")
	return value
}

func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func NormalizeTarget(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "span", "tracespan", "trace span":
		return "Span"
	case "resource":
		return "Resource"
	case "metric":
		return "Metric"
	case "log":
		return "Log"
	case "sdk":
		return "SDK"
	default:
		return normalizeWhitespace(s)
	}
}
