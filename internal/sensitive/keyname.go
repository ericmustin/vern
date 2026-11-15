package sensitive

import "strings"

// MaxKeyNameFields bounds the per-field key-name checks in one rule query:
// sensitive-key fields beyond it are not key-checked and are
// disclosed as a blind spot.
const MaxKeyNameFields = 32

// keyName is the attribute key a field path names: the path without a
// ".text" suffix (the match_only_text subfield of a keyword, which holds the
// values the keyword drops for length).
func keyName(field string) string { return strings.TrimSuffix(field, ".text") }

// KeyNameFields returns the fields whose key name is a sensitive key, one
// per key name in order of first appearance: the first MaxKeyNameFields are
// checked by the rule query, the rest overflow. When both a
// keyword and its ".text" subfield are present, the ".text" field stands for
// the key: it holds every value, including those the keyword drops for
// length.
func KeyNameFields(fields []string) (checked, overflow []string) {
	var keys []string
	pick := map[string]string{}
	for _, f := range fields {
		k := keyName(f)
		if !SensitiveKey(k) {
			continue
		}
		prev, seen := pick[k]
		if !seen {
			keys = append(keys, k)
		}
		if !seen || (f != k && prev == k) {
			pick[k] = f
		}
	}
	for _, k := range keys {
		if len(checked) < MaxKeyNameFields {
			checked = append(checked, pick[k])
		} else {
			overflow = append(overflow, pick[k])
		}
	}
	return checked, overflow
}
