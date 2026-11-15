// Package spec embeds the vendored Instrumentation Score spec rules.
package spec

import "embed"

// FS holds rules/*.md and specification.md.
//
//go:embed rules/*.md specification.md
var FS embed.FS
