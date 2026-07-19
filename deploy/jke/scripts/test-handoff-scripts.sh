#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
mkdir -p "$tmp_dir/bin" "$tmp_dir/work"

cat >"$tmp_dir/bin/multica" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
args="$*"
case "${MOCK_MODE:?}" in
  canary)
    case "$args" in
      *" issue create "*) printf '%s\n' '{"id":"123e4567-e89b-12d3-a456-426614174001"}' ;;
      *" issue get "*) printf '%s\n' '{"status":"done"}' ;;
      *" issue comment list "*) printf '%s\n' '[{"content":"CANARY_OK"}]' ;;
      *" issue usage "*)
        count=0
        [[ ! -f "$MOCK_USAGE_COUNT" ]] || count="$(cat "$MOCK_USAGE_COUNT")"
        count=$((count + 1))
        printf '%s\n' "$count" >"$MOCK_USAGE_COUNT"
        if (( count == 1 )); then
          exit 4
        fi
        printf '%s\n' '{"task_count":1,"total_output_tokens":42}'
        ;;
      *) echo "unexpected multica call: $args" >&2; exit 1 ;;
    esac
    ;;
  deploy)
    case "$args" in
      *" daemon status"*) echo "daemon running" ;;
      *" runtime list "*) printf '%s\n' '[{"provider":"claude","status":"online"},{"provider":"codex","status":"online"}]' ;;
      *) echo "unexpected multica call: $args" >&2; exit 1 ;;
    esac
    ;;
esac
EOF

cat >"$tmp_dir/bin/helm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$MOCK_HELM_LOG"
if [[ "${1:-}" == "history" ]]; then
  printf '%s\n' '[]'
fi
EOF

cat >"$tmp_dir/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
args="$*"
case "$args" in
  "config current-context") echo "approved-context" ;;
  *"clusters[0].cluster.server"*) echo "https://approved.example" ;;
  *"jsonpath={..namespace}"*) : ;;
  *" rollout status "*) : ;;
  *) echo "unexpected kubectl call: $args" >&2; exit 1 ;;
esac
EOF

cat >"$tmp_dir/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
args="$*"
printf '%s\n' "$args" >>"${MOCK_CURL_LOG:?}"
case "$args" in
  *"/healthz"*) printf '%s\n' '{"status":"ok","checks":{"db":"ok","migrations":"ok"}}' ;;
  *"/login"*|*"/download"*) printf '200' ;;
  *"/ws?workspace_id="*) printf '101'; exit 28 ;;
  *) echo "unexpected curl call: $args" >&2; exit 1 ;;
esac
EOF

chmod +x "$tmp_dir/bin/"*

usage_count="$tmp_dir/usage-count"
canary_output="$({
  cd "$tmp_dir/work"
  PATH="$tmp_dir/bin:$PATH" \
    MOCK_MODE=canary \
    MOCK_USAGE_COUNT="$usage_count" \
    HANDOFF_CANARY_POLL_INTERVAL_SECONDS=1 \
    "$repo_root/deploy/jke/scripts/run-agent-canary.sh" \
      staging 123e4567-e89b-12d3-a456-426614174000 5
})"
grep -q 'canary passed issue=123e4567-e89b-12d3-a456-426614174001' <<<"$canary_output"
[[ "$(cat "$usage_count")" == "2" ]]

digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
set +e
invalid_timeout_output="$(
  HANDOFF_HELM_TIMEOUT=invalid \
    "$repo_root/deploy/jke/scripts/deploy-handoff.sh" staging "$digest" "$digest" 2>&1
)"
invalid_timeout_rc=$?
set -e
[[ $invalid_timeout_rc -eq 2 ]]
grep -q 'HANDOFF_HELM_TIMEOUT must be' <<<"$invalid_timeout_output"

helm_log="$tmp_dir/helm.log"
curl_log="$tmp_dir/curl.log"
(
  cd "$repo_root"
  PATH="$tmp_dir/bin:$PATH" \
    MOCK_MODE=deploy \
    MOCK_HELM_LOG="$helm_log" \
    MOCK_CURL_LOG="$curl_log" \
    HANDOFF_EXPECTED_KUBE_CONTEXT=approved-context \
    HANDOFF_EXPECTED_KUBE_SERVER=https://approved.example \
    HANDOFF_HELM_TIMEOUT=90s \
    deploy/jke/scripts/deploy-handoff.sh staging "$digest" "$digest"
)
grep -q -- '--atomic --wait --timeout 90s' "$helm_log"
grep -q -- '--connect-timeout 5 --retry 18 --retry-delay 5 --retry-max-time 90 --retry-all-errors https://handoff-staging.oxygent.org.cn/healthz' "$curl_log"

echo "handoff-script-tests-ok"
