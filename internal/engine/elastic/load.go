package elastic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/collector/pdata/plog/plogotlp"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/pdata/pmetric/pmetricotlp"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/pdata/ptrace/ptraceotlp"

	"github.com/ericmustin/vern/internal/engine"
	"github.com/ericmustin/vern/internal/otlpin"
)

// ErrTooManyRequests is returned when Elasticsearch answers 429. The run is
// aborted without retry.
var ErrTooManyRequests = errors.New("elasticsearch rejected the sample (429 Too Many Requests); run aborted")

// Load sanitizes s (it mutates the sample), posts it to the native OTLP
// endpoints as protobuf in batches no larger than BatchBytes, then reconciles
// what Elasticsearch indexed against what was sent. Nothing in the sample is
// retried: a 429 aborts the run.
func (e *Engine) Load(ctx context.Context, id engine.RunID, s *otlpin.Sample) (*engine.LoadStats, error) {
	Sanitize(s, id)
	sum := otlpin.Summarize(s)
	st := &engine.LoadStats{Sent: map[string]int{}, Indexed: map[string]int{}, Failed: map[string]int{}}
	if sum.Spans > 0 {
		st.Sent["traces"] = sum.Spans
	}
	if n := sum.LogRecords + sum.SpanEvents; n > 0 {
		st.Sent["logs"] = n
	}
	if sum.DataPoints > 0 {
		st.Sent["metrics"] = sum.DataPoints
	}
	limit := e.cfg.BatchBytes

	if sum.Spans > 0 {
		var m ptrace.ProtoMarshaler
		p := &packer[ptrace.Traces]{
			limit: limit, size: m.TracesSize, newT: ptrace.NewTraces,
			merge: func(d, s ptrace.Traces) { s.ResourceSpans().MoveAndAppendTo(d.ResourceSpans()) },
			flush: func(b ptrace.Traces) error {
				raw, err := m.MarshalTraces(b)
				if err != nil {
					return fmt.Errorf("encode traces: %w", err)
				}
				body, err := e.post(ctx, "traces", raw)
				if err != nil || len(body) == 0 {
					return err
				}
				resp := ptraceotlp.NewExportResponse()
				if err := resp.UnmarshalProto(body); err != nil {
					return fmt.Errorf("decode traces response: %w", err)
				}
				st.Failed["traces"] += int(resp.PartialSuccess().RejectedSpans())
				return nil
			},
		}
		if err := feedTraces(s.Traces, limit, p.add); err != nil {
			return nil, err
		}
		if err := p.close(); err != nil {
			return nil, err
		}
	}
	if sum.LogRecords > 0 {
		var m plog.ProtoMarshaler
		p := &packer[plog.Logs]{
			limit: limit, size: m.LogsSize, newT: plog.NewLogs,
			merge: func(d, s plog.Logs) { s.ResourceLogs().MoveAndAppendTo(d.ResourceLogs()) },
			flush: func(b plog.Logs) error {
				raw, err := m.MarshalLogs(b)
				if err != nil {
					return fmt.Errorf("encode logs: %w", err)
				}
				body, err := e.post(ctx, "logs", raw)
				if err != nil || len(body) == 0 {
					return err
				}
				resp := plogotlp.NewExportResponse()
				if err := resp.UnmarshalProto(body); err != nil {
					return fmt.Errorf("decode logs response: %w", err)
				}
				st.Failed["logs"] += int(resp.PartialSuccess().RejectedLogRecords())
				return nil
			},
		}
		if err := feedLogs(s.Logs, limit, p.add); err != nil {
			return nil, err
		}
		if err := p.close(); err != nil {
			return nil, err
		}
	}
	if sum.DataPoints > 0 {
		var m pmetric.ProtoMarshaler
		p := &packer[pmetric.Metrics]{
			limit: limit, size: m.MetricsSize, newT: pmetric.NewMetrics,
			merge: func(d, s pmetric.Metrics) { s.ResourceMetrics().MoveAndAppendTo(d.ResourceMetrics()) },
			flush: func(b pmetric.Metrics) error {
				raw, err := m.MarshalMetrics(b)
				if err != nil {
					return fmt.Errorf("encode metrics: %w", err)
				}
				body, err := e.post(ctx, "metrics", raw)
				if err != nil || len(body) == 0 {
					return err
				}
				resp := pmetricotlp.NewExportResponse()
				if err := resp.UnmarshalProto(body); err != nil {
					return fmt.Errorf("decode metrics response: %w", err)
				}
				st.Failed["metrics"] += int(resp.PartialSuccess().RejectedDataPoints())
				return nil
			},
		}
		if err := feedMetrics(s.Metrics, limit, p.add); err != nil {
			return nil, err
		}
		if err := p.close(); err != nil {
			return nil, err
		}
	}

	if err := e.reconcile(ctx, id, st); err != nil {
		return nil, err
	}
	return st, nil
}

