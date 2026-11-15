package sensitive

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"alice@example.com": "al…om (17 chars)",
		"hunter2":           "••• (7 chars)",
		"12345678":          "••• (8 chars)",
		"123456789":         "12…89 (9 chars)",
		"":                  "••• (0 chars)",
		"héllo wörld ünï":   "hé…nï (15 chars)",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	in := "alice-supersecret@example.com"
	if got := Redact(in); strings.Contains(got, "supersecret") || strings.Contains(got, "alice-") {
		t.Errorf("Redact leaks the middle: %q", got)
	}
}

func seg(field, value string) string { return FormatExample(field, value) }

func TestFormatExample(t *testing.T) {
	if got := FormatExample("a.b", "v"); got != "a.b\x1fv" {
		t.Errorf("got %q", got)
	}
}

func TestLocate(t *testing.T) {
	raw := strings.Join([]string{
		seg("attributes.http.method", "GET"),
		seg("attributes.msg", "contact alice@example.com now"),
		seg("attributes.password", "hunter2"),
		seg("attributes.note", "card 4111-1111-1111-1111"),
	}, SegmentSep)
	f, id, v, ok := Locate(raw)
	if !ok || f != "attributes.password" || id != KeyDetectorID || v != "hunter2" {
		t.Errorf("key list must win: %q %q %q %v", f, id, v, ok)
	}
	raw = strings.Join([]string{
		seg("attributes.password", ""), // empty value under a sensitive key is fine
		seg("attributes.note", "card 4111-1111-1111-1111"),
		seg("attributes.msg", "contact alice@example.com now"),
	}, SegmentSep)
	f, id, _, ok = Locate(raw)
	if !ok || f != "attributes.msg" || id != "email" { // detector order: email before credit_card
		t.Errorf("detector order: %q %q %v", f, id, ok)
	}
	if _, _, _, ok := Locate(seg("a", "1.2.3") + SegmentSep + seg("b", "ok") + SegmentSep + "no separator here"); ok {
		t.Error("clean example must not locate")
	}
	if _, _, _, ok := Locate(""); ok {
		t.Error("empty must not locate")
	}
	// A value containing a newline and the multi-value separator still locates.
	if _, id, _, ok := Locate(seg("body", "a\n␟alice@example.com")); !ok || id != "email" {
		t.Errorf("multiline: %q %v", id, ok)
	}
}

