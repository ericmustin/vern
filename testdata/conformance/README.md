# Conformance fixtures

Small OTLP samples that check each rule's ES|QL query against
`spec/rules/<RULE>.md`. Each case is scored on its own through
`samplescore.Score` (the same path `vern score` uses) on Elasticsearch 9.5.5
with the free Basic license.

```bash
make es-test-up
go test -tags integration ./internal/conformance/ -v
make es-test-down
```

`go test ./internal/conformance/` (no tag, no cluster) checks that the fixtures
are well formed. CI runs both: the `test` job runs the cluster-free checks, and
the `integration` job runs the tagged tests against an Elasticsearch service
container.

## Layout

```
testdata/conformance/
  <RULE>/            one directory per rule, named by rule ID (e.g. RES-005)
    expect.yaml      what each case must produce
    pass.jsonl       required case
    fail.jsonl       required case
    <key>.jsonl      optional extra cases, one per extra key in expect.yaml
    <key>.jsonl.gz   a case file may be gzipped instead (e.g. MET-001/fail)
  _schema/full.jsonl the schema lint sample (see below)
```

## expect.yaml

```yaml
rule: RES-005
cases:
  pass: { status: evaluated, services: { conf-res-005: true } }
  fail: { status: evaluated, services: { "(missing service.name)": false } }
  # optional: other cases (e.g. not_applicable) as extra keys whose file is <key>.jsonl
  empty: { status: evaluated, services: { "(missing service.name)": false } }
```

- `rule`: the rule ID. It must match the directory name and be an enabled rule
  in `configs/esql-mappings.yaml`.
- `cases`: a map from case key to expectation. The key names the file:
  `<key>.jsonl` or `<key>.jsonl.gz` in the same directory (the `.gz` is
  used when it exists; having both is an error). `pass` and `fail` are
  required. Every `.jsonl` and `.jsonl.gz` in the directory must have a key.
- `status`: the rule's status in the report: `evaluated`, `not_applicable`,
  `not_assessable`, `not_implemented` or `error`.
- `services`: only for `evaluated`, and then required. The exact map from
  service name to `rule_passed`. Every service the rule reports must be listed,
  and nothing else. A row with a null or empty `service.name` is reported as
  `"(missing service.name)"`. Quote that key in YAML.
- `scored`: optional, only for `evaluated`. When given, the rule's `scored`
  flag in the report must equal it. An advisory rule (SPA-003) is evaluated but
  never scored, so its cases set `scored: false`. Omit it to leave the flag
  unchecked.

Unknown keys in `expect.yaml` are an error.

For every case the harness also asserts that no other rule has status
`error`. Most other rules come out `not_applicable` or `not_assessable` on a
small single-signal fixture; that is expected.

## Writing a case

- One small JSONL file per case: OTLP JSON export documents, one per line
  (`resourceSpans`, `resourceLogs` or `resourceMetrics`), as the collector's
  `file` exporter writes them. Gzip a case that must be large (MET-001's
  `fail` needs 10,000 series) as `<key>.jsonl.gz` with `gzip -9 -n`; the
  tests read it through `otlpin.Decode`, which handles gzip.
- Name the service `conf-<rule-lowercase>` (for example `conf-res-005`), or
  `conf-<rule-lowercase>-<suffix>` when one sample needs several services (for
  example `conf-met-003-good` passing and `conf-met-003-bad` failing in the
  same case). A resource may instead leave `service.name` absent or empty when
  that is the point of the case. The cluster-free test enforces this.
- Use any fixed timestamps. `Score` shifts the sample so the newest item lands
  at now − 1m, and sets the query time window from the sample's span. A case
  with metric data points must span at most 1 hour: the metrics data streams
  are time series indices, and Elasticsearch rejects points older than about
  2 hours. Other cases may span up to 30 days minus 1 hour (the traces and
  logs data streams accept old documents), which keeps them inside the longest
  look-back any query uses (LOG-001's 30 days). Rules with a
  `min_observation` need long spans: LOG-001's cases span 15 days. Some queries
  use a fixed window instead of the sample's span (MET-001 the last hour,
  MET-003 the last 14 days, LOG-001 the last 30 days), so keep their points
  inside it.
- Read the pass/fail criteria from `spec/rules/<RULE>.md`. The query must
  agree with the spec. Where they disagree, the spec wins: fix the query in
  `configs/esql-mappings.yaml`, add a `notes:` line saying why, and name the
  fixture that exposed it.
- Add extra cases for the edges the spec spells out (empty values, case
  variants, multi-valued attributes, preconditions that make the rule not
  apply). Put a one-line YAML comment above each extra case saying what it
  covers.
- Cases run in parallel, at most 4 at a time, each in its own run-scoped data
  streams.

## Schema lint (`_schema/full.jsonl`)

`TestConformance_SchemaLint` scores `_schema/full.jsonl` with
`esql.unmapped_fields: off`, the opt-in SDK rule switched on, and the
environment and service-namespace scope filters set. It passes its own copy of
the embedded mappings with every `min_observation` cleared, so rules that need
a long sample (LOG-001) are planned instead of being marked `not_assessable`.
With NULLIFY off, a misspelled field fails planning with `Unknown column`
instead of quietly reading as null.

Every rule in the report must come back `evaluated`: a rule that is skipped
(`not_applicable`, `not_assessable`, `not_implemented`) was never planned, so
it was never linted. The only exceptions are listed, each with its reason, in
`schemaLintAllowed` in `internal/conformance/conformance_integration_test.go`
(today: the disabled or planned rules with no real query). An allowlisted rule
that does get evaluated also fails the test, so remove it from the list when it
gains a query. When you add or enable a rule, make `full.jsonl` give it rows.

The sample therefore sets every field any rule references: traces, logs and
metrics, the resource attributes the rules read, and every semconv
resource-only key as a span attribute (RES-004 references
`attributes.<key>` for each). When a rule starts reading a new field, add it
here. After `vern semconv sync` changes the resource-only key list,
`TestFixtures_SchemaCoversResourceOnlyKeys` lists the keys to add to the
`schema step` span.

Sensitive-data cases (RES-010, SPA-006, LOG-003, MET-008) may also set:

- `blind_spots_contains: ["text", ...]`: each substring must appear in one of
  the report's sample blind spots.
- `min_checks: N`: every service's pass message ("No sensitive data matched N
  checks") must report at least N checks, so a field-discovery failure cannot
  pass as a clean result. Set it to the count the fixture produces today.
