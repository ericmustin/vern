//go:build integration

package sensitive

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestLuceneMatchesGo_Live runs every detector's Lucene pattern as an ES|QL
// RLIKE against the Go test corpus and requires the same verdict as Go.
func TestLuceneMatchesGo_Live(t *testing.T) {
	base := os.Getenv("VERN_TEST_ES_URL")
	if base == "" {
		base = "http://localhost:9200"
	}
	client := &http.Client{Timeout: 20 * time.Second}
	if resp, err := client.Get(base + "/_cluster/health"); err != nil {
		t.Skipf("Elasticsearch unreachable at %s: %v", base, err)
	} else {
		resp.Body.Close()
	}
	rng0 := rand.New(rand.NewSource(58))
	corpus := []string{
		"41234567-8901-4234-a567-890123456789", "55123456-7890-4123-8456-7890abcdef12", "0.4182736451827364",
		"score=0.5512345678901234", "4182736451827364.5", "card 4111111111111111. Next", "4111111111111111.",
		"card_4111111111111111", "password=****", "{'password': '********'}", "password=<redacted>",
		"-Dspring.datasource.password=${DB_PASSWORD}", `{"password"=>"[FILTERED]"}`, `{"password"=>"hunter2"}`,
		"password=\nnext", "password=\x1eattributes.x\x1fy", "password=%", "token={x}", "next_token=abc123",
		"pageToken=abc123", "continuation-token=abc123", "auth_token=abc", "accessToken: \"abc\"", "x?token=abc",
		"valid_token=1", "logo@2x.png", "user@1000.service", "getty@tty1.service", "git@github.com", "u@163.com",
		"a@b.service.com", "sk-learn-worker-7d4b9c8f6-x2k4p", "sk-payments-service-prod",
		"sk-proj-AbC_dEf-123456789012345678", "sk-" + strings.Repeat("aB3", 10),
		"alice@example.com", "x alice@example.com y", "alice@example", "4111111111111111",
		"4111 1111 1111 1111", "card 4111-1111-1111-1111 x", "94111111111111111", "378282246310005",
		"3714 496353 98431", "5555555555554444", "2221000000000009", "2720 9999 9999 9999", "6011111111111117",
		"9999 1234 5678 9012", "ssn 123-45-6789.", "0123-45-6789", "Authorization: Bearer eyJhbGciOiJIUzI1NiIs...",
		"Bearer authentication", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n",
		"key=sk-proj-canaryCANARY1234567890", "disk-proj-canaryCANARY1234567890", "ghp_" + strings.Repeat("a1B2", 9),
		"AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLEX", "-Dspring.datasource.password=hunter2",
		"https://api.example.com/reset?token=abc123", `{"password": "correct-horse"}`, "token_count=5", "password=",
		"diagnosis=flu", "patient_id=12345", "iban=DE89370400440532013000", "cvv=123",
		"-----BEGIN RSA PRIVATE KEY-----", "-----BEGIN CERTIFICATE-----", "3f2b8c1e-9a4d-4e6f-8b2a-1c3d5e7f9a0b",
		"4bf92f3577b34da6a3ce929d0e0e4736", "2026-11-14T16:13:29.123Z", "v1.71.0", "10.0.0.1", "1731600000",
		"a\nsecret='x'", "a\nsecret='y'", "name\x1fvalue\x1ealice@example.com\x1fq",
		"a@12x.ab.ab.ab", "a@12x.example.com b@corp.test", "a@12y.example.com", "a@x2x.example.com",
		"a@.12x.com", "a@12X.example.com", "a@" + strings.Repeat("1", 300) + "x" + strings.Repeat(".ab", 100),
		// Placeholder values, masked cards, comma decimals, sk-ant keys.
		"password=true", "password=FALSE", "secret=yes", "secret=No", "password=null", "password=None",
		"password=nil", "password=REDACTED", "password=hidden", "password=(hidden)", "password=Masked",
		"password=xxxxx", "password=XXXX", "password=#####", "password=x*x\u2022X#", `{"password": "null"}`,
		`{"password":"(hidden)"}`, "password='None', next=1", "password=SecretStr('**********')",
		"password=SecretStr('xxxx')", "password=SecretStr('hunter2')", `"api_key": "masked"`, "iban=XXXX",
		"patient_id=none", "card_number=411111******1111", "card_number=411111xxxxxx1111", "card_number=411111***1111",
		"card_number=411111******", "is_secret=true", "password=null\nnext line", "password=none&user=bob",
		"password=nullable123", "password=nonexistent", "password=truely", "password=xxxxa", "password=x1",
		"password=hidden1", "password=null, password=hunter2", "password=12345", "password=yesterday",
		"password=n\u00f6ne", "password=\u00e9t\u00e9", "password=\u2022\u2022\u2022", "password=x\u00e9",
		"3,4111111111111111", "0,5555555555554444", "score=0,4111111111111111", ",4111111111111111",
		"a,4111111111111111", "4111111111111111,5500000000000004", "sk-ant-api03-" + strings.Repeat("aB3_-", 18),
		"key=sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWx", "sk-ant-api123-AbCdEfGhIjKlMnOpQrStUvWx", "sk-ant-worker-pool",
		// Multi-values joined with valueSep, and the same lists joined
		// with a space (which would read as a card number).
		"4317" + valueSep + "4318" + valueSep + "8080" + valueSep + "9090", "5100; 5101; 5102; 5103", "6011; 6012; 6013; 6014",
		"4317 4318 8080 9090", "5100 5101 5102 5103", "x; 4111111111111111; y", "1234; 4111 1111 1111 1111",
	}
	rngMV := rand.New(rand.NewSource(67))
	for i := 0; i < 40; i++ {
		var vs []string
		for n := 3 + rngMV.Intn(3); n > 0; n-- {
			vs = append(vs, fmt.Sprintf("%04d", rngMV.Intn(10000)))
		}
		corpus = append(corpus, strings.Join(vs, valueSep))
	}
	for i := 0; i < 30; i++ {
		corpus = append(corpus, strings.Replace(strconv.FormatFloat(rng0.Float64()*1000, 'f', 16, 64), ".", ",", 1))
	}
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 60; i++ {
		corpus = append(corpus, fmt.Sprintf("%08x-%04x-4%03x-a%03x-%012x", rng.Uint32(), rng.Intn(1<<16), rng.Intn(1<<12), rng.Intn(1<<12), rng.Int63n(1<<48)),
			strconv.FormatFloat(rng.Float64(), 'g', -1, 64), fmt.Sprintf("%032x", rng.Uint64())+fmt.Sprintf("%016x", rng.Uint64()))
	}
	for _, d := range Detectors() {
		for _, in := range corpus {
			q := fmt.Sprintf("ROW s = %s | EVAL m = s RLIKE \"\"\"%s\"\"\" | KEEP m", jsonString(in), d.Lucene)
			body, _ := json.Marshal(map[string]string{"query": q})
			resp, err := client.Post(base+"/_query", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("es: %v", err)
			}
			var out struct {
				Values [][]any        `json:"values"`
				Error  map[string]any `json:"error"`
			}
			err = json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			if err != nil || out.Error != nil || len(out.Values) != 1 {
				t.Fatalf("%s on %q: err=%v es=%v", d.ID, in, err, out.Error)
			}
			if got, want := out.Values[0][0] == true, d.Match(in); got != want {
				t.Errorf("%s on %q: Lucene %v, Go %v", d.ID, in, got, want)
			}
		}
	}
}

