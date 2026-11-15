#!/usr/bin/env bash
# Tests for ci/vern-ci.sh using a stub VERN_BIN and stub docker.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT=$(cd "$HERE/.." && pwd)/vern-ci.sh
BASH_BIN=$(command -v bash)
ROOT=$(mktemp -d)
trap 'rm -rf "$ROOT"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# Per-test sandbox: $T is the working directory; stubs record into $T/rec.
new_sandbox() {
  T=$(mktemp -d "$ROOT/t.XXXXXX")
  mkdir -p "$T/bin" "$T/tools" "$T/rec"
  cat >"$T/bin/vern" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$STUB_REC/args"
printf '%s\n' "${OTEL_RESOURCE_ATTRIBUTES-}" >"$STUB_REC/otel_attrs"
printf '%s\n' "${VERN_ELASTICSEARCH_URL-}" >"$STUB_REC/es_url"
printf '%s\n' "${OTEL_EXPORTER_OTLP_HEADERS-<unset>}" >"$STUB_REC/otlp_headers"
printf '%s\n' "${VERN_OTLP_HEADERS-<unset>}" >"$STUB_REC/vern_otlp_headers"
touch "$STUB_REC/called"
if [ -n "${STUB_SLEEP:-}" ]; then
  trap 'echo TERM >"$STUB_REC/got_signal"; exit 143' TERM INT
  touch "$STUB_REC/started"
  sleep 30 &
  wait $!
fi
if [ -n "${STUB_WRITE:-}" ]; then
  mkdir -p vern-report
  echo "# stub report" >vern-report/report.md
  echo "score=87.5" >vern-report/outputs.env
fi
exit "${STUB_EXIT:-0}"
STUB
  chmod +x "$T/bin/vern"
  # Minimal tool dir so a PATH without docker still resolves coreutils.
  local t p
  for t in bash env mkdir cat dirname rm sed tr head touch sleep; do
    p=$(command -v "$t" || true)
    [ -n "$p" ] && ln -sf "$p" "$T/tools/$t"
  done
  RC=0
}

# run_script [VAR=value ...]: runs vern-ci.sh in $T with a clean-ish env.
# PATH has the tools dir only (no docker) unless a test adds $T/dockerbin.
run_script() {
  (
    cd "$T"
    env -i HOME="$T" PATH="${TEST_PATH:-$T/tools}" STUB_REC="$T/rec" \
      VERN_BIN="$T/bin/vern" "$@" "$BASH_BIN" "$SCRIPT" \
      >"$T/stdout" 2>"$T/stderr"
  ) || { RC=$?; cat "$T/stderr" >&2; }
}

make_docker_stub() {
  mkdir -p "$T/dockerbin"
  cat >"$T/dockerbin/docker" <<'STUB'
#!/usr/bin/env bash
echo "$*" >>"$STUB_REC/docker_calls"
case "$1" in
  run) echo "fakecontainerid" ;;
  port)
    n=$(cat "$STUB_REC/port_n" 2>/dev/null || echo 0)
    echo $((n + 1)) >"$STUB_REC/port_n"
    if [ "$n" -lt "${STUB_PORT_EMPTY:-0}" ]; then exit 1; fi
    printf '127.0.0.1:49999\n[::]:49999\n' ;;
  rm) ;;
esac
exit 0
STUB
  chmod +x "$T/dockerbin/docker"
}

test_no_match() {
  new_sandbox
  make_docker_stub
  TEST_PATH="$T/dockerbin:$T/tools" run_script 'VERN_FILES=nope/*.jsonl'
  [ "$RC" = 1 ] || fail "exit $RC, want 1"
  grep -qF 'no files matched: nope/*.jsonl' "$T/stderr" || fail "stderr: $(cat "$T/stderr")"
  [ ! -e "$T/rec/called" ] || fail "vern was called"
  [ ! -e "$T/rec/docker_calls" ] || fail "docker was called"
}

