#!/usr/bin/env bash
set -euo pipefail

profile="${1:?usage: run-agent-canary.sh <profile> <agent-id> [timeout-seconds]}"
agent_id="${2:?usage: run-agent-canary.sh <profile> <agent-id> [timeout-seconds]}"
timeout_seconds="${3:-1800}"
[[ "$profile" == "staging" ]] || { echo "canary only accepts the staging profile" >&2; exit 2; }
[[ "$timeout_seconds" =~ ^[0-9]+$ ]] || exit 2

description_file="$PWD/.handoff-canary-description.$$"
trap 'rm -f "$description_file"' EXIT
printf '%s\n' 'Synthetic staging canary. Reply with CANARY_OK and mark this issue done. Do not change repositories or external resources.' >"$description_file"
issue_json="$(multica --profile staging issue create --title "[staging-canary] $(date -u +%Y%m%dT%H%M%SZ)" --description-file "$description_file" --assignee-id "$agent_id" --status todo --output json)"
issue_id="$(jq -er '.id' <<<"$issue_json")"
deadline=$((SECONDS + timeout_seconds))
while (( SECONDS < deadline )); do
  issue="$(multica --profile staging issue get "$issue_id" --output json)"
  if [[ "$(jq -r '.status' <<<"$issue")" == "done" ]]; then
    multica --profile staging issue comment list "$issue_id" --recent 10 --output json | jq -e '.. | strings | select(contains("CANARY_OK"))' >/dev/null
    multica --profile staging issue usage "$issue_id" --output json | jq -e '.. | numbers | select(. > 0)' >/dev/null
    printf 'canary passed issue=%s\n' "$issue_id"
    exit 0
  fi
  sleep 10
done
echo "canary timed out issue=$issue_id" >&2
exit 1
