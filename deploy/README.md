# Vern compose bundle

Score an OpenTelemetry sample on a free, self-managed Elasticsearch, from a browser or the CLI.

```bash
cp deploy/.env.example deploy/.env   # set ELASTIC_PASSWORD (local-only placeholder inside)
make up                              # builds vern:dev, starts Elasticsearch, vern-init and vern serve
                                     # prints UI: http://127.0.0.1:8080
make score SAMPLE=path/to/sample.jsonl.gz
make down                            # removes containers and volumes
```

## Local UI and API

After `make up`, open http://127.0.0.1:8080 and drop OTLP JSON files (`.json`, `.jsonl`, gzip or zip) on the page, or use the example data. Each upload is loaded, scored and deleted; nothing is stored. The same endpoint works from the command line:

```bash
curl -s -F file=@web/examples/demo.jsonl.gz 127.0.0.1:8080/api/v1/runs \
  | jq '.score.overall_display, .partial, (.problems // [] | length)'
```

The port is published on `127.0.0.1` only; set `VERN_PORT` to publish another host port. Uploads are limited to 30 MiB compressed (larger ones get 413), and at most `VERN_MAX_CONCURRENT_RUNS` (`--max-concurrent`, default 1) runs score at once; the rest get 429 with `Retry-After`. Raise it only with a larger Elasticsearch heap: on the stack's 1 GiB heap, two concurrent runs with the sensitive-data rules trip Elasticsearch's parent circuit breaker (HTTP 429 on loads and queries). `vern serve --help` lists every setting and its env var.

Notes on `deploy/.env`: a password containing `$` must be single-quoted (`ELASTIC_PASSWORD='pa$word'`). Change the placeholder before using `compose.dev.yaml`, because 9200 is then reachable from localhost.

## Reports

`make score` prints the Markdown report to stdout. The JSON report is not kept: it is written inside the `--rm` container and removed with it. To keep both, mount a directory the container's uid (65532) can write and pass the output paths:

```bash
mkdir -p out && chmod 0777 out
docker compose -f deploy/compose.yaml run --rm -v "$PWD/out:/out" vern \
  score /samples/demo.jsonl.gz --output-json /out/vern-report.json --output-md /out/vern-report.md
```

`/samples` is `web/examples` unless you set `SAMPLES_DIR`. Or run the host CLI against the dev overlay (below), which writes `vern-report.json` and `vern-report.md` to the current directory:

```bash
VERN_ELASTICSEARCH_URL=http://127.0.0.1:9200 VERN_ELASTICSEARCH_USERNAME=elastic \
  VERN_ELASTICSEARCH_PASSWORD='<your ELASTIC_PASSWORD>' vern score path/to/sample.jsonl.gz
```

## What runs

- `elasticsearch`: 9.5.5, single node, security on, Basic license, 1g heap. It joins the `internal` `backend` network only and publishes no ports.
- `vern-init`: one-shot job (Elasticsearch image, for bash and curl). It creates the `metrics-otel@custom` component template (7-day metric look-back) with the `elastic` superuser, then mints an API key named `vern`. It is idempotent: if the stored key still authenticates, `make up` mints nothing.
- `vern`: the CLI image (`vern:dev`, distroless, nonroot uid 65532) running `vern serve` on `127.0.0.1:${VERN_PORT:-8080}`. Its healthcheck runs `vern healthcheck`, which GETs `/healthz` (the image has no shell or curl). It reads the key from `/run/vern/api-key` (`VERN_API_KEY_FILE`) and mounts your sample directory read-only at `/samples`. `make score` uses `docker compose run --rm vern score ...`, which replaces `serve` and publishes no ports, so it works while the UI is up. `docker compose stop` gives an in-flight run up to 135s to finish and purge.

## Key file

`/run/vern/api-key` lives in the `vern-secrets` volume. vern-init runs as root so it can `chown 65532:65532` and `chmod 0400` the file: only the `vern` container's uid can read it, and the volume is mounted read-only there. The key is never printed.

## API key privileges (role `vern_runs`)

- cluster: `monitor`
- indices `traces-*.otel-vern*`, `logs-*.otel-vern*`, `metrics-*.otel-vern*`: `create_doc`, `auto_configure`, `read`, `view_index_metadata`, `manage`, `read_failure_store`

These are enough for loading through the native OTLP endpoint (`/_otlp`). The key has no template privileges, so vern-init creates the component template; `vern score` only checks for it (a 403 on that check logs a warning) and never changes it unless you pass `--setup-template`.

## Dev overlay

`deploy/compose.dev.yaml` publishes Elasticsearch on `127.0.0.1:9200` only, for host CLI runs and integration tests:

```bash
docker compose -f deploy/compose.yaml -f deploy/compose.dev.yaml up -d --wait elasticsearch
```

Do not run it alongside `make es-test-up` (same port).

## Run the UI from source

`make serve` runs `go run . serve` on `127.0.0.1:8080` against `VERN_ELASTICSEARCH_URL`. Stop the compose `vern` service first (`make down`), since it uses the same port, or set `VERN_LISTEN_ADDR`. Against the security-off test container:

```bash
make es-test-up
VERN_ELASTICSEARCH_URL=http://127.0.0.1:9200 VERN_SETUP_TEMPLATE=true make serve
make es-test-down
```

`VERN_SETUP_TEMPLATE=true` lets vern create the `metrics-otel@custom` template, as vern-init does in compose; use it only on single-node or throwaway clusters. vern serve starts even when Elasticsearch is down: `/healthz` answers 200, `/readyz` answers 503 and turns 200 once Elasticsearch is up, without a restart.
