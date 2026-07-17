#!/usr/bin/env bash
set -euo pipefail

base_url="${1:?usage: verify-handoff-environment.sh <base-url> [profile|default]}"
profile="${2:-default}"
base_url="${base_url%/}"

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
