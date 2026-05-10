# vern

A small Go CLI that turns the [Instrumentation Score spec](https://github.com/instrumentation-score/spec) into an Elastic Workflows YAML and uploads it to an Elastic Serverless project. Once deployed, the workflow runs on a schedule, evaluates each rule against your OTel data with ES|QL, writes per-service results to an index, and computes a 0–100 score for every service.

The CLI does not execute queries itself. It is a code generator: **map → generate → sync**.

```
spec/rules/*.md  ──┐
                   │
configs/esql-     ──┼──►  vern generate  ──►  workflows.yaml  ──►  vern sync  ──►  Kibana / Elastic Serverless
mappings.yaml     ──┘
                                                                          │
vern.yaml ───────────────────────────────────────────────────────────────►│ scheduled workflow
                                                                          ▼
                                                            instrumentation-score-results
                                                                          │
                                                                          ▼
                                                            per-service score (0-100)
```

## Quick start

### Install

From a local checkout:

```bash
make install
```

By default this installs `vern` to `~/.local/bin/vern`. Make sure `~/.local/bin`
is on your `PATH`, then verify:

```bash
vern --help
```

You can choose a different install location:

```bash
make install BINDIR=/usr/local/bin
```

Or use Go's standard install flow:

```bash
go install .
```

`go install .` writes the binary to `$(go env GOBIN)` if set, otherwise
`$(go env GOPATH)/bin`; that directory must also be on your `PATH`.

### Generate and sync

```bash
# 1. Configure (defaults are usually fine)
$EDITOR vern.yaml

# 2. Generate the workflow YAML
vern generate --output workflows.yaml

# 3. Sync the workflow + dashboards to Elastic Serverless
export KIBANA_URL=https://<your-project>.kb.<region>.elastic.cloud
export KIBANA_API_KEY=<base64-api-key>
vern sync --replace
```

`vern sync --replace` deletes any prior workflow with the same name before uploading, so re-syncs are idempotent. The same `vern sync` invocation also imports the bundled Kibana saved objects (`dashboards.ndjson`) — two dashboards, two saved searches, and one data view.

After the first run completes, you'll find:

- **Kibana → Workflows** → "Instrumentation Score Evaluation" (run on its schedule, or trigger manually from the UI)
- **Kibana → Dashboards** → "Vern — Instrumentation Score Overview" (color-coded leaderboard, average-score metric, pass/fail pie chart, and a Lens datatable with row-level drilldown to the per-service dashboard) and "Vern — Service Drill-down" (pick a service from the control to see its score gauge, pass/fail pie, and per-rule breakdown; click any **rule_id** to open the upstream spec)
- **APM → service detail pages** → a deployment annotation per service labelled `Vern Instrumentation Score: <n> (<category>)`

Optional:

```bash
# Set up an Elastic Agent Builder agent that can answer "what's the score for X?",
# "best/worst services", "show failing rules for X with example doc ids", etc.
vern agent setup
```

## Commands

### `vern generate`

Resolves the ES|QL query templates in `configs/esql-mappings.yaml` (substituting index patterns, time window, etc. from `vern.yaml`) and writes a complete Elastic Workflows YAML.

| Flag | Default | Description |
|---|---|---|
| `--config` | `vern.yaml` | Path to vern config |
| `--mappings` | from config | Path to ES\|QL mappings (otherwise `vern.yaml`'s `mappings:` field) |
| `--output`, `-o` | `workflows.yaml` | Output workflow YAML |
| `--dashboards` | next to `--output` | Output Kibana saved-objects NDJSON path (default: `dashboards.ndjson` next to the workflow) |

### `vern agent setup`

Creates (or updates) two saved objects in **Kibana → Agent Builder**:

1. **Skill `vern-instrumentation-score-governance`** — a reusable governance skill containing the data-schema knowledge, score formula, spec-URL pattern, and concrete DSL query patterns for the canonical governance questions (see below). Skills in Agent Builder are discoverable by any agent in the project; you can attach this skill to other agents you build.
2. **Agent `vern-instrumentation-score`** — a thin agent that references the skill via `skill_ids` plus a short instruction telling it "use the skill for these questions." All the heavy lifting lives in the skill.

This follows the Agent Builder convention used by built-in skills (`streams-management`, `dashboard-management`, `knowledge-indicators-management`): one broad skill per domain, with the description telling the LLM when to invoke it and the content carrying the data-specific instructions.

**Governance question patterns the skill knows:**

- "what's the instrumentation score for `<svc>`?" — score lookup
- "which 5 services have the worst scores?" — ranking
- "show me services scoring below 50" — compliance threshold
- "show me services failing RES-005 (`service.name` presence)" — spec rule failures
- "show me failing rules for `cart`" — per-service diagnostic
- "compare `cart` and `payment`" — side-by-side benchmark
- "pull a violating example for `<svc>`'s SPA-004 failure" — evidence retrieval
- "what does SPA-004 check?" — spec citation

The skill cites the upstream spec on rule mentions, includes example doc ids on evidence pulls, and offers the matching dashboard URL when a visual follow-up would help.

It uses `PUT /api/agent_builder/{skills,agents}/{id}` (idempotent) with `POST` fallback for first-time creation. Open `<kibana>/app/agent_builder` to chat after running.

| Flag | Default | Description |
|---|---|---|
| `--kibana-url` | `$KIBANA_URL` | Kibana base URL |
| `--api-key` | `$KIBANA_API_KEY` | Kibana API key with Agent Builder write permission |

### `vern sync`

Wraps the workflow YAML as `{"workflows": [{"yaml": "..."}]}` and POSTs to `<kibana-url>/api/workflows` with the headers Kibana expects (`kbn-xsrf`, `x-elastic-internal-origin`, `Authorization: ApiKey ...`).

| Flag | Default | Description |
|---|---|---|
| `--workflow`, `-w` | `workflows.yaml` | Path to workflow YAML |
| `--dashboards` | `dashboards.ndjson` | Path to saved-objects NDJSON (auto-imported after the workflow upload) |
| `--kibana-url` | `$KIBANA_URL` | Kibana base URL (not Elasticsearch) |
| `--api-key` | `$KIBANA_API_KEY` | Kibana API key |
| `--replace` | `false` | Delete existing workflow(s) with the same `name:` before upload (idempotent updates) |
| `--skip-dashboards` | `false` | Skip the saved-objects import step |
| `--dry-run` | `false` | Validate YAML and print payload size; no network call |

## Configuration (`vern.yaml`)

```yaml
backend: esql
mappings: ./configs/esql-mappings.yaml
format: elastic

esql:
  time_window: "30d"               # lookback for eval queries
  index_patterns:
    traces:  "traces-*.otel-*"      # native OTel ingest
    metrics: "metrics-*.otel-*"
    logs:    "logs-*.otel-*"
  result_index: "instrumentation-score-results"
  schedule: "1h"                    # workflow trigger interval
  cardinality_threshold: 10000      # MET-001 (when re-enabled)

elastic:
  # kibana_url: "https://...kb.elastic.cloud"   # prefer KIBANA_URL env var
  # api_key: "..."                              # prefer KIBANA_API_KEY env var

exclusions:
  services: []
  service_patterns: []
```

If you ingest via the legacy APM agent rather than the Serverless OTLP endpoint, change `traces` to `traces-apm*` and rewrite the field paths in `configs/esql-mappings.yaml` (Elastic APM uses `service.name` etc. directly, not `resource.attributes.*`).

## How the generated workflow is shaped

```
bootstrap_mapping            # establish result-index mapping
eval_<rule>  ┐  ×N
store_<rule> ┘     foreach over eval output, indexes one doc per (rule, service)
calculate_scores             # ES|QL aggregating per-service score from result index
store_totals                 # foreach: one _TOTAL doc per service for dashboard reads
annotate_apm                 # foreach: one APM annotation per service in observability-annotations
```

Per enabled rule, vern emits **two** steps:

1. `eval_<rule>` — `elasticsearch.esql.query` returning rows shaped `(rule_passed, service.name, example, extent)`.
2. `store_<rule>` — `foreach` over the eval rows; each iteration writes one document to the result index with rule metadata + per-service fields.

Plus four scaffolding steps:

- `bootstrap_mapping` (first) — indexes a `_BOOTSTRAP` placeholder so the result index has a mapping for `service.name`, `rule_passed`, etc. before the score query parses. Filtered out by `rule_id != "_BOOTSTRAP"`.
- `calculate_scores` — ES|QL that aggregates per-service results from the past 2h and emits the score table.
- `store_totals` — `foreach` over `calculate_scores.output.values`, writing a `_TOTAL` row per service to the same result index. Dashboards read from these rows so they don't re-run rule queries.
- `annotate_apm` — `foreach` writing to `observability-annotations` (the index APM service-detail pages read from). Each annotation appears as a deployment marker on the service's APM timeline.

For 10 enabled rules + score, the workflow has 1 + 10×2 + 1 + 1 + 1 = 24 top-level steps.

## Rule coverage

Source of truth: `spec/rules/*.md` (vendored copy of the upstream spec). What ships in `configs/esql-mappings.yaml`:

| Rule | Impact | Status | Notes |
|---|---|---|---|
| RES-001 | Normal | ✅ enabled | `resource.attributes.service.instance.id` presence |
| RES-002 | Important | ✅ enabled | uniqueness via logical-resource COALESCE (k8s pod uid/name, host.name, container.id, k8s.node.name) |
| RES-003 | Important | ✅ enabled | `k8s.pod.uid` present when k8s context exists |
| RES-005 | Critical | ✅ enabled | `service.name` present and non-empty |
| SPA-001 | Normal | ✅ enabled | ≤10 INTERNAL spans per trace per service |
| SPA-002 | Normal | ⛔ disabled | orphan spans — uses ES\|QL IN-subquery; `requires-validation` |
| SPA-003 | Important | ✅ enabled | span-name cardinality heuristic (≤200 distinct OR ≤30% ratio); spec criteria is TODO |
| SPA-004 | Important | ✅ enabled | root spans not CLIENT |
| SPA-005 | Important | ✅ enabled | ≤20 short INTERNAL spans per trace (<5ms duration) |
| LOG-001 | Important | ✅ enabled | no DEBUG logs in production within 14d |
| LOG-002 | Important | ✅ enabled | logs have `severity_number` set |
| MET-001 | Important | ⛔ disabled | metric cardinality — `needs-design`; native OTel metric docs encode metric name as field path under `metrics.*`, not as a `metric.name` keyword |
| MET-003 | Important | ⛔ disabled | consistent units per metric — same schema issue as MET-001 |
| RES-004 | Important | not implemented | semantic-convention attribute level check; needs an attribute-level catalog |
| MET-002 / 004 / 005 / 006 | Important/Normal | not implemented | unit-correctness, histogram bucket consistency, name-vs-unit, name-vs-attribute key |
| SDK-001 | Low | not implemented | SDK / runtime version support window |

To toggle a rule, set `enabled: true` / `false` in `configs/esql-mappings.yaml` and re-run `vern generate`.

## Dashboards

`vern generate` emits `dashboards.ndjson` alongside the workflow. `vern sync` imports it via the Kibana saved-objects API. The bundle ships:

- **Data view** `vern-instrumentation-score` over `instrumentation-score-results*` with a URL formatter on `rule_id` (clicks open the upstream spec page) and number/percent formatters on `score`/`extent`.
- **Saved searches** `Vern: Service Scores` (filter `rule_id:"_TOTAL"`) and `Vern: Per-Rule Breakdown` (filter `NOT rule_id:_TOTAL AND NOT rule_id:_BOOTSTRAP`).
- **Dashboard: Vern — Instrumentation Score Overview** — leaderboard of all services. Default sort: score DESC. **Click any column header to re-sort** (e.g. score ASC for the worst services first; category to group by tier). Click a `service.name` cell → popup → **Open Service Drill-down** to navigate to the per-service dashboard with that service.name filter applied.
- **Dashboard: Vern — Service Drill-down** — `service.name` options-list control on top; selecting a service filters the score row and the per-rule breakdown table below. Click any `rule_id` to jump to the upstream spec.

The dashboards read **only** from the result index (`instrumentation-score-results`) — the rule queries themselves run inside the workflow, not in the dashboard.

### Drill-down from overview

The overview's score table has a `DASHBOARD_TO_DASHBOARD_DRILLDOWN` configured: clicking a `service.name` value pops up the standard Kibana value menu with **Open Service Drill-down** alongside the default "Filter for value / Filter out value" actions. Selecting it carries `service.name=<value>` as a filter to the drill-down dashboard, where the options-list control auto-applies it.

To make a service map → drill-down flow: combine this with the **APM Custom Link** described below.

The data view ID is fixed at `vern-instrumentation-score`. If you redirect dashboards to a different result index, change `esql.result_index` in `vern.yaml` before `vern generate` (the data view's `title` is rendered from that value).

## Surfacing in the Kibana UI

Three patterns, ranked by automation:

### 1. APM annotations (automated by vern)

The workflow's `annotate_apm` step writes one document per service per run to the `observability-annotations` index — the same index APM service-detail pages read from. Each annotation appears as a **vertical deployment marker on the latency / throughput / failed-transactions charts on the APM service detail Overview tab**, with the score in the marker tooltip (`Vern Instrumentation Score: <n> (<category>)`).

> **Where to look:** APM (or Observability → APM) → pick a service → **Overview** tab → annotations show up as small downward triangles on the time-series charts. They're *not* on a "Dashboards" tab — APM does not have a dashboards tab for OTel-native services. The APM annotation is a chart marker, not a navigable link.
>
> If your service detail page shows no markers, verify the workflow wrote them: `GET /api/apm/services/<name>/annotation/search?start=...&end=...&environment=ENVIRONMENT_ALL` should return `type: version` entries with the score in the `text` field. Markers only render inside the dashboard's currently-selected time range; widen it if needed.

The annotation is a passive marker (no clickable URL). To get a clickable jump to the Vern dashboard from a service detail page, set up Custom Links (option 2).

### 2. APM Custom Links (one-time manual setup) — adds a "Vern" link to every service detail page

This is the closest thing to a "Vern dashboard tab" on the service detail page: a clickable link that appears in the **Custom Links** card on every service's Overview tab and opens the Vern drill-down dashboard pre-filtered to that service.

**Where to set it up**: open Kibana → **Observability → APM → Settings** (gear icon) → **Custom Links** → **Create**:

| Field | Value |
|---|---|
| Label | `Vern Instrumentation Score` |
| URL | `/app/dashboards#/view/vern-drilldown?_a=(filters:!((meta:(key:service.name,params:(query:'{{service.name}}')),query:(match_phrase:(service.name:'{{service.name}}')))))` |
| Filters | (optional) leave empty to show on every service, or `service.name: *` |

The `{{service.name}}` placeholder is substituted by Kibana when rendering the link on each service page.

After saving, every APM service detail page → Overview tab will show "Vern Instrumentation Score" under the **Custom links** card; clicking it opens the drill-down with `service.name` already filtered.

> Note: the `/api/apm/settings/custom_links` HTTP endpoint isn't exposed in Elastic Serverless, so this step is UI-only (takes about 30 seconds, once).

### 3. Direct dashboard URLs

For deep-links (Slack messages, runbooks, etc.):

```
<kibana-url>/app/dashboards#/view/vern-overview
<kibana-url>/app/dashboards#/view/vern-drilldown?_a=(filters:!((meta:(key:service.name,params:(query:'<service>')),query:(match_phrase:(service.name:'<service>')))))
```

### Score formula

Straight from the spec:

```
Score = Σ(P_i × W_i) / Σ(T_i × W_i) × 100
```

Weights: Critical=40, Important=30, Normal=20, Low=10. Categories: ≥90 Excellent, ≥75 Good, ≥50 Needs Improvement, else Poor.

### Output contract

Every rule's eval query MUST return exactly four columns in this order:

1. `rule_passed` (bool)
2. `service.name` (keyword)
3. `example` (keyword) — sample doc id or descriptor
4. `extent` (double, 0.0–1.0) — proportion of evidence violating the rule

The store step indexes by ordinal position via `{{ foreach.item[0..3] }}`, so column order matters.

## Troubleshooting

**`Unknown column [@timestamp]`** — the `traces:` index pattern in `vern.yaml` isn't matching anything. Check actual data streams via `_resolve/index/*` in Kibana DevTools. Native OTel ingest creates `traces-generic.otel-default`, not `traces-apm*`.

**`Unknown column [resource.attributes.X]`** — that field hasn't been indexed in your cluster, so it's not in the mapping. Either disable the rule that uses it, or adjust the field path. Common variants: Elastic APM uses flat `service.name` / `service.node.name`, native OTel uses `resource.attributes.service.name` / `resource.attributes.service.instance.id`.

**`Unknown column [service.name]` in the score step** — happens on a brand-new result index where no foreach has yet written per-service rows. The `bootstrap_mapping` step is meant to prevent this; if you see it anyway, run the workflow once manually in Kibana so bootstrap establishes the mapping.

**Empty results** — check that your eval `time_window` actually covers data freshness. Demo / archival data often needs `30d` or longer. The window applies to all per-rule queries except LOG-001 (hardcoded 14d per spec).

**Workflow uploaded as `Untitled workflow` with `valid: false`** — the YAML failed schema validation. Inspect Kibana's parsed `definition` to find the bad step. Common cause: `foreach` syntax mismatch (must be `foreach: "..."` and `steps: [...]` at the step level, not nested under `with:`).

**Multiple `instrumentation-score-evaluation-N` copies** — Kibana auto-suffixes when names collide. Use `vern sync --replace` to delete prior copies by name before uploading.

**`Unknown column [@timestamp]` in the score step** — the workflow auto-runs on creation, but the multi-foreach pipeline can take 30–60s to complete on first run. If your check is too eager, you'll see partial state. Either wait a minute or trigger explicitly: `POST /api/workflows/test` with body `{"inputs":{},"workflowId":"<id>"}`.

**Dashboards import says `409 Conflict`** — saved objects already exist with those IDs. The sync uses `?overwrite=true` so this should self-heal; if not, delete `vern-*` saved objects in Kibana → Stack Management → Saved Objects and resync.

**No `_TOTAL` documents in the result index** — confirm `calculate_scores` produced rows by running the same query in DevTools. If empty, `time_window` likely doesn't cover any data. Increase to `30d` or longer.

**`AVG(score) must be aggregate_metric_double, ... found value [score] type [text]`** — the result data stream's `score` field got dynamically mapped as `text` because the first document Elasticsearch saw had `score` as a Liquid-substituted string. Vern's bootstrap step now writes `score`, `category`, etc. as proper types **on first index** so new data streams are clean. If your data stream was created with an older vern, either:
1. Delete the data stream once and let the workflow recreate it (you'll lose history): `DELETE _data_stream/instrumentation-score-results` (Kibana DevTools)
2. Live with it — vern's dashboards use `score::double` casts so they work either way.

**APM annotations don't appear on the service Overview tab** — three things to check:
1. Confirm the workflow wrote annotations: `POST <kibana>/api/console/proxy?path=observability-annotations/_search&method=POST` with body `{"query":{"term":{"tags":"vern"}}}`. If empty, the `annotate_apm` step hasn't run yet (run the workflow once via *Test* in the Kibana Workflows UI or `POST /api/workflows/test` with `{"inputs":{},"workflowId":"<id>"}`).
2. Confirm APM sees them: `GET <kibana>/api/apm/services/<svc>/annotation/search?start=...&end=...&environment=ENVIRONMENT_ALL`. If the workflow wrote them but APM doesn't see them, the service.name in your annotation may not exactly match the APM service.name (e.g. `adservice` vs `ad`).
3. Confirm the chart's time range covers the annotation's `@timestamp` — markers only render inside the active range.

**Drill-down from overview navigates but doesn't apply the filter** — the drilldown uses Kibana's `FILTER_TRIGGER`, which fires when you click a value cell (popup shows "Filter for value", "Open Service Drill-down", etc.). Selecting "Open Service Drill-down" carries the filter; the destination dashboard's options-list control auto-applies it. If the filter doesn't transfer, confirm `useCurrentFilters: true` is set in the drilldown action (it is by default in `vern generate`).

## Layout

```
vern/
├── main.go                          # entry point
├── cmd/
│   ├── root.go                      # cobra root, --config flag
│   ├── generate.go                  # vern generate
│   └── sync.go                      # vern sync
├── internal/
│   ├── config/                      # vern.yaml parsing + defaults
│   ├── mappings/                    # esql-mappings.yaml load + Go-template resolution
│   ├── workflow/elastic/            # Elastic Workflows YAML structs + formatter
│   ├── dashboard/                   # Kibana saved-objects NDJSON builder (data view + saved searches + Lens panels + dashboards)
│   ├── agent/                       # Agent Builder skill + agent definitions (skill_content.md is the governance prompt)
│   └── sync/                        # Kibana HTTP client (workflows, saved-objects import, agent_builder skills/agents)
├── configs/
│   └── esql-mappings.yaml           # rule → ES|QL query mapping (the source you'll most often edit)
├── spec/
│   ├── specification.md             # vendored copy of upstream spec
│   └── rules/                       # vendored RES-/SPA-/LOG-/MET-/SDK-*.md
├── vern.yaml                        # default runtime config
├── go.mod / go.sum
└── .gitignore                       # excludes .env, the vern binary, generated workflows.yaml
```

## Testing

```bash
go test ./...                  # all tests
go test ./internal/mappings/   # template resolution
go test ./internal/workflow/elastic/  # YAML structure / Liquid pass-through
```

The Go tests don't talk to a cluster. To smoke-test queries against real data, use Kibana's DevTools console with `_query` and the rule's resolved ES|QL.

## Spec compatibility

Targeting [instrumentation-score/spec](https://github.com/instrumentation-score/spec) v0.1. Rule IDs and impacts are pinned in `configs/esql-mappings.yaml`; if the upstream spec evolves, update both the vendored copy under `spec/rules/` and the matching mapping entry.

## License

[MIT](./LICENSE) © Eric Mustin
