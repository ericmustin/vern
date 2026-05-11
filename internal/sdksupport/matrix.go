// Package sdksupport vendors a small support matrix for OpenTelemetry SDK
// languages and renders it as an ES|QL expression for SDK-001.
//
// This matrix is approximate and drifts over time. Operators who care about
// SDK currency should update it from each SDK's project README on a regular
// cadence — there is no upstream wire-format for "supported versions" we can
// auto-sync from. Sources cited inline below.
package sdksupport

import (
	"fmt"
	"sort"
	"strings"
)

// LanguageSupport records the minimum-supported SDK version for a given
// telemetry.sdk.language value. Versions are SemVer strings; comparison is
// lexical on the major component (sufficient for the spec which says SDK
// versions "SHOULD be within supported values").
type LanguageSupport struct {
	// MinSDKVersionMajor is compared against the first dotted segment of
	// resource.attributes.telemetry.sdk.version. Below this is unsupported.
	MinSDKVersionMajor int
	// SourceNote names the upstream README the matrix was reconciled against.
	SourceNote string
}

// Matrix is the vendored support window. To refresh, edit this map and bump
// the comment above each entry. The README in each row points at the canonical
// source of truth.
var Matrix = map[string]LanguageSupport{
	// https://github.com/open-telemetry/opentelemetry-js — SDK 2.x supports
	// Node 18, 20, 22.
	"nodejs":     {MinSDKVersionMajor: 2, SourceNote: "opentelemetry-js v2.x"},
	"javascript": {MinSDKVersionMajor: 2, SourceNote: "opentelemetry-js v2.x"},
	// https://github.com/open-telemetry/opentelemetry-java — current GA: 1.x
	"java": {MinSDKVersionMajor: 1, SourceNote: "opentelemetry-java 1.x"},
	// https://github.com/open-telemetry/opentelemetry-python — current GA: 1.x
	"python": {MinSDKVersionMajor: 1, SourceNote: "opentelemetry-python 1.x"},
	// https://github.com/open-telemetry/opentelemetry-go — current GA: 1.x
	"go": {MinSDKVersionMajor: 1, SourceNote: "opentelemetry-go 1.x"},
	// https://github.com/open-telemetry/opentelemetry-dotnet — current GA: 1.x
	"dotnet": {MinSDKVersionMajor: 1, SourceNote: "opentelemetry-dotnet 1.x"},
	// https://github.com/open-telemetry/opentelemetry-ruby — current GA: 1.x
	"ruby": {MinSDKVersionMajor: 1, SourceNote: "opentelemetry-ruby 1.x"},
}

// RenderESQLCase emits an ES|QL CASE expression that evaluates to true when
// the SDK is considered supported. langField and versionField are the ES|QL
// column expressions for the language and version, e.g.
// "resource.attributes.telemetry.sdk.language" and
// "resource.attributes.telemetry.sdk.version".
//
// The expression treats unknown languages as supported=true (the rule should
// not penalize SDKs Vern doesn't have a matrix for). Only known-language +
// below-minimum-version combinations are flagged as unsupported.
func RenderESQLCase(langField, versionField string) string {
	if len(Matrix) == 0 {
		return "true"
	}

	// Sort languages for deterministic codegen.
	langs := make([]string, 0, len(Matrix))
	for l := range Matrix {
		langs = append(langs, l)
	}
	sort.Strings(langs)

	var b strings.Builder
	b.WriteString("CASE(\n")
	for _, lang := range langs {
		sup := Matrix[lang]
		fmt.Fprintf(&b,
			"            %s == %q AND TO_INTEGER(SPLIT(COALESCE(%s, \"0\"), \".\")[0]) < %d, false,\n",
			langField, lang, versionField, sup.MinSDKVersionMajor,
		)
	}
	b.WriteString("            true\n          )")
	return b.String()
}
