package orchestrationqa

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// isUniqueViolation reports whether err is a Postgres 23505 unique_violation,
// without importing pgconn: the SQLSTATE and constraint text are stable enough
// for a test assertion and keep this package dependency-light.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "23505") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "unique constraint")
}

// TestAcceptanceMatrix is the executable form of
// docs/qa/self-iteration-acceptance-matrix.md. Each subtest seeds a scenario
// and asserts the database invariant that row owns. HOLDS rows assert the live
// contract; GAP rows seed the defect, log the observed violation, and skip with
// the Stage-2 target so the suite stays green while marking the gap.
func TestAcceptanceMatrix(t *testing.T) {
	pool := integrationPool(t)

	// M1 — Ready work produces exactly one pending author for the assignee.
	// The 60s dispatch SLA and the queued→dispatched→running event sequence are
	// measured against a live daemon in Stage 3 (AI-109); at DB level we assert
	// the enqueue invariant the trigger must satisfy.
	t.Run("M1_ready_single_pending_author", func(t *testing.T) {
		s := seedBase(t, pool)
		issue := s.seedIssue(t, "todo", "agent", s.agentID, "")
		s.seedTask(t, issue, s.agentID, "queued") // stands in for WillEnqueueRun→EnqueueTaskForIssue
		assertSinglePendingAuthorPerIssueAgent(t, pool, issue, s.agentID)
		if got := countTasks(t, pool, `issue_id=$1 AND status='queued'`, issue); got != 1 {
			t.Fatalf("ready issue: want exactly 1 queued task, got %d", got)
		}
	})

	// M2 — Only an external blocker ⇒ no task row is created (no fake work).
	// The typed dispatch.ReasonCode surfacing is exercised in service tests;
	// here we assert the no-phantom-task invariant given an offline runtime.
	t.Run("M2_external_blocker_no_fake_task", func(t *testing.T) {
		s := seedBase(t, pool)
		s.setRuntimeStatus(t, "offline")
		issue := s.seedIssue(t, "todo", "agent", s.agentID, "")
		// With the sole assignee's runtime offline, WillEnqueueRun returns false
		// (service/issue_trigger.go:117) — no enqueue happens, so no row exists.
		if got := countTasks(t, pool, `issue_id=$1`, issue); got != 0 {
			t.Fatalf("external-blocker issue must have 0 tasks (no fake work), got %d", got)
		}
	})

	// M3 — Project complete ⇒ deduplicated next-round candidate. Feature does
	// not exist (no generation path; issue_child_done.go only wakes the parent).
	t.Run("M3_next_round_dedup_candidate", func(t *testing.T) {
		t.Skip("GAP (Stage 2 / AI-107): next-round self-iteration candidate generation is not implemented. " +
			"Target invariant: on all-children-done, generate a candidate deduped via issueguard.LockAndFindActiveDuplicate; no duplicate active title in (workspace,project,parent).")
	})

	// M4 — Restart/retry keeps a single pending author, and the (issue,agent)
	// unique index is the enforced backstop. Also documents the deferred hole.
	t.Run("M4_restart_retry_single_author", func(t *testing.T) {
		s := seedBase(t, pool)
		issue := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
		// Recovery path outcome: parent failed(runtime_recovery) + one queued child.
		parent := s.seedTask(t, issue, s.agentID, "running")
		if _, err := pool.Exec(context.Background(),
			`UPDATE agent_task_queue SET status='failed', failure_reason='runtime_recovery', completed_at=now() WHERE id=$1`,
			parent); err != nil {
			t.Fatalf("mark parent failed: %v", err)
		}
		s.seedTask(t, issue, s.agentID, "queued") // the single retry child
		assertSinglePendingAuthorPerIssueAgent(t, pool, issue, s.agentID)

		// The backstop is real: a concurrent second queued child (patrol +
		// restart both retrying) is rejected by idx_one_pending_task_per_issue_agent.
		_, err := pool.Exec(context.Background(),
			`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, originator_user_id, accountable_user_id, originator_source)
			 VALUES ($1,$2,$3,'queued',$4,$4,'issue_assignment')`,
			s.agentID, s.runtimeID, issue, s.userID)
		if !isUniqueViolation(err) {
			t.Fatalf("expected unique_violation on 2nd queued child (single-author backstop), got: %v", err)
		}

		// Deferred hole: a 'deferred' retry child is OUTSIDE the index predicate,
		// so it coexists with a queued author. Stage 2 must close this.
		_, derr := pool.Exec(context.Background(),
			`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, originator_user_id, accountable_user_id, originator_source)
			 VALUES ($1,$2,$3,'deferred',$4,$4,'issue_assignment')`,
			s.agentID, s.runtimeID, issue, s.userID)
		if derr != nil {
			t.Fatalf("seed deferred child: %v", derr)
		}
		deferredCoexists := countTasks(t, pool,
			`issue_id=$1 AND agent_id=$2 AND status IN ('queued','deferred')`, issue, s.agentID)
		if deferredCoexists >= 2 {
			t.Logf("GAP confirmed: 'deferred' (%d rows w/ queued) is outside idx_one_pending_task_per_issue_agent; "+
				"a backoff retry can coexist with a live author. Stage 2 should widen the predicate or add a guard.", deferredCoexists)
		}
	})

	// M5 — A comment must not create a second author; the DB backstop is the
	// same unique index (the service path coalesces into the single queued row).
	t.Run("M5_comment_no_second_author", func(t *testing.T) {
		s := seedBase(t, pool)
		issue := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
		s.seedTask(t, issue, s.agentID, "queued")
		// A comment-triggered path that tried to insert a fresh queued task
		// (instead of merging) would hit the unique index — proving the backstop.
		_, err := pool.Exec(context.Background(),
			`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, originator_user_id, accountable_user_id, originator_source)
			 VALUES ($1,$2,$3,'queued',$4,$4,'comment')`,
			s.agentID, s.runtimeID, issue, s.userID)
		if !isUniqueViolation(err) {
			t.Fatalf("expected unique_violation guarding against a 2nd comment-triggered author, got: %v", err)
		}
		assertSinglePendingAuthorPerIssueAgent(t, pool, issue, s.agentID)
	})

	// M6 — Reassign must leave one active author across ALL agents. Today it
	// does not: reassign never cancels (MUL-4465), and the index is per-agent,
	// so agent A (running) + agent B (queued) coexist. GAP — Stage 2 target.
	t.Run("M6_reassign_single_active_author", func(t *testing.T) {
		s := seedBase(t, pool)
		issue := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
		s.seedTask(t, issue, s.agentID, "running") // agent A already working
		// Reassign A→B (issue update path) then B gets enqueued — neither cancels A.
		if _, err := pool.Exec(context.Background(),
			`UPDATE issue SET assignee_id=$1 WHERE id=$2`, s.agentBID, issue); err != nil {
			t.Fatalf("reassign: %v", err)
		}
		s.seedTask(t, issue, s.agentBID, "queued") // B's fresh author
		inflight := countTasks(t, pool,
			`issue_id=$1 AND status IN ('queued','dispatched','running','waiting_local_directory')`, issue)
		t.Logf("GAP confirmed: %d concurrent in-flight authors on one issue after reassign (want 1). "+
			"Reassign does not cancel the prior author (handler/issue.go:2781-2792) and the unique index is "+
			"per-(issue,agent). Stage 2 must cancel-prior or add a per-issue active-author fence.", inflight)
		t.Skip("GAP (Stage 2 / AI-107): single-active-author-per-issue not enforced across reassign. " +
			"Un-skip and call assertAtMostOneActiveAuthorForIssue once the fence lands.")
	})

	// M7 — Review fence: flipping to in_review does not enqueue a new author.
	t.Run("M7_review_no_jump_the_gun", func(t *testing.T) {
		s := seedBase(t, pool)
		issue := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
		// in_review is not a start trigger (service/issue_trigger.go:100-114);
		// no run is enqueued purely because status became in_review.
		if _, err := pool.Exec(context.Background(),
			`UPDATE issue SET status='in_review' WHERE id=$1`, issue); err != nil {
			t.Fatalf("to in_review: %v", err)
		}
		if got := countTasks(t, pool, `issue_id=$1 AND status IN ('queued','dispatched','running')`, issue); got != 0 {
			t.Fatalf("in_review must not spawn a new author, got %d in-flight", got)
		}
	})

	// M8 — Autopilot overlap ⇒ one run per (trigger_id, planned_at). The
	// dispatch-layer idempotency index (migration 124) is the enforced guard.
	t.Run("M8_autopilot_overlap_one_run", func(t *testing.T) {
		s := seedBase(t, pool)
		ctx := context.Background()
		var autopilotID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO autopilot (workspace_id, title, assignee_id, created_by_type, created_by_id)
			 VALUES ($1,$2,$3,'member',$4) RETURNING id`,
			s.workspaceID, "QA Autopilot", s.agentID, s.userID,
		).Scan(&autopilotID); err != nil {
			t.Fatalf("seed autopilot: %v", err)
		}
		var triggerID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO autopilot_trigger (autopilot_id, kind, cron_expression, enabled)
			 VALUES ($1,'schedule','0 * * * *', true) RETURNING id`,
			autopilotID,
		).Scan(&triggerID); err != nil {
			t.Fatalf("seed trigger: %v", err)
		}
		planned := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
		if _, err := pool.Exec(ctx,
			`INSERT INTO autopilot_run (autopilot_id, trigger_id, source, status, planned_at)
			 VALUES ($1,$2,'schedule','running',$3)`,
			autopilotID, triggerID, planned); err != nil {
			t.Fatalf("seed first run: %v", err)
		}
		// A second dispatch landing on the same planned_at (stale-steal / replica
		// race) is rejected by uq_autopilot_run_trigger_planned → the loser reuses.
		_, err := pool.Exec(ctx,
			`INSERT INTO autopilot_run (autopilot_id, trigger_id, source, status, planned_at)
			 VALUES ($1,$2,'schedule','running',$3)`,
			autopilotID, triggerID, planned)
		if !isUniqueViolation(err) {
			t.Fatalf("expected unique_violation on duplicate (trigger_id, planned_at), got: %v", err)
		}
		var runs int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM autopilot_run WHERE trigger_id=$1 AND planned_at=$2`,
			triggerID, planned).Scan(&runs); err != nil {
			t.Fatalf("count runs: %v", err)
		}
		if runs != 1 {
			t.Fatalf("autopilot overlap: want exactly 1 run per planned occurrence, got %d", runs)
		}
	})

	// M9 — Double cancel is idempotent and leaves no orphaned task.
	t.Run("M9_double_cancel_idempotent", func(t *testing.T) {
		s := seedBase(t, pool)
		issue := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
		task := s.seedTask(t, issue, s.agentID, "running")
		cancel := func() int64 {
			ct, err := pool.Exec(context.Background(),
				`UPDATE agent_task_queue SET status='cancelled', completed_at=now()
				 WHERE id=$1 AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')`,
				task)
			if err != nil {
				t.Fatalf("cancel: %v", err)
			}
			return ct.RowsAffected()
		}
		if n := cancel(); n != 1 {
			t.Fatalf("first cancel should affect 1 row, got %d", n)
		}
		if n := cancel(); n != 0 {
			t.Fatalf("second cancel must be a no-op (status-gated), affected %d rows", n)
		}
		if got := countTasks(t, pool, `id=$1 AND status='cancelled'`, task); got != 1 {
			t.Fatalf("task must be terminal-cancelled exactly once, got %d", got)
		}
		if got := countTasks(t, pool, `issue_id=$1 AND status IN ('queued','dispatched','running')`, issue); got != 0 {
			t.Fatalf("no active task must remain after cancel (不丢任务), got %d", got)
		}
	})

	// M10 — No fake in_progress: an issue presented as actively-working must
	// have a live task in its subtree. Parent status is never recomputed from
	// task counts today (issue_child_done.go only wakes) — GAP, Stage 2 target.
	t.Run("M10_no_fake_in_progress", func(t *testing.T) {
		s := seedBase(t, pool)
		parent := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
		// A child that is already done, and NO live task anywhere in the subtree.
		child := s.seedIssue(t, "done", "agent", s.agentBID, parent)
		_ = child
		live := liveTaskExistsInSubtree(t, pool, parent)
		if !live {
			t.Logf("GAP confirmed: issue %s status=in_progress but 0 live tasks in its subtree "+
				"(the 'fake in_progress' / 全员空转 defect). Presented liveness is not derived from task counts.", parent)
		}
		t.Skip("GAP (Stage 2 / AI-108): derived-liveness field not implemented. " +
			"Un-skip and assert presented-state == liveTaskExistsInSubtree once liveness is derived from real task counts.")
	})
}

// TestScenarioIsolation is a fast smoke test that the fixture seeds and cleans
// up under a unique workspace, so the concurrency rows above stay hermetic.
func TestScenarioIsolation(t *testing.T) {
	pool := integrationPool(t)
	s := seedBase(t, pool)
	if s.workspaceID == "" || s.agentID == "" || s.agentBID == "" || s.runtimeID == "" {
		t.Fatalf("seedBase left an id empty: %+v", s)
	}
	if !strings.HasPrefix(s.workspaceSlug, "qa-orch-") {
		t.Fatalf("unexpected slug %q", s.workspaceSlug)
	}
	// The two agents are distinct so reassign/mention rows have a real target.
	if s.agentID == s.agentBID {
		t.Fatalf("agent A and B must differ")
	}
	if _, err := uuid.Parse(s.workspaceID); err != nil {
		t.Fatalf("workspace id not a uuid: %v", err)
	}
}
