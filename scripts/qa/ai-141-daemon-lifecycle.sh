#!/usr/bin/env bash
set -euo pipefail

# AI-141 process-level smoke: disposable PostgreSQL, real server, real daemon,
# and a credential-free Codex-compatible process. Setup and lifecycle actions
# go through public HTTP/CLI surfaces; SQL is used only for read-only evidence.

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
artifact_dir=${AI141_ARTIFACT_DIR:-"$repo_root/artifacts/ai-141"}
mkdir -p "$artifact_dir"

run_id="$(date +%s)-$$"
container="ai141-postgres-${run_id}"
server_pid=""
daemon_pid=""
cleanup() {
  if [[ -n "$daemon_pid" ]]; then kill "$daemon_pid" 2>/dev/null || true; wait "$daemon_pid" 2>/dev/null || true; fi
  if [[ -n "$server_pid" ]]; then kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; fi
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$artifact_dir/home" "$artifact_dir/codex-home"
}
trap cleanup EXIT
trap 'rc=$?; printf "FAIL line=%s rc=%s\n" "$LINENO" "$rc" >"$artifact_dir/result.log"; exit "$rc"' ERR

docker run -d --name "$container" -e POSTGRES_PASSWORD=multica -e POSTGRES_USER=multica -e POSTGRES_DB=multica -p 127.0.0.1::5432 pgvector/pgvector:pg17 >"$artifact_dir/postgres.cid"
pg_port=$(docker inspect -f '{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}' "$container")
db_url="postgres://multica:multica@127.0.0.1:${pg_port}/multica?sslmode=disable"
for _ in $(seq 1 60); do docker exec "$container" pg_isready -U multica >/dev/null 2>&1 && break; sleep 1; done

export GOCACHE=${GOCACHE:-/tmp/ai141-go-cache}
export DATABASE_URL=$db_url
cd "$repo_root/server"
go run ./cmd/migrate up >"$artifact_dir/migrate.log" 2>&1
if [[ "${AI141_SKIP_BUILD:-0}" != 1 ]]; then
  go build -o "$artifact_dir/multica-server" ./cmd/server
  if ! go build -o "$artifact_dir/multica" ./cmd/multica 2>"$artifact_dir/multica-build.log"; then
  # Some hermetic runners mount the shared module cache read-only while the
  # candidate branch adds goldmark. Fetch that one pinned module over Git and
  # use a disposable modfile; never mutate go.mod/go.sum or the shared cache.
  tool_deps=$(mktemp -d)
  git clone --quiet --depth 1 --branch v1.8.4 git@github.com:yuin/goldmark.git "$tool_deps/goldmark"
  cp go.mod "$tool_deps/ai141.mod"
  cp go.sum "$tool_deps/ai141.sum"
  go mod edit -modfile="$tool_deps/ai141.mod" -replace="github.com/yuin/goldmark=$tool_deps/goldmark"
    go build -modfile="$tool_deps/ai141.mod" -o "$artifact_dir/multica" ./cmd/multica
  fi
  go build -o "$artifact_dir/daemon-test-agent" ./cmd/daemon-test-agent
fi
for binary in multica-server multica daemon-test-agent; do
  [[ -x "$artifact_dir/$binary" ]] || { echo "missing prebuilt $artifact_dir/$binary" >&2; exit 1; }
done

server_port=${AI141_SERVER_PORT:-$((20000 + RANDOM % 20000))}
export PORT=$server_port APP_ENV=development MULTICA_DEV_VERIFICATION_CODE=141141 JWT_SECRET=ai141-disposable-only
"$artifact_dir/multica-server" >"$artifact_dir/server.log" 2>&1 & server_pid=$!
base_url="http://127.0.0.1:${server_port}"
for _ in $(seq 1 60); do curl -fsS "$base_url/health" >/dev/null 2>&1 && break; sleep 1; done

