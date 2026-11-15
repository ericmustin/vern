package sensitive

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

func matching(s string) []string {
	var ids []string
	for _, d := range Detectors() {
		if d.Match(s) {
			ids = append(ids, d.ID)
		}
	}
	return ids
}

func has(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// Every example value from the four rule files, with a detector that must fire.
func TestDetectors_SpecExamples(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"-Djavax.net.ssl.trustStorePassword=MyS3cret!", "secret_kv"},
		{"-Dspring.datasource.password=hunter2", "secret_kv"},
		{"api_key=sk-proj-abc123def456", "secret_kv"},
		{"Failed login for user alice@example.com from 10.0.0.1", "email"},
		{"Payment processed for card 4111-1111-1111-1111", "credit_card"},
		{"Authorization: Bearer eyJhbGciOiJIUzI1NiIs...", "bearer_token"},
		{`{"user": {"ssn": "123-45-6789", "name": "Alice"}}`, "ssn"},
		{`{"user": {"ssn": "123-45-6789", "name": "Alice"}}`, "financial_kv"},
		{"alice@example.com", "email"},
		{`{"password": "correct-horse"}`, "secret_kv"},
		{"SELECT * FROM users WHERE email='alice@example.com'", "email"},
		{"https://api.example.com/users?ssn=123-45-6789", "ssn"},
		{"4111-1111-1111-1111", "credit_card"},
		{"Login failed: password=hunter2, user=alice@example.com", "secret_kv"},
		{"Login failed: password=hunter2, user=alice@example.com", "email"},
		{"https://api.example.com/reset?token=abc123", "secret_kv"},
		{"orders.alice@example.com.count", "email"},
		{"diagnosis=flu", "health_kv"},
		{"patient_id=12345", "health_kv"},
		{"insurance_id=ZX-99", "health_kv"},
		{"AKIAIOSFODNN7EXAMPLE", "aws_access_key"},
		{"key AKIAIOSFODNN7EXAMPLE here", "aws_access_key"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIE", "private_key"},
		{"-----BEGIN PRIVATE KEY-----", "private_key"},
		{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "jwt"},
		{"iban=DE89370400440532013000", "financial_kv"},
		{"cvv=123", "financial_kv"},
		{"ghp_" + strings.Repeat("a1B2", 9), "api_key"},
		{"sk-proj-AbC_dEf-123456789012345678", "api_key"},
		{"sk-" + strings.Repeat("aB3", 10), "api_key"},
		{"git@github.com", "email"}, {"a@1.com", "email"}, {"u@163.com", "email"},
		{`{"password"=>"hunter2"}`, "secret_kv"}, {"auth_token=abc", "secret_kv"}, {"accessToken: \"abc\"", "secret_kv"},
		{"x?token=abc", "secret_kv"}, {"PASSWORD=Hunter2", "secret_kv"},
		{"4111111111111111.", "credit_card"}, {"card 4111111111111111. Next", "credit_card"},
		{"sk_live_" + strings.Repeat("a1", 12), "api_key"},
	} {
		if got := matching(c.in); !has(got, c.want) {
			t.Errorf("%q: detectors %v, want %s among them", c.in, got, c.want)
		}
	}
}

func TestDetectors_Cards(t *testing.T) {
	for _, in := range []string{
		"4111111111111111", "4111 1111 1111 1111", "4111-1111-1111-1111", "4222222222222", // visa 16, 13
		"5555555555554444", "5105 1051 0510 5100", "2221000000000009", "2720 9999 9999 9999", // mastercard
		"378282246310005", "3714 496353 98431", // amex
		"6011111111111117", "6500 0000 0000 0000", // discover
		"card=4111111111111111;", "(4111111111111111)",
	} {
		if !has(matching(in), "credit_card") {
			t.Errorf("%q should match credit_card", in)
		}
	}
	for _, in := range []string{
		"9999 1234 5678 9012", "2220000000000000", "2721000000000000", "5611111111111111",
		"41111111111111111",  // 17 digits
		"a4111111111111111",  // alnum neighbour
		"411111111111111111", // 18 digits
	} {
		if has(matching(in), "credit_card") {
			t.Errorf("%q must not match credit_card", in)
		}
	}
}

