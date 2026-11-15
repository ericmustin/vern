#!/usr/bin/env bash
# Idempotent bootstrap: component template + least-privilege API key for vern.
# Credentials are passed to curl through process-substituted config files, not argv.
set -euo pipefail

ES="${ES_URL:-http://elasticsearch:9200}"
KEY_FILE=/run/vern/api-key

# su_curl: curl as the elastic superuser.
su_curl() {
  curl -s --max-time 30 -K <(printf 'user = "elastic:%s"\n' "$ELASTIC_PASSWORD") "$@"
}
# su_code: print the HTTP code; 000 when curl itself failed (never aborts set -e).
su_code() { su_curl -o /dev/null -w '%{http_code}' "$@" || true; }

# Right after the cluster turns yellow the security index may not be ready:
# retry 503 and 000 (transport failure) a bounded number of times.
SEC_TRIES=30
announced=0
wait_note() {
  if [ "$announced" = 0 ]; then
    echo "vern-init: waiting for the security index"
    announced=1
  fi
  sleep 2
}

# 1. metrics-otel@custom: accept metric points up to 7 days old.
code=000
for _ in $(seq "$SEC_TRIES"); do
  code=$(su_code "$ES/_component_template/metrics-otel@custom")
  case "$code" in 503|000) wait_note ;; *) break ;; esac
done
if [ "$code" = 404 ]; then
  code=$(su_code -X PUT -H 'Content-Type: application/json' \
    "$ES/_component_template/metrics-otel@custom" \
    -d '{"template":{"settings":{"index.look_back_time":"7d","index.number_of_replicas":0}},"_meta":{"managed_by":"vern"}}')
  [ "$code" = 200 ] || { echo "vern-init: creating component template failed (HTTP $code)" >&2; exit 1; }
  echo "vern-init: created component template metrics-otel@custom"
elif [ "$code" != 200 ]; then
  echo "vern-init: checking component template failed (HTTP $code)" >&2; exit 1
fi

# 2. API key: keep the stored one while it still authenticates.
if [ -s "$KEY_FILE" ]; then
  code=$(curl -s --max-time 30 -o /dev/null -w '%{http_code}' \
    -K <(printf 'header = "Authorization: ApiKey %s"\n' "$(cat "$KEY_FILE")") \
    "$ES/_security/_authenticate")
  case "$code" in
    200) echo "vern-init: existing API key is valid; nothing to do"; exit 0 ;;
    401) echo "vern-init: stored API key rejected (HTTP 401); minting a new one" ;;
    *)   echo "vern-init: checking stored API key failed (HTTP $code)" >&2; exit 1 ;;
  esac
fi

# Invalidate orphaned keys named vern so they do not pile up.
code=000
for _ in $(seq "$SEC_TRIES"); do
  code=$(su_code -X DELETE -H 'Content-Type: application/json' "$ES/_security/api_key" -d '{"name":"vern"}')
  case "$code" in 503|000) wait_note ;; *) break ;; esac
done
# 404: nothing to invalidate.
[ "$code" = 200 ] || [ "$code" = 404 ] || { echo "vern-init: invalidating old API keys failed (HTTP $code)" >&2; exit 1; }

create_key() {
  su_curl -w '\n%{http_code}' -X POST -H 'Content-Type: application/json' "$ES/_security/api_key" -d '{
  "name": "vern",
  "role_descriptors": {
    "vern_runs": {
      "cluster": ["monitor"],
      "indices": [{
        "names": ["traces-*.otel-vern*","logs-*.otel-vern*","metrics-*.otel-vern*"],
        "privileges": ["create_doc","auto_configure","read","view_index_metadata","manage","read_failure_store"]
      }]
    }
  }
}' || true
}

resp=""
code=000
for _ in $(seq "$SEC_TRIES"); do
  out=$(create_key)
  code=$(printf '%s' "$out" | tail -n1)
  resp=$(printf '%s' "$out" | sed '$d')
  case "$code" in 503|000) wait_note ;; *) break ;; esac
done
[ "$code" = 200 ] || { echo "vern-init: creating API key failed (HTTP $code)" >&2; exit 1; }

encoded=$(printf '%s' "$resp" | sed -n 's/.*"encoded":"\([^"]*\)".*/\1/p')
[ -n "$encoded" ] || { echo "vern-init: no encoded key in response" >&2; exit 1; }

umask 077
printf '%s' "$encoded" > "$KEY_FILE.tmp"
chown 65532:65532 "$KEY_FILE.tmp"
chmod 0400 "$KEY_FILE.tmp"
mv -f "$KEY_FILE.tmp" "$KEY_FILE"
echo "vern-init: wrote API key to $KEY_FILE"