test_no_docker_no_url() {
  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl'
  [ "$RC" = 1 ] || fail "exit $RC, want 1"
  grep -q VERN_ELASTICSEARCH_URL "$T/stderr" || fail "stderr lacks URL var"
  grep -q docker "$T/stderr" || fail "stderr lacks docker"
  [ ! -e "$T/rec/called" ] || fail "vern was called"
}

test_files_required() {
  new_sandbox
  run_script
  [ "$RC" = 1 ] || fail "exit $RC, want 1"
  grep -q VERN_FILES "$T/stderr" || fail "stderr lacks VERN_FILES"
}

test_reuses_url() {
  new_sandbox
  make_docker_stub
  # A docker that fails when called.
  cat >"$T/dockerbin/docker" <<'STUB'
#!/usr/bin/env bash
echo called >>"$STUB_REC/docker_calls"
exit 1
STUB
  : >"$T/a.jsonl"
  TEST_PATH="$T/dockerbin:$T/tools" run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200'
  [ "$RC" = 0 ] || fail "exit $RC: $(cat "$T/stderr")"
  [ ! -e "$T/rec/docker_calls" ] || fail "docker was called"
  want=$(printf '%s\n' score --output-json vern-report/report.json --output-md vern-report/report.md --outputs-file vern-report/outputs.env --ready-timeout 180s -- a.jsonl)
  [ "$(cat "$T/rec/args")" = "$want" ] || fail "argv: $(cat "$T/rec/args")"
  [ "$(cat "$T/rec/es_url")" = http://es.example:9200 ] || fail "es url not passed through"
}

test_min_score_and_setup_template_opt_in() {
  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' 'VERN_MIN_SCORE=70' 'VERN_SETUP_TEMPLATE=true'
  [ "$RC" = 0 ] || fail "exit $RC"
  grep -qx -- '--min-score' "$T/rec/args" || fail "no --min-score"
  grep -qx 70 "$T/rec/args" || fail "no 70"
  grep -qx -- '--setup-template' "$T/rec/args" || fail "no --setup-template"
}

test_setup_template_when_docker_started() {
  new_sandbox
  make_docker_stub
  : >"$T/a.jsonl"
  TEST_PATH="$T/dockerbin:$T/tools" run_script 'VERN_FILES=a.jsonl'
  [ "$RC" = 0 ] || fail "exit $RC: $(cat "$T/stderr")"
  grep -qx -- '--setup-template' "$T/rec/args" || fail "no --setup-template"
  [ "$(cat "$T/rec/es_url")" = http://127.0.0.1:49999 ] || fail "es url: $(cat "$T/rec/es_url")"
  grep -q '^run -d --rm --name vern-ci-es-' "$T/rec/docker_calls" || fail "no docker run"
  grep -q -- '-p 127.0.0.1::9200' "$T/rec/docker_calls" || fail "no loopback publish"
  grep -q '^port vern-ci-es-' "$T/rec/docker_calls" || fail "no docker port"
  grep -q '^rm -f vern-ci-es-' "$T/rec/docker_calls" || fail "container not removed"
}

test_docker_cleanup_on_failure() {
  new_sandbox
  make_docker_stub
  : >"$T/a.jsonl"
  TEST_PATH="$T/dockerbin:$T/tools" run_script 'VERN_FILES=a.jsonl' STUB_EXIT=1
  [ "$RC" = 1 ] || fail "exit $RC, want 1"
  grep -q '^rm -f vern-ci-es-' "$T/rec/docker_calls" || fail "container not removed"
}

test_exit_code_passthrough() {
  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' STUB_EXIT=2
  [ "$RC" = 2 ] || fail "exit $RC, want 2"
}

test_github_summary_and_outputs() {
  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' STUB_WRITE=1 \
    "GITHUB_STEP_SUMMARY=$T/summary.md" "GITHUB_OUTPUT=$T/gh_output"
  [ "$RC" = 0 ] || fail "exit $RC"
  grep -qF '# stub report' "$T/summary.md" || fail "summary missing report"
  grep -qx 'score=87.5' "$T/gh_output" || fail "output missing score"
}

test_missing_outputs_on_failure() {
  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' STUB_EXIT=1 \
    "GITHUB_STEP_SUMMARY=$T/summary.md" "GITHUB_OUTPUT=$T/gh_output"
  [ "$RC" = 1 ] || fail "exit $RC, want 1"
  grep -qF 'vern score exited 1: see the job log' "$T/summary.md" || fail "summary: $(cat "$T/summary.md" 2>&1)"
  [ "$(cat "$T/gh_output")" = exit_code=1 ] || fail "outputs should hold only exit_code: $(cat "$T/gh_output")"
}

test_summary_reason_with_report() {
  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' STUB_WRITE=1 STUB_EXIT=2 \
    VERN_MIN_SCORE=70 "GITHUB_STEP_SUMMARY=$T/summary.md" "GITHUB_OUTPUT=$T/gh_output"
  [ "$RC" = 2 ] || fail "exit $RC, want 2"
  grep -qF '# stub report' "$T/summary.md" || fail "summary missing report"
  grep -qF 'vern score exited 2: score below --min-score 70' "$T/summary.md" || fail "no gate reason: $(cat "$T/summary.md")"
  grep -qx 'score=87.5' "$T/gh_output" || fail "output missing score"
  grep -qx 'exit_code=2' "$T/gh_output" || fail "no exit_code=2: $(cat "$T/gh_output")"

  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' STUB_WRITE=1 STUB_EXIT=1 \
    "GITHUB_STEP_SUMMARY=$T/summary.md" "GITHUB_OUTPUT=$T/gh_output"
  [ "$RC" = 1 ] || fail "exit $RC, want 1"
  grep -qF '# stub report' "$T/summary.md" || fail "summary missing report"
  grep -qF 'vern score exited 1: see the job log' "$T/summary.md" || fail "no error reason: $(cat "$T/summary.md")"
  grep -qx 'exit_code=1' "$T/gh_output" || fail "no exit_code=1"
}

test_die_writes_summary() {
  new_sandbox
  run_script 'VERN_FILES=nope/*.jsonl' "GITHUB_STEP_SUMMARY=$T/summary.md" "GITHUB_OUTPUT=$T/gh_output"
  [ "$RC" = 1 ] || fail "exit $RC, want 1"
  grep -qF 'vern-ci: no files matched: nope/*.jsonl' "$T/summary.md" || fail "summary: $(cat "$T/summary.md" 2>&1)"
  grep -qx 'exit_code=1' "$T/gh_output" || fail "no exit_code=1"

  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' "GITHUB_STEP_SUMMARY=$T/summary.md"
  [ "$RC" = 1 ] || fail "exit $RC, want 1"
  grep -qF 'vern-ci: set VERN_ELASTICSEARCH_URL' "$T/summary.md" || fail "summary: $(cat "$T/summary.md" 2>&1)"
}

test_success_writes_exit_code_0() {
  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' STUB_WRITE=1 \
    "GITHUB_STEP_SUMMARY=$T/summary.md" "GITHUB_OUTPUT=$T/gh_output"
  [ "$RC" = 0 ] || fail "exit $RC"
  grep -qx 'exit_code=0' "$T/gh_output" || fail "no exit_code=0"
  ! grep -q 'exited' "$T/summary.md" || fail "failure reason on success"
}

test_otlp_endpoint_flag() {
  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' \
    VERN_OTLP_ENDPOINT=http://collector.example:4318 \
    OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=http://job.example:4318/v1/metrics
  [ "$RC" = 0 ] || fail "exit $RC"
  want=$(printf '%s\n' --otlp-endpoint http://collector.example:4318)
  case "$(cat "$T/rec/args")" in *"$want"*) ;; *) fail "argv lacks --otlp-endpoint: $(cat "$T/rec/args")" ;; esac
}

test_otlp_headers_inherited_when_input_empty() {
  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' \
    OTEL_EXPORTER_OTLP_HEADERS=authorization=job-level VERN_OTLP_HEADERS=
  [ "$RC" = 0 ] || fail "exit $RC"
  [ "$(cat "$T/rec/otlp_headers")" = authorization=job-level ] || fail "job-level headers lost: $(cat "$T/rec/otlp_headers")"
  ! grep -q job-level "$T/rec/args" || fail "headers on argv"
}

test_otlp_headers_input_wins() {
  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' \
    OTEL_EXPORTER_OTLP_HEADERS=authorization=job-level VERN_OTLP_HEADERS=authorization=from-input
  [ "$RC" = 0 ] || fail "exit $RC"
  [ "$(cat "$T/rec/otlp_headers")" = authorization=from-input ] || fail "input headers not used: $(cat "$T/rec/otlp_headers")"
  [ "$(cat "$T/rec/vern_otlp_headers")" = '<unset>' ] || fail "VERN_OTLP_HEADERS leaked to vern"
  ! grep -q from-input "$T/rec/args" || fail "headers on argv"

  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200'
  [ "$(cat "$T/rec/otlp_headers")" = '<unset>' ] || fail "headers set from nothing: $(cat "$T/rec/otlp_headers")"
}

test_ci_resource_attributes() {
  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' \
    OTEL_RESOURCE_ATTRIBUTES=team=x GITHUB_ACTIONS=true GITHUB_SERVER_URL=https://github.com \
    GITHUB_REPOSITORY=o/r GITHUB_SHA=abc123 GITHUB_REF_NAME=main 'GITHUB_WORKFLOW=a,b=c' GITHUB_RUN_ID=
  [ "$RC" = 0 ] || fail "exit $RC"
  got=$(cat "$T/rec/otel_attrs")
  case "$got" in *vcs.ref.head.revision=abc123*) ;; *) fail "no revision: $got" ;; esac
  case "$got" in *,team=x) ;; *) fail "user attrs must come last (so they win): $got" ;; esac
  case "$got" in *vcs.repository.url.full=https://github.com/o/r*) ;; *) fail "no repo url: $got" ;; esac
  case "$got" in *'cicd.pipeline.name=a%2Cb%3Dc'*) ;; *) fail "value not percent-encoded: $got" ;; esac
  case "$got" in *cicd.pipeline.run.id*) fail "empty source var not skipped: $got" ;; esac
}

