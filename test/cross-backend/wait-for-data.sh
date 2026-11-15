#!/usr/bin/env bash
# Poll each backend until its traces store holds at least WAIT_MIN_DOCS rows.
# Containers being "healthy" only means the service is up — this waits for the
# collector to have actually delivered telemetry.
set -euo pipefail

TIMEOUT="${WAIT_TIMEOUT:-180}"
MIN_DOCS="${WAIT_MIN_DOCS:-50}"
CH_AUTH="${CH_AUTH:-default:vern-dev}"
INTERVAL=3

es_count() {
  curl -sf "http://localhost:9200/traces-*/_count" 2>/dev/null \
    | sed -n 's/.*"count":\([0-9]*\).*/\1/p' || true
}

ch_count() {
  curl -sf -u "$CH_AUTH" "http://localhost:8123/" \
    --data-binary 'SELECT count() FROM otel.otel_traces' 2>/dev/null \
    | tr -d '[:space:]' || true
}

os_count() {
  curl -sf "http://localhost:9201/ss4o_traces-*/_count" 2>/dev/null \
    | sed -n 's/.*"count":\([0-9]*\).*/\1/p' || true
}

deadline=$(( $(date +%s) + TIMEOUT ))
while :; do
  es=$(es_count);  es=${es:-0}
  ch=$(ch_count);  ch=${ch:-0}
  os=$(os_count);  os=${os:-0}

  printf '[wait-for-data] elasticsearch=%s clickhouse=%s opensearch=%s (need >=%s)\n' \
    "$es" "$ch" "$os" "$MIN_DOCS"

  if [ "${es:-0}" -ge "$MIN_DOCS" ] && \
     [ "${ch:-0}" -ge "$MIN_DOCS" ] && \
     [ "${os:-0}" -ge "$MIN_DOCS" ]; then
    echo "[wait-for-data] all backends have data"
    exit 0
  fi

  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "[wait-for-data] TIMEOUT after ${TIMEOUT}s" >&2
    exit 1
  fi
  sleep "$INTERVAL"
done