// jsonString renders s as an ES|QL string literal (ES|QL has no \u escapes; other control characters stay raw).
func jsonString(s string) string {
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

// esqlRow runs `ROW s = <in> | EVAL m = <expr> | KEEP m` and returns m.
func esqlRow(t *testing.T, in, expr string) any {
	t.Helper()
	base := os.Getenv("VERN_TEST_ES_URL")
	if base == "" {
		base = "http://localhost:9200"
	}
	client := &http.Client{Timeout: 20 * time.Second}
	s := "null"
	if in != "\x00null" {
		s = jsonString(in)
	}
	body, _ := json.Marshal(map[string]string{"query": "ROW s = " + s + " | EVAL m = " + expr + " | KEEP m"})
	resp, err := client.Post(base+"/_query", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Skipf("Elasticsearch unreachable at %s: %v", base, err)
	}
	defer resp.Body.Close()
	var out struct {
		Values [][]any        `json:"values"`
		Error  map[string]any `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Error != nil || len(out.Values) != 1 {
		t.Fatalf("%s on %q: err=%v es=%v", expr, in, err, out.Error)
	}
	return out.Values[0][0]
}

// TestKeyNameCheckMatchesGo_Live runs the key-name check the rule query uses
// (a non-placeholder value) against Go's isPlaceholder.
func TestKeyNameCheckMatchesGo_Live(t *testing.T) {
	expr := `COALESCE(TRIM(s) NOT RLIKE """` + placeholderLucene + `""", false)`
	for _, in := range []string{
		"", "  ", "\t****\n", "****", "•••", "xxxx", "XXXX", "####", "x*X#", "[FILTERED]", "<redacted>",
		"[REDACTED]", "Redacted", "${DB_PASSWORD}", "$DB_PASSWORD", "null", "None", "TRUE", "false", "yes", "NO",
		"nil", "hidden", "(hidden)", "masked", "SecretStr('**********')", "411111******1111", " null ", "hunter2",
		"nullable", "x1", "411111***1111", "SecretStr('hunter2')", "$", "secret value", "opaque-session-value-1",
		"\x00null",
	} {
		got := esqlRow(t, in, expr) == true
		want := in != "\x00null" && !isPlaceholder(in)
		if got != want {
			t.Errorf("key check on %q: ES|QL %v, Go %v", in, got, want)
		}
	}
}

// TestNullSafeSegment_Live pins why every segment is wrapped in COALESCE:
// CONCAT returns null when any argument is null, which would hide every other
// field of the document.
func TestNullSafeSegment_Live(t *testing.T) {
	if got := esqlRow(t, "\x00null", `CONCAT("a", "b", s)`); got != nil {
		t.Fatalf("CONCAT with a null argument = %v, want null", got)
	}
	seg := segment("s", ValueBudget([]string{"s"}, nil))
	if !strings.Contains(Query([]string{"s"}), seg) {
		t.Fatal("Query does not use segment()")
	}
	if got := esqlRow(t, "\x00null", `CONCAT("a", `+seg+`, "b")`); got != "ab" {
		t.Errorf("segment of a null field = %v, want an empty contribution", got)
	}
	if got := esqlRow(t, "v\x1ew\x1fx", `CONCAT("a", `+seg+`, "b")`); got != "a"+SegmentSep+"s"+FieldSep+"v w xb" {
		t.Errorf("segment = %q, want separators in the value scrubbed", got)
	}
}

// TestQuery_Live runs generated pipelines over ROW documents: a sensitive
// key fails, a clean doc passes with the check count, and an empty field
// list yields no rows (not applicable). Columns: rule_passed, service.name,
// example, extent (the workflow store step reads them by position).
func TestQuery_Live(t *testing.T) {
	base := os.Getenv("VERN_TEST_ES_URL")
	if base == "" {
		base = "http://localhost:9200"
	}
	client := &http.Client{Timeout: 30 * time.Second}
	rows := func(q string) [][]any {
		body, _ := json.Marshal(map[string]string{"query": q})
		resp, err := client.Post(base+"/_query", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Skipf("Elasticsearch unreachable: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Values [][]any        `json:"values"`
			Error  map[string]any `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Error != nil {
			t.Fatalf("query failed: err=%v es=%v", err, out.Error)
		}
		return out.Values
	}
	fields := []string{"name", "attributes.password"}
	doc := func(pw string) string {
		return "ROW `resource.attributes.service.name` = \"svc\", name = \"GET /\", `attributes.password` = " + jsonString(pw) + "\n"
	}
	got := rows(doc("opaque-1") + Query(fields))
	if len(got) != 1 || got[0][0] != false || got[0][3] != 1.0 {
		t.Fatalf("sensitive key: rows %v", got)
	}
	if f, id, _, ok := Locate(got[0][2].(string)); !ok || f != "attributes.password" || id != KeyDetectorID {
		t.Errorf("Locate = %s %s %v", f, id, ok)
	}
	got = rows(doc("********") + Query(fields))
	want := "No sensitive data matched " + strconv.Itoa(2*len(Detectors())) + " checks"
	if len(got) != 1 || got[0][0] != true || got[0][2] != want {
		t.Errorf("placeholder: rows %v, want a pass with %q", got, want)
	}
	// Numeric multi-values are joined with "; " and read as no card;
	// a real card among multi-values still matches.
	mv := []string{"ports", "codes"}
	arrays := func(codes string) string {
		return "ROW `resource.attributes.service.name` = \"svc\", ports = [4317, 4318, 8080, 9090], codes = " + codes + "\n"
	}
	got = rows(arrays(`["5100", "5101", "5102", "5103"]`) + Query(mv))
	if len(got) != 1 || got[0][0] != true {
		t.Errorf("numeric arrays: rows %v, want a pass", got)
	}
	got = rows(arrays(`["6011", "4111 1111 1111 1111", "6014"]`) + Query(mv))
	if len(got) != 1 || got[0][0] != false {
		t.Fatalf("card among multi-values: rows %v, want a failure", got)
	}
	if f, id, _, ok := Locate(got[0][2].(string)); !ok || f != "codes" || id != "credit_card" {
		t.Errorf("Locate = %s %s %v", f, id, ok)
	}
	if got = rows(doc("x") + Query(nil)); len(got) != 0 {
		t.Errorf("empty field list: rows %v, want none", got)
	}
}
