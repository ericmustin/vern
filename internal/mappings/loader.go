package mappings

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

func Load(path string) (*MappingsFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mappings %s: %w", path, err)
	}
	return LoadBytes(data, path)
}

// LoadBytes parses mappings YAML from memory; name is used in error messages.
func LoadBytes(data []byte, name string) (*MappingsFile, error) {
	var m MappingsFile
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse mappings %s: %w", name, err)
	}
	// A sensitive rule scans only its scan_groups' fields: without any, its
	// query would scan nothing and pass silently.
	var noGroups []string
	for _, r := range m.Rules {
		if r.Sensitive && len(r.ScanGroups) == 0 {
			noGroups = append(noGroups, r.SpecRuleID)
		}
	}
	if len(noGroups) > 0 {
		return nil, fmt.Errorf("mappings %s: sensitive rules need scan_groups: %s", name, strings.Join(noGroups, ", "))
	}
	return &m, nil
}
