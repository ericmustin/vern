# End-to-end demo

This walkthrough takes you from a captured OTLP sample to a CI score that is exported over OTLP. Run every command from the repository root (the `vern/` directory). You need Docker and `make`, plus `jq` (`make capture` uses it to summarize the capture).

## 1. Capture a sample

```bash
make capture
```

This runs a collector and two demo generators in Docker for 90 seconds (`CAPTURE_SECONDS=90`) and writes `demo/samples/capture.jsonl`. The collector publishes no host ports and adds no attributes. The generators are deliberately imperfect, so some rules fail.

## 2. Score it locally

```bash
cp deploy/.env.example deploy/.env   # once; edit the placeholder password if you like
make up
make score SAMPLE=demo/samples/capture.jsonl
make down
```

`make up` builds the image, starts a single-node Elasticsearch, creates an API key and starts the [local UI](#local-ui). `make score` prints the Markdown report to stdout. `make down` removes the containers and volumes. See [deploy/README.md](../deploy/README.md) for details.

## 3. Commit the sample

Compress the capture and commit it to the repository you want to gate:

```bash
mkdir -p captures
gzip -9 -c demo/samples/capture.jsonl > captures/demo.jsonl.gz
git add captures/demo.jsonl.gz
git commit -m "Add instrumentation sample"
```

This repository already has a committed sample, `web/examples/demo.jsonl.gz`, made the same way. See `web/examples/README.md`.

## 4. Score your capture in CI

Add a workflow that scores the committed capture on every pull request that changes it. Save this as `.github/workflows/score-captures.yml`:

```yaml
name: score-captures

on:
  pull_request:
    paths: ['captures/**']
  workflow_dispatch:

permissions:
  contents: read

jobs:
  score:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: ericmustin/vern@<tag>
        with:
          files: captures/*.jsonl.gz
          min-score: "70"
```

Replace `<tag>` with a release tag that contains `vern score`. Inside this repository, use `uses: ./` with `version: source` instead. See [CI.md](CI.md#github-actions) for all inputs and outputs.

This repository's own `.github/workflows/score-demo.yml` is separate: it scores the bundled sample `web/examples/demo.jsonl.gz`, not `captures/`. It runs on pull requests that touch the sample, the rule mappings or definitions, the Go code, the `Dockerfile` or the CI files, and from the Actions tab.

## 5. Export the score into a collector

The workflow starts a collector with the `debug` exporter and passes `otlp-endpoint: http://localhost:4318`. You can do the same locally with the config in `ci/test/collector-debug.yaml`:

```bash
docker run -d --name otelcol -p 4318:4318 \
  -v "$PWD/ci/test/collector-debug.yaml:/etc/otelcol-contrib/config.yaml" \
  otel/opentelemetry-collector-contrib:0.152.0
make build
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 VERN_BIN=./vern \
  VERN_FILES=web/examples/demo.jsonl.gz VERN_MIN_SCORE=0 bash ci/vern-ci.sh
docker logs otelcol 2>&1 | grep instrumentation_score
docker rm -f otelcol
```

`ci/vern-ci.sh` starts its own throwaway Elasticsearch, so you do not need `make up` for this step. The logs show the `instrumentation_score`, `instrumentation_score.rule.passed` and `instrumentation_score.rules` metrics.

## Local UI

`make up` prints `UI: http://127.0.0.1:8080`. Open it and drop the capture from step 1, or click "Try example data" to score `web/examples/demo.jsonl.gz`. The page shows the same score as `make score`: for the bundled sample, 82.9, partial (21 of 26 rules scored). The API returns the same report as JSON:

```bash
make up
curl -s -F file=@web/examples/demo.jsonl.gz 127.0.0.1:8080/api/v1/runs \
  | jq '.score.overall_display, .partial, (.problems // [] | length)'
make down
```

The output is `"82.9"`, `true` and `0`. Each upload is scored and deleted; the Privacy page explains what is kept (nothing) and what is logged. See [deploy/README.md](../deploy/README.md#local-ui-and-api) for limits and settings.