test_gitlab_resource_attributes() {
  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' \
    GITLAB_CI=true CI_PROJECT_URL=https://gitlab.example/g/p CI_COMMIT_SHA=def456 \
    CI_COMMIT_REF_NAME=main CI_PROJECT_PATH=g/p CI_PIPELINE_ID=42
  [ "$RC" = 0 ] || fail "exit $RC"
  got=$(cat "$T/rec/otel_attrs")
  case "$got" in *vcs.ref.head.revision=def456*) ;; *) fail "no revision: $got" ;; esac
  case "$got" in *cicd.pipeline.name=g/p*) ;; *) fail "no pipeline name fallback: $got" ;; esac
}

test_user_resource_attributes_win() {
  new_sandbox
  : >"$T/a.jsonl"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' \
    OTEL_RESOURCE_ATTRIBUTES=vcs.ref.head.name=release-1 GITHUB_ACTIONS=true GITHUB_REF_NAME=main GITHUB_SHA=abc123
  [ "$RC" = 0 ] || fail "exit $RC"
  got=$(cat "$T/rec/otel_attrs")
  # Later entries win in OTEL_RESOURCE_ATTRIBUTES, so the user's value must follow the CI one.
  [ "$got" = vcs.ref.head.revision=abc123,vcs.ref.head.name=main,vcs.ref.head.name=release-1 ] || fail "order: $got"
}

