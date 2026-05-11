# Advanced Reference

See [README.md](../README.md) for the overview and screenshots. This document
covers commands, config flow, local validation, development, and release.

## Install

From this checkout:

```bash
make install                          # → ~/.local/bin/vern
make install BINDIR=/usr/local/bin    # pick a different target
```

Or use Go directly:

```bash
go install .
```

Make sure `$(go env GOBIN)` or `$(go env GOPATH)/bin` is on your `PATH`.

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

It finishes with links to Workflows, the overview dashboard, the drill-down
dashboard, and Agent Builder.

### `vern review`

Checks reproducibility and coverage. Use this before publishing changes.

```bash
vern review
vern review --format json
vern review --strict-coverage
vern review --live-es-url https://<your-project>.es.<region>.elastic.cloud
```

`--live-es-url` executes rendered rule ES|QL against Elasticsearch. Auth is
read from `ELASTIC_API_KEY` or `KIBANA_API_KEY`.

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

### `vern spec sync` / `vern spec status`

Compares the locally vendored Instrumentation Score spec under `./spec/` with
upstream at the pinned ref (`spec.upstream_ref` in `vern.yaml`). `status` is
read-only; `sync --apply` overwrites local files.

```bash
vern spec status
vern spec sync --apply
```

### `vern semconv sync`

Fetches the upstream OpenTelemetry semantic conventions at the pinned ref
(`semconv.upstream_ref` in `vern.yaml`) and regenerates
`internal/semconv/attribute_keys.go` and `placement.go`. The committed catalog
powers MET-006 (metric name collisions with semconv keys) and RES-004 (semconv
attribute placement).

```bash
vern semconv sync           # dry run — prints catalog summary
vern semconv sync --apply   # regenerate Go files + VERSION
```

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

## Config Reference

Full `vern.yaml`:

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

filters:
  # Restrict evaluation to specific deployment environments (case-insensitive).
  environments: []
  # Restrict evaluation to specific service namespaces.
  service_namespaces: []
  # SDK-001 is opt-in because it depends on a vendored support matrix.
  enable_sdk_rules: false

spec:
  upstream_repo: "instrumentation-score/spec"
  upstream_ref: "main"

semconv:
  upstream_repo: "open-telemetry/semantic-conventions"
  upstream_ref: "v1.37.0"
```

`filters.environments` and `filters.service_namespaces` are appended to every
rule query as `AND (...)` predicates. Scope to prod-like environments to keep
findings actionable; leave empty to evaluate across all data.

Coverage goal: zero missing mappings, enable rules only when they can produce
accurate service-level rows, and document blockers where full coverage is not
yet possible.

## Local Validation

The demo stack validates rule ES|QL against local Elasticsearch. It does not
run local Kibana or Elastic Workflows.

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
| `internal/semconv` | Generated semconv catalog + placement |
| `internal/specsync` | Upstream spec drift detection |
| `internal/sdksupport` | Vendored SDK/runtime support matrix (SDK-001) |
| `internal/sync` | Kibana API client |
| `demo/` | Local validation stack |
| `agent-skill.md` | Generated Agent Builder skill markdown |

## Release

Local release build:

```bash
make dist
```

GitHub releases are published by `.github/workflows/release.yml` when a tag
that starts with `v` is pushed:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The workflow runs tests, builds Linux/macOS/Windows binaries for amd64 and
arm64, writes checksums, and attaches artifacts to the GitHub release.
