package otlpin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// jsonStream decodes JSONL (one OTLP object per line) or a single, possibly
// pretty-printed, OTLP document.
func (d *decoder) jsonStream(name string, br *bufio.Reader) error {
	if head, _ := br.Peek(3); bytes.Equal(head, []byte{0xEF, 0xBB, 0xBF}) {
		br.Discard(3)
	}
	first := true // no non-blank line seen yet
	for line := 1; ; line++ {
		buf, rerr := br.ReadBytes('\n')
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return fmt.Errorf("%s:%d: %w", name, line, rerr)
		}
		atEOF := rerr != nil
		trimmed := bytes.TrimSpace(buf)
		if len(trimmed) > 0 {
			isFirst := first
			first = false
			err := d.object(trimmed)
			if err != nil {
				var se *json.SyntaxError
				if !errors.As(err, &se) {
					return fmt.Errorf("%s:%d: %w", name, line, err)
				}
				// A syntax error on the first line may be one or more
				// pretty-printed documents (e.g. Python's console exporter).
				if isFirst && !atEOF {
					rest, rerr := io.ReadAll(br)
					if rerr != nil {
						return fmt.Errorf("%s: %w", name, rerr)
					}
					return d.valueStream(name, line, err, append(buf, rest...))
				}
				if atEOF {
					d.s.Notes = append(d.s.Notes, fmt.Sprintf("%s: skipped truncated last line (line %d)", name, line))
					return nil
				}
				return fmt.Errorf("%s:%d: %w", name, line, err)
			}
		}
		if atEOF {
			return nil
		}
	}
}

// valueStream decodes back-to-back JSON values (pretty-printed or not). If the
// very first value is not valid JSON, firstErr is reported as a line error.
func (d *decoder) valueStream(name string, line int, firstErr error, data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	for n := 0; ; n++ {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if n == 0 {
				return fmt.Errorf("%s:%d: %w", name, line, firstErr)
			}
			return fmt.Errorf("%s: value %d: %w", name, n+1, err)
		}
		if err := d.object(raw); err != nil {
			return fmt.Errorf("%s: value %d: %w", name, n+1, err)
		}
	}
}

// object classifies one JSON object by its top-level keys and merges it.
func (d *decoder) object(b []byte) error {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(b, &keys); err != nil {
		return err
	}
	has := func(k string) bool { _, ok := keys[k]; return ok }
	switch {
	case has("resourceSpans"):
		return d.traces(b)
	case has("resourceLogs"):
		return d.logs(b)
	case has("resourceMetrics"):
		return d.metrics(b)
	case has("resource") && has("scopeSpans"):
		return d.traces(wrap("resourceSpans", b))
	case has("resource") && has("scopeLogs"):
		return d.logs(wrap("resourceLogs", b))
	case has("resource") && has("scopeMetrics"):
		return d.metrics(wrap("resourceMetrics", b))
	case has("context") && contextHasTraceID(keys["context"]):
		return ErrConsoleExporter
	}
	return ErrUnrecognized
}

func contextHasTraceID(raw json.RawMessage) bool {
	var c map[string]json.RawMessage
	if json.Unmarshal(raw, &c) != nil {
		return false
	}
	_, ok := c["trace_id"]
	return ok
}

func wrap(key string, obj []byte) []byte {
	out := make([]byte, 0, len(obj)+len(key)+8)
	out = append(out, `{"`...)
	out = append(out, key...)
	out = append(out, `":[`...)
	out = append(out, obj...)
	return append(out, `]}`...)
}

func (d *decoder) add(n int) error {
	d.items += n
	if d.items > d.lim.MaxItems {
		return fmt.Errorf("more than %d spans, events, log records and data points: %w", d.lim.MaxItems, ErrTooManyItems)
	}
	return nil
}

func (d *decoder) traces(b []byte) error {
	t, err := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(b)
	if err != nil {
		return err
	}
	n := t.SpanCount()
	rs := t.ResourceSpans()
	for i := 0; i < rs.Len(); i++ {
		for j := 0; j < rs.At(i).ScopeSpans().Len(); j++ {
			sp := rs.At(i).ScopeSpans().At(j).Spans()
			for k := 0; k < sp.Len(); k++ {
				n += sp.At(k).Events().Len()
			}
		}
	}
	if err := d.add(n); err != nil {
		return err
	}
	d.objects++
	rs.MoveAndAppendTo(d.s.Traces.ResourceSpans())
	return nil
}

func (d *decoder) logs(b []byte) error {
	l, err := (&plog.JSONUnmarshaler{}).UnmarshalLogs(b)
	if err != nil {
		return err
	}
	if err := d.add(l.LogRecordCount()); err != nil {
		return err
	}
	d.objects++
	l.ResourceLogs().MoveAndAppendTo(d.s.Logs.ResourceLogs())
	return nil
}

func (d *decoder) metrics(b []byte) error {
	m, err := (&pmetric.JSONUnmarshaler{}).UnmarshalMetrics(b)
	if err != nil {
		return err
	}
	if err := d.add(m.DataPointCount()); err != nil {
		return err
	}
	d.objects++
	m.ResourceMetrics().MoveAndAppendTo(d.s.Metrics.ResourceMetrics())
	return nil
}