func TestRedactExample_RoundTrip(t *testing.T) {
	raw := seg("attributes.http.method", "GET") + SegmentSep + seg("attributes.msg", "alice@example.com")
	if got, want := RedactExample(raw), "attributes.msg: email address (al…om (17 chars))"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if got, want := RedactExample(seg("attributes.password", "hunter2")), "attributes.password: sensitive key name (••• (7 chars))"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRedactExample_GarbageIsRedacted(t *testing.T) {
	for _, raw := range []string{"", "garbage", "alice@example.com", seg("a", "fine"), SegmentSep + FieldSep} {
		if got := RedactExample(raw); got != "<field unknown>: <detector unknown> (redacted)" {
			t.Errorf("RedactExample(%q) = %q", raw, got)
		}
	}
}

func TestRedactExample_NeverLeaksValue(t *testing.T) {
	secrets := []string{
		"alice-middle-part@example.com",
		"password=ThisIsTheSecretMiddle99",
		"Bearer abcdefghijklmnopqrstuvwxyz0123",
		"4111-1111-1111-1111",
		"123-45-6789 and more",
		"AKIAIOSFODNN7EXAMPLE",
		"sk-proj-abcdefghijklmnopqrstuv",
	}
	for _, s := range secrets {
		out := RedactExample(seg("attributes.x", "pre "+s+" post"))
		for _, frag := range []string{"middle", "Middle", "mnop", "1111-1111", "6789", "IOSFODNN", "cdefghij", "pre ", " post"} {
			if strings.Contains(out, frag) {
				t.Errorf("RedactExample leaks %q for %q: %s", frag, s, out)
			}
		}
	}
	// A field path that is itself an email is masked.
	out := RedactExample(seg("attributes.user.bob@corp.example", "pw") + SegmentSep + seg("attributes.password", "x"))
	if strings.Contains(out, "bob@corp") {
		t.Errorf("field leaked: %s", out)
	}
	out = RedactExample(seg("metric.bob-the-user@corp.example", "alice@example.com"))
	if strings.Contains(out, "bob-the") {
		t.Errorf("field leaked: %s", out)
	}
}

func TestLocate_PlaceholderUnderSensitiveKey(t *testing.T) {
	for _, v := range []string{"", "   ", "****", "••••••", "xxxx", "XXXX", "####", "[FILTERED]", "<redacted>",
		"[REDACTED]", "REDACTED", "redacted", "${DB_PASSWORD}", "$DB_PASSWORD", "null", "None", "true", "FALSE"} {
		if _, id, _, ok := Locate(seg("attributes.password", v)); ok {
			t.Errorf("placeholder %q under a sensitive key located as %s", v, id)
		}
	}
	if _, id, _, ok := Locate(seg("attributes.password", "hunter2")); !ok || id != KeyDetectorID {
		t.Errorf("real value must locate: %q %v", id, ok)
	}
	// A placeholder does not hide a real detector hit elsewhere.
	raw := seg("attributes.password", "****") + SegmentSep + seg("attributes.msg", "alice@example.com")
	if f, id, _, ok := Locate(raw); !ok || f != "attributes.msg" || id != "email" {
		t.Errorf("got %q %q %v", f, id, ok)
	}
}

func TestRedactExample_RawSeparatorsInValue(t *testing.T) {
	raw := "card on file" + SegmentSep + "My PIN is 4417 and SSN-ish note" + FieldSep + "alice@example.com"
	out := RedactExample(raw)
	for _, frag := range []string{"PIN", "4417", "SSN-ish", "card on file"} {
		if strings.Contains(out, frag) {
			t.Errorf("leaked %q: %s", frag, out)
		}
	}
	if want := "<field masked>: email address (al…om (17 chars))"; out != want {
		t.Errorf("got %q want %q", out, want)
	}
	long := strings.Repeat("a", 300)
	if out := RedactExample(seg(long, "alice@example.com")); !strings.HasPrefix(out, "<field masked>: ") {
		t.Errorf("long field printed: %.40s", out)
	}
	if out := RedactExample(seg("attributes.ok-path_1[0]", "alice@example.com")); !strings.HasPrefix(out, "attributes.ok-path_1[0]: ") {
		t.Errorf("valid field masked: %s", out)
	}
}

// pathologicalEmail is a worst-case value for the email detector: an '@', a
// long digit run, 'x', then many ".ab" labels. A matcher that rescans the
// digit run at every '.' is quadratic (261k characters took over 180 s).
func pathologicalEmail(n int) string {
	digits := n / 2
	v := "a@" + strings.Repeat("1", digits) + "x" + strings.Repeat(".ab", (n-digits)/3)
	return v
}

func TestRedactExample_PathologicalEmailIsLinear(t *testing.T) {
	v := pathologicalEmail(261117)
	raw := seg("attributes.a", v) + SegmentSep + seg("attributes.b", "123-45-6789")
	start := time.Now()
	out := RedactExample(raw)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("RedactExample on a %d-byte pathological value took %s, want < 2s", len(v), d)
	}
	if want := "attributes.b: social security number (12…89 (11 chars))"; out != want {
		t.Errorf("got %q want %q", out, want)
	}
	start = time.Now()
	if Detectors()[0].Match(v) {
		t.Error("a <digits>x first label must not match email")
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("email Match took %s", d)
	}
}

func TestRedactExampleContext_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw := seg("attributes.msg", "alice@example.com")
	if got := RedactExampleContext(ctx, raw); got != UnknownExample {
		t.Errorf("cancelled: got %q want %q", got, UnknownExample)
	}
	if got := RedactExampleContext(context.Background(), raw); got == UnknownExample {
		t.Errorf("live context must redact normally, got %q", got)
	}
}