// post sends one protobuf body to /_otlp/v1/<signal> and returns the 2xx
// response body.
func (e *Engine) post(ctx context.Context, signal string, raw []byte) ([]byte, error) {
	if len(raw) > maxBody {
		return nil, fmt.Errorf("a single %s record encodes to %d bytes, over the %d byte Elasticsearch request limit", signal, len(raw), maxBody)
	}
	status, body, err := e.do(ctx, http.MethodPost, "_otlp/v1/"+signal, "application/x-protobuf", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("send %s: %w", signal, err)
	}
	switch {
	case status == http.StatusTooManyRequests:
		return nil, ErrTooManyRequests
	case status < 200 || status > 299:
		hint := ""
		if status == http.StatusRequestEntityTooLarge {
			hint = " (batching should have prevented this)"
		}
		return nil, fmt.Errorf("elasticsearch rejected %s batch: status %d%s: %s", signal, status, hint, printable(body, 500))
	}
	return body, nil
}

// printable renders up to n bytes of body safely: error bodies from the OTLP
// endpoint can be binary protobuf.
func printable(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return strconv.Quote(string(b))
}

// reconcile refreshes the run's streams, then fills st.Indexed and adds the
// failure-store counts to st.Failed.
func (e *Engine) reconcile(ctx context.Context, id engine.RunID, st *engine.LoadStats) error {
	_, pats := e.Session(id)
	patterns := map[string]string{"traces": pats.Traces, "logs": pats.Logs, "metrics": pats.Metrics}
	var all []string
	for _, sig := range []string{"traces", "logs", "metrics"} {
		if st.Sent[sig] > 0 {
			all = append(all, patterns[sig], patterns[sig]+"::failures")
		}
	}
	if len(all) == 0 {
		return nil
	}
	// A data stream refresh does not refresh its failure store on
	// Elasticsearch 9.5.5: refresh the ::failures selector explicitly, or
	// freshly rejected documents are invisible to the count.
	q := url.Values{"allow_no_indices": {"true"}, "ignore_unavailable": {"true"}, "expand_wildcards": {"all"}}
	status, body, err := e.do(ctx, http.MethodPost, strings.Join(all, ",")+"/_refresh?"+q.Encode(), "", nil)
	if err != nil {
		return fmt.Errorf("refresh: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("refresh: status %d: %s", status, printable(body, 500))
	}
	for _, sig := range []string{"traces", "logs", "metrics"} {
		if st.Sent[sig] == 0 {
			continue
		}
		if sig == "metrics" {
			// Grouped into documents: not comparable to data point counts.
			st.Indexed[sig] = -1
		} else {
			n, err := e.count(ctx, patterns[sig])
			if err != nil {
				return fmt.Errorf("count %s: %w", sig, err)
			}
			st.Indexed[sig] = n
		}
		n, err := e.count(ctx, patterns[sig]+"::failures")
		if err != nil {
			return fmt.Errorf("count %s failures: %w", sig, err)
		}
		st.Failed[sig] += n
	}
	return nil
}

// count runs FROM <pattern> | STATS c = COUNT(*). A wildcard that matches
// nothing yields 0.
func (e *Engine) count(ctx context.Context, pattern string) (int, error) {
	payload, _ := json.Marshal(map[string]string{"query": "FROM " + pattern + " | STATS c = COUNT(*)"})
	status, body, err := e.do(ctx, http.MethodPost, "_query?format=json", "application/json", bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("es|ql status %d: %s", status, printable(body, 500))
	}
	var res struct {
		Values [][]float64 `json:"values"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return 0, fmt.Errorf("decode es|ql response: %w", err)
	}
	if len(res.Values) != 1 || len(res.Values[0]) != 1 {
		return 0, fmt.Errorf("unexpected es|ql count shape: %s", printable(body, 200))
	}
	return int(res.Values[0][0]), nil
}
