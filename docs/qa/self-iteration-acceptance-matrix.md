# Self-Iteration Scheduling — E2E Acceptance Matrix (AI-106 / Stage 1)

Owner: QA 自动化工程师. Parent: **AI-102** (P0 自迭代编排闭环). Sibling contracts:
AI-103 (state-machine ADR), AI-104 (reproduction failing tests), AI-105 (observability UX).

This matrix is the **source-of-truth grid** for what "correct" self-iteration
scheduling looks like. Every row states: preconditions, the trigger, the
**expected event sequence**, the **database invariants** (checkable SQL), the
pass criteria, and whether the invariant **HOLDS today** or is a **GAP** to be
built in Stage 2 (AI-107/AI-108). Each row is grounded in real code
(`file:line`) so Stage 2/3 cannot drift from it.

> Boundary with AI-104: AI-104 owns the *reproduction* tests that prove the
> current defects fail. AI-106 owns this acceptance matrix + the reusable
> harness/fixtures (`server/internal/orchestrationqa`) that Stage 3 (AI-109)
> re-runs against the fixed backend to prove the closure. Rows marked **GAP**
> are the ones AI-104 also reproduces; here they define the *target* invariant.

## Ground truth: the data model that the invariants query

| Object | Table | Status values | Source |
|---|---|---|---|
| Task | `agent_task_queue` | `queued, dispatched, running, completed, failed, cancelled, waiting_local_directory, deferred` | `migrations/001_init.up.sql:127`, `+109`, `+128` |
| Autopilot | `autopilot` | `active, paused, archived` | `migrations/042_autopilot.up.sql` |
| Autopilot run | `autopilot_run` | `issue_created, running, completed, failed` (skipped/pending **removed**) | `migrations/043_fix_orphaned_autopilot_runs.up.sql:23` |
| Scheduler lease | `sys_cron_executions` | `RUNNING, SUCCESS, FAILED` | scheduler |

**Key invariant-bearing constraints (the whole matrix leans on these):**

- `idx_one_pending_task_per_issue_agent` — UNIQUE `(issue_id, agent_id) WHERE status IN ('queued','dispatched')` — `migrations/037_fix_pending_task_unique_index.up.sql`.
  **⚠ This is per-(issue, agent), NOT per-issue** (037 deliberately replaced the
  per-issue index from `022_task_lifecycle_guards.up.sql`). It also does **not**
  cover `running`/`deferred`. Both facts drive the GAP rows below.
- `uq_autopilot_run_trigger_planned` — UNIQUE `(trigger_id, planned_at) WHERE both NOT NULL` — `migrations/124_autopilot_run_planned_at.up.sql`. Dispatch-layer idempotency for scheduled overlap.
- `sys_cron_executions (job_name, scope_kind, scope_id, plan_time)` unique — scheduler-layer single-winner (`scheduler/db_ops.go:57 tryClaim`).
- Attribution CHECK: `originator_user_id ⟹ accountable_user_id = originator_user_id` — `migrations/197/198`.

## The matrix

Legend for **Status**: ✅ HOLDS = invariant enforced by current code; 🔴 GAP =
target invariant NOT yet enforced, Stage 2 must implement; 🟡 PARTIAL = enforced
in one path but bypassable.

### M1 — Ready work is dispatched within 60s

- **Precondition:** project has an issue `status=todo`, assignee = agent, agent
  `runtime_id` bound and its runtime `status=online`, no pending task.
- **Trigger:** issue created-assigned, or `backlog → todo/in_progress`.
- **Expected event sequence:** `WillEnqueueRun`→true (`service/issue_trigger.go:89`)
  → `EnqueueTaskForIssue` (`service/task.go:908`) → row inserted `status=queued`
  (`agent.sql:185 CreateAgentTask`) → `task:queued` broadcast → daemon
  `ClaimTask` (`service/task.go:1987`) → `dispatched` → `running`.
- **DB invariants:**
  - After trigger, exactly **1** row `WHERE issue_id=? AND agent_id=? AND status IN ('queued','dispatched')`.
  - `queued.created_at` to first `dispatched_at` ≤ 60s (SLA; measured, not enforced by schema).
