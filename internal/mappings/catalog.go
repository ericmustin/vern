package mappings

import (
	"github.com/ericmustin/vern/internal/sdksupport"
	"github.com/ericmustin/vern/internal/semconv"
)

// SemconvCatalog returns the semantic-convention and SDK-matrix template data
// that callers pass as the ResolveWithData override.
func SemconvCatalog() TemplateData {
	return TemplateData{
		SemconvAttributeKeys:          semconv.QuotedCSV(semconv.AttributeKeys),
		SemconvResourceOnlyKeys:       semconv.QuotedCSV(semconv.ResourceOnlyKeys),
		SemconvSpanOnlyKeys:           semconv.QuotedCSV(semconv.SpanOnlyKeys),
		SemconvLogOnlyKeys:            semconv.QuotedCSV(semconv.LogOnlyKeys),
		SemconvMetricOnlyKeys:         semconv.QuotedCSV(semconv.MetricOnlyKeys),
		MET001AttrKeys:                semconv.MET001AttrAllowlist(),
		MisplacedResourceKeys:         semconv.ResourceOnlyKeys,
		MET006SemconvKeys:             semconv.QuotedCSV(semconv.MET006CuratedKeys()),
		MET006SemconvKeysSingleQuoted: semconv.SingleQuotedCSV(semconv.MET006CuratedKeys()),
		SDKSupportMatrix:              sdksupport.RenderESQLCase("resource.attributes.telemetry.sdk.language", "resource.attributes.telemetry.sdk.version"),
	}
}
