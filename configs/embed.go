// Package configs embeds the default ES|QL rule mappings so the binary runs
// with no configs/ directory on disk.
package configs

import _ "embed"

// ESQLMappings is the embedded esql-mappings.yaml.
//
//go:embed esql-mappings.yaml
var ESQLMappings []byte
