# Self-Iteration Scheduling — Fault-Injection Plan (AI-106 / Stage 1)

Owner: QA 自动化工程师. Companion to `self-iteration-acceptance-matrix.md`.

Goal: drive every matrix race **deterministically**. The reproduction rule from
AI-104 is explicit — **禁止用 sleep 脆弱断言**. This plan encodes the
synchronization primitives so the closure tests (Stage 3 / AI-109) are stable.

## Principles

1. **No wall-clock sleeps for ordering.** Use goroutine barriers
   (`chan struct{}` + `sync.WaitGroup`), `SELECT … FOR UPDATE` contention, and
   `pg_sleep()` *inside a trigger function* to hold a DB lock for a bounded
   window — the pattern already used by `createSleepTrigger`
   (`service/task_claim_race_test.go:107`) and `pgcron_concurrent_test.go`.
2. **Control time via columns, not the clock.** The scheduler reads DB `now()`
   (`scheduler/db_ops.go:30 dbNow`); tests must **backdate** `stale_after`,
   `next_retry_at`, `fire_at`, `last_seen_at`, `dispatched_at` directly to move
   an entity across a threshold instead of waiting. There is no injectable
   fake clock — this is deliberate (`stale_steal_test.go`).
3. **Assert the DB invariant, then the event.** The durable contract is the
   row state (unique indexes, status). Event/broadcast ordering is secondary
   and asserted where the service layer is wired.
4. **Skip cleanly without Postgres.** Reuse the `integrationPool(t)` /
   `t.Skipf` gate (`scheduler/stale_steal_test.go:19`, `handler/handler_test.go:38`).

## Reusable harness inventory (do not re-invent)

| Helper | Location | Use |
|---|---|---|
| `integrationPool(t)` | `scheduler/stale_steal_test.go:19` | DB pool or skip |
| `newTaskClaimRacePool(t)` / `newCancelFinalizePool(t)` / `newHeadShaDedupPool(t)` | `service/*_test.go` | per-file pools |
| `createClaimCapacityFixture` | `service/task_claim_race_test.go:140` | seed agent+issue+capacity |
| `createSleepTrigger` | `service/task_claim_race_test.go:107` | hold a row lock a bounded time |
| `setupHandlerTestFixture` | `handler/handler_test.go:92` | workspace+user+agent+online runtime |
| `uniqueJobName` / `cleanupExecutions` | `scheduler/stale_steal_test.go:48,38` | isolate cron partitions |
| `quoteIdent` / `quoteLiteral` | `service/task_claim_race_test.go:225` | safe dynamic SQL |
| `orchestrationqa` fixtures | `server/internal/orchestrationqa/fixtures_test.go` (this deliverable) | seed a full ready/blocked/overlap scenario + invariant assertions |

## Per-scenario injection recipes

### F-M1 — Ready ⇒ dispatch latency
- Seed ready issue+agent+online runtime (`seedReadyScenario`).
- Fire the trigger, then poll for the `queued` row with a **bounded retry loop
  on a `context` deadline** (not sleep-then-assert). Record
  `queued.created_at → dispatched_at` delta; assert ≤ 60s against a real daemon
  in Stage 3. Unit level asserts only the row appears + single-author.

### F-M2 — External blocker
- Seed issue+agent but set runtime `status='offline'` (or `agent.archived_at=now()`,
  or `runtime_id=NULL`). Fire trigger. Assert `COUNT(*)=0` tasks and that the
  service returns the expected `dispatch.ReasonCode`
  (`runtime_offline`/`target_unavailable`). No sleep needed.

### F-M3 — Next-round generation (PENDING)
- Seed a project with all children `done`. Assert the dedup invariant would hold
  (no duplicate normalized title). Currently `t.Skip` — nothing generates.

### F-M4 — Restart-retry vs patrol-reassign double-author
- Seed a `running` task. In two goroutines gated by one `start` channel:
  (a) call the recovery path (`RecoverOrphanedTasksForRuntime` SQL →
  `HandleFailedTasks`), (b) an independent enqueue/patrol attempt on the same
  (issue, agent). Barrier both, release together. Assert the **unique-index**
  invariant: `COUNT(status IN ('queued','dispatched'))_by_(issue,agent) ≤ 1`.
  Separately test the **deferred hole**: force `CreateRetryTask` with a
  non-NULL `fire_at` (child = `deferred`) concurrently and show `deferred` is
  outside the index — this is the Stage-2 target to close.

### F-M5 — Comment second-task
- Seed a `queued` direct task. Post a plain member comment via the comment
  handler path. Assert still one `(queued|dispatched)` row and its
  `coalesced_comment_ids` contains the new comment id. Merge-miss branch:
  force the merge query to miss (comment arrives after the row left `queued`)
  and assert the fail-closed / defer behavior — no second row.

### F-M6 — Reassign double-author (documents current gap)
- Seed A `running`. Reassign to B via the issue update path. Assert **current**
  reality: rows for both A (`running`) and B (`queued`) can exist → 2 authors.
  Mark as the failing target; Stage 2 flips the assertion once cancel-prior /
  per-issue fence lands.

### F-M7 — Review 抢跑
- (a) Set issue `→ in_review`; assert **0** new tasks enqueued.
- (b) HEAD-SHA dedup: seed a `queued` task stamped with old `head_sha`; advance
  the resolved review SHA; assert `HasPendingTaskForIssueAndAgent` **misses**
  (returns false) so a fresh request is not shadowed. No timing.

### F-M8 — Autopilot overlap
- Seed a schedule trigger. Fire N concurrent `DispatchAutopilotForPlan` at one
  `planned_at` from distinct runner ids (mirror `concurrent_claim_test.go`'s
  N-goroutine + `start` channel). Assert exactly one `autopilot_run` for
  `(trigger_id, planned_at)` (PK conflict on the rest) and ≤1 issue/task.
  Stale-steal: backdate `sys_cron_executions.stale_after`, re-fire, assert
  reuse (not a duplicate run).

### F-M9 — Double cancel
- Seed active task. Two goroutines call cancel (issue-scope + agent-scope /
  user + daemon) gated by a `start` channel. Assert exactly one row transitions
  to `cancelled` (`completed_at` set once), `ReconcileAgentStatus` leaves the
  agent not-`working`, and no row remains active. Idempotency comes from the
  status-gated `UPDATE … WHERE status IN (active set)` so no barrier subtlety.

### F-M10 — Fake in_progress
- Seed a parent `in_progress` with children all terminal and **0** live tasks
  in the subtree. Assert the target liveness predicate
  (`EXISTS(live task in subtree)`) is **false** while status says in_progress →
  currently failing (the defect). Stage 2 adds the derived-liveness field; the
  assertion flips to "presented state == derived state".

## Environment & safety

- All recipes run against a **disposable** Postgres (`DATABASE_URL`); tests
  skip when absent. **禁止连接生产/Staging 数据库**. No production writes.
- Concurrency tests must clean their own rows (`t.Cleanup`) and use unique
  workspace slugs / job-name partitions to stay hermetic under `-race`.
- Run with `go test -race ./internal/orchestrationqa/...` in CI once a test DB
  is provisioned; the harness is otherwise a compile-checked skip.