email="ai141-${run_id}@example.invalid"
curl -fsS -H 'Content-Type: application/json' -d "{\"email\":\"$email\"}" "$base_url/auth/send-code" >"$artifact_dir/send-code.json"
verify_response=$(curl -fsS -H 'Content-Type: application/json' -d "{\"email\":\"$email\",\"code\":\"141141\"}" "$base_url/auth/verify-code")
jwt=$(jq -r .token <<<"$verify_response")
jq 'del(.token)' <<<"$verify_response" >"$artifact_dir/verify-code.json"
pat=$(curl -fsS -H "Authorization: Bearer $jwt" -H 'Content-Type: application/json' -d '{"name":"AI-141 disposable harness","expires_in_days":1}' "$base_url/api/tokens/" | jq -r .token)

# A Multica-managed invocation carries its own mat_ task credential. It must
# never leak into the nested disposable CLI; that CLI authenticates only with
# the PAT minted by the isolated server above.
unset MULTICA_TOKEN MULTICA_WORKSPACE_ID MULTICA_AGENT_ID MULTICA_AGENT_NAME MULTICA_TASK_ID MULTICA_TASK_SLOT MULTICA_DAEMON_PORT
export MULTICA_SERVER_URL=$base_url MULTICA_APP_URL=$base_url CODEX_HOME="$artifact_dir/codex-home" HOME="$artifact_dir/home"
mkdir -p "$HOME"
cli_cwd=$(mktemp -d)
profile="ai141-${run_id}"
mcli() { (cd "$cli_cwd" && "$artifact_dir/multica" --profile "$profile" "$@"); }
export MULTICA_TOKEN=$jwt
workspace_json=$(mcli workspace create --name AI-141 --slug "ai141-${run_id}" --issue-prefix AIE --output json 2>"$artifact_dir/workspace-create.log")
workspace_id=$(jq -r .id <<<"$workspace_json")
mcli login --token "$pat" >"$artifact_dir/login.log" 2>&1
export MULTICA_TOKEN=$pat
mcli workspace switch "$workspace_id" >/dev/null

control_file="$artifact_dir/agent-mode"
printf 'complete\n' >"$control_file"
export MULTICA_CODEX_PATH="$artifact_dir/daemon-test-agent"
(cd "$cli_cwd" && "$artifact_dir/multica" --profile "$profile" daemon start --foreground --daemon-id "ai141-${run_id}" --poll-interval 250ms --heartbeat-interval 250ms --max-concurrent-tasks 6 --no-auto-update) >"$artifact_dir/daemon-first.log" 2>&1 & daemon_pid=$!
for _ in $(seq 1 60); do
  runtime_json=$(mcli runtime list --output json 2>/dev/null || true)
  printf '%s\n' "${runtime_json:-[]}" >"$artifact_dir/runtime-list.json"
  runtime_id=$(jq -r '[.[] | select(.provider=="codex")][0].id // empty' <<<"${runtime_json:-[]}")
  [[ -n "$runtime_id" ]] && break
  sleep 1
done
[[ -n "${runtime_id:-}" ]] || { echo "runtime did not register" >&2; exit 1; }
agent_json=$(mcli agent create --name AI-141-test-agent --runtime-id "$runtime_id" --custom-args "[\"--multica-test-control=$control_file\"]" --max-concurrent-tasks 6 --output json 2>"$artifact_dir/agent-create.log")
agent_id=$(jq -r .id <<<"$agent_json")
agent2_json=$(mcli agent create --name AI-141-reassign-agent --runtime-id "$runtime_id" --custom-args "[\"--multica-test-control=$control_file\"]" --max-concurrent-tasks 6 --output json 2>"$artifact_dir/agent2-create.log")
agent2_id=$(jq -r .id <<<"$agent2_json")
for _ in $(seq 1 30); do
  intro_active=$(docker exec "$container" psql -U multica -Atc "SELECT count(*) FROM agent_task_queue WHERE agent_id IN ('${agent_id}','${agent2_id}') AND issue_id IS NULL AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')")
  [[ "$intro_active" -eq 0 ]] && break
  sleep 1
