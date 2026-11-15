package sensitive

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Placeholders are values that say a secret was withheld rather than
// carrying one: empty, masks, redaction markers,
// environment references, bare words such as null or hidden, Pydantic's
// SecretStr('**********') and first-6/last-4 masked card numbers. A
// placeholder under a sensitive key, or as the value of a key-value pair,
// is not sensitive data.

// placeholderWords are bare words that mean "no value here", in any case.
var placeholderWords = []string{"true", "false", "yes", "no", "null", "none", "nil",
	"redacted", "hidden", "masked"}

// maskRunes are the mask characters: runs of them are placeholders.
const maskRunes = "xX*•#"

// maskClass is one mask character, as a regex class.
const maskClass = "[*\u2022xX\\#]"

// placeholderLucene matches a whole (trimmed) placeholder value. Like the
// detectors, it is written for a raw ES|QL string literal and is the source
// of the Go matcher, so the key-name check in ES|QL and Locate agree.
var placeholderLucene = "(" + strings.Join([]string{
	maskClass + "+",
	`\[` + ci("filtered") + `\]`,
	`\<` + ci("redacted") + `\>`,
	`\[` + ci("redacted") + `\]`,
	`\$\{.*\}`,
	`\$[A-Za-z_][A-Za-z0-9_]*`,
	cis(placeholderWords...),
	`\(` + ci("hidden") + `\)`,
	ci("secretstr") + `\('` + maskClass + `+'\)`,
	`[0-9]+` + maskClass + `{4,}[0-9]+`,
}, "|") + ")?"

var placeholderRE = regexp.MustCompile(`(?s)^(?:` + placeholderLucene + `)$`)

// isPlaceholder reports whether value, trimmed, is a placeholder.
func isPlaceholder(value string) bool {
	return placeholderRE.MatchString(strings.TrimSpace(value))
}

// Key-value values are matched one token at a time: the value's characters
// up to the next delimiter. kvToken accepts a token that is not a
// placeholder, followed by anything. It is a plain regular expression (no
// Lucene complement), built from a small automaton whose only cycles are
// self-loops, so Lucene and Go read the same string the same way.

// tokenDelims end a token (with whitespace and control characters, which
// are outside the token class anyway).
const tokenDelims = "\"',;&)]} "

// firstExcluded cannot start a value: mask and template starts
// (*, U+2022, [FILTERED], <redacted>, ${ENV}, %s, {x}).
const firstExcluded = "*[<>$%{•"

type tnode struct {
	edges map[rune]*tnode // explicit transitions; may include delimiters
	def   *tnode          // every other token character; nil rejects
	end   bool            // the token may end here (delimiter or end of string)
	first bool            // the start state: def covers first characters only
	free  bool            // the token is not a placeholder: anything follows
	dead  bool            // the token is a placeholder: reject
}

var (
	tFree = &tnode{free: true}
	tDead = &tnode{dead: true}
)

func newNode(end bool) *tnode { return &tnode{edges: map[rune]*tnode{}, def: tFree, end: end} }

// isTokenRune reports whether r can appear in a token.
func isTokenRune(r rune) bool {
	if r >= 0x21 && r <= 0x7e {
		return !strings.ContainsRune(tokenDelims, r)
	}
	return r >= 0xa1
}

func buildKVToken() *tnode {
	start := newNode(false)
	start.first = true
	set := func(n *tnode, runes string, to *tnode) {
		for _, r := range runes {
			n.edges[r] = to
		}
	}
	const digits = "0123456789"

	// Runs of mask characters.
	mask := newNode(false)
	set(mask, maskRunes, mask)
	set(start, "xX#", mask) // * and U+2022 cannot start a value at all

	// Digits, 4+ mask characters, digits: a first-6/last-4 masked card.
	dig, dmd := newNode(true), newNode(false)
	set(start, digits, dig)
	set(dig, digits, dig)
	prev := dig
	for i := 0; i < 4; i++ {
		m := newNode(true)
		set(prev, maskRunes, m)
		prev = m
	}
	set(prev, maskRunes, prev)
	set(prev, digits, dmd)
	set(dmd, digits, dmd)

	// Placeholder words, case-insensitive, as a trie. "(hidden" is the token
	// of "(hidden)", whose ")" is a delimiter.
	word := func(w string) *tnode {
		n := start
		for _, r := range w {
			lo, up := unicode.ToLower(r), unicode.ToUpper(r)
			next := n.edges[lo]
			switch {
			case next == mask || next == dig:
				panic("sensitive: placeholder word collides with mask or digits: " + w)
			case next == nil:
				next = newNode(true)
			}
			n.edges[lo], n.edges[up] = next, next
			n = next
		}
		return n
	}
	for _, w := range append(append([]string(nil), placeholderWords...), "(hidden") {
		word(w).end = false
	}

	// SecretStr('<mask>') is a placeholder; SecretStr('<anything else>') is not.
	s := word("secretstr(")
	q1, q2 := newNode(true), newNode(true)
	s.edges['\''] = q1
	set(q1, maskRunes, q2)
	set(q2, maskRunes, q2)
	q2.edges['\''] = tDead
	return start
}

