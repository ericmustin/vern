#!/usr/bin/env python3
# Generates canaries.jsonl: OTLP JSON lines (traces, logs, metrics) with fake
# canaries in every location RES-010, SPA-006, LOG-003 and MET-008 list.
# Used by internal/conformance/sensitive_integration_test.go and
# internal/engine/elastic/fields_integration_test.go.
#
#   python3 testdata/sensitive/gen_canaries.py > testdata/sensitive/canaries.jsonl
import json, sys
C = "user alice@example.com ssn 123-45-6789 card 4111 1111 1111 1111 key sk-proj-canaryCANARY1234567890"
def v(x):
    if isinstance(x, bool): return {"boolValue": x}
    if isinstance(x, int): return {"intValue": str(x)}
    if isinstance(x, str): return {"stringValue": x}
    if isinstance(x, list): return {"arrayValue": {"values": [v(e) for e in x]}}
    if isinstance(x, dict): return {"kvlistValue": {"values": [{"key": k, "value": v(e)} for k, e in x.items()]}}
    raise TypeError(x)
def attrs(d): return [{"key": k, "value": v(e)} for k, e in d.items()]
def tagged(loc): return loc + " " + C
def long(loc):
    tail = " " + C
    s = loc + " " + "x" * (1500 - len(loc) - 1 - len(tail)) + tail
    assert len(s) == 1500
    return s
NESTED = {"inner": {"email": "alice@example.com"}, "ssn": "123-45-6789"}
ARGS = ["java", "-Dpassword=alice@example.com", "--ssn=123-45-6789", "--card=4111 1111 1111 1111", "--key=sk-proj-canaryCANARY1234567890"]
def resource(extra=None):
    d = {"service.name": "canary-sensitive", "canary.res.str": tagged("res.str"),
         "process.command_args": ARGS, "canary.res.int": 123456789, "canary.res.map": NESTED}
    d.update(extra or {})
    return {"attributes": attrs(d)}
def scope(sig):
    return {"name": tagged("scope.name"), "version": tagged("scope.version"),
            "attributes": attrs({"canary.scope.str": tagged("scope.str"), "canary.scope.int": 123456789})}
MALFORMED = {"canary.ip": "alice@example.com", "canary.timestamp": "123-45-6789", "canary_score": "4111 1111 1111 1111", "canary.usage": "sk-proj-canaryCANARY1234567890"}
T0 = 1700000000000000000
TID = "5b8efff798038103d269b633813fc60c"
traces = {"resourceSpans": [{"resource": resource({"canary.res.long": long("res.long")}), "scopeSpans": [{"scope": scope("t"), "spans": [{
    "traceId": TID, "spanId": "eee19b7ec3c1b174", "name": tagged("span.name"), "kind": 2,
    "startTimeUnixNano": str(T0), "endTimeUnixNano": str(T0 + 1000000),
    "attributes": attrs({"canary.span.str": tagged("span.str"), "canary.span.int": 4111111111111111,
        "canary.span.arr": ["alice@example.com", "123-45-6789", "sk-proj-canaryCANARY1234567890"],
        "canary.span.intarr": [123456789, 4111111111111111],
        "canary.span.map": NESTED, "canary.span.long": long("span.long"),
        "canary.span.maplong": {"v": long("map.long")},
        "url.full": "https://api.example.com/users?ssn=123-45-6789&email=alice@example.com"}),
    "events": [{"timeUnixNano": str(T0 + 500), "name": tagged("event.name"),
        "attributes": attrs({"canary.event.str": tagged("event.str"), "canary.event.int": 123456789,
            "exception.message": tagged("exception.message"), "exception.stacktrace": tagged("exception.stacktrace")})}],
    "links": [{"traceId": "0af7651916cd43dd8448eb211c80319c", "spanId": "b7ad6b7169203331",
        "attributes": attrs({"canary.link.str": tagged("link.str"), "canary.link.int": 123456789})}],
    "status": {"code": 2, "message": tagged("status.message")}},
    {"traceId": TID, "spanId": "aaa19b7ec3c1b174", "parentSpanId": "eee19b7ec3c1b174", "name": "malformed", "kind": 1,
     "startTimeUnixNano": str(T0), "endTimeUnixNano": str(T0 + 1000), "attributes": attrs(MALFORMED)}]}]}]}