done
[[ "$intro_active" -eq 0 ]] || { echo "agent intro tasks did not drain" >&2; exit 1; }
printf 'block\n' >"$control_file"
issue_ids=()
for slot in $(seq 1 6); do
  issue_json=$(mcli issue create --title "AI-141 lifecycle slot ${slot}" --status todo --assignee-id "$agent_id" --allow-duplicate --output json 2>>"$artifact_dir/issue-create.log")
  issue_ids+=("$(jq -r .id <<<"$issue_json")")
done
quoted_ids=""
for id in "${issue_ids[@]}"; do quoted_ids+="${quoted_ids:+,}'${id}'"; done
restart_issue_id=${issue_ids[5]}

for _ in $(seq 1 60); do
  running_slots=$(docker exec "$container" psql -U multica -Atc "SELECT count(*) FROM agent_task_queue WHERE issue_id IN (${quoted_ids}) AND status='running'")
  [[ "$running_slots" -eq 6 ]] && break
  sleep 1
done
[[ "$running_slots" -eq 6 ]] || { echo "six slots never reached running (got $running_slots)" >&2; exit 1; }

# Preempt/cancel one real running task via its public endpoint.
cancel_task=$(docker exec "$container" psql -U multica -Atc "SELECT id FROM agent_task_queue WHERE issue_id='${issue_ids[0]}' AND status='running' ORDER BY created_at DESC LIMIT 1")
mcli issue cancel-task "$cancel_task" --issue "${issue_ids[0]}" --output json >"$artifact_dir/cancel.json"

# Review and external-wait fences: cancel the active executions through the
# API, declare the waits through issue status/metadata APIs, then prove the
# server patrol does not resurrect assignment controllers.
review_task=$(docker exec "$container" psql -U multica -Atc "SELECT id FROM agent_task_queue WHERE issue_id='${issue_ids[1]}' AND status='running' ORDER BY created_at DESC LIMIT 1")
mcli issue cancel-task "$review_task" --issue "${issue_ids[1]}" --output json >/dev/null
mcli issue status "${issue_ids[1]}" in_review >/dev/null
external_task=$(docker exec "$container" psql -U multica -Atc "SELECT id FROM agent_task_queue WHERE issue_id='${issue_ids[2]}' AND status='running' ORDER BY created_at DESC LIMIT 1")
mcli issue cancel-task "$external_task" --issue "${issue_ids[2]}" --output json >/dev/null
mcli issue metadata set "${issue_ids[2]}" --key waiting_on --value "external approval" --type string >/dev/null
mcli issue status "${issue_ids[2]}" blocked >/dev/null

# Race reassign and comment triggers against live controllers. Both operations
# use server APIs; the unique issue_assignment fence must remain intact.
mcli issue update "${issue_ids[3]}" --assignee-id "$agent2_id" --output json >"$artifact_dir/reassign.json" & reassign_pid=$!
printf 'AI-141 concurrent comment trigger [@test-agent](mention://agent/%s)\n' "$agent_id" >"$cli_cwd/comment.md"
mcli issue comment add "${issue_ids[4]}" --content-file ./comment.md --output json >"$artifact_dir/comment.json" & comment_pid=$!
wait "$reassign_pid"; wait "$comment_pid"

# Prove task-side comment delivery, not merely persistence of the comment row.
# The daemon records the exact IDs embedded into the task prompt atomically in
# delivered_comment_ids.
comment_id=$(jq -r .id "$artifact_dir/comment.json")
for _ in $(seq 1 60); do
  delivered_comment=$(docker exec "$container" psql -U multica -Atc "SELECT count(*) FROM agent_task_queue WHERE issue_id='${issue_ids[4]}' AND '${comment_id}'::uuid = ANY(delivered_comment_ids)")
  [[ "$delivered_comment" -ge 1 ]] && break
  sleep 1
