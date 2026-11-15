#!/usr/bin/env bash
# vern-ci.sh: score OTLP captures in CI with `vern score`.
#
# Configured by environment (no positional arguments):
#   VERN_FILES               required; paths or globs separated by spaces, tabs or
#                            newlines (a YAML block works). Paths must not contain
#                            whitespace
#   VERN_MIN_SCORE           unset; passed as --min-score (exit 2 when below)
#   VERN_OUTPUT_DIR          vern-report; gets report.json, report.md, outputs.env
#   VERN_ELASTICSEARCH_URL   unset; reuse that cluster. Otherwise a throwaway
#                            Elasticsearch is started with Docker and removed on exit
#   VERN_ES_IMAGE            docker.elastic.co/elasticsearch/elasticsearch:9.5.5
#   VERN_SETUP_TEMPLATE      unset; "true" passes --setup-template to a reused
#                            cluster (it changes a cluster-wide component
#                            template). Always passed for a Docker-started cluster
#   VERN_BIN                 vern
#   VERN_READY_TIMEOUT       180s
#   VERN_OTLP_ENDPOINT       unset; passed as --otlp-endpoint (wins over the
#                            OTEL_EXPORTER_OTLP_[METRICS_]ENDPOINT variables)
#   VERN_OTLP_HEADERS        unset; when non-empty, exported as
#                            OTEL_EXPORTER_OTLP_HEADERS. When empty, an inherited
#                            OTEL_EXPORTER_OTLP_HEADERS is left alone
#
# Exits with the exit code of `vern score` (0 ok, 1 error, 2 below the gate).
# Appends report.md (and, on failure, a one-line reason) to $GITHUB_STEP_SUMMARY
# and outputs.env plus exit_code=<code> to $GITHUB_OUTPUT when those are set.
# CI resource attributes are added to OTEL_RESOURCE_ATTRIBUTES; entries the user
# already set there win.
set -euo pipefail

die() {
  echo "vern-ci: $*" >&2
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    echo "vern-ci: $*" >>"$GITHUB_STEP_SUMMARY" || true
  fi
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    echo "exit_code=1" >>"$GITHUB_OUTPUT" || true
  fi
  exit 1
}

VERN_BIN=${VERN_BIN:-vern}
VERN_OUTPUT_DIR=${VERN_OUTPUT_DIR:-vern-report}
VERN_ES_IMAGE=${VERN_ES_IMAGE:-docker.elastic.co/elasticsearch/elasticsearch:9.5.5}
VERN_READY_TIMEOUT=${VERN_READY_TIMEOUT:-180s}

[ -n "${VERN_FILES:-}" ] || die "VERN_FILES is required (whitespace-separated paths or globs)"

# Expand globs here (nullglob semantics): a pattern that matches nothing is an error.
files=()
IFS=$' \t\n' read -r -d '' -a patterns <<<"$VERN_FILES" || true
for pattern in "${patterns[@]}"; do
  matches=()
  while IFS= read -r m; do
    matches+=("$m")
  done < <(compgen -G "$pattern" || true)
  [ "${#matches[@]}" -gt 0 ] || die "no files matched: $pattern"
  files+=("${matches[@]}")
done