func TestDetectors_FalsePositiveGuards(t *testing.T) {
	for _, in := range []string{
		"3f2b8c1e-9a4d-4e6f-8b2a-1c3d5e7f9a0b",                             // uuid
		"d41d8cd98f00b204e9800998ecf8427e",                                 // md5
		"da39a3ee5e6b4b0d3255bfef95601890afd80709",                         // sha1
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", // sha256
		"2026-11-14T16:13:29.123Z",
		"4bf92f3577b34da6a3ce929d0e0e4736", // trace id
		"00f067aa0ba902b7",                 // span id
		"v1.71.0", "10.0.0.1", "1731600000", "1731600000123456789",
		"9999 1234 5678 9012",
		"1.2.3-rc.1+build.5", "GET /api/v1/orders/12345", "token_count=5", "max_tokens=100",
		"gen_ai.usage.input_tokens=42", "tokens: 5", "PWD=/home/app", "secretary=bob",
		"password=", `{"password": ""}`, "Bearer authentication", "disk-usage-report-for-volume-2",
		"user@", "@babel/core", "pkg@1.2.3", "k8s.pod.name", "Invalid token: expired",
		// hyphenated UUIDs and floats that used to match as cards
		"41234567-8901-4234-a567-890123456789", "55123456-7890-4123-8456-7890abcdef12",
		"0.4182736451827364", "score=0.5512345678901234", "4182736451827364.5", "x=0.6011111111111117",
		"1.2345678901234567e+06", "card_4111111111111111",
		// redacted placeholders in key-value form
		"password=****", "{'password': '********'}", "password=<redacted>",
		"-Dspring.datasource.password=${DB_PASSWORD}", `{"password"=>"[FILTERED]"}`,
		"password=\nnext", "password=\x1eattributes.x\x1fy", "password=%", "token={x}", "secret=[REDACTED]",
		// pagination cursors, non-secret compounds
		"next_token=abc123", "nextToken=abc123", "page_token=abc123", "pageToken=abc123",
		"continuation-token=abc123", "NEXT_PAGE_TOKEN=abc", "valid_token=1", "invalidToken=1",
		// email look-alikes
		"logo@2x.png", "user@1000.service", "getty@tty1.service", "icon@3X.png",
		// kebab names with the sk- prefix
		"sk-learn-worker-7d4b9c8f6-x2k4p", "sk-payments-service-prod",
	} {
		if got := matching(in); len(got) != 0 {
			t.Errorf("%q must not match any detector, got %v", in, got)
		}
	}
}

