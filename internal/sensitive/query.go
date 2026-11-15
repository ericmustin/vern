package sensitive

import (
	"strconv"
	"strings"
)

// MetricNameField is the field label of a metric-name segment. Metric names
// exist only as field names (metrics.<name>), so MET-008 checks them in Go
// and reports a match under this label.
const MetricNameField = "metric_name"

// Query returns the ES|QL pipeline that follows a sensitive-data rule's FROM
// and WHERE. Per document it builds one string of
// "<field>FieldSep<value>" segments, each prefixed by SegmentSep, over
// fields (a missing field adds nothing; multi-values are joined by "; ";
// each value is scanned up to ValueBudget characters; separators inside
// values become spaces), and a document is a hit when a sensitive-key field
// holds a non-placeholder value (at most MaxKeyNameFields key checks) or the
// string matches a detector (one RLIKE per detector). Per service it
// returns the rule shape: rule_passed, service.name, extent (hit documents
// over all documents) and example (the raw segment string of one hit
// document, which carries the matched value and must go through
// RedactExample; on a pass, "No sensitive data matched <N> checks" with N =
// fields x detectors). With no fields the pipeline yields no rows, which
// the runner reports as not applicable.
func Query(fields []string) string { return query(fields, nil, 0) }

// MetricQuery is Query for MET-008: metricNames, checked against the
// detectors in Go (MatchingNames: names are schema, not values), add a
// segment "metric_name FieldSep <name>" to the documents that carry a
// matching metric, so ES|QL decides pass, extent and owning services as for
// any other value. Every name counts towards the pass message's checks.
func MetricQuery(fields, metricNames []string) string {
	return query(fields, MatchingNames(metricNames), len(metricNames))
}

// MatchingNames returns the names that any detector matches, in order.
func MatchingNames(names []string) []string {
	var out []string
	for _, n := range names {
		for _, d := range catalog {
			if d.Match(n) {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// valueSep joins the values of a multi-valued field. It is not a card
// group separator (space, -) nor a key-value token character, so a list of
// numbers (ports 4317, 4318, 8080, 9090) never reads as a card number or
// one value.
const valueSep = "; "

// concatLimit is the largest string ES|QL CONCAT builds: one byte more fails
// the whole query (esql_client_exception), not just the row.
const concatLimit = 1 << 20

// budgetMargin is headroom kept below concatLimit.
const budgetMargin = 4096

// ValueBudget is the number of characters of each field's value (all its
// values joined) that the rule query scans: the rest of a longer value is
// not scanned. It is computed from the same inputs as Query or
// MetricQuery (metricNames nil for Query), so it is the K of the rendered
// query: floor((concatLimit - budgetMargin - label bytes) / (4 x fields)),
// four bytes being the widest UTF-8 character. It is 0 without fields.
func ValueBudget(fields, metricNames []string) int {
	return valueBudget(fields, MatchingNames(metricNames))
}

func valueBudget(fields, metricHits []string) int {
	if len(fields) == 0 {
		return 0
	}
	labels := 0
	for _, f := range fields {
		labels += len(fieldLabel(f))
	}
	for _, n := range metricHits {
		labels += len(metricLabel(n))
	}
	k := (concatLimit - budgetMargin - labels) / (4 * len(fields))
	if k < 1 {
		k = 1
	}
	return k
}

// fieldLabel is the text before a field's value in its segment.
func fieldLabel(f string) string { return SegmentSep + scrub(f) + FieldSep }

// metricLabel is a matching metric name's whole segment.
func metricLabel(n string) string { return SegmentSep + FormatExample(MetricNameField, scrub(n)) }

func query(fields, metricHits []string, names int) string {
	const tail = "| RENAME resource.attributes.service.name AS service.name\n" +
		"| KEEP rule_passed, service.name, example, extent"
	if len(fields) == 0 && len(metricHits) == 0 {
		return "| WHERE false\n" +
			"| STATS total = COUNT(*) BY resource.attributes.service.name\n" +
			"| EVAL rule_passed = true, extent = 0.0, example = \"\"\n" + tail
	}
	var b strings.Builder
	b.WriteString("| EVAL seg = CONCAT(")
	sep := "\n    "
	k := valueBudget(fields, metricHits)
	for _, f := range fields {
		b.WriteString(sep + segment(f, k))
		sep = ",\n    "
	}
	for _, n := range metricHits {
		b.WriteString(sep + "CASE(" + quoteField("metrics."+n) + " IS NOT NULL, " +
			esqlString(metricLabel(n)) + `, "")`)
		sep = ",\n    "
	}
	if len(fields)+len(metricHits) == 1 {
		b.WriteString(sep + `""`) // CONCAT takes at least two arguments
	}
	b.WriteString(")\n| EVAL hit = CASE(")
	checked, _ := KeyNameFields(fields)
	for _, f := range checked {
		b.WriteString("\n    COALESCE(TRIM(" + values(f) + `) NOT RLIKE """` + placeholderLucene + `""", false), true,`)
	}
	for _, d := range catalog {
		b.WriteString("\n    seg RLIKE \"\"\"" + d.Lucene + "\"\"\", true,")
	}
	checks := (len(fields) + names) * len(catalog)
	b.WriteString("\n    false)\n" +
		"| STATS total = COUNT(*), hits = COUNT(*) WHERE hit, example = TOP(seg, 1, \"asc\") WHERE hit BY resource.attributes.service.name\n" +
		"| EVAL rule_passed = hits == 0,\n" +
		"    extent = CASE(total != 0, hits::double / total::double, 0.0),\n" +
		"    example = CASE(hits == 0, \"No sensitive data matched " + strconv.Itoa(checks) + " checks\", example)\n" + tail)
	return b.String()
}

// segment renders field's segment: SegmentSep, the field, FieldSep and its
// values, cut to k characters (LEFT, so the CONCAT stays under concatLimit)
// with raw separators replaced by spaces. LEFT comes first
// because REPLACE returns null for a string over 1 MiB, which would drop
// the value; replacing one character by one leaves the length unchanged.
// COALESCE makes a missing field contribute "" instead of nulling the whole
// CONCAT.
func segment(field string, k int) string {
	return "COALESCE(CONCAT(" + esqlString(fieldLabel(field)) + ", REPLACE(LEFT(" + values(field) + ", " +
		strconv.Itoa(k) + `), "[` + SegmentSep + FieldSep + `]", " ")), "")`
}

// values renders every value of field as one string (multi-values joined by
// valueSep); TO_STRING reads every scannable type, including union types.
func values(field string) string {
	return "MV_CONCAT(TO_STRING(" + quoteField(field) + "), " + esqlString(valueSep) + ")"
}

// quoteField backtick-quotes an ES|QL field name.
func quoteField(f string) string { return "`" + strings.ReplaceAll(f, "`", "``") + "`" }

// scrub replaces raw separators in a name with spaces, so a name cannot
// forge segments.
func scrub(s string) string {
	return strings.NewReplacer(SegmentSep, " ", FieldSep, " ").Replace(s)
}

// esqlString renders s as a plain ES|QL string literal (ES|QL has no \u
// escapes; other control characters stay raw).
func esqlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