done
[[ "$delivered_comment" -ge 1 ]] || { echo "comment $comment_id was not delivered task-side" >&2; exit 1; }

# Cross the real 30-second server sweeper boundary and require evidence that
# ready-recovery actually attempted a pass. The cancelled todo issue above is
# the missing-controller candidate; review/external-wait issues are fences.
for _ in $(seq 1 45); do
  rg -q 'ready recovery coordinator: pass complete' "$artifact_dir/server.log" && break
  sleep 1
done
rg -q 'ready recovery coordinator: pass complete' "$artifact_dir/server.log" || { echo "ready-recovery patrol did not run" >&2; exit 1; }
declared_wait_active=$(docker exec "$container" psql -U multica -Atc "SELECT count(*) FROM agent_task_queue WHERE issue_id IN ('${issue_ids[1]}','${issue_ids[2]}') AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')")
[[ "$declared_wait_active" -eq 0 ]] || { echo "declared wait/review fence resurrected $declared_wait_active controllers" >&2; exit 1; }

kill -KILL "$daemon_pid"; wait "$daemon_pid" 2>/dev/null || true; daemon_pid=""
restart_at=$(date +%s)
printf 'complete\n' >"$control_file"
(cd "$cli_cwd" && "$artifact_dir/multica" --profile "$profile" daemon start --foreground --daemon-id "ai141-${run_id}" --poll-interval 250ms --heartbeat-interval 250ms --max-concurrent-tasks 6 --no-auto-update) >"$artifact_dir/daemon-restart.log" 2>&1 & daemon_pid=$!
for _ in $(seq 1 60); do
  active=$(docker exec "$container" psql -U multica -Atc "SELECT count(*) FROM agent_task_queue WHERE issue_id='${restart_issue_id}' AND trigger_evidence_kind='issue_assignment' AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')")
  completed=$(docker exec "$container" psql -U multica -Atc "SELECT count(*) FROM agent_task_queue WHERE issue_id='${restart_issue_id}' AND status='completed'")
  [[ "$active" -le 1 && "$completed" -ge 1 ]] && break
  sleep 1
done
elapsed=$(( $(date +%s) - restart_at ))
[[ "$elapsed" -le 60 && "$active" -le 1 && "$completed" -ge 1 ]] || { echo "restart recovery failed: elapsed=$elapsed active=$active completed=$completed" >&2; exit 1; }

duplicate_controllers=$(docker exec "$container" psql -U multica -Atc "SELECT count(*) FROM (SELECT issue_id FROM agent_task_queue WHERE issue_id IN (${quoted_ids}) AND trigger_evidence_kind='issue_assignment' AND status IN ('queued','dispatched','running','waiting_local_directory','deferred') GROUP BY issue_id HAVING count(*) > 1) d")
comment_events=$(docker exec "$container" psql -U multica -Atc "SELECT count(*) FROM comment WHERE issue_id='${issue_ids[4]}'")
[[ "$duplicate_controllers" -eq 0 && "$comment_events" -ge 1 ]] || { echo "concurrency fence failed: duplicates=$duplicate_controllers comment_events=$comment_events" >&2; exit 1; }
docker exec "$container" psql -U multica -Atc "SELECT issue_id,status,trigger_evidence_kind,trigger_comment_id,coalesced_comment_ids,delivered_comment_ids,created_at,completed_at FROM agent_task_queue WHERE issue_id IN (${quoted_ids}) ORDER BY issue_id,created_at" >"$artifact_dir/task-evidence.tsv"
printf 'PASS elapsed_seconds=%s six_running_slots=%s active_issue_assignment=%s duplicate_controllers=%s comment_events=%s delivered_comment=%s patrol_pass=1 declared_wait_active=%s completed=%s issue_id=%s\n' "$elapsed" "$running_slots" "$active" "$duplicate_controllers" "$comment_events" "$delivered_comment" "$declared_wait_active" "$completed" "$restart_issue_id" | tee "$artifact_dir/result.log"
