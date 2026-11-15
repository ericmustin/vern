package otlpin

import (
	"strconv"
	"unicode/utf8"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

// LongValueChars is Elasticsearch's ignore_above for keyword fields: a
// longer string value is not indexed and is null to ES|QL.
const LongValueChars = 1024

// Field groups, named as mappings.Fields names them: where a value lands for
// the sensitive-data scan.
const (
	GroupResource  = "Resource"
	GroupScope     = "Scope"
	GroupSpan      = "Span"
	GroupSpanEvent = "SpanEvent"
	GroupLog       = "Log"
	GroupMetric    = "Metric"
)

// ValueLocation is where a value lands: its field group and the
// Elasticsearch field (e.g. {Span, "attributes.db.query.text"}).
type ValueLocation struct{ Group, Field string }

// valueStats collects the facts the sensitive-data blind spots need about
// the sample's values (see Summary).
type valueStats struct {
	long    map[ValueLocation]int
	longest map[string]int
}

// str notes a string value that lands in field of group as one value.
func (v *valueStats) str(group, field, s string) {
	n := chars(s)
	v.max(group, n)
	if n > LongValueChars {
		v.addLong(group, field)
	}
}

// attrs notes every attribute of m, under prefix + key.
func (v *valueStats) attrs(group, prefix string, m pcommon.Map) {
	m.Range(func(k string, val pcommon.Value) bool {
		v.value(group, prefix+k, val)
		return true
	})
}

// value notes an attribute or body value: its scanned length for the
// group's longest value, and each string over LongValueChars at field. Only
// top-level strings and array elements are keyword values; a map is one
// flattened field, which has no length limit.
func (v *valueStats) value(group, field string, val pcommon.Value) {
	v.max(group, valueChars(val))
	switch val.Type() {
	case pcommon.ValueTypeStr:
		if chars(val.Str()) > LongValueChars {
			v.addLong(group, field)
		}
	case pcommon.ValueTypeSlice:
		sl := val.Slice()
		for i := 0; i < sl.Len(); i++ {
			if e := sl.At(i); e.Type() == pcommon.ValueTypeStr && chars(e.Str()) > LongValueChars {
				v.addLong(group, field)
			}
		}
	}
}

func (v *valueStats) addLong(group, field string) {
	if v.long == nil {
		v.long = map[ValueLocation]int{}
	}
	v.long[ValueLocation{group, field}]++
}

func (v *valueStats) max(group string, n int) {
	if n == 0 || n <= v.longest[group] {
		return
	}
	if v.longest == nil {
		v.longest = map[string]int{}
	}
	v.longest[group] = n
}

// chars is the length of s in characters.
func chars(s string) int { return utf8.RuneCountInString(s) }

// valueChars approximates how many characters the scan reads for val: a
// string's length, array elements joined with "; " (two characters), a map
// as its JSON (keys, values and punctuation).
func valueChars(val pcommon.Value) int {
	var buf [32]byte
	switch val.Type() {
	case pcommon.ValueTypeStr:
		return chars(val.Str())
	case pcommon.ValueTypeInt:
		return len(strconv.AppendInt(buf[:0], val.Int(), 10))
	case pcommon.ValueTypeDouble:
		return len(strconv.AppendFloat(buf[:0], val.Double(), 'g', -1, 64))
	case pcommon.ValueTypeBool:
		return len(strconv.AppendBool(buf[:0], val.Bool()))
	case pcommon.ValueTypeBytes:
		return (val.Bytes().Len() + 2) / 3 * 4 // base64
	case pcommon.ValueTypeSlice:
		sl := val.Slice()
		n := 0
		for i := 0; i < sl.Len(); i++ {
			if i > 0 {
				n += 2
			}
			n += valueChars(sl.At(i))
		}
		return n
	case pcommon.ValueTypeMap:
		n := 2
		val.Map().Range(func(k string, e pcommon.Value) bool {
			n += chars(k) + valueChars(e) + 6 // quotes, colon, comma
			return true
		})
		return n
	}
	return 0
}
