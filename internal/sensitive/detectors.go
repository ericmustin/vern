// Package sensitive holds the sensitive-data detector catalog shared by the
// RES-010, SPA-006, LOG-003 and MET-008 rules.
//
// Elasticsearch decides which documents match, with each detector's Lucene
// pattern as an ES|QL RLIKE. Go only locates the evidence in the example the
// query returns and redacts it; a matched value never leaves this package
// unmasked.
package sensitive

import (
	"regexp"
	"strings"
)

// Detector is one sensitive-data detector.
type Detector struct {
	ID    string // stable identifier, e.g. "email"
	Label string // human text, e.g. "email address"
	// Lucene is the whole-string pattern for ES|QL RLIKE. Rules for it:
	// no (?i) (use [aA] classes), RLIKE operator characters (@ ~ & # < ")
	// escaped, and no `.*X.*` wrapping where counted repeats overlap (use
	// the boundary form `(.*[^c])?X([^c].*)?`). It is written for a raw
	// ES|QL string literal ("""..."""); inside a plain "..." literal every
	// backslash must be doubled.
	Lucene string
	// Go accepts exactly the strings Lucene accepts (whole-string match,
	// dot matches newlines). For the email detector, whose Lucene uses
	// complement/intersection that RE2 cannot express, Go is a superset and
	// match applies the exclusions; always call Match, not Go directly.
	Go    *regexp.Regexp
	match func(string) bool
}

// Match reports whether s is accepted by the detector (same verdict as the
// Lucene pattern as RLIKE).
func (d Detector) Match(s string) bool {
	if d.match != nil {
		return d.match(s)
	}
	return d.Go.MatchString(s)
}

// ci returns word with every letter replaced by a [xX] class.
func ci(word string) string {
	var b strings.Builder
	for _, r := range word {
		l, u := strings.ToLower(string(r)), strings.ToUpper(string(r))
		if l == u {
			b.WriteRune(r)
			continue
		}
		b.WriteString("[" + l + u + "]")
	}
	return b.String()
}

// cis maps ci over words and joins them into one alternation.
func cis(words ...string) string {
	out := make([]string, len(words))
	for i, w := range words {
		out[i] = ci(w)
	}
	return "(" + strings.Join(out, "|") + ")"
}

// bounded wraps x so it matches anywhere in a string but only between
// characters outside the class (the start and end of the string count as
// boundaries). class is the inside of a [^...] negated class.
func bounded(class, x string) string {
	return "(.*[^" + class + "])?" + x + "([^" + class + "].*)?"
}

// cardBounded is bounded() for card numbers with asymmetric boundaries, so
// digit runs inside UUIDs (-), identifiers (_), versions and decimals (. and
// ,) do not match: the left neighbour is not alphanumeric or . - _ , (a ','
// counts only when it is not preceded by a digit, so comma decimals such as
// 0,4111111111111111 do not match but a list a,4111111111111111 does); the
// right neighbour is not alphanumeric or - _, and a '.' is allowed only at
// the end or before a non-digit.
func cardBounded(x string) string {
	const c = `a-zA-Z0-9.\-_`
	left := "((.*[^" + c + ",])|(.*[^0-9])?,)?"
	return left + x + `(\.([^0-9].*)?|[^` + c + `].*)?`
}

// sepOpt is the optional separator between 4-digit card groups.
const sepOpt = `[ \-]?`

// kvWith builds a key-value pattern around a key expression: the key, then
// `=` (or `=>`, or a quoted `:` as in JSON), then a value token that is not
// a placeholder (kvToken: masks, null, none, hidden, SecretStr('***'),
// masked card numbers and the like do not match).
func kvWith(key string) string {
	eq := `=\>?[ ]*[\"']?`
	colon := `:[ ]*[\"']`
	return "(" + key + `)[\"']?[ ]*(` + eq + "|" + colon + ")" + kvToken
}

// kv builds a key-value pattern whose key ENDS in one of the words, so
// token_count=5 and max_tokens=100 do not match.
func kv(words ...string) string { return kvWith(".*" + cis(words...)) }

// secretKV is kv for credentials. Bare "token" needs a left boundary and a
// compound (auth_token, accessToken) a non-letter before it, so pagination
// cursors (next_token, pageToken, continuation-token) are not flagged.
func secretKV() string {
	words := ".*" + cis("password", "passwd", "secret", "api_key", "apikey", "api-key",
		"access_key", "accesskey", "private_key", "privatekey")
	compound := cis("auth", "access", "refresh", "api", "csrf", "xsrf", "session", "bearer",
		"id", "jwt", "oauth", "device", "user", "bot", "github", "gitlab", "slack", "npm",
		"secret", "private", "personal", "client")
	tok := ci("token")
	return kvWith(words + `|(.*[^a-zA-Z])?` + compound + `[_\-]?` + tok +
		`|(.*[^a-zA-Z0-9_\-])?` + tok)
}