test_glob_expansion() {
  new_sandbox
  mkdir "$T/d"
  : >"$T/d/1.jsonl"
  : >"$T/d/2.jsonl"
  run_script 'VERN_FILES=d/*.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200'
  [ "$RC" = 0 ] || fail "exit $RC"
  grep -qx d/1.jsonl "$T/rec/args" || fail "1.jsonl not expanded"
  grep -qx d/2.jsonl "$T/rec/args" || fail "2.jsonl not expanded"
}

test_multiline_and_tab_files() {
  new_sandbox
  mkdir "$T/d"
  : >"$T/a.jsonl"
  : >"$T/b.jsonl"
  : >"$T/d/c.jsonl"
  run_script "VERN_FILES=$(printf 'a.jsonl\nb.jsonl\td/*.jsonl\n')" 'VERN_ELASTICSEARCH_URL=http://es.example:9200'
  [ "$RC" = 0 ] || fail "exit $RC"
  for f in a.jsonl b.jsonl d/c.jsonl; do
    grep -qx "$f" "$T/rec/args" || fail "$f missing from argv: $(cat "$T/rec/args")"
  done
}

test_stale_report_removed() {
  new_sandbox
  : >"$T/a.jsonl"
  mkdir "$T/vern-report"
  echo "# OLD report" >"$T/vern-report/report.md"
  echo "score=1" >"$T/vern-report/outputs.env"
  run_script 'VERN_FILES=a.jsonl' 'VERN_ELASTICSEARCH_URL=http://es.example:9200' STUB_EXIT=1 \
    "GITHUB_STEP_SUMMARY=$T/summary.md" "GITHUB_OUTPUT=$T/gh_output"
  [ "$RC" = 1 ] || fail "exit $RC, want 1"
  ! grep -q OLD "$T/summary.md" || fail "stale report published"
  grep -qF 'vern score exited 1' "$T/summary.md" || fail "no failure line"
  ! grep -q '^score=' "$T/gh_output" || fail "stale outputs published"
}

