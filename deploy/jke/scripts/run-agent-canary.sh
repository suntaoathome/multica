#!/usr/bin/env bash
set -euo pipefail

profile="${1:?usage: run-agent-canary.sh <profile> <agent-id> [timeout-seconds]}"
agent_id="${2:?usage: run-agent-canary.sh <profile> <agent-id> [timeout-seconds]}"
timeout_seconds="${3:-1800}"
poll_interval_seconds="${HANDOFF_CANARY_POLL_INTERVAL_SECONDS:-10}"
[[ "$profile" == "staging" ]] || { echo "canary only accepts the staging profile" >&2; exit 2; }
[[ "$agent_id" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]] || {
  echo "agent-id must be a UUID" >&2
  exit 2
}
[[ "$timeout_seconds" =~ ^[1-9][0-9]*$ ]] || { echo "timeout-seconds must be a positive integer" >&2; exit 2; }
[[ "$poll_interval_seconds" =~ ^[1-9][0-9]*$ ]] || { echo "HANDOFF_CANARY_POLL_INTERVAL_SECONDS must be a positive integer" >&2; exit 2; }

description_file="$PWD/.handoff-canary-description.$$"
trap 'rm -f "$description_file"' EXIT
printf '%s\n' 'Synthetic staging canary. Reply with CANARY_OK and mark this issue done. Do not change repositories or external resources.' >"$description_file"
issue_json="$(multica --profile staging issue create --title "[staging-canary] $(date -u +%Y%m%dT%H%M%SZ)" --description-file "$description_file" --assignee-id "$agent_id" --status todo --output json)"
issue_id="$(jq -er '.id' <<<"$issue_json")"
deadline=$((SECONDS + timeout_seconds))
while (( SECONDS < deadline )); do
  issue=""
  if issue="$(multica --profile staging issue get "$issue_id" --output json 2>/dev/null)"; then
    status="$(jq -r '.status // empty' <<<"$issue" 2>/dev/null || true)"
    if [[ "$status" == "done" ]]; then
      # Comments and usage can become visible shortly after the issue status
      # flips to done. Keep polling within the original deadline so a healthy
      # canary is not reported as failed because of eventual consistency.
      if multica --profile staging issue comment list "$issue_id" --recent 10 --output json 2>/dev/null \
          | jq -e '.. | strings | select(contains("CANARY_OK"))' >/dev/null \
        && multica --profile staging issue usage "$issue_id" --output json 2>/dev/null \
          | jq -e '.. | numbers | select(. > 0)' >/dev/null; then
        printf 'canary passed issue=%s\n' "$issue_id"
        exit 0
      fi
    fi
  fi
  remaining=$((deadline - SECONDS))
  (( remaining > 0 )) || break
  sleep_seconds="$poll_interval_seconds"
  (( sleep_seconds <= remaining )) || sleep_seconds="$remaining"
  sleep "$sleep_seconds"
done
echo "canary timed out issue=$issue_id" >&2
exit 1