# Percent-encode the characters the OTel env spec reserves in values.
encode() {
  local v=$1
  v=${v//%/%25}
  v=${v//,/%2C}
  v=${v//=/%3D}
  printf '%s' "$v"
}

# CI attributes go first: later entries win, so the user's own
# OTEL_RESOURCE_ATTRIBUTES (appended below) override them.
attrs=
add_attr() {
  [ -n "$2" ] || return 0
  attrs=${attrs:+$attrs,}$1=$(encode "$2")
}

if [ -n "${GITHUB_ACTIONS:-}" ]; then
  repo=
  if [ -n "${GITHUB_SERVER_URL:-}" ] && [ -n "${GITHUB_REPOSITORY:-}" ]; then
    repo=$GITHUB_SERVER_URL/$GITHUB_REPOSITORY
  fi
  add_attr vcs.repository.url.full "$repo"
  add_attr vcs.ref.head.revision "${GITHUB_SHA:-}"
  add_attr vcs.ref.head.name "${GITHUB_REF_NAME:-}"
  add_attr cicd.pipeline.name "${GITHUB_WORKFLOW:-}"
  add_attr cicd.pipeline.run.id "${GITHUB_RUN_ID:-}"
elif [ -n "${GITLAB_CI:-}" ]; then
  add_attr vcs.repository.url.full "${CI_PROJECT_URL:-}"
  add_attr vcs.ref.head.revision "${CI_COMMIT_SHA:-}"
  add_attr vcs.ref.head.name "${CI_COMMIT_REF_NAME:-}"
  add_attr cicd.pipeline.name "${CI_PIPELINE_NAME:-${CI_PROJECT_PATH:-}}"
  add_attr cicd.pipeline.run.id "${CI_PIPELINE_ID:-}"
fi
if [ -n "${OTEL_RESOURCE_ATTRIBUTES:-}" ]; then
  attrs=${attrs:+$attrs,}$OTEL_RESOURCE_ATTRIBUTES
fi
if [ -n "$attrs" ]; then
  export OTEL_RESOURCE_ATTRIBUTES=$attrs
fi

# Header values stay in the environment (never on argv). An empty
# VERN_OTLP_HEADERS must not clobber an inherited OTEL_EXPORTER_OTLP_HEADERS.
if [ -n "${VERN_OTLP_HEADERS:-}" ]; then
  export OTEL_EXPORTER_OTLP_HEADERS=$VERN_OTLP_HEADERS
fi
unset VERN_OTLP_HEADERS

container=
# shellcheck disable=SC2329 # invoked through the EXIT trap
cleanup() {
  if [ -n "$container" ]; then
    docker rm -f "$container" >/dev/null 2>&1 || true
  fi
}
child=
sig=0
# shellcheck disable=SC2329 # invoked through traps
on_signal() {
  sig=$1
  if [ -n "$child" ]; then
    kill -"$2" "$child" 2>/dev/null || true
  else
    exit $((128 + sig))
  fi
}
trap cleanup EXIT
trap 'on_signal 2 INT' INT
trap 'on_signal 15 TERM' TERM
trap 'on_signal 1 HUP' HUP

setup_template=false
if [ -n "${VERN_ELASTICSEARCH_URL:-}" ]; then
  [ "${VERN_SETUP_TEMPLATE:-}" = true ] && setup_template=true
else
  command -v docker >/dev/null 2>&1 ||
    die "set VERN_ELASTICSEARCH_URL to an Elasticsearch to use, or install docker so a throwaway one can be started"
  name=vern-ci-es-$$
  # Register the name before starting so the trap also covers a partial start.
  container=$name
  docker run -d --rm --name "$name" -p 127.0.0.1::9200 \
    -e discovery.type=single-node \
    -e xpack.security.enabled=false \
    -e xpack.license.self_generated.type=basic \
    -e "ES_JAVA_OPTS=-Xms1g -Xmx1g" \
    "$VERN_ES_IMAGE" >/dev/null || die "docker run failed for $VERN_ES_IMAGE"
  hostport=
  for _ in 1 2 3 4 5; do
    hostport=
    read -r hostport < <(docker port "$name" 9200/tcp 2>/dev/null) || true
    [ -z "$hostport" ] || break
    sleep 0.5
  done
  hostport=${hostport##*:}
  [ -n "$hostport" ] || die "could not read the published port of $name"
  export VERN_ELASTICSEARCH_URL=http://127.0.0.1:$hostport
  setup_template=true
fi

mkdir -p "$VERN_OUTPUT_DIR"
args=(score
  --output-json "$VERN_OUTPUT_DIR/report.json"
  --output-md "$VERN_OUTPUT_DIR/report.md"
  --outputs-file "$VERN_OUTPUT_DIR/outputs.env"
  --ready-timeout "$VERN_READY_TIMEOUT")
[ -z "${VERN_MIN_SCORE:-}" ] || args+=(--min-score "$VERN_MIN_SCORE")
[ "$setup_template" = false ] || args+=(--setup-template)
[ -z "${VERN_OTLP_ENDPOINT:-}" ] || args+=(--otlp-endpoint "$VERN_OTLP_ENDPOINT")

# Stale results from an earlier run must not be published after a failure.
rm -f "$VERN_OUTPUT_DIR/report.json" "$VERN_OUTPUT_DIR/report.md" "$VERN_OUTPUT_DIR/outputs.env"

# Background + wait so a TERM/INT reaches vern and the EXIT trap still runs.
"$VERN_BIN" "${args[@]}" -- "${files[@]}" &
child=$!
rc=0
wait "$child" || rc=$?
if [ "$sig" -ne 0 ]; then
  # The first wait returned because of our trap; reap the child for real.
  wait "$child" 2>/dev/null || true
  child=
  exit $((128 + sig))
fi
child=

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  if [ -f "$VERN_OUTPUT_DIR/report.md" ]; then
    cat "$VERN_OUTPUT_DIR/report.md" >>"$GITHUB_STEP_SUMMARY"
  fi
  if [ "$rc" -ne 0 ]; then
    if [ "$rc" -eq 2 ]; then
      why="score below --min-score ${VERN_MIN_SCORE:-}, or the run was not scored"
    else
      why="see the job log"
    fi
    printf '\nvern score exited %s: %s\n' "$rc" "$why" >>"$GITHUB_STEP_SUMMARY"
  fi
fi
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  if [ -f "$VERN_OUTPUT_DIR/outputs.env" ]; then
    cat "$VERN_OUTPUT_DIR/outputs.env" >>"$GITHUB_OUTPUT"
  fi
  echo "exit_code=$rc" >>"$GITHUB_OUTPUT"
fi

exit "$rc"
