// Package report writes Vern evaluation reports to disk in JSON and Markdown.
package report

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/ericmustin/vern/internal/evaluate"
)

// WriteJSON serialises an evaluation report to path, with two-space indent.
func WriteJSON(path string, rep *evaluate.Report) error {
	if rep == nil {
		return fmt.Errorf("nil report")
	}
	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