- **Pass:** queued row exists ≤ (trigger + dispatch poll) and is claimed by the bound runtime; no row with a different runtime.
- **Status:** ✅ HOLDS for the enqueue; ⏱ the 60s SLA is a **timing assertion** (harness `M1`), depends on claim-poll cadence — Stage 3 measures under real daemon.

### M2 — Only an external blocker ⇒ no fake task, explicit reason

- **Precondition:** issue ready but the sole blocker is external: runtime
  offline, or agent archived, or no runtime bound.
- **Trigger:** same as M1.
- **Expected sequence:** `WillEnqueueRun` returns `false` when
  `!agent.RuntimeID.Valid || agent.ArchivedAt.Valid` (`service/issue_trigger.go:117-121`);
  autopilot path returns a typed `dispatch.ReasonCode` via
  `shouldSkipDispatch`/`agentReadinessReasonCode` (`service/autopilot.go:1186,1266`) →
  `runtime_offline` / `target_unavailable`.
- **DB invariants:**
  - **0** rows inserted into `agent_task_queue` for this (issue, agent).
  - No issue is left displaying `in_progress` on account of this trigger.
  - The skip reason is a stable `dispatch.ReasonCode` (`internal/dispatch/reason.go`), never a free-text string.
- **Pass:** no phantom task; caller receives a precise machine-readable reason.
- **Status:** ✅ HOLDS at the enqueue/dispatch decision. 🔴 GAP: there is **no
  surfaced per-project "why is nobody working" reason** persisted for the
  Agents/Project views — that projection is AI-105/AI-108. Harness `M2`
  asserts the no-task invariant; the reason-surfacing assertion is Stage 2.

### M3 — Project complete ⇒ deduplicated next-round candidate

- **Precondition:** every non-terminal issue in a project reaches `done`.
- **Trigger:** last child `→ done`.
- **Expected sequence (TARGET):** orchestrator generates the next self-iteration
  candidate issue(s), deduplicated by normalized title within
  (workspace, project, parent) using `issueguard.LockAndFindActiveDuplicate`
  (`internal/issueguard/duplicate.go:44`) and the autopilot-recent-window dedup
  (`:79`).
- **DB invariants (TARGET):**
  - No duplicate active issue with the same normalized title in scope.
  - Any generated candidate is attributable (originator/accountable set per `197`).
- **Status:** 🔴 **GAP — feature does not exist.** Grep confirms no next-round
  generation; only `issue_child_done.go` wakes the parent assignee
  (`handler/issue_child_done.go:49`), which merely *notifies* — it does not
  generate work. Harness `M3` is `t.Skip("PENDING Stage 2 (AI-107): next-round
  candidate generation not implemented")` and encodes the dedup invariant so
  Stage 2 has an executable target.

### M4 — daemon restart / retry keeps a single author (no double-retry)

- **Precondition:** agent has a `running` task; daemon restarts.
- **Trigger:** daemon `RecoverOrphans` (`daemon/daemon.go:2093`) →
  `RecoverOrphanedTasksForRuntime` (`agent.sql:722`, sets
  `failure_reason='runtime_recovery'`) → `HandleFailedTasks` (`task.go:3568`) →
  `MaybeRetryFailedTask` (`task.go:3234`). **Concurrent** with a patrol/reassign
  touching the same issue.
- **Expected sequence:** parent → `failed(runtime_recovery)` → **≤1** child via
  `CreateRetryTask` (`agent.sql:290`), `attempt=parent.attempt+1`,
  `parent_task_id`/`retry_of_task_id=parent.id`.
- **DB invariants:**
  - At most **1** row `WHERE (issue_id,agent_id) AND status IN ('queued','dispatched')` at all times.
  - No two live children of the same parent.
  - `child.attempt ≤ retryAttemptCeiling(reason, max_attempts)` — retry budget honored (`task.go:3234`).
