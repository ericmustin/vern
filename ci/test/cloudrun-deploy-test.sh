#!/usr/bin/env bash
# Tests for deploy/cloudrun/deploy.sh using stub gcloud and docker on PATH.
# The stubs record their arguments; nothing here touches the network.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=$(cd "$HERE/../.." && pwd)/deploy/cloudrun/deploy.sh
ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

new_sandbox() {
  T=$(mktemp -d "$ROOT/t.XXXXXX")
  mkdir -p "$T/bin" "$T/home"
  : >"$T/gcloud.log"
  : >"$T/docker.log"
  cat >"$T/bin/gcloud" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$STUB_DIR/gcloud.log"
case "$*" in
  "billing projects describe"*)
    [ -n "${STUB_NO_BILLING:-}" ] || echo "billingAccounts/AAAA-BBBB-CCCC" ;;
  "billing budgets list"*)
    [ -z "${STUB_BUDGET_EXISTS:-}" ] || echo "billingAccounts/AAAA-BBBB-CCCC/budgets/b1" ;;
  "artifacts repositories describe"*)
    [ -n "${STUB_REPO_EXISTS:-}" ] || exit 1 ;;
  "run services replace"*)
    cp "$4" "$STUB_DIR/rendered.yaml" ;;
  "run services describe"*)
    echo "https://stub.invalid" ;;
  "auth print-access-token"*)
    echo "stub-token" ;;
esac
exit 0
STUB
  cat >"$T/bin/docker" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$STUB_DIR/docker.log"
echo "DOCKER_CONFIG=${DOCKER_CONFIG-<unset>}" >>"$STUB_DIR/docker.env"
[ "$1" != login ] || cat >"$STUB_DIR/docker.stdin"
exit 0
STUB
  chmod +x "$T/bin/gcloud" "$T/bin/docker"
  RC=0
}

# run_script [VAR=value ...] -- [args]: runs deploy.sh with the stubs first on
# PATH and a clean env.
run_script() {
  local envs=() args=()
  while [ $# -gt 0 ] && [ "$1" != -- ]; do envs+=("$1"); shift; done
  [ $# -eq 0 ] || shift
  args=("$@")
  (
    cd "$T"
    env -i HOME="$T/home" PATH="$T/bin:$PATH" STUB_DIR="$T" "${envs[@]}" \
      bash "$SCRIPT" ${args[@]+"${args[@]}"} >"$T/stdout" 2>"$T/stderr"
  ) || RC=$?
}

BASE=(GCLOUD_CONFIG=test-config GCP_PROJECT=my-project)

# Missing GCLOUD_CONFIG: non-zero exit, no gcloud calls.
new_sandbox
run_script GCP_PROJECT=my-project IMAGE=img --
[ "$RC" -ne 0 ] || fail "missing GCLOUD_CONFIG exited 0"
grep -q GCLOUD_CONFIG "$T/stderr" || fail "error does not name GCLOUD_CONFIG"
[ ! -s "$T/gcloud.log" ] || fail "gcloud ran without GCLOUD_CONFIG"

# Missing GCP_PROJECT.
new_sandbox
run_script GCLOUD_CONFIG=test-config IMAGE=img --
[ "$RC" -ne 0 ] || fail "missing GCP_PROJECT exited 0"
[ ! -s "$T/gcloud.log" ] || fail "gcloud ran without GCP_PROJECT"

# Fresh deploy with IMAGE set: no docker, both flags on every call.
new_sandbox
run_script "${BASE[@]}" IMAGE=example/vern:test --
[ "$RC" -eq 0 ] || { cat "$T/stderr"; fail "deploy exited $RC"; }
[ -s "$T/gcloud.log" ] || fail "no gcloud calls recorded"
while IFS= read -r line; do
  case "$line" in *" --configuration=test-config"*" --project=my-project") ;; *) fail "call lacks both flags: $line" ;; esac
done <"$T/gcloud.log"
[ ! -s "$T/docker.log" ] || fail "docker ran although IMAGE was set"
grep -q "^services enable run.googleapis.com artifactregistry.googleapis.com cloudbilling.googleapis.com billingbudgets.googleapis.com " "$T/gcloud.log" || fail "APIs not enabled as specified"
grep -q "^artifacts repositories create vern .*--location us-central1" "$T/gcloud.log" || fail "repository not created"
grep -q "^run services replace .* --region us-central1" "$T/gcloud.log" || fail "service not deployed"
grep -q "^run services add-iam-policy-binding vern --region us-central1 --member=allUsers --role=roles/run.invoker" "$T/gcloud.log" || fail "no public invoker binding"
bud=$(grep "^billing budgets create" "$T/gcloud.log") || fail "budget not created"
for want in --billing-account=AAAA-BBBB-CCCC --display-name=vern-vern --filter-projects=projects/my-project --budget-amount=30USD \
  --threshold-rule=percent=0.5 --threshold-rule=percent=0.9 --threshold-rule=percent=1.0; do
  case "$bud" in *"$want"*) ;; *) fail "budget create lacks $want: $bud" ;; esac