// kvToken is the regular expression for a non-placeholder token and the
// rest of the string. It has the shape (W.*|E): W is a token prefix that
// already proves the token is not a placeholder (it leaves the placeholder
// automaton, or the token ends at a delimiter in a non-placeholder state),
// and E a whole non-placeholder token that ends the string. Sharing one
// trailing .* keeps Lucene's determinization small: a .* after every branch
// makes it give up ("too complex to determinize") once a key precedes it.
var kvToken = func() string {
	start := buildKVToken()
	type edge struct {
		key   rune
		class string
		to    *tnode
	}
	// split returns the self-loop class (or "") and the other explicit edges.
	split := func(n *tnode) (string, []edge, map[rune]bool) {
		byTarget := map[*tnode][]rune{}
		explicit := map[rune]bool{}
		for r, to := range n.edges {
			explicit[r] = true
			if !to.dead {
				byTarget[to] = append(byTarget[to], r)
			}
		}
		var loop string
		var out []edge
		for to, rs := range byTarget {
			sort.Slice(rs, func(i, j int) bool { return rs[i] < rs[j] })
			if to == n {
				loop = "(" + runeClass(rs) + ")*"
				continue
			}
			out = append(out, edge{rs[0], runeClass(rs), to})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
		return loop, out, explicit
	}
	alt := func(loop string, parts []string) string {
		switch len(parts) {
		case 0:
			return ""
		case 1:
			return loop + parts[0]
		}
		return loop + "(" + strings.Join(parts, "|") + ")"
	}
	wmemo, ememo := map[*tnode]string{}, map[*tnode]string{}
	var witness, atEnd func(n *tnode) string
	witness = func(n *tnode) string {
		if s, ok := wmemo[n]; ok {
			return s
		}
		loop, edges, explicit := split(n)
		var parts []string
		for _, e := range edges {
			if w := witness(e.to); w != "" {
				parts = append(parts, e.class+w)
			}
		}
		if n.def != nil && !n.def.dead {
			if !n.def.free {
				panic("sensitive: kv token default edge must lead to the free state")
			}
			parts = append(parts, tokenClass(explicit, n.first, false))
		}
		if n.end {
			parts = append(parts, tokenClass(explicit, false, true))
		}
		wmemo[n] = alt(loop, parts)
		return wmemo[n]
	}
	atEnd = func(n *tnode) string {
		if s, ok := ememo[n]; ok {
			return s
		}
		loop, edges, _ := split(n)
		var parts []string
		for _, e := range edges {
			if x := atEnd(e.to); x != "" {
				parts = append(parts, e.class+x)
			}
		}
		if n.end {
			parts = append(parts, "()")
		}
		ememo[n] = alt(loop, parts)
		return ememo[n]
	}
	return "(" + witness(start) + ".*|" + atEnd(start) + ")"
}()

// escapeRune writes r for a regex: ASCII letters and digits as is, other
// ASCII escaped, non-ASCII raw.
func escapeRune(b *strings.Builder, r rune) {
	if r < 0x80 && !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
		b.WriteByte('\\')
	}
	b.WriteRune(r)
}

// runeClass renders a set of runes as one character or a class.
func runeClass(rs []rune) string {
	var b strings.Builder
	if len(rs) == 1 {
		escapeRune(&b, rs[0])
		return b.String()
	}
	b.WriteByte('[')
	for _, r := range rs {
		escapeRune(&b, r)
	}
	b.WriteByte(']')
	return b.String()
}

// tokenClass renders the token characters minus exclude (and minus
// firstExcluded when first). With negate it renders the complement: every
// character that is neither a token character nor in exclude. Only letter
// and digit runs become ranges; punctuation is listed one by one.
func tokenClass(exclude map[rune]bool, first, negate bool) string {
	in := func(r rune) bool {
		if !isTokenRune(r) {
			return false
		}
		if first && strings.ContainsRune(firstExcluded, r) {
			return false
		}
		return true
	}
	var b strings.Builder
	b.WriteByte('[')
	if negate {
		b.WriteByte('^')
		var extra []rune
		for r := range exclude {
			if !in(r) {
				extra = append(extra, r)
			}
		}
		sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
		for _, r := range extra {
			escapeRune(&b, r)
		}
		exclude = nil // the complement lists every token character
	}
	keep := func(r rune) bool { return in(r) && !exclude[r] }
	alnum := func(r rune) bool { return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }
	for r := rune(0x21); r <= 0x7e; r++ {
		if !keep(r) {
			continue
		}
		if !alnum(r) {
			escapeRune(&b, r)
			continue
		}
		end := r
		for end+1 <= 0x7e && alnum(end+1) && keep(end+1) && sameKind(r, end+1) {
			end++
		}
		b.WriteRune(r)
		if end > r {
			if end > r+1 {
				b.WriteByte('-')
			}
			b.WriteRune(end)
		}
		r = end
	}
	// Non-ASCII: U+00A1..U+10FFFF minus the excluded runes.
	var holes []rune
	for _, r := range []rune(firstExcluded + maskRunes) {
		if r >= 0xa1 && !keep(r) {
			holes = append(holes, r)
		}
	}
	for r := range exclude {
		if r >= 0xa1 {
			holes = append(holes, r)
		}
	}
	sort.Slice(holes, func(i, j int) bool { return holes[i] < holes[j] })
	lo := rune(0xa1)
	for _, h := range holes {
		if h < lo {
			continue
		}
		if h > lo {
			b.WriteRune(lo)
			if h-1 > lo {
				b.WriteByte('-')
				b.WriteRune(h - 1)
			}
		}
		lo = h + 1
	}
	b.WriteRune(lo)
	b.WriteByte('-')
	b.WriteRune(0x10ffff)
	b.WriteByte(']')
	return b.String()
}

// sameKind keeps ranges within digits, lower case or upper case letters.
func sameKind(a, b rune) bool {
	kind := func(r rune) int {
		switch {
		case r >= '0' && r <= '9':
			return 0
		case r >= 'a' && r <= 'z':
			return 1
		}
		return 2
	}
	return kind(a) == kind(b)
}
