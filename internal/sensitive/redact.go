package sensitive

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

const (
	// FieldSep separates a field path from its value inside one segment.
	FieldSep = "\x1f" // U+001F
	// SegmentSep separates segments. ES|QL has no \u escapes, so the query
	// embeds these raw characters; they survive literals, CONCAT and _query.
	SegmentSep = "\x1e" // U+001E

	// KeyDetectorID is the detector ID Locate reports for a hit on the
	// sensitive key list rather than on a value pattern.
	KeyDetectorID = "sensitive_key"
	keyLabel      = "sensitive key name"

	// UnknownExample is the redacted example when the evidence cannot be
	// located (or the search was cut short): it carries nothing of the value.
	UnknownExample = "<field unknown>: <detector unknown> (redacted)"
)

// fieldPath is what a field path looks like; anything else (whitespace, text
// from a value that held a raw separator) is not printed.
var fieldPath = regexp.MustCompile(`^[A-Za-z0-9_.@:/\-\[\]]+$`)

// Redact returns the masked preview of value: at most the first 2 and last 2
// characters plus the length, e.g. "al…om (17 chars)". Values of 8 characters
// or fewer are fully masked ("••• (7 chars)").
func Redact(value string) string {
	r := []rune(value)
	n := strconv.Itoa(len(r)) + " chars"
	if len(r) <= 8 {
		return "••• (" + n + ")"
	}
	return string(r[:2]) + "…" + string(r[len(r)-2:]) + " (" + n + ")"
}

// FormatExample builds one segment of the example string: the field path,
// FieldSep, then the value.
func FormatExample(field, value string) string { return field + FieldSep + value }

// Locate finds the first sensitive segment in raw, the concatenation of
// "<field>FieldSep<value>" segments joined by SegmentSep that the rule query
// returns for one document. A non-empty value under a sensitive key wins
// first; otherwise the first segment matching a detector, in Detectors order.
// The returned value is unmasked: callers must pass it only to Redact.
func Locate(raw string) (field, detectorID, value string, ok bool) {
	return locate(context.Background(), raw)
}

// locate is Locate that gives up (ok false) once ctx is done; it checks ctx
// before each detector pass over the segments.
func locate(ctx context.Context, raw string) (field, detectorID, value string, ok bool) {
	type seg struct{ field, value string }
	var segs []seg
	for _, s := range strings.Split(raw, SegmentSep) {
		f, v, found := strings.Cut(s, FieldSep)
		if found {
			segs = append(segs, seg{f, v})
		}
	}
	for _, s := range segs {
		if !isPlaceholder(s.value) && SensitiveKey(keyName(s.field)) {
			return s.field, KeyDetectorID, s.value, true
		}
	}
	for _, d := range catalog {
		if ctx.Err() != nil {
			return "", "", "", false
		}
		for _, s := range segs {
			if d.Match(s.value) {
				return s.field, d.ID, s.value, true
			}
		}
	}
	return "", "", "", false
}

// RedactExample turns the raw example from a failing rule query into
// "<field>: <label> (<masked preview>)", or a fixed placeholder when nothing
// can be located. Nothing beyond the masked preview of the value survives. A
// field path that itself looks sensitive (an attribute key holding an email)
// is masked too.
func RedactExample(raw string) string { return RedactExampleContext(context.Background(), raw) }

// RedactExampleContext is RedactExample that returns UnknownExample once ctx
// is done, so a cancelled run stops scanning. The scan is linear in len(raw)
// (about 0.5 s per MiB that matches nothing), so callers bound the total.
func RedactExampleContext(ctx context.Context, raw string) string {
	field, id, value, ok := locate(ctx, raw)
	if !ok || ctx.Err() != nil {
		return UnknownExample
	}
	label := keyLabel
	for _, d := range catalog {
		if d.ID == id {
			label = d.Label
			break
		}
	}
	if !fieldPath.MatchString(field) || len(field) > 256 {
		field = "<field masked>"
	} else {
		for _, d := range catalog {
			if d.Match(field) {
				field = Redact(field)
				break
			}
		}
	}
	return field + ": " + label + " (" + Redact(value) + ")"
}