- **Pass:** exactly one recovery author; retry budget capped; no orphaned `running`.
- **Status:** 🟡 PARTIAL. The `(issue,agent)` unique index blocks a second
  **queued** child, but `CreateRetryTask` has **no `ON CONFLICT`/`NOT EXISTS`
  guard** (`agent.sql:344` INSERT…SELECT) and `deferred` children are **outside
  the index predicate**. Concurrent FailTask-in-tx retry (`task.go:3000`) +
  sweeper `MaybeRetryFailedTask` can both attempt; the index saves the queued
  case but not the deferred/backoff case. Harness `M4` asserts single-author +
  documents the deferred hole for Stage 2 to close.

### M5 — Comment does not spawn a second author

- **Precondition:** agent has a `queued`/`dispatched`/`running` direct task on the issue.
- **Trigger:** a plain member comment on the same issue.
- **Expected sequence:** `enqueueCommentAgentTriggers` (`handler/comment.go:1518`)
  sees `AlreadyPending` → `mergeCommentIntoPendingTask` (`:1753` →
  `MergeCommentIntoPendingTask` `agent.sql:929`) folds the comment into the
  single **queued** row; on merge-miss `decidePostMergeMiss` (`:1671`) defers if
  an active task exists and **fails closed** on query error.
- **DB invariants:**
  - Still exactly **1** `(queued|dispatched)` row for (issue, agent) after the comment.
  - `coalesced_comment_ids` on that row grows to include the new comment; no new row.
  - Attribution invariant unbroken after merge (`190` guards the `#5192` bypass).
- **Pass:** comment coalesces/defers; never a second concurrent author.
- **Status:** ✅ HOLDS (merge + fail-closed + unique-index backstop). Harness `M5`
  asserts the single-row + coalesced-id invariant.

### M6 — Reassign coordination leaves one active author

- **Precondition:** agent A has a `running` task on the issue.
- **Trigger:** issue reassigned A → B (or B mentioned).
- **Expected sequence (TARGET):** the prior author's live task is cancelled or
  fenced so exactly one agent is the active author.
- **DB invariants (TARGET):** at most **1** row across **all** agents `WHERE issue_id=? AND status IN ('queued','dispatched','running')`.
- **Status:** 🔴 **GAP.** Reassignment intentionally does **NOT** cancel
  (`handler/issue.go:2781-2792`, MUL-4113/MUL-4465); only `DeleteIssue` cancels
  (`task.go:1612 CancelTasksForIssue`). The unique index is per-(issue,**agent**)
  so A-running + B-queued coexist → two authors on one issue. This is the
  "单 Issue 单作者" violation. Harness `M6` asserts the **current** (2-author)
  reality as a documented failing target and states the Stage-2 fix options
  (cancel-prior on reassign, or a per-issue active-author fence).

### M7 — Review fence: `in_review` does not jump the gun ("review 抢跑")

- **Precondition:** issue `in_review`; a reviewer task may be running; HEAD may advance.
- **Trigger:** status flips / comments while under review.
- **Expected sequence:** `WillEnqueueRun` only fires on assign or
  `backlog → non-terminal` (`service/issue_trigger.go:100-114`); `in_review` is
  not a start trigger. `hasPendingRun` keys dedup on the reviewed HEAD SHA
  (`issue_trigger.go:171` → `ResolveIssueReviewSHA task.go:942`, TEN-356) so a
  pending run against an old HEAD does not shadow a fresh request after HEAD moved.
- **DB invariants:**
  - No new author task created purely because status became `in_review`.
  - Head-SHA-keyed dedup: a pending task on old SHA does not block a request on a new SHA.
- **Pass:** review does not pre-empt; SHA dedup correct.
- **Status:** ✅ HOLDS for the trigger gate + SHA dedup. Harness `M7` asserts no
  enqueue on `→in_review` and the SHA-keyed dedup boundary.

### M8 — Autopilot overlap ⇒ one run per planned occurrence

- **Precondition:** a scheduled autopilot trigger; two dispatch attempts land on
  the same `planned_at` (stale-steal, replica race, leftover pg_cron).
