# Score a sample in CI

`vern score` loads OTLP JSON captures into a throwaway Elasticsearch, scores that run's data only, writes reports, and deletes the data. This page covers capturing a sample, running it on GitHub Actions or GitLab CI, gating on a minimum score, and exporting the score over OTLP.

## Capture a sample

A sample is a file of OTLP JSON lines (`.jsonl` or `.jsonl.gz`). Capture it without changing the data, so the score reflects what your SDKs emit. Do not add resource attributes in the collector.

Run an OpenTelemetry Collector with the `file` exporter and point your instrumented application at it:

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch: {}

exporters:
  file:
    path: /samples/capture.jsonl
    format: json

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [file]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [file]
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [file]
```

Stop the collector gracefully so the exporter flushes the file, then compress and commit the result:

```bash
mkdir -p captures
gzip -9 -c capture.jsonl > captures/checkout.jsonl.gz
```

This is the config in `demo/capture/collector.yaml`; `make capture` runs it with two demo generators.

Some SDKs and declarative configurations offer an experimental OTLP file exporter. Check your SDK's docs, and check that `vern score` decodes the file before you rely on it.

## GitHub Actions

The composite action starts a throwaway Elasticsearch with Docker, scores the files, writes the report to the job summary, uploads the report directory as an artifact, and sets outputs.

```yaml
name: instrumentation-score
on: [pull_request]

permissions:
  contents: read

