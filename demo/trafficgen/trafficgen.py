import os
import random
import time

from opentelemetry import trace
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.trace import SpanKind

random.seed(42)

endpoint = os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel-collector:4318")
rate = float(os.environ.get("VERN_DEMO_RATE", "2"))

resource = Resource.create(
    {
        "service.name": "vern-demo-checkout",
        "service.instance.id": "checkout-1",
        "service.version": "1.0.0",
        "deployment.environment.name": "production",
        "k8s.pod.uid": "checkout-pod-uid",
        "k8s.pod.name": "checkout-pod",
        "k8s.namespace.name": "vern-demo",
        "k8s.node.name": "vern-demo-node",
        "host.id": "vern-demo-host-id",
        "host.name": "vern-demo-host",
        "container.id": "vern-demo-container",
        "service.node.name": "checkout-node",
    }
)

provider = TracerProvider(resource=resource)
provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint=f"{endpoint}/v1/traces")))
trace.set_tracer_provider(provider)
tracer = trace.get_tracer("vern-demo-trafficgen")


def work(seconds: float) -> None:
    time.sleep(seconds)


def checkout_flow() -> None:
    with tracer.start_as_current_span("POST /checkout", kind=SpanKind.SERVER) as root:
        root.set_attribute("http.request.method", "POST")
        root.set_attribute("http.route", "/checkout")
        with tracer.start_as_current_span("validate cart", kind=SpanKind.INTERNAL):
            work(0.003)
        with tracer.start_as_current_span("reserve inventory", kind=SpanKind.INTERNAL):
            work(0.006)
        with tracer.start_as_current_span("POST payment", kind=SpanKind.CLIENT) as span:
            span.set_attribute("server.address", "payments.example")
            work(0.012)


def noisy_flow() -> None:
    with tracer.start_as_current_span("GET /cart", kind=SpanKind.SERVER) as root:
        root.set_attribute("http.request.method", "GET")
        root.set_attribute("http.route", "/cart")
        for i in range(4):
            with tracer.start_as_current_span(f"cart validation step {i}", kind=SpanKind.INTERNAL):
                work(0.001)


flows = [checkout_flow, noisy_flow]

print(f"vern demo traffic generator exporting to {endpoint}")
while True:
    random.choice(flows)()
    work(1.0 / rate)