// TestLuceneMatchesGo checks that Go compiled from the Lucene string behaves as the
// Lucene semantics demand: whole-string matching, boundary forms, escapes.
// detectors_live_integration_test.go runs the same cases on Elasticsearch.
func TestLuceneMatchesGo(t *testing.T) {
	cases := []struct {
		id, in string
		want   bool
	}{
		{"email", "alice@example.com", true},
		{"email", "x alice@example.com y", true},
		{"email", "alice@example", false},
		{"email", "alice example.com", false},
		// <digits>x first label (the @2x image suffix): one check per '@'.
		{"email", "logo@2x.png", false},
		{"email", "a@12x.ab.ab.ab", false},
		{"email", "a@12x.example.com b@corp.test", true},
		{"email", "a@12y.example.com", true},
		{"email", "a@x2x.example.com", true},
		{"email", "a@.12x.com", true}, // empty first label: not <digits>x
		{"credit_card", "card 4111111111111111 x", true},
		{"credit_card", "94111111111111111", false},
		{"credit_card", "4111111111111111", true},
		{"ssn", "ssn 123-45-6789.", true},
		{"ssn", "0123-45-6789", false},
		{"ssn", "123-45-67890", false},
		{"bearer_token", "BEARER abcdefghijklmnop", true},
		{"bearer_token", "xbearer abcdefghijklmnop", false},
		{"api_key", "key=sk-proj-canaryCANARY1234567890", true},
		{"api_key", "disk-proj-canaryCANARY1234567890", false},
		{"aws_access_key", "AKIAIOSFODNN7EXAMPLE", true},
		{"aws_access_key", "AKIAIOSFODNN7EXAMPLEX", false},
		{"secret_kv", "A=1&Password=Hunter2", true},
		{"secret_kv", "a\nsecret='y'", true}, // a lone x is a mask placeholder
		{"secret_kv", "password=", false},
		{"private_key", "x -----BEGIN EC PRIVATE KEY----- y", true},
		{"private_key", "-----BEGIN CERTIFICATE-----", false},
	}
	byID := map[string]Detector{}
	for _, d := range Detectors() {
		byID[d.ID] = d
	}
	for _, c := range cases {
		if got := byID[c.id].Match(c.in); got != c.want {
			t.Errorf("%s on %q = %v, want %v", c.id, c.in, got, c.want)
		}
	}
	// Lucene hygiene: no (?i); no unescaped RLIKE operators. Backslash escapes
	// the next character (so `\\@` is an escaped backslash then a bare @). The
	// email pattern uses & and ~ on purpose (intersection and complement).
	for _, d := range Detectors() {
		if strings.Contains(d.Lucene, "(?") {
			t.Errorf("%s: Lucene uses a (?...) group", d.ID)
		}
		ops := `@~&#<>"`
		if d.ID == "email" {
			ops = `@#<>"`
		}
		s := d.Lucene
		for i := 0; i < len(s); i++ {
			if s[i] == '\\' {
				i++
				continue
			}
			if strings.IndexByte(ops, s[i]) >= 0 {
				t.Errorf("%s: unescaped %q at %d", d.ID, s[i], i)
			}
		}
	}
	if got, want := len(LucenePatterns()), len(Detectors()); got != want {
		t.Errorf("LucenePatterns = %d, want %d", got, want)
	}
}

func TestDetectorIDsAndOrder(t *testing.T) {
	var ids []string
	for _, d := range Detectors() {
		ids = append(ids, d.ID)
	}
	want := "email credit_card ssn bearer_token jwt api_key aws_access_key secret_kv health_kv financial_kv private_key"
	if strings.Join(ids, " ") != want {
		t.Errorf("ids = %v", ids)
	}
}

func TestSensitiveKey(t *testing.T) {
	for f, want := range map[string]bool{
		"attributes.http.request.header.authorization": true,
		"resource.attributes.enduser.id":               true,
		"attributes.user.email":                        true,
		"attributes.db_password":                       true,
		"attributes.http.request.header.x-api-key":     true,
		"attributes.http.request.header.cookie":        true,
		"Password":                                     true,
		"attributes.token_count":                       false,
		"attributes.http.response.body.size":           false,
		"attributes.gen_ai.usage.input_tokens":         false,
		"attributes.id":                                false,
		"":                                             false,
	} {
		if got := SensitiveKey(f); got != want {
			t.Errorf("SensitiveKey(%q) = %v, want %v", f, got, want)
		}
	}
}

func TestDetectors_RandomFalsePositives(t *testing.T) {
	rng := rand.New(rand.NewSource(20261114))
	hexs := "0123456789abcdef"
	hex := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = hexs[rng.Intn(16)]
		}
		return string(b)
	}
	uuid := func() string {
		v := "89ab"[rng.Intn(4)]
		return hex(8) + "-" + hex(4) + "-4" + hex(3) + "-" + string(v) + hex(3) + "-" + hex(12)
	}
	var card Detector
	all := Detectors()
	for _, d := range all {
		if d.ID == "credit_card" {
			card = d
		}
	}
	gens := map[string]func() string{
		"uuid":       uuid,
		"float":      func() string { return strconv.FormatFloat(rng.Float64(), 'g', -1, 64) },
		"float-key":  func() string { return "score=" + strconv.FormatFloat(rng.Float64(), 'f', -1, 64) },
		"trace-id":   func() string { return hex(32) },
		"hash-sha1":  func() string { return hex(40) },
		"epoch-nano": func() string { return fmt.Sprint(1_700_000_000_000_000_000 + rng.Int63n(1e17)) },
	}
	for name, g := range gens {
		bad := 0
		for i := 0; i < 200_000; i++ {
			if v := g(); card.Match(v) {
				bad++
				if bad <= 3 {
					t.Errorf("%s %q matches credit_card", name, v)
				}
			}
		}
		if bad != 0 {
			t.Errorf("%s: %d/200000 card false positives", name, bad)
		}
		for i := 0; i < 5_000; i++ {
			v := g()
			if got := matching(v); len(got) != 0 {
				t.Fatalf("%s %q matches %v", name, v, got)
			}
		}
	}
}