jobs:
  score:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Score
        id: vern
        uses: ericmustin/vern@vX.Y.Z
        with:
          files: captures/*.jsonl.gz
          min-score: "70"
      - name: Show the score
        run: echo "score=${{ steps.vern.outputs.score }} category=${{ steps.vern.outputs.category }}"
```

Replace `vX.Y.Z` with a release tag that contains `vern score`. There is no moving major tag such as `@v1`. Pinning the action to a release tag also pins the vern binary it downloads to that release.

### Inputs

| Input | Default | Meaning |
| --- | --- | --- |
| `files` | required | OTLP capture files or globs, separated by spaces or newlines. Paths must not contain whitespace. |
| `min-score` | unset | Fail the step (exit code 2) when the score is below this value. Unset means no gate. |
| `otlp-endpoint` | unset | OTLP/HTTP base endpoint to export the score to. It wins over `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`. Unset falls back to those variables from the job or workflow `env:`; with none set, export is disabled. |
| `otlp-headers` | unset | Export headers as `key=value` pairs separated by commas. Pass a secret; the action masks every value. Unset keeps an `OTEL_EXPORTER_OTLP_HEADERS` set in the job or workflow `env:`. |
| `version` | unset | Unset runs the release matching the tag the action is referenced by (`@v1.2.3` runs vern v1.2.3), or `latest` when the action is referenced by a branch or SHA. `latest` downloads the newest GitHub release, `source` builds from the action checkout, a tag such as `v1.2.3` pins a release. |
| `elasticsearch-image` | script default | Elasticsearch image for the throwaway cluster (`docker.elastic.co/elasticsearch/elasticsearch:9.5.5`). |
| `output-dir` | `vern-report` | Directory for `report.json`, `report.md` and `outputs.env`. |
| `artifact-name` | `vern-report` | Name of the uploaded artifact. |
| `upload-artifact` | `true` | Upload the output directory as an artifact. |

### Outputs

| Output | Meaning |
| --- | --- |
| `score` | Instrumentation score, 0 to 100. |
| `category` | Score category. |
| `scored` | `true` when a score was computed. |
| `report-json` | Path of the JSON report. |
| `report-md` | Path of the Markdown report. |
| `exit-code` | Exit code of `vern score`: `0`, `1` (error) or `2` (below `min-score`, or not scored while `min-score` is set). See [Exit codes](#exit-codes). |

### Notes

- `latest` (also what an unset `version` means for a branch or SHA ref) only works once a tagged release that contains `vern score` exists. Until then, use `version: source` or pin a tag.
- If the action runs more than once in a job, or in a matrix, set a distinct `artifact-name` for each use. Artifact names must be unique within a run.
- If `vern score` fails before it writes a report (a decode, connection or configuration error), there is no report or outputs file, and the action's outputs are empty except `exit-code`. The step still fails with the exit code. On any failure the job summary ends with a one-line reason, such as `vern score exited 2: score below --min-score 70, or the run was not scored`.
- To report a failed gate without failing the job, use `continue-on-error` and read `exit-code`, which tells a gate failure (`2`) from an error (`1`):

  ```yaml
  - name: Score
    id: vern
    uses: ericmustin/vern@vX.Y.Z
    continue-on-error: true
    with:
      files: captures/*.jsonl.gz
      min-score: "70"
  - name: Fail on errors only
    if: steps.vern.outputs.exit-code == '1'
    run: exit 1
  ```
- To export the score, pass `otlp-endpoint` and, if the backend needs authentication, `otlp-headers` from a secret:

  ```yaml
  with:
    files: captures/*.jsonl.gz
    otlp-endpoint: https://otlp.example.com
    otlp-headers: ${{ secrets.OTLP_HEADERS }}
  ```
- If the job runs in a `container:`, the throwaway Elasticsearch is still published on the runner host (`-p 127.0.0.1::9200`), which the job container cannot reach, so vern waits out the ready timeout and fails. Run Elasticsearch as a job service instead and point the action at it. Set `VERN_SETUP_TEMPLATE: "true"` too: the action passes `--setup-template` only for a cluster it starts itself, and the service is just as ephemeral. In a container the release download also needs the `gh` CLI, `tar` and `sha256sum` in the job image (or use `version: source`).

  ```yaml
  jobs:
    score:
      runs-on: ubuntu-latest
      container: debian:bookworm
      services:
        elasticsearch:
          image: docker.elastic.co/elasticsearch/elasticsearch:9.5.5
          env:
            discovery.type: single-node
            xpack.security.enabled: "false"
            xpack.license.self_generated.type: basic
            ES_JAVA_OPTS: -Xms1g -Xmx1g
      steps:
        - uses: actions/checkout@v4
        - uses: ericmustin/vern@vX.Y.Z
          env:
            VERN_ELASTICSEARCH_URL: http://elasticsearch:9200
            VERN_SETUP_TEMPLATE: "true"
          with:
            files: captures/*.jsonl.gz
  ```

## GitLab CI

Include the template and extend the hidden `.vern-score` job:

```yaml
include:
  - remote: https://raw.githubusercontent.com/ericmustin/vern/vX.Y.Z/ci/gitlab/vern-score.gitlab-ci.yml

score:
  extends: .vern-score
  variables:
    VERN_IMAGE: ghcr.io/ericmustin/vern:vX.Y.Z-ci
    VERN_FILES: captures/*.jsonl.gz
    VERN_MIN_SCORE: "70"
```

Replace both `vX.Y.Z` with the same release tag. The job runs Elasticsearch as a job-private service, so it needs no Docker-in-Docker. It reads `VERN_*` variables and keeps `vern-report/` as an artifact even when the job fails.

- Set `VERN_IMAGE` to the `-ci` image of the tag you include. The template cannot know the ref it was included from, so by default it uses the floating `ghcr.io/ericmustin/vern:ci` image, which follows the newest release. Without the pin, the include is fixed but the vern binary is not, and the two can drift apart.
- Exit code 2 means the score is below `VERN_MIN_SCORE`. To report a failed gate without failing the pipeline, keep errors fatal and allow only code 2:

  ```yaml
  score:
    extends: .vern-score
    allow_failure:
      exit_codes: [2]
  ```

- The first image publish creates the GHCR package as private. The repository owner must make `ghcr.io/ericmustin/vern` public before GitLab runners can pull it anonymously. Or set `VERN_IMAGE` to a copy in your own registry.
- If you override `VERN_OUTPUT_DIR`, also override `artifacts:paths` to match.
- The template sets `VERN_SETUP_TEMPLATE: "true"`. The service is ephemeral, so changing its templates is safe.
- To export the score, set `OTEL_EXPORTER_OTLP_ENDPOINT` (or `VERN_OTLP_ENDPOINT`) and `OTEL_EXPORTER_OTLP_HEADERS` as CI/CD variables (mask the headers).

## Variables for `ci/vern-ci.sh`

Both wrappers run `ci/vern-ci.sh`, which you can also call yourself. It takes no arguments, only environment variables:

| Variable | Default | Meaning |
| --- | --- | --- |
| `VERN_FILES` | required | Paths or globs separated by whitespace. A glob that matches nothing is an error. Globs are expanded by bash without `globstar`, so `**` matches like `*` (one directory level); list each directory instead. |
| `VERN_MIN_SCORE` | unset | Passed as `--min-score`. |
| `VERN_OUTPUT_DIR` | `vern-report` | Gets `report.json`, `report.md` and `outputs.env`. |
| `VERN_ELASTICSEARCH_URL` | unset | Reuse this cluster. Otherwise a throwaway Elasticsearch is started with Docker and removed on exit. |
| `VERN_ES_IMAGE` | `docker.elastic.co/elasticsearch/elasticsearch:9.5.5` | Image for the throwaway cluster. |
| `VERN_SETUP_TEMPLATE` | unset | `true` passes `--setup-template` to a reused cluster. |
| `VERN_BIN` | `vern` | The vern binary to run. |
| `VERN_READY_TIMEOUT` | `180s` | How long to wait for Elasticsearch. |
| `VERN_OTLP_ENDPOINT` | unset | Passed as `--otlp-endpoint`, so it wins over the `OTEL_EXPORTER_OTLP_*ENDPOINT` variables. |
| `VERN_OTLP_HEADERS` | unset | When non-empty, exported as `OTEL_EXPORTER_OTLP_HEADERS` (header values never go on the command line). When empty, an existing `OTEL_EXPORTER_OTLP_HEADERS` is kept. |

`--setup-template` creates the `metrics-otel@custom` component template, which is cluster-wide. The script passes it only when it started Elasticsearch itself, or when `VERN_SETUP_TEMPLATE=true`. Do not set it against a shared production cluster.

On GitHub and GitLab the script adds these resource attributes to the exported metrics: `vcs.repository.url.full`, `vcs.ref.head.revision`, `vcs.ref.head.name`, `cicd.pipeline.name` and `cicd.pipeline.run.id`. A value you set yourself in `OTEL_RESOURCE_ATTRIBUTES` wins over the CI one.

When `GITHUB_OUTPUT` is set, the script also appends `exit_code=<code>` to it. When `GITHUB_STEP_SUMMARY` is set and the run fails, the summary ends with a one-line reason.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | The run succeeded and any gate passed. |
| 1 | Error: decode, ingest or query failure, a rule that ended with status `error`, every item dropped, or an export failure. |
| 2 | The score is below `--min-score`, or the run is not scored while `--min-score` is set. |

When both 1 and 2 apply, the exit code is 1. Reports and the outputs file are written before the exit code is decided. vern prints the reason once on stderr, prefixed `gate:` for code 2 and `error:` for code 1.

## Export the score over OTLP

`vern score --otlp-endpoint <base-url>` sends the result as OTLP/HTTP protobuf metrics to `<base-url>/v1/metrics`. Set headers with `--otlp-header key=value` (repeatable, values are URL-decoded). Without the flags, vern reads the standard `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS` and `OTEL_EXPORTER_OTLP_METRICS_HEADERS` (which wins over `OTEL_EXPORTER_OTLP_HEADERS` for the same header); the flags win. Header values are never logged.

Metrics (instrumentation scope `github.com/ericmustin/vern`):

| Metric | Unit | Value | Attributes |
| --- | --- | --- | --- |
| `instrumentation_score` | `{score}` | 0 to 100 | One point with `instrumentation_score.scope=overall`, and one per service with `instrumentation_score.scope=service` and `instrumentation_score.service.name`. Every point has `instrumentation_score.category`. Emitted only when the run is scored. |
| `instrumentation_score.rule.passed` | `1` | 1 or 0 per evaluated rule and service | `instrumentation_score.rule.id`, `instrumentation_score.rule.impact`, `instrumentation_score.service.name`, `instrumentation_score.rule.scored` (bool). |
| `instrumentation_score.rules` | `{rule}` | Number of rules with each status | `instrumentation_score.rule.status`. |

Resource attributes: `service.name=vern`, `service.version` (the vern version), `instrumentation_score.spec.version` (the spec version of the rules), then everything in `OTEL_RESOURCE_ATTRIBUTES`, which wins on conflict.

## Runner requirements

- GitHub: a Linux runner with Docker (the action supports Linux only) and about 2 GB of free RAM for Elasticsearch.
- GitLab: a runner that can run job services, with about 2 GB of free memory.
- Anywhere else: Docker, or an Elasticsearch 9.5+ you point `VERN_ELASTICSEARCH_URL` at.

See [DEMO.md](DEMO.md) for an end-to-end walkthrough.
