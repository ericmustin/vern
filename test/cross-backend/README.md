# Cross-backend validation harness

Checks that `vern evaluate` produces equivalent Instrumentation Score results
across Elastic, ClickHouse, and OpenSearch when all three get identical
telemetry. A disagreement points at a bug in a rule mapping.

## How it works

```
  trafficgen ─┐
  otelgen-logs ├─OTLP─▶ otel-collector ─┬─▶ elasticsearch ─┐
  metricsgen ┘                          ├─▶ clickhouse ────┼─▶ vern evaluate ×3 ─▶ vern compare
                                        └─▶ opensearch ────┘
```

One OTel collector receives a single OTLP stream and fans it to all three
backends through their standard exporters. `vern evaluate` then runs each
backend's rule queries; `vern compare` diffs the three reports.

## Requirements

- Docker with at least 6 GB of memory (8 GB is comfortable). With less, the
  OpenSearch container is OOM-killed. In Docker Desktop: Settings → Resources →
  Memory.
- Go toolchain (the harness builds `vern` from source).

## Usage

```bash
# from the repo root
make cross-backend-validate     # up → wait for data → evaluate ×3 → compare
make cross-backend-down         # tear down + wipe volumes
```

Or step by step, from `test/cross-backend/`:

```bash
make up          # docker compose up, gated on healthchecks
make evaluate    # waits for ingested data, then runs vern evaluate ×3
make compare     # diffs the three reports
make down        # tear down
```

Outputs land in `test/cross-backend/reports/` (gitignored):
`elastic.json` / `clickhouse.json` / `opensearch.json` plus their `.md`
siblings, and `compare.md` / `compare.json`.

## Rule coverage

Six rules are mapped for every backend, with one expected gap:

| Rule | Impact | Signal | elastic | clickhouse | opensearch |
|---|---|---|---|---|---|
| RES-005 | Critical | traces | yes | yes | yes |
| RES-001 | Normal | traces | yes | yes | yes |
| SPA-003 | Important | traces | yes | yes | yes |
| LOG-002 | Important | logs | yes | yes | yes |
| MET-001 | Important | metrics | yes | yes | **gap** |
| MET-006 | Important | metrics | yes | yes | **gap** |

The OTel collector's `opensearchexporter` does not support the metrics
signal, so `ss4o_metrics-*` is never created and MET-001 and MET-006 soft-fail
on OpenSearch. The Makefile passes
`--expected-gap opensearch:MET-001 --expected-gap opensearch:MET-006` to
`vern compare`, so the gap does not count against the verdict. Closing the gap means adding Data Prepper.

## Comparison tolerances

Sampling, time-window granularity, and approximate cardinality counting cause
small drift between backends, so `vern compare` checks within tolerances:

| Check | Default | Exit code on breach |
|---|---|---|
| `rule_passed` mismatch (same service+rule, opposite boolean) | strict | 1 |
| `extent` spread when `rule_passed` agrees | ±0.05 | 2 |
| per-service `score` spread | ±10 points | 3 |
| service-set coverage per backend | ≥80% of the union | 4 |

A backend with no row for a rule counts as "not evaluated", never as a
disagreement. Exit code 0 means pass.

Tune with `--extent-tolerance`, `--score-tolerance`, `--service-overlap-min`
on the `vern compare` invocation in `Makefile`.

## Debugging a failed comparison

`reports/compare.md` shows a per-backend summary, a coverage table, and the
`(service, rule)` cells that disagree. To dig into a specific disagreement:

- Inspect the per-backend report (`reports/<backend>.md`) for the rule's
  per-service extent.
- Re-run a single backend: `vern evaluate --backend <name> --config
  vern-configs/<name>.yaml` from the repo root.
- Query the backend directly: Elasticsearch on `:9200`, ClickHouse on `:8123`,
  OpenSearch on `:9201`.
- A `rule_passed` mismatch usually means the rule's query differs in meaning
  between the `configs/<backend>-mappings.yaml` files. Compare the three for
  that rule ID.

## CI

`make cross-backend-validate` exits non-zero on any out-of-tolerance
disagreement, so a CI job only needs to run it and publish
`reports/compare.md` as an artifact. No workflow for it is committed.
