# Demo sample

`demo.jsonl.gz` is a vendor-neutral OpenTelemetry sample: OTLP JSON lines
(`resourceSpans`, `resourceMetrics`, `resourceLogs`) written by the upstream
collector's `file` exporter. The collector injects and modifies nothing, so the
data is what the SDKs emitted.

## How it was made

```bash
make capture                      # CAPTURE_SECONDS=90 by default
gzip -9 -c demo/samples/capture.jsonl > web/examples/demo.jsonl.gz
```

`make capture` runs `demo/capture/compose.yaml` (collector, `demo/trafficgen`,
`demo/metricsgen`; no host ports) and writes `demo/samples/capture.jsonl`
(gitignored). The Python SDK resource attributes are the generators' own fake
values (`vern-demo-host`, `checkout-pod`, ...); nothing from the capturing
machine is included.

Contents: services `vern-demo-checkout` (traces) and `vern-demo-metrics`
(metrics and logs), 782 spans, 172 metric data points across 6 metric names
(7 metric streams: `vern.demo.latency` appears with two units), 225 log
records, spanning about 90 seconds.

## Deliberately imperfect

`metricsgen` is built to fail some rules. Scored on Elasticsearch 9.5.5, the
sample gets 82.9 (Good, partial: 21 of 26 rules scored). `vern-demo-metrics`
scores 69.6 and fails these rules:

- MET-002: `vern.demo.queue.depth` is a gauge with no unit.
- MET-003: `vern.demo.latency` is recorded with two units, `ms` and `s`.
- MET-005: `vern.demo.sent.bytes` has its unit in the metric name.
- MET-006: `server.port` is a metric named after a semconv attribute key.
- RES-004: `host.name` is set as a log attribute rather than a resource
  attribute.

`vern-demo-checkout` scores 100. Its noisy flow (`GET /cart` with four tiny
`cart validation step N` internal spans) is not flagged: SPA-005 passes because
no trace has more than 20 spans under 5ms.