func TestSensitiveKey_NoGenericSuffix(t *testing.T) {
	for f, want := range map[string]bool{
		"attributes.is_secret": false, "attributes.has_password": false,
		"attributes.next_page_token": false, "attributes.cancellation_token": false,
		"attributes.pwd": false, "attributes.access_token": true, "attributes.db_password": true,
		"attributes.http.request.header.proxy-authorization": true, "attributes.x-auth-token": true,
	} {
		if got := SensitiveKey(f); got != want {
			t.Errorf("SensitiveKey(%q) = %v, want %v", f, got, want)
		}
	}
}

// Placeholder values under kv keys, masked cards, comma decimals, sk-ant keys.
func TestDetectors_R58(t *testing.T) {
	kvIDs := []string{"secret_kv", "health_kv", "financial_kv"}
	for _, in := range []string{
		"password=true", "password=FALSE", "secret=yes", "secret=No", "password=null", "password=None",
		"password=nil", "password=REDACTED", "password=hidden", "password=(hidden)", "password=Masked",
		"password=xxxxx", "password=XXXX", "password=#####", "password=x*x•X#", `{"password": "null"}`,
		`{"password":"(hidden)"}`, "password='None', next=1", "password=SecretStr('**********')",
		"password=SecretStr('xxxx')", `"api_key": "masked"`, "iban=XXXX", "patient_id=none",
		"card_number=411111******1111", "card_number=411111xxxxxx1111", "cvv=***", "is_secret=true",
		"password=null\nnext line", "password=none&user=bob",
	} {
		got := matching(in)
		for _, id := range kvIDs {
			if has(got, id) {
				t.Errorf("%q must not match %s (placeholder value), got %v", in, id, got)
			}
		}
	}
	for _, c := range []struct{ in, want string }{
		{"password=hunter2", "secret_kv"}, {"password=nullable123", "secret_kv"}, {"password=nonexistent", "secret_kv"},
		{"password=truely", "secret_kv"}, {"password=xxxxa", "secret_kv"}, {"password=x1", "secret_kv"},
		{"password=hidden1", "secret_kv"}, {"password=SecretStr('hunter2')", "secret_kv"},
		{"password=null, password=hunter2", "secret_kv"}, {"password=12345", "secret_kv"}, {`{"password": "nope"}`, "secret_kv"},
		{"card_number=4111111111111111", "financial_kv"}, {"card_number=411111***1111", "financial_kv"},
		{"card_number=411111******", "financial_kv"}, {"iban=DE89370400440532013000", "financial_kv"},
		{"patient_id=12345", "health_kv"}, {"password=yesterday", "secret_kv"}, {"password=nox", "secret_kv"},
	} {
		if got := matching(c.in); !has(got, c.want) {
			t.Errorf("%q: detectors %v, want %s among them", c.in, got, c.want)
		}
	}
	// Comma decimals: a left ',' is a card boundary only when not preceded by a digit.
	for _, in := range []string{"3,4111111111111111", "0,5555555555554444", "score=0,4111111111111111"} {
		if has(matching(in), "credit_card") {
			t.Errorf("%q must not match credit_card (comma decimal)", in)
		}
	}
	for _, in := range []string{",4111111111111111", "a,4111111111111111", "cards: x,4111111111111111", "4111111111111111,5500000000000004"} {
		if !has(matching(in), "credit_card") {
			t.Errorf("%q should match credit_card", in)
		}
	}
	// sk-ant keys.
	for _, in := range []string{
		"sk-ant-api03-" + strings.Repeat("aB3_-", 18), "key=sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWx",
		"sk-ant-api123-AbCdEfGhIjKlMnOpQrStUvWx",
	} {
		if !has(matching(in), "api_key") {
			t.Errorf("%q should match api_key", in)
		}
	}
	if has(matching("sk-ant-worker-pool"), "api_key") {
		t.Errorf("sk-ant-worker-pool must not match api_key")
	}
}

