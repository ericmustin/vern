# Deploying vern serve

`vern serve` is the web UI and `POST /api/v1/runs` API that score uploaded OTLP samples. Each upload is loaded into Elasticsearch, scored and deleted, so the host keeps no data. It needs an Elasticsearch 9.5.5 cluster it can reach.

## Local with compose

```bash
cp deploy/.env.example deploy/.env   # set ELASTIC_PASSWORD
make up                              # Elasticsearch, vern-init and vern serve
```

Open http://127.0.0.1:8080. See [`deploy/README.md`](../deploy/README.md) for the settings.

## Cloud Run

`deploy/cloudrun/deploy.sh` runs vern and Elasticsearch as two containers in one Cloud Run service. Elasticsearch listens on loopback only, has no credentials, and keeps its data in the instance's memory.

```bash
GCLOUD_CONFIG=my-config GCP_PROJECT=my-project deploy/cloudrun/deploy.sh
```

`GCLOUD_CONFIG` names a gcloud configuration, and every gcloud call passes it with `--project`, so the active configuration is never used. Optional env: `REGION` (default `us-central1`), `SERVICE` (`vern`), `BUDGET_USD` (`30`) and `IMAGE`, which skips the build.

The script is safe to re-run. It enables the five APIs it needs, creates the Artifact Registry repository `vern`, builds and pushes the image, deploys the service with public access, creates a budget alert at 50%, 90% and 100%, and prints the URL. The push uses a short-lived token in a temporary docker config and logs out afterwards. On macOS this replaces, then removes, any keychain credential you already had for that Artifact Registry host. Elasticsearch comes from Docker Hub. It also excludes Cloud Run's request logs, which hold client IPs, so the hosted instance logs only vern's own request lines, as the Privacy page states.

The project needs a linked billing account. `--delete` also removes the shared `vern` repository, so it affects every service that uses it. Remove everything with:

```bash
GCLOUD_CONFIG=my-config GCP_PROJECT=my-project deploy/cloudrun/deploy.sh --delete --yes
```

After an idle period the first visit takes about a minute while Elasticsearch starts. The page shows that it is starting and enables upload when the scorer is ready. Until then `/readyz` and `POST /api/v1/runs` answer 503.

## Any VM with compose

Install Docker, copy the repository, and run `make up`. To serve other hosts, note that compose binds port 8080 to 127.0.0.1 only, so put a TLS-terminating proxy on the VM in front of it. Set `VERN_HSTS=true` only behind HTTPS.

## Your own Elasticsearch

Point `vern serve` at any Elasticsearch 9.5+ cluster:

```bash
VERN_ELASTICSEARCH_URL=https://es.example.com:9200 VERN_API_KEY=... vern serve
```

`VERN_SETUP_TEMPLATE=true` creates a cluster-wide component template, so use it only on a single-node or throwaway cluster. `vern serve --help` lists every setting.

## Cost and limits

An instance (2 vCPU, 3.5 GiB, CPU always allocated) costs about $0.16 an hour while it is up. It stays up for about 15 minutes after the last request, then scales to zero. The free tier covers roughly 30 instance-hours a month. If the instance were kept busy around the clock, the worst case is about $3.70 a day. Max instances is 1.

Uploads are limited to 30 MB compressed, 200 MB decompressed and 500,000 items, and a run stops after 120 seconds. One run scores at a time; the next gets 429. Cloud Run rejects requests over 32 MiB itself, with a 413 that has an HTML body.