logs = {"resourceLogs": [{"resource": resource({"canary.res.long": long("res.long")}), "scopeLogs": [{"scope": scope("l"), "logRecords": [
    {"timeUnixNano": str(T0), "observedTimeUnixNano": str(T0), "severityNumber": 9, "severityText": "INFO",
     "eventName": tagged("event_name"), "body": v(tagged("body.str")),
     "attributes": attrs({"canary.log.str": tagged("log.str"), "canary.log.int": 123456789,
        "canary.log.arr": ["alice@example.com", "123-45-6789"], "canary.log.map": NESTED, "canary.log.long": long("log.long")})},
    {"timeUnixNano": str(T0 + 1), "observedTimeUnixNano": str(T0 + 1), "severityNumber": 9,
     "body": v({"user": {"ssn": "123-45-6789", "email": "alice@example.com", "name": "Alice"},
                "password": "correct-horse", "card": 4111111111111111, "note": tagged("body.map"),
                "blob": long("bodymap.long")}),
     "attributes": attrs({"canary.log.kind": "map-body"})},
    {"timeUnixNano": str(T0 + 2), "observedTimeUnixNano": str(T0 + 2), "severityNumber": 9,
     "body": v(["alice@example.com", "123-45-6789", 4111111111111111, "sk-proj-canaryCANARY1234567890"]),
     "attributes": attrs({"canary.log.kind": "array-body"})},
    {"timeUnixNano": str(T0 + 3), "observedTimeUnixNano": str(T0 + 3), "severityNumber": 9,
     "body": v(long("body.long")), "attributes": attrs({"canary.log.kind": "long-body"})},
    {"timeUnixNano": str(T0 + 4), "observedTimeUnixNano": str(T0 + 4), "severityNumber": 9,
     "body": v("malformed"), "attributes": attrs(dict(MALFORMED, **{"canary.log.kind": "malformed"}))},
]}]}]}
def gauge(name, dp_attrs, desc="", unit="", exemplar=None):
    dp = {"timeUnixNano": str(T0), "asDouble": 1.0, "attributes": attrs(dp_attrs)}
    if exemplar:
        dp["exemplars"] = [{"timeUnixNano": str(T0), "asDouble": 1.0, "traceId": TID, "spanId": "eee19b7ec3c1b174",
                            "filteredAttributes": attrs(exemplar)}]
    return {"name": name, "description": desc, "unit": unit, "gauge": {"dataPoints": [dp]}}
EX = {"canary.ex.str": tagged("ex.str"), "canary.ex.int": 123456789}
sum_dp = {"startTimeUnixNano": str(T0 - 1000000000), "timeUnixNano": str(T0), "asInt": "3",
          "attributes": attrs({"canary.dp.kind": "sum"}),
          "exemplars": [{"timeUnixNano": str(T0), "asInt": "1", "traceId": TID, "spanId": "eee19b7ec3c1b174", "filteredAttributes": attrs(EX)}]}
metrics = {"resourceMetrics": [
  {"resource": resource(), "scopeMetrics": [{"scope": scope("m"), "metrics": [
    gauge("orders.alice@example.com.count",
          {"canary.dp.str": tagged("dp.str"), "canary.dp.int": 4111111111111111,
           "canary.dp.arr": ["alice@example.com", "123-45-6789"], "canary.dp.map": NESTED},
          desc=tagged("metric.description"), unit="sk-proj-canaryCANARY1234567890", exemplar=EX),
    {"name": "canary.sum", "description": tagged("sum.description"), "unit": "{alice@example.com}",
     "sum": {"aggregationTemporality": 2, "isMonotonic": True, "dataPoints": [sum_dp]}},
    gauge("canary.gauge.dplong", {"canary.dp.long": long("dp.long")}),
    gauge("canary.gauge.malformed", dict(MALFORMED, **{"canary.dp.kind": "malformed"})),
  ]}]},
  {"resource": resource({"canary.res.long": long("res.long")}), "scopeMetrics": [{"scope": {"name": "canary.reslong"}, "metrics": [
    gauge("canary.gauge.reslong", {"canary.dp.kind": "reslong"})]}]},
]}
for obj in (traces, logs, metrics):
    sys.stdout.write(json.dumps(obj, separators=(",", ":")) + "\n")