- **Trigger:** overlapping `DispatchAutopilotForPlan` (`service/autopilot.go:356`).
- **Expected sequence:** `sys_cron_executions` single-winner
  (`scheduler/db_ops.go:57 tryClaim`) + `uq_autopilot_run_trigger_planned`
  makes a double-create a PK conflict; the loser reuses the existing run
  (`autopilot.go:377-407`).
- **DB invariants:**
  - Exactly **1** `autopilot_run` per `(trigger_id, planned_at)`.
  - ≤1 issue and ≤1 task created per planned occurrence.
- **Pass:** no duplicate run/issue/task from overlap.
- **Status:** ✅ HOLDS (idempotency indexes). Note: `concurrency_policy`
  (skip/queue/replace) was **removed** in `migrations/043` — do not test it.
  Harness `M8` asserts the `(trigger_id, planned_at)` uniqueness.

### M9 — Double cancel is a no-op; no lost/orphaned task

- **Precondition:** a task is active; two controllers issue cancel
  (`handler/chat.go:1288 CancelTaskByUser`, `handler/daemon.go:3651 CancelTask`).
- **Trigger:** concurrent cancels of the same row.
- **Expected sequence:** each cancel query gates on `status IN (active set)`
  (`agent.sql:375 CancelAgentTasksByIssue` and siblings) so the second cancel
  affects 0 rows; `ReconcileAgentStatus` runs; `task:cancelled` broadcast once.
- **DB invariants:**
  - Row ends in terminal `cancelled` exactly once; no double side effects.
  - Agent status reconciles to available (no stuck `working`).
  - "不丢任务": no row left `running`/`dispatched` with a dead controller.
- **Pass:** idempotent cancel; agent freed.
- **Status:** ✅ HOLDS. Note recovery semantics: `cancelled` is terminal and
  **non-retryable** (`retryableReasons` `task.go:3124` excludes it) — "取消后能恢复"
  means recovery via an explicit re-trigger (new comment/status), not
  auto-retry. Harness `M9` asserts single-terminal + agent reconcile.

### M10 — No "fake in_progress": liveness derives from real task counts

- **Precondition:** a parent issue shows `status=in_progress` but has 0
  `queued`/`running`/`dispatched` tasks in its subtree.
- **Trigger:** stage barrier closes but the woken assignee never enqueues, or a
  status was set manually.
- **Expected sequence (TARGET):** activity/liveness shown to users is computed
  from real task counts, not from `issue.status`; a parent with no live work is
  shown as waiting/blocked with a reason, not "in progress".
- **DB invariants (TARGET):** for any issue presented as "actively working",
  `EXISTS (SELECT 1 FROM agent_task_queue WHERE issue_id in subtree AND status IN ('queued','dispatched','running'))`.
- **Status:** 🔴 **GAP.** Parent status is **never recomputed** from child/task
  counts — `issue_child_done.go` only *notifies* the parent assignee
  (`handler/issue_child_done.go:49`, "the server only detects the barrier and
  wakes"). So `in_progress` with 0 live tasks is representable. Harness `M10`
  asserts the target liveness invariant against a seeded fake-in_progress
  fixture (currently failing → Stage 2 target).

## Coverage summary

| # | Scenario | Invariant status | Also in AI-104 repro? |
|---|---|---|---|
| M1 | Ready ⇒ dispatch ≤60s | ✅ enqueue / ⏱ SLA | — |
| M2 | External blocker ⇒ no fake task + reason | ✅ / 🔴 reason projection | yes |
| M3 | Project complete ⇒ dedup next round | 🔴 not built | yes |
| M4 | daemon restart/retry single author | 🟡 deferred hole | yes |
| M5 | Comment ⇒ no 2nd author | ✅ | yes |
| M6 | Reassign ⇒ one active author | 🔴 two authors today | yes |
| M7 | Review fence (no 抢跑) | ✅ | yes |
| M8 | Autopilot overlap ⇒ one run | ✅ | yes |
| M9 | Double cancel no-op, no lost task | ✅ | yes |
| M10 | No fake in_progress | 🔴 not derived | yes |

See `self-iteration-fault-injection-plan.md` for how each row is driven
deterministically (no `time.Sleep`), and `server/internal/orchestrationqa/` for
the executable harness + fixtures.
