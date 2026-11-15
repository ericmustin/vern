#!/usr/bin/env bash
# Deploys vern (with an Elasticsearch sidecar) to Cloud Run, idempotently.
#
#   GCLOUD_CONFIG=name GCP_PROJECT=my-project deploy/cloudrun/deploy.sh
#   GCLOUD_CONFIG=name GCP_PROJECT=my-project deploy/cloudrun/deploy.sh --delete --yes
#
# Required env: GCLOUD_CONFIG (gcloud configuration name), GCP_PROJECT.
# Optional env: REGION (us-central1), SERVICE (vern), BUDGET_USD (30),
#   IMAGE (default: build and push to Artifact Registry repository "vern").
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)

die() { echo "deploy.sh: $*" >&2; exit 1; }

DELETE=0
YES=0
for arg in "$@"; do
  case "$arg" in
    --delete) DELETE=1 ;;
    --yes) YES=1 ;;
    *) die "unknown argument: $arg (supported: --delete --yes)" ;;
  esac
done

: "${GCLOUD_CONFIG:?GCLOUD_CONFIG is required (the name of a gcloud configuration)}"
: "${GCP_PROJECT:?GCP_PROJECT is required}"
REGION=${REGION:-us-central1}
SERVICE=${SERVICE:-vern}
BUDGET_USD=${BUDGET_USD:-30}
REPO=vern
BUDGET_NAME="vern-$SERVICE"
REGISTRY="$REGION-docker.pkg.dev"

# Every gcloud call names its configuration and project explicitly, so the
# active configuration is never used.
g() { gcloud "$@" --configuration="$GCLOUD_CONFIG" --project="$GCP_PROJECT"; }

billing_account() {
  local acct
  acct=$(g billing projects describe "$GCP_PROJECT" --format='value(billingAccountName)')
  acct=${acct#billingAccounts/}
  [ -n "$acct" ] || die "project $GCP_PROJECT has no billing account linked; link one, then re-run"
  echo "$acct"
}

# Cloud Run's request log stores each client's IP, user agent and referer, so
# it is excluded from the _Default sink; vern's own request lines remain.
EXCLUSION="vern-$SERVICE-requests"
exclusion_present() {
  local names
  names=$(g logging sinks describe _Default --format='value(exclusions[].name)')
  case ";$names;" in *";$EXCLUSION;"*) return 0 ;; esac
  return 1
}

budget_names() {
  g billing budgets list --billing-account="$1" --filter="displayName=$BUDGET_NAME" --format='value(name)'
}

# delete_or_absent runs a gcloud delete and tolerates only "not found"; any
# other failure (permissions, network) stops the script.
delete_or_absent() {
  local out
  if ! out=$(g "$@" 2>&1); then
    case "$out" in
      *NOT_FOUND*|*"not found"*|*"Not found"*|*"could not be found"*) echo "already gone: $1 $2" ;;
      *) echo "$out" >&2; die "gcloud $1 $2 $3 failed" ;;
    esac
  else
    [ -z "$out" ] || echo "$out"
  fi
}

if [ "$DELETE" = 1 ]; then
  [ "$YES" = 1 ] || die "--delete removes the service, the repository and the budget; pass --yes to confirm"
  # Resolve the billing account first, so a failure here deletes nothing.
  acct=$(billing_account)
  delete_or_absent run services delete "$SERVICE" --region "$REGION" --quiet
  delete_or_absent artifacts repositories delete "$REPO" --location "$REGION" --quiet
  for b in $(budget_names "$acct"); do
    g billing budgets delete "$b" --billing-account="$acct"
  done
  if exclusion_present; then
    g logging sinks update _Default --remove-exclusions="$EXCLUSION"
  fi
  echo "Deleted service $SERVICE, repository $REPO and budget $BUDGET_NAME."
  exit 0
fi

# 1. Enable only the APIs this needs.
g services enable run.googleapis.com artifactregistry.googleapis.com \
  cloudbilling.googleapis.com billingbudgets.googleapis.com logging.googleapis.com

# 2. Create the repository if missing.
if ! g artifacts repositories describe "$REPO" --location "$REGION" >/dev/null 2>&1; then
  g artifacts repositories create "$REPO" --repository-format=docker \
    --location "$REGION" --description "vern images"
fi

# 3. Build and push the image, unless IMAGE is given.
if [ -z "${IMAGE:-}" ]; then
  TAG=$(git -C "$ROOT" rev-parse --short=12 HEAD)
  if [ -n "$(git -C "$ROOT" status --porcelain)" ]; then TAG="$TAG-dirty"; fi
  IMAGE="$REGISTRY/$GCP_PROJECT/$REPO/vern:$TAG"
  # Build with the normal docker config: a fresh DOCKER_CONFIG would lose
  # the CLI plugins (buildx) that the Dockerfile needs.
  docker build --platform linux/amd64 --target runtime --build-arg "VERSION=$TAG" -t "$IMAGE" "$ROOT"
  # Only login and push use a throwaway config with a short-lived token, so
  # the global config.json is untouched. An OS credential store (such as the
  # macOS keychain) ignores DOCKER_CONFIG, so the logout removes the
  # credential from there too, on success and on failure.
  DOCKER_TMP=$(mktemp -d)
  docker_cleanup() {
    DOCKER_CONFIG="$DOCKER_TMP" docker logout "$REGISTRY" >/dev/null 2>&1 || true
    rm -rf "$DOCKER_TMP"
  }
  trap docker_cleanup EXIT
  g auth print-access-token | DOCKER_CONFIG="$DOCKER_TMP" docker login -u oauth2accesstoken --password-stdin "$REGISTRY"
  DOCKER_CONFIG="$DOCKER_TMP" docker push "$IMAGE"
  docker_cleanup
  trap - EXIT
fi

# 4. Render the service and deploy it.
RENDERED=$(mktemp)
trap 'rm -f "$RENDERED"' EXIT
sed -e "s|\${SERVICE}|$SERVICE|g" -e "s|\${IMAGE}|$IMAGE|g" "$HERE/service.yaml" >"$RENDERED"
g run services replace "$RENDERED" --region "$REGION"
rm -f "$RENDERED"
trap - EXIT
g run services add-iam-policy-binding "$SERVICE" --region "$REGION" \
  --member=allUsers --role=roles/run.invoker

# Exclude the platform request log (client IPs) from storage, if not yet.
if ! exclusion_present; then
  FILTER="resource.type=\"cloud_run_revision\" AND resource.labels.service_name=\"$SERVICE\" AND log_id(\"run.googleapis.com/requests\")"
  g logging sinks update _Default --add-exclusion="name=$EXCLUSION,filter=$FILTER"
fi

# 5. Budget alert at 50%, 90% and 100%, if missing.
acct=$(billing_account)
if [ -z "$(budget_names "$acct")" ]; then
  g billing budgets create --billing-account="$acct" --display-name="$BUDGET_NAME" \
    --filter-projects="projects/$GCP_PROJECT" --budget-amount="${BUDGET_USD}USD" \
    --threshold-rule=percent=0.5 --threshold-rule=percent=0.9 --threshold-rule=percent=1.0
fi

# 6. Print the URL.
URL=$(g run services describe "$SERVICE" --region "$REGION" --format='value(status.url)')
echo "Deployed $SERVICE: $URL"
