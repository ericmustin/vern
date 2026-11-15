# Metrics and logs generator for the demo, capture and cross-backend stacks
# (replaces otelgen v0.5.2 `metrics`, which refuses to run). Emits metrics and
# logs via the OTel Python SDK, with deliberate failing fixtures:
#   - MET-003: vern.demo.latency recorded with two units (ms and s)
#   - MET-006: a metric named after a semconv key (server.port)
#   - a unitless gauge (vern.demo.queue.depth)
#   - RES-004: host.name set as a log attribute instead of a resource attribute
# VERN_DEMO_DURATION (seconds, default 0 = run forever) bounds the run; the
# SDK providers are flushed and shut down before exit.
import os
import random
import time

from opentelemetry import metrics
from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter
from opentelemetry.metrics import Observation
from opentelemetry.sdk.metrics import Histogram, MeterProvider
from opentelemetry.sdk.metrics.export import AggregationTemporality
from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
from opentelemetry.sdk.resources import Resource

endpoint = os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318")
resource = Resource.create({
    "service.name": "vern-demo-metrics",
    "service.instance.id": "metrics-1",
    "deployment.environment.name": "production",
})
reader = PeriodicExportingMetricReader(OTLPMetricExporter(endpoint=f"{endpoint}/v1/metrics", preferred_temporality={Histogram: AggregationTemporality.DELTA}), export_interval_millis=5000)
metrics.set_meter_provider(MeterProvider(resource=resource, metric_readers=[reader]))
meter = metrics.get_meter("vern-demo-metricsgen")

requests = meter.create_counter("vern.demo.requests", unit="{request}")
duration = meter.create_histogram("http.server.request.duration", unit="s")
bytes_sent = meter.create_counter("vern.demo.sent.bytes", unit="By")
# Same metric name, two units (MET-003) and a metric named after a semconv key (MET-006).
latency_ms = meter.create_histogram("vern.demo.latency", unit="ms")
latency_s = metrics.get_meter("vern-demo-metricsgen-legacy").create_histogram("vern.demo.latency", unit="s")
port = meter.create_counter("server.port", unit="{port}")
meter.create_observable_gauge("vern.demo.queue.depth", callbacks=[lambda _o: [Observation(random.randint(0, 50))]])

import logging
from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor

log_provider = LoggerProvider(resource=resource)
log_provider.add_log_record_processor(BatchLogRecordProcessor(OTLPLogExporter(endpoint=f"{endpoint}/v1/logs")))
log = logging.getLogger("vern-demo-metricsgen")
log.setLevel(logging.DEBUG)
log.addHandler(LoggingHandler(level=logging.DEBUG, logger_provider=log_provider))

print(f"metricsgen exporting to {endpoint}", flush=True)
duration_s = float(os.environ.get("VERN_DEMO_DURATION", "0"))
deadline = time.time() + duration_s
while duration_s == 0 or time.time() < deadline:
    route = random.choice(["/checkout", "/cart"])
    requests.add(1, {"http.route": route})
    duration.record(random.uniform(0.005, 0.2), {"http.route": route, "http.request.method": "GET"})
    bytes_sent.add(random.randint(100, 5000))
    latency_ms.record(random.uniform(5, 200))
    latency_s.record(random.uniform(0.005, 0.2))
    port.add(1)
    log.info("request on %s", route)
    if random.random() < 0.3:
        log.debug("cache miss on %s", route)
    if random.random() < 0.05:
        # host.name is a resource-only semconv key; set here as a log attribute (RES-004).
        log.warning("slow request on %s", route, extra={"host.name": "vern-demo-host"})
    time.sleep(0.5)

# Bounded run: flush everything before exiting 0.
log_provider.force_flush()
log_provider.shutdown()
metrics.get_meter_provider().force_flush()
metrics.get_meter_provider().shutdown()
