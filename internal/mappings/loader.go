package mappings

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

func Load(path string) (*MappingsFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mappings %s: %w", path, err)
	}

	var m MappingsFile
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse mappings %s: %w", path, err)
	}
	return &m, nil
}