test_docker_port_retry() {
  new_sandbox
  make_docker_stub
  : >"$T/a.jsonl"
  TEST_PATH="$T/dockerbin:$T/tools" run_script 'VERN_FILES=a.jsonl' STUB_PORT_EMPTY=1
  [ "$RC" = 0 ] || fail "exit $RC: $(cat "$T/stderr")"
  [ "$(cat "$T/rec/es_url")" = http://127.0.0.1:49999 ] || fail "es url: $(cat "$T/rec/es_url")"
  [ "$(cat "$T/rec/port_n")" = 2 ] || fail "port calls: $(cat "$T/rec/port_n")"
}

test_docker_port_gives_up() {
  new_sandbox
  make_docker_stub
  : >"$T/a.jsonl"
  TEST_PATH="$T/dockerbin:$T/tools" run_script 'VERN_FILES=a.jsonl' STUB_PORT_EMPTY=99
  [ "$RC" = 1 ] || fail "exit $RC, want 1"
  grep -q '^rm -f vern-ci-es-' "$T/rec/docker_calls" || fail "container not removed"
}

test_term_forwarded_and_cleanup() {
  new_sandbox
  make_docker_stub
  : >"$T/a.jsonl"
  (
    cd "$T"
    exec env -i HOME="$T" PATH="$T/dockerbin:$T/tools" STUB_REC="$T/rec" \
      VERN_BIN="$T/bin/vern" VERN_FILES=a.jsonl STUB_SLEEP=1 "$BASH_BIN" "$SCRIPT" \
      >"$T/stdout" 2>"$T/stderr"
  ) &
  local pid=$!
  for _ in $(seq 1 100); do
    [ -e "$T/rec/started" ] && break
    sleep 0.1
  done
  [ -e "$T/rec/started" ] || fail "stub never started"
  kill -TERM "$pid"
  wait "$pid" || RC=$?
  [ "$RC" = 143 ] || fail "exit $RC, want 143"
  [ -e "$T/rec/got_signal" ] || fail "stub did not get the signal"
  grep -q '^rm -f vern-ci-es-' "$T/rec/docker_calls" || fail "container not removed"
}

tests=$(declare -F | sed -n 's/^declare -f \(test_.*\)$/\1/p')
for t in $tests; do
  echo "RUN  $t"
  "$t"
  echo "PASS $t"
done
echo "all tests passed"
