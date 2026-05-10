package coverage

import (
	"fmt"
	"os"
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

func LoadSpecRules(dir string) ([]SpecRule, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read rules dir %s: %w", dir, err)
	}

	var rules []SpecRule
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" || strings.HasPrefix(entry.Name(), "_") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		rule, err := parseSpecRule(path)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}

	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	return rules, nil
}

func parseSpecRule(path string) (SpecRule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SpecRule{}, fmt.Errorf("read spec rule %s: %w", path, err)
	}
	body := string(data)
	rule := SpecRule{
		ID:          specField(body, "Rule ID"),
		Description: specField(body, "Description"),
		Target:      specField(body, "Target"),
		Impact:      specField(body, "Impact"),
		Path:        path,
	}
	if rule.ID == "" {
		return SpecRule{}, fmt.Errorf("parse spec rule %s: missing Rule ID", path)
	}
	if rule.Target == "" {
		return SpecRule{}, fmt.Errorf("parse spec rule %s: missing Target", path)
	}
	if rule.Impact == "" {
		return SpecRule{}, fmt.Errorf("parse spec rule %s: missing Impact", path)
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