// emailTLDDeny are final labels that are file or unit-name suffixes, not mail
// domains (logo@2x.png, user@1000.service).
var emailTLDDeny = []string{"service", "socket", "mount", "automount", "timer", "target",
	"scope", "slice", "path", "device", "swap", "jpg", "jpeg", "gif", "svg", "webp", "ico",
	"css", "js", "json", "html", "htm", "txt", "md", "yaml", "yml", "xml"}

// emailLucene is anywhere-in-string email: local part, @, a domain whose
// final label is letters (not denied), and whose first label is not
// <digits>x (the @2x image suffix).
func emailLucene() string {
	deny := cis(emailTLDDeny...)
	g := `(([a-zA-Z0-9.\-]+\.([a-zA-Z]{2,}&~` + deny + `))&~([0-9]+[xX]\..*))`
	return `.*[a-zA-Z0-9._%+\-]\@` + g + `([^a-zA-Z0-9\-].*)?`
}

// emailPositive is emailLucene without the exclusions (a superset), for Go.
const emailPositive = `.*[a-zA-Z0-9._%+\-]\@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}([^a-zA-Z0-9\-].*)?`

var (
	emailDenySet = func() map[string]bool {
		m := map[string]bool{}
		for _, w := range emailTLDDeny {
			m[w] = true
		}
		return m
	}()
	emailDigitsX   = regexp.MustCompile(`^[0-9]+[xX]$`) // a whole first label
	emailAllLetter = regexp.MustCompile(`^[a-zA-Z]{2,}$`)
)

// matchEmail mirrors emailLucene: some '@' preceded by a local-part character
// is followed by a domain G (a prefix of the maximal domain run, ending at the
// run's end or before a '.') whose last label is letters, not denied, and
// whose first label is not <digits>x.
//
// It runs in linear time: every candidate G contains the run's first '.', so
// "G matches <digits>x\..*" is the same test for all of them (the run's first
// label is <digits>x) and is computed once per '@'; domain runs after
// different '@'s do not overlap.
func matchEmail(s string) bool {
	isDom := func(c byte) bool {
		return c == '.' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '@' || i == 0 || !(isDom(s[i-1]) || s[i-1] == '_' || s[i-1] == '%' || s[i-1] == '+') {
			continue
		}
		j := i + 1
		for j < len(s) && isDom(s[j]) {
			j++
		}
		run := s[i+1 : j]
		first := run
		if d := strings.IndexByte(run, '.'); d >= 0 {
			first = run[:d]
		}
		if emailDigitsX.MatchString(first) {
			continue // every candidate G starts with <digits>x.
		}
		for k := 1; k <= len(run); k++ {
			if k < len(run) && run[k] != '.' {
				continue
			}
			g := run[:k]
			dot := strings.LastIndex(g, ".")
			if dot < 1 {
				continue
			}
			tld := g[dot+1:]
			if emailAllLetter.MatchString(tld) && !emailDenySet[strings.ToLower(tld)] {
				return true
			}
		}
	}
	return false
}

var catalog = buildCatalog()

