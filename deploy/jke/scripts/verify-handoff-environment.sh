#!/usr/bin/env bash
set -euo pipefail

base_url="${1:?usage: verify-handoff-environment.sh <base-url> [profile|default]}"
profile="${2:-default}"
base_url="${base_url%/}"

case "$profile" in
  staging)
    [[ "$base_url" == "https://handoff-staging.oxygent.org.cn" ]] || {
      echo "staging profile only accepts the staging HTTPS host" >&2
      exit 2
    }
    ;;
  default)
    [[ "$base_url" == "https://handoff.oxygent.org.cn" ]] || {
      echo "default profile only accepts the production HTTPS host" >&2
      exit 2
    }
    ;;
  *) echo "unknown profile: $profile" >&2; exit 2 ;;
esac

curl_json() {
  curl --fail --silent --show-error --retry 3 --retry-all-errors "$1"
}

health="$(curl_json "$base_url/healthz")"
jq -e '.status == "ok" and .checks.db == "ok" and .checks.migrations == "ok"' <<<"$health" >/dev/null

for path in /login /download; do
  code="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' --retry 3 --retry-all-errors "$base_url$path")"
  if [[ "$code" != "200" ]]; then
    echo "$path returned HTTP $code" >&2
    exit 1
  fi
done

# An unauthenticated WebSocket handshake must reach the WS handler. The dummy
# workspace ID avoids an early request-validation response; a 101 proves TLS,
# proxy upgrade routing, and the application endpoint are alive. curl times out
# after the upgrade because it is not a WebSocket client, so preserve the code.
ws_code="$(curl --http1.1 --silent --output /dev/null --write-out '%{http_code}' \
  --max-time 2 \
  -H 'Connection: Upgrade' -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' -H 'Sec-WebSocket-Key: SGFuZG9mZlNtb2tlVGVzdA==' \
  "$base_url/ws?workspace_id=00000000-0000-0000-0000-000000000000" || true)"
[[ "$ws_code" == "101" ]] || {
  echo "/ws upgrade probe returned HTTP $ws_code" >&2
  exit 1
}

multica_cmd=(multica)
if [[ "$profile" != "default" ]]; then
  multica_cmd+=(--profile "$profile")
fi

daemon_status="$("${multica_cmd[@]}" daemon status)"
grep -q 'running' <<<"$daemon_status"

runtimes="$("${multica_cmd[@]}" runtime list --output json)"
jq -e '
  ([.[] | select(.provider == "claude" and .status == "online")] | length) >= 1 and
  ([.[] | select(.provider == "codex" and .status == "online")] | length) >= 1
' <<<"$runtimes" >/dev/null

printf 'OK %s profile=%s\n' "$base_url" "$profile"