done
grep -q "^  name: vern$" "$T/rendered.yaml" || fail "rendered service name"
grep -q "image: example/vern:test" "$T/rendered.yaml" || fail "rendered image"
grep -q 'maxScale: "1"' "$T/rendered.yaml" || fail "rendered maxScale"
# shellcheck disable=SC2016
if grep -q '\${' "$T/rendered.yaml"; then fail "unrendered placeholder left in service.yaml"; fi
grep -q "https://stub.invalid" "$T/stdout" || fail "URL not printed"

# Re-run when everything exists: nothing is created again.
new_sandbox
run_script "${BASE[@]}" IMAGE=img STUB_REPO_EXISTS=1 STUB_BUDGET_EXISTS=1 --
[ "$RC" -eq 0 ] || { cat "$T/stderr"; fail "idempotent re-run exited $RC"; }
if grep -q "repositories create\|budgets create" "$T/gcloud.log"; then fail "re-run created an existing resource"; fi

# Custom SERVICE, REGION and BUDGET_USD flow through.
new_sandbox
run_script "${BASE[@]}" IMAGE=img SERVICE=demo REGION=europe-west1 BUDGET_USD=5 --
[ "$RC" -eq 0 ] || fail "custom env exited $RC"
grep -q "^  name: demo$" "$T/rendered.yaml" || fail "custom SERVICE not rendered"
grep -q "budgets create .*--display-name=vern-demo .*--budget-amount=5USD" "$T/gcloud.log" || fail "custom budget"
grep -q "run services replace .* --region europe-west1" "$T/gcloud.log" || fail "custom REGION"

# No billing account: fail clearly.
new_sandbox
run_script "${BASE[@]}" IMAGE=img STUB_NO_BILLING=1 --
[ "$RC" -ne 0 ] || fail "missing billing account exited 0"
grep -qi "billing account" "$T/stderr" || fail "no clear billing message"

# Default build path: docker build, temp DOCKER_CONFIG, no configure-docker.
new_sandbox
run_script "${BASE[@]}" --
[ "$RC" -eq 0 ] || { cat "$T/stderr"; fail "build path exited $RC"; }
grep -q "^build --platform linux/amd64 --target runtime --build-arg VERSION=" "$T/docker.log" || fail "docker build args"
grep -q "^login -u oauth2accesstoken --password-stdin us-central1-docker.pkg.dev$" "$T/docker.log" || fail "docker login args"
grep -q "^push us-central1-docker.pkg.dev/my-project/vern/vern:" "$T/docker.log" || fail "docker push"
[ "$(cat "$T/docker.stdin")" = stub-token ] || fail "token not piped to docker login"
if grep -q "configure-docker" "$T/gcloud.log" "$T/docker.log"; then fail "configure-docker was used"; fi
if [ -d "$T/home/.docker" ]; then fail "global docker config was created"; fi
dc=$(sed 's/^DOCKER_CONFIG=//' "$T/docker.env" | sort -u)
[ "$(echo "$dc" | wc -l)" -eq 1 ] || fail "DOCKER_CONFIG varied between calls"
case "$dc" in "<unset>"|"$T/home/.docker"|"") fail "DOCKER_CONFIG not a temp dir: $dc" ;; esac
[ ! -e "$dc" ] || fail "temp DOCKER_CONFIG not removed"

# --delete without --yes deletes nothing.
new_sandbox
run_script "${BASE[@]}" -- --delete
[ "$RC" -ne 0 ] || fail "--delete without --yes exited 0"
[ ! -s "$T/gcloud.log" ] || fail "--delete without --yes ran gcloud"

# --delete --yes removes service, repository and budget.
new_sandbox
run_script "${BASE[@]}" STUB_BUDGET_EXISTS=1 -- --delete --yes
[ "$RC" -eq 0 ] || { cat "$T/stderr"; fail "--delete --yes exited $RC"; }
grep -q "^run services delete vern --region us-central1" "$T/gcloud.log" || fail "service not deleted"
grep -q "^artifacts repositories delete vern --location us-central1" "$T/gcloud.log" || fail "repository not deleted"
grep -q "^billing budgets delete billingAccounts/AAAA-BBBB-CCCC/budgets/b1" "$T/gcloud.log" || fail "budget not deleted"
while IFS= read -r line; do
  case "$line" in *" --configuration=test-config"*" --project=my-project") ;; *) fail "delete call lacks both flags: $line" ;; esac
done <"$T/gcloud.log"

echo "cloudrun-deploy-test: all passed"