func buildCatalog() []Detector {
	mastercard := `(5[1-5][0-9]{2}|222[1-9]|22[3-9][0-9]|2[3-6][0-9]{2}|27[01][0-9]|2720)` +
		sepOpt + `[0-9]{4}` + sepOpt + `[0-9]{4}` + sepOpt + `[0-9]{4}`
	visa := `4[0-9]{3}` + sepOpt + `[0-9]{4}` + sepOpt + `[0-9]{4}` + sepOpt + `[0-9]([0-9]{3})?`
	amex := `3[47][0-9]{2}` + sepOpt + `[0-9]{6}` + sepOpt + `[0-9]{5}`
	discover := `(6011|65[0-9]{2})` + sepOpt + `[0-9]{4}` + sepOpt + `[0-9]{4}` + sepOpt + `[0-9]{4}`

	keyChars := `a-zA-Z0-9_\-`
	defs := []struct{ id, label, lucene string }{
		{"email", "email address",
			emailLucene()},
		// Strict brand prefix and length, no Luhn check. Alphanumeric
		// neighbours are not boundaries, so digit runs inside hex IDs and
		// hashes never match.
		{"credit_card", "credit card number",
			cardBounded("(" + visa + "|" + mastercard + "|" + amex + "|" + discover + ")")},
		{"ssn", "social security number",
			bounded(`a-zA-Z0-9\-`, `[0-9]{3}-[0-9]{2}-[0-9]{4}`)},
		{"bearer_token", "bearer token",
			bounded(`a-zA-Z`, ci("bearer")+` [a-zA-Z0-9._+/=\-]{16,}`)},
		{"jwt", "JSON web token",
			bounded(keyChars, `eyJ[a-zA-Z0-9_\-]{8,}\.eyJ[a-zA-Z0-9_\-]{8,}\.[a-zA-Z0-9_\-]+`)},
		{"api_key", "API key",
			bounded(keyChars, `(sk-(proj|svcacct|admin|ant-api[0-9]+)-[a-zA-Z0-9_\-]{20,}`+
				`|sk-[a-zA-Z0-9]{20,}`+
				`|gh[pousr]_[a-zA-Z0-9]{36,}`+
				`|xox[baprs]-[a-zA-Z0-9\-]{10,}`+
				`|AIza[a-zA-Z0-9_\-]{35}`+
				`|[sr]k_(live|test)_[a-zA-Z0-9]{16,})`)},
		{"aws_access_key", "AWS access key ID",
			bounded(`0-9A-Z`, `(AKIA|ASIA)[0-9A-Z]{16}`)},
		{"secret_kv", "password or secret in a key-value pair",
			secretKV()},
		{"health_kv", "health data in a key-value pair",
			kv("diagnosis", "patient_id", "patientid", "patient-id", "insurance_id",
				"insuranceid", "insurance-id", "medical_record", "medicalrecord")},
		{"financial_kv", "financial identifier in a key-value pair",
			kv("ssn", "iban", "account_number", "accountnumber", "account_no", "bank_account",
				"routing_number", "tax_id", "taxid", "card_number", "cardnumber",
				"credit_card", "creditcard", "cvv", "cvc")},
		{"private_key", "private key block",
			`.*-----BEGIN ([A-Z]+ )*PRIVATE KEY-----.*`},
	}
	out := make([]Detector, len(defs))
	for i, d := range defs {
		out[i] = Detector{
			ID: d.id, Label: d.label, Lucene: d.lucene,
			Go: regexp.MustCompile(`(?s)^(?:` + d.lucene + `)$`),
		}
		if d.id == "email" {
			out[i].Go = regexp.MustCompile(`(?s)^(?:` + emailPositive + `)$`)
			out[i].match = matchEmail
		}
	}
	return out
}

// Detectors returns the catalog in evaluation order. The slice is a copy.
func Detectors() []Detector { return append([]Detector(nil), catalog...) }

// LucenePatterns returns each detector's Lucene pattern, in Detectors order.
func LucenePatterns() []string {
	out := make([]string, len(catalog))
	for i, d := range catalog {
		out[i] = d.Lucene
	}
	return out
}

// keyNames are the sensitive key names. A non-empty, non-placeholder value
// under one of these matches whatever its format. Compound names are listed
// explicitly; there is no generic suffix matching (is_secret, next_page_token
// are not sensitive).
var keyNames = []string{
	"password", "passwd", "secret", "token", "api_key", "api-key", "apikey",
	"access_key", "private_key", "client_secret", "ssn", "credit_card",
	"card_number", "cvv", "authorization", "cookie", "set-cookie",
	"enduser.id", "user.id", "user.email", "patient_id", "insurance_id",
	"diagnosis",
	"db_password", "x-api-key", "access_token", "refresh_token", "id_token",
	"auth_token", "x-auth-token", "x-csrf-token", "x-xsrf-token", "session_token",
	"proxy-authorization", "secret_key", "secret_access_key", "aws_secret_access_key",
	"client-secret", "private-key",
}

// SensitiveKey reports whether the field names a sensitive key: the whole
// key, its last dotted segment, or a dotted suffix (enduser.id) is in the
// key list. Comparison ignores case.
func SensitiveKey(field string) bool {
	f := strings.ToLower(strings.TrimSpace(field))
	if f == "" {
		return false
	}
	last := f
	if i := strings.LastIndex(f, "."); i >= 0 {
		last = f[i+1:]
	}
	for _, k := range keyNames {
		if f == k || last == k || strings.HasSuffix(f, "."+k) {
			return true
		}
	}
	return false
}
