package sensitive

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

const goldenSmall = "../../testdata/sensitive/query_small.golden"

// readable replaces the raw separators and the detector and placeholder
// patterns with short markers, so the golden file stays legible and does not
// change with every detector tweak (TestQuery_PatternsVerbatim pins them).
func readable(q string) string {
	for _, d := range Detectors() {
		q = strings.ReplaceAll(q, `"""`+d.Lucene+`"""`, "<lucene:"+d.ID+">")
	}
	q = strings.ReplaceAll(q, `"""`+placeholderLucene+`"""`, "<lucene:placeholder>")
	q = strings.ReplaceAll(q, SegmentSep, "<RS>")
	return strings.ReplaceAll(q, FieldSep, "<US>")
}

func TestQuery_GoldenSmall(t *testing.T) {
	got := readable(Query([]string{"name", "attributes.password"}))
	if *update {
		if err := os.WriteFile(goldenSmall, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenSmall)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if got != string(want) {
		t.Errorf("query differs from %s (run with -update after checking):\n%s", goldenSmall, got)
	}
}

func TestQuery_PatternsVerbatim(t *testing.T) {
	q := Query([]string{"attributes.password", "name"})
	for _, d := range Detectors() {
		if !strings.Contains(q, `seg RLIKE """`+d.Lucene+`""", true`) {
			t.Errorf("%s pattern not embedded verbatim", d.ID)
		}
		if strings.Contains(d.Lucene, `"""`) || strings.HasSuffix(d.Lucene, `"`) {
			t.Errorf("%s pattern cannot sit in a raw ES|QL literal", d.ID)
		}
	}
	if !strings.Contains(q, `NOT RLIKE """`+placeholderLucene+`"""`) {
		t.Error("placeholder pattern not embedded verbatim")
	}
	if strings.Count(q, " RLIKE ") != len(Detectors())+1 {
		t.Errorf("want %d RLIKEs (one per detector plus one key-name check), got %d", len(Detectors())+1, strings.Count(q, " RLIKE "))
	}
}

func TestQuery_EscapingAndCase(t *testing.T) {
	q := Query([]string{"attributes.x"})
	if !strings.Contains(q, `\@`) || !strings.Contains(q, `[aA]`) {
		t.Error("patterns should escape @ and spell case as [aA]")
	}
	if strings.Contains(q, "(?i)") {
		t.Error("(?i) silently matches nothing in Lucene")
	}
	// Field names are backtick-quoted (doubling backticks) and string
	// literals escape quotes and backslashes; raw separators never come from
	// a field name.
	q = Query([]string{"attributes.we`ird\"na\\me\x1e"})
	if !strings.Contains(q, "TO_STRING(`attributes.we``ird\"na\\me\x1e`)") {
		t.Errorf("field not backtick-quoted:\n%s", q)
	}
	if !strings.Contains(q, `CONCAT("`+SegmentSep+`attributes.we`+"`"+`ird\"na\\me `+FieldSep+`", `) {
		t.Errorf("field literal not escaped:\n%s", q)
	}
}

func TestQuery_NullSafeSegments(t *testing.T) {
	fields := []string{"name", "attributes.a"}
	q := Query(fields)
	k := strconv.Itoa(ValueBudget(fields, nil))
	for _, f := range fields {
		seg := `COALESCE(CONCAT("` + SegmentSep + f + FieldSep + `", REPLACE(LEFT(MV_CONCAT(TO_STRING(` + "`" + f + "`" +
			`), "; "), ` + k + `), "[` + SegmentSep + FieldSep + `]", " ")), "")`
		if !strings.Contains(q, seg) {
			t.Errorf("segment for %s is not null-safe, bounded and scrubbed:\n%s", f, q)
		}
	}
}

// Each value is cut so the worst-case CONCAT (4 bytes per character)
// stays under ES|QL's 1 MiB limit; few fields keep a large budget.
func TestValueBudget(t *testing.T) {
	if ValueBudget(nil, nil) != 0 {
		t.Error("no fields: budget 0")
	}
	one := ValueBudget([]string{"body.text"}, nil)
	if one < 250_000 {
		t.Errorf("one field: budget %d, want about 260k", one)
	}
	for _, n := range []int{1, 2, 50, 1000, 2000} {
		var fields []string
		for i := 0; i < n; i++ {
			fields = append(fields, fmt.Sprintf("attributes.some.fairly.long.attribute.key.%04d", i))
		}
		names := []string{"orders.alice@example.com.count", "http.server.duration"}
		k := ValueBudget(fields, names)
		worst := budgetMargin + 4*k*n
		for _, f := range fields {
			worst += len(fieldLabel(f))
		}
		worst += len(metricLabel(names[0]))
		if k < 1 || worst > concatLimit {
			t.Errorf("%d fields: budget %d, worst case %d bytes > %d", n, k, worst, concatLimit)
		}
		if q := MetricQuery(fields, names); !strings.Contains(q, `"; "), `+strconv.Itoa(k)+`), `) {
			t.Errorf("%d fields: query does not use budget %d", n, k)
		}
	}
	if k := ValueBudget(make([]string, 1000), nil); k < 200 {
		t.Errorf("1000 fields: budget %d", k)
	}
}

// Multi-values are joined by "; ", so numeric arrays never read as a
// card (or any other) match, while one real card value still matches.
func TestMultiValueJoinNoFalsePositives(t *testing.T) {
	for _, vals := range [][]string{
		{"4317", "4318", "8080", "9090"}, {"5100", "5101", "5102", "5103"}, {"6011", "6012", "6013", "6014"},
	} {
		if got := matching(strings.Join(vals, valueSep)); len(got) != 0 {
			t.Errorf("%v joined matches %v", vals, got)
		}
		if !has(matching(strings.Join(vals, " ")), "credit_card") {
			t.Errorf("%v joined by spaces should read as a card number", vals)
		}
	}
	rng := rand.New(rand.NewSource(67))
	prefixes := []int{4000, 4111, 4999, 5100, 5555, 2221, 2720, 6011, 6500, 3400, 3700}
	for i := 0; i < 20_000; i++ {
		n := 3 + rng.Intn(3)
		vals := make([]string, n)
		for j := range vals {
			if rng.Intn(2) == 0 {
				vals[j] = strconv.Itoa(prefixes[rng.Intn(len(prefixes))] + rng.Intn(10))
			} else {
				vals[j] = strconv.Itoa(1000 + rng.Intn(9000))
			}
		}
		if got := matching(strings.Join(vals, valueSep)); len(got) != 0 {
			t.Fatalf("%v joined matches %v", vals, got)
		}
	}
	for _, card := range []string{"4111 1111 1111 1111", "5555555555554444", "6011-1111-1111-1117"} {
		if !has(matching(strings.Join([]string{"8080", card, "9090"}, valueSep)), "credit_card") {
			t.Errorf("a card among multi-values should still match: %s", card)
		}
	}
}

func TestQuery_KeyNameBranchFirst(t *testing.T) {
	q := Query([]string{"name", "attributes.user.email.text", "attributes.password"})
	hit := q[strings.Index(q, "hit = CASE("):]
	k1 := strings.Index(hit, "TO_STRING(`attributes.user.email.text`)")
	k2 := strings.Index(hit, "TO_STRING(`attributes.password`)")
	d0 := strings.Index(hit, "seg RLIKE")
	if k1 < 0 || k2 < 0 || d0 < 0 || !(k1 < k2 && k2 < d0) {
		t.Errorf("key-name branches must come first, in field order: %d %d %d\n%s", k1, k2, d0, hit)
	}
	if strings.Contains(hit, "TO_STRING(`name`)") {
		t.Error("name is not a sensitive key")
	}
	// Bounded at MaxKeyNameFields.
	var many []string
	for i := 0; i < MaxKeyNameFields+5; i++ {
		many = append(many, "attributes.k"+strconv.Itoa(i)+".password")
	}
	if n := strings.Count(Query(many), "NOT RLIKE"); n != MaxKeyNameFields {
		t.Errorf("key-name checks = %d, want %d", n, MaxKeyNameFields)
	}
}

func TestQuery_EmptyFieldsNotApplicable(t *testing.T) {
	for _, q := range []string{Query(nil), MetricQuery(nil, []string{"http.server.duration"})} {
		if !strings.Contains(q, "| WHERE false") || strings.Contains(q, "RLIKE") {
			t.Errorf("empty field list must yield no rows:\n%s", q)
		}
		if !strings.HasSuffix(q, "| KEEP rule_passed, service.name, example, extent") {
			t.Errorf("must keep the rule shape:\n%s", q)
		}
	}
}

func TestQuery_PassMessageCountsChecks(t *testing.T) {
	n := len(Detectors())
	if q := Query([]string{"a", "b", "c"}); !strings.Contains(q, `"No sensitive data matched `+strconv.Itoa(3*n)+` checks"`) {
		t.Errorf("want %d checks:\n%s", 3*n, q)
	}
	q := MetricQuery([]string{"a"}, []string{"orders.count", "orders.alice@example.com.count"})
	if !strings.Contains(q, `"No sensitive data matched `+strconv.Itoa(3*n)+` checks"`) {
		t.Errorf("metric names count as checks too (want %d):\n%s", 3*n, q)
	}
	if !strings.Contains(q, "| KEEP rule_passed, service.name, example, extent") {
		t.Error("missing KEEP")
	}
}

func TestMatchingNames(t *testing.T) {
	got := MatchingNames([]string{"http.server.duration", "orders.alice@example.com.count", "jvm.memory.used", "tenant.123-45-6789.requests"})
	if strings.Join(got, ",") != "orders.alice@example.com.count,tenant.123-45-6789.requests" {
		t.Errorf("MatchingNames = %v", got)
	}
}

func TestMET008_MetricNameCheck(t *testing.T) {
	q := MetricQuery([]string{"unit"}, []string{"http.server.duration", "orders.alice@example.com.count"})
	// The matching name becomes a segment only on docs that carry the metric,
	// formatted like any other segment so Locate and RedactExample cover it.
	want := "CASE(`metrics.orders.alice@example.com.count` IS NOT NULL, " +
		`"` + SegmentSep + FormatExample(MetricNameField, "orders.alice@example.com.count") + `", "")`
	if !strings.Contains(q, want) {
		t.Errorf("missing metric-name segment %s in:\n%s", want, q)
	}
	if strings.Contains(q, "http.server.duration") {
		t.Error("non-matching metric names stay out of the query")
	}
	field, id, _, ok := Locate(SegmentSep + FormatExample(MetricNameField, "orders.alice@example.com.count"))
	if !ok || field != MetricNameField || id != "email" {
		t.Errorf("Locate = %q %q %v", field, id, ok)
	}
	if got := RedactExample(SegmentSep + FormatExample(MetricNameField, "orders.alice@example.com.count")); strings.Contains(got, "alice") {
		t.Errorf("RedactExample leaks: %s", got)
	}
}

func TestLocate_TextSubfieldKey(t *testing.T) {
	f, id, _, ok := Locate(SegmentSep + "attributes.password.text" + FieldSep + "correct-horse")
	if !ok || f != "attributes.password.text" || id != KeyDetectorID {
		t.Errorf("Locate = %q %q %v", f, id, ok)
	}
}
