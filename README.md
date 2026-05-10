# vern

Vern is a Go CLI for deploying an OpenTelemetry Instrumentation Score evaluator
to Elastic Serverless.

It reads the vendored [Instrumentation Score spec](https://github.com/instrumentation-score/spec),
maps implemented rules to ES|QL, generates an Elastic Workflow, imports Kibana
dashboards, and can set up an Agent Builder assistant that knows how to query
the generated score data.

Vern currently emits a **partial Instrumentation Score**: implemented and
enabled rules are scored, while missing, disabled, and heuristic rules are
reported as coverage metadata.

```text
spec/rules/*.md + configs/esql-mappings.yaml + vern.yaml
  -> vern setup
  -> Elastic Workflow + Kibana dashboards + Agent Builder skill/agent
  -> instrumentation-score-results
  -> per-service partial score (0-100)
```

## Install

From this checkout:

```bash
make install
vern --help
```

By default this installs to `~/.local/bin/vern`. You can choose another target:

```bash
make install BINDIR=/usr/local/bin
```

Or use Go directly:

```bash
go install .
```

Make sure `$(go env GOBIN)` or `$(go env GOPATH)/bin` is on your `PATH`.

## Quick Start

Set your Kibana URL and API key, then run the full setup flow:

```bash
export KIBANA_URL=https://<your-project>.kb.<region>.elastic.cloud
export KIBANA_API_KEY=<base64-api-key>

vern setup --replace
```

`vern setup` runs:

1. Review config and rule coverage
2. Generate `workflows.yaml`, `dashboards.ndjson`, and `agent-skill.md`
3. Upload or replace the Elastic Workflow
4. Import Kibana dashboards
5. Create or update the Agent Builder skill and agent
6. Print links to the generated Kibana assets

It finishes with links to Workflows, the overview dashboard, the drill-down
dashboard, and Agent Builder.

`agent-skill.md` is the exact generated markdown sent to Agent Builder. Review
that file when you want to inspect the assistant instructions without opening
Kibana.

Preview the flow without writing files or changing Kibana:

```bash
vern setup --dry-run --kibana-url https://<your-project>.kb.<region>.elastic.cloud
```

## Commands

### `vern setup`

Full setup: review, generate, sync, dashboard import, and Agent Builder setup.

| Flag | Default | Description |
|---|---|---|
| `--config` | `vern.yaml` | Config file |
| `--mappings` | from config | ES\|QL mappings file |
| `--workflow`, `-w` | `workflows.yaml` | Workflow YAML output |
| `--dashboards` | next to workflow | Dashboard NDJSON output |
| `--agent-skill` | next to workflow | Agent Builder skill markdown output |
| `--kibana-url` | `$KIBANA_URL` | Kibana base URL |
| `--api-key` | `$KIBANA_API_KEY` | Kibana API key |
| `--replace` | `false` | Replace existing workflow with the same name |
| `--dry-run` | `false` | Print planned steps only |
| `--skip-dashboards` | `false` | Skip dashboard write/import |
| `--skip-agent` | `false` | Skip Agent Builder setup |
| `--strict-coverage` | `false` | Fail when score coverage is partial |

### `vern review`

Checks reproducibility and coverage. Use this before publishing changes.

```bash
vern review
vern review --format json
vern review --strict-coverage
vern review --live-es-url http://localhost:9200
```

`--live-es-url` executes rendered rule ES|QL against Elasticsearch. This is
intended for local/demo validation.

### `vern generate`

Generates workflow and dashboard artifacts without uploading them.

```bash
vern generate --output workflows.yaml
```

| Flag | Default | Description |
|---|---|---|
| `--config` | `vern.yaml` | Config file |
| `--mappings` | from config | ES\|QL mappings file |
| `--output`, `-o` | `workflows.yaml` | Workflow YAML output |
| `--dashboards` | next to output | Dashboard NDJSON output |
| `--agent-skill` | next to output | Agent Builder skill markdown output |

### `vern sync`

Uploads an existing workflow YAML and imports dashboard saved objects.

```bash
vern sync --replace
```

| Flag | Default | Description |
|---|---|---|
| `--workflow`, `-w` | `workflows.yaml` | Workflow YAML path |
| `--dashboards` | `dashboards.ndjson` | Dashboard NDJSON path |
| `--kibana-url` | `$KIBANA_URL` | Kibana base URL |
| `--api-key` | `$KIBANA_API_KEY` | Kibana API key |
| `--replace` | `false` | Replace existing workflow with the same name |
| `--skip-dashboards` | `false` | Skip dashboard import |
| `--dry-run` | `false` | Validate without uploading |

### `vern agent setup`

Creates or updates the Agent Builder skill and agent only.

```bash
vern agent setup
```

| Flag | Default | Description |
|---|---|---|
| `--kibana-url` | `$KIBANA_URL` | Kibana base URL |
| `--api-key` | `$KIBANA_API_KEY` | Kibana API key |
| `--mappings` | from config | ES\|QL mappings file used for coverage content |

## Config

Default config lives in `vern.yaml`.

```yaml
backend: esql
rules_dir: ./spec/rules
mappings: ./configs/esql-mappings.yaml
format: elastic

esql:
  time_window: "30d"
  score_lookback: "2h"
  index_patterns:
    traces: "traces-*.otel-*"
    metrics: "metrics-*.otel-*"
    logs: "logs-*.otel-*"
  result_index: "instrumentation-score-results"
  annotations_index: "observability-annotations"
  schedule: "1h"
  cardinality_threshold: 10000
```

The default index patterns target native OTel ingest in Elastic Serverless. If
you use legacy Elastic APM data streams, update the index patterns and field
paths in `configs/esql-mappings.yaml`.

## Current Review

Current `vern review` result:

| Check | Status | Details |
|---|---|---|
| Config | Pass | `vern.yaml` |
| Mappings | Pass | `./configs/esql-mappings.yaml` |
| Spec version | Pass | `0.1` |
| Score completeness | Warning | Score is partial |
| Workflow config flow | Pass | Uses configured result and annotations indexes |
| Dashboard config flow | Pass | Uses configured result index |
| Agent skill config flow | Pass | Uses configured result and signal index patterns |
| Errors | Pass | `0` |
| Warnings | Warning | `1` coverage warning |

## Rule Coverage

| Rule | Target | Impact | Status | Notes |
|---|---|---|---|---|
| `RES-005` | Resource | Critical | Enabled | `service.name` is present |
| `RES-001` | Resource | Normal | Enabled | `service.instance.id` is present |
| `RES-002` | Resource | Important | Enabled | `service.instance.id` is unique across logical resources |
| `RES-003` | Resource | Important | Enabled | `k8s.pod.uid` is present for Kubernetes telemetry |
| `SPA-001` | Span | Normal | Enabled | Limited number of `INTERNAL` spans per service |
| `SPA-003` | Span | Important | Heuristic | Span-name cardinality; upstream criteria is TODO |
| `SPA-004` | Span | Important | Enabled | Root spans are not `CLIENT` spans |
| `SPA-005` | Span | Important | Enabled | Traces do not contain many short-duration spans |
| `LOG-001` | Log | Important | Enabled | Debug logs are not enabled in production for longer than 14 days |
| `LOG-002` | Log | Important | Enabled | Log records have severity set |
| `SPA-002` | Span | Normal | Disabled | Orphan-span query requires validation |
| `MET-001` | Metric | Important | Disabled | Needs metric cardinality query design |
| `MET-003` | Metric | Important | Disabled | Needs metric unit consistency query design |
| `RES-004` | Resource, Log, Span | Important | Missing | Requires semantic-convention placement catalog |
| `MET-002` | Metric | Important | Missing | Useful metric units |
| `MET-004` | Metric | Normal | Missing | Histogram bucket consistency |
| `MET-005` | Metric | Normal | Missing | Metric names should not contain unit names |
| `MET-006` | Metric | Important | Missing | Metric names should not equal semantic convention attribute keys |
| `SDK-001` | SDK | Low | Missing | Requires SDK/runtime support metadata |

Summary: `10` enabled, `1` heuristic, `3` disabled, `6` missing.

## Local Validation

The demo stack validates rule ES|QL against local Elasticsearch. It does not run
local Kibana or Elastic Workflows.

```bash
make demo-up
make demo-review
make demo-validate
make demo-down
```

`make demo-up` starts Elasticsearch, an EDOT/OTel collector, a deterministic
trace generator, and a small log generator.

## Development

```bash
go test ./...
go run . review
go run . setup --dry-run --kibana-url https://example.kb.us-east-1.elastic.cloud
make dist
```

Useful paths:

| Path | Purpose |
|---|---|
| `cmd/` | Cobra commands |
| `internal/config` | Config parsing and defaults |
| `internal/coverage` | Vendored spec parsing and coverage summary |
| `internal/review` | Reproducibility checks |
| `internal/mappings` | Mapping load and template resolution |
| `internal/workflow/elastic` | Elastic Workflow generation |
| `internal/dashboard` | Kibana saved object generation |
| `internal/agent` | Agent Builder skill and agent definitions |
| `internal/sync` | Kibana API client |
| `demo/` | Local validation stack |
| `agent-skill.md` | Generated Agent Builder skill markdown |

## Release

Local release build:

```bash
make dist
```

GitHub releases are published by `.github/workflows/release.yml` when a tag that
starts with `v` is pushed:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The workflow runs tests, builds Linux/macOS/Windows binaries for amd64 and
arm64, writes checksums, and attaches artifacts to the GitHub release.

## License

[MIT](./LICENSE) © Eric Mustin