func TestRandomCommaDecimalsNoCard(t *testing.T) {
	rng := rand.New(rand.NewSource(58))
	var card Detector
	for _, d := range Detectors() {
		if d.ID == "credit_card" {
			card = d
		}
	}
	bad := 0
	for i := 0; i < 100_000; i++ {
		v := strings.Replace(strconv.FormatFloat(rng.Float64()*float64(rng.Intn(1000)+1), 'f', 16, 64), ".", ",", 1)
		if card.Match(v) {
			bad++
			if bad <= 3 {
				t.Errorf("%q matches credit_card", v)
			}
		}
	}
	if bad != 0 {
		t.Errorf("%d/100000 comma-decimal card false positives", bad)
	}
}

func TestPlaceholder(t *testing.T) {
	for _, v := range []string{
		"", "  ", "****", "••••", "xxxx", "XXXX", "####", "[FILTERED]", "<redacted>", "[REDACTED]", "Redacted",
		"${DB_PASSWORD}", "$DB_PASSWORD", "null", "None", "TRUE", "false", "yes", "NO", "nil", "hidden", "(hidden)",
		"masked", "SecretStr('**********')", "411111******1111", " null ",
	} {
		if !isPlaceholder(v) {
			t.Errorf("isPlaceholder(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"hunter2", "nullable", "x1", "411111***1111", "SecretStr('hunter2')", "$", "secret value"} {
		if isPlaceholder(v) {
			t.Errorf("isPlaceholder(%q) = true, want false", v)
		}
	}
}

func TestKeyNameFields(t *testing.T) {
	fields := []string{"name", "attributes.password", "attributes.user.email.text", "attributes.http.method", "attributes.text"}
	checked, overflow := KeyNameFields(fields)
	if strings.Join(checked, ",") != "attributes.password,attributes.user.email.text" || len(overflow) != 0 {
		t.Errorf("checked = %v, overflow = %v", checked, overflow)
	}
	var many []string
	for i := 0; i < 40; i++ {
		many = append(many, fmt.Sprintf("attributes.k%02d.password", i))
	}
	checked, overflow = KeyNameFields(append([]string{"name"}, many...))
	if len(checked) != MaxKeyNameFields || len(overflow) != 40-MaxKeyNameFields {
		t.Fatalf("checked %d, overflow %d", len(checked), len(overflow))
	}
	if checked[0] != "attributes.k00.password" || overflow[0] != fmt.Sprintf("attributes.k%02d.password", MaxKeyNameFields) {
		t.Errorf("order: checked[0]=%s overflow[0]=%s", checked[0], overflow[0])
	}
}

// TestKeyNameFields_DedupesByKeyName: a keyword field and its ".text"
// subfield name one key and take one slot; the ".text" field is kept, since
// it also holds the values the keyword drops for length.
func TestKeyNameFields_DedupesByKeyName(t *testing.T) {
	fields := []string{"attributes.password", "attributes.token", "attributes.password.text", "attributes.token", "attributes.secret.text"}
	checked, overflow := KeyNameFields(fields)
	if got := strings.Join(checked, ","); got != "attributes.password.text,attributes.token,attributes.secret.text" || len(overflow) != 0 {
		t.Errorf("checked = %s, overflow = %v", got, overflow)
	}
	// 40 keys, each as keyword + .text: 32 slots, 8 keys overflow (once each).
	var many []string
	for i := 0; i < 40; i++ {
		k := fmt.Sprintf("attributes.k%02d.password", i)
		many = append(many, k, k+".text")
	}
	checked, overflow = KeyNameFields(many)
	if len(checked) != MaxKeyNameFields || len(overflow) != 40-MaxKeyNameFields {
		t.Fatalf("checked %d, overflow %d", len(checked), len(overflow))
	}
	for _, f := range append(append([]string(nil), checked...), overflow...) {
		if !strings.HasSuffix(f, ".text") {
			t.Errorf("%s: want the .text field of each key", f)
		}
	}
}
