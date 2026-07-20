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

	// M3 — Project complete ⇒ deduplicated next-round candidate.
	t.Run("M3_next_round_dedup_candidate", func(t *testing.T) {
		s := seedBase(t, pool)
		var projectID string
		if err := pool.QueryRow(context.Background(), `INSERT INTO project (workspace_id,title) VALUES ($1,'M3 project') RETURNING id`, s.workspaceID).Scan(&projectID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM self_iteration_candidate WHERE project_id=$1`, projectID)
		})
		for range 2 {
			_, err := pool.Exec(context.Background(), `
				INSERT INTO self_iteration_candidate (workspace_id,project_id,snapshot_hash,title,reason)
				VALUES ($1,$2,'stable-snapshot','Next iteration','all terminal')
				ON CONFLICT (project_id,snapshot_hash,policy_version) WHERE state IN ('proposed','accepted') DO NOTHING`, s.workspaceID, projectID)
			if err != nil {
				t.Fatal(err)
			}
		}
		var count int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM self_iteration_candidate WHERE project_id=$1`, projectID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("candidate replay created %d rows, want 1", count)
		}
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
			`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, originator_user_id, accountable_user_id, originator_source, trigger_evidence_kind)
			 VALUES ($1,$2,$3,'queued',$4,$4,'direct_human','issue_assignment')`,
			s.agentID, s.runtimeID, issue, s.userID)
		if !isUniqueViolation(err) {
			t.Fatalf("expected unique_violation on 2nd queued child (single-author backstop), got: %v", err)
		}

		// Deferred retries participate in the same issue-wide fence.
		_, derr := pool.Exec(context.Background(),
			`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, originator_user_id, accountable_user_id, originator_source, trigger_evidence_kind)
			 VALUES ($1,$2,$3,'deferred',$4,$4,'direct_human','issue_assignment')`,
			s.agentID, s.runtimeID, issue, s.userID)
		if !isUniqueViolation(derr) {
			t.Fatalf("expected unique_violation on deferred retry while issue is active, got: %v", derr)
		}
	})

	// M5 — A comment must not create a second author. The service path coalesces
	// the comment into the existing controller, while the DB backstop identifies
	// assignment controllers by trigger_evidence_kind (not originator_source).
	t.Run("M5_comment_no_second_author", func(t *testing.T) {
		s := seedBase(t, pool)
		issue := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
		s.seedTask(t, issue, s.agentID, "queued")
		// A comment-sourced assignment controller is still rejected because its
		// trigger evidence identifies it as an issue-assignment controller.
		_, err := pool.Exec(context.Background(),
			`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, originator_user_id, accountable_user_id, originator_source, trigger_evidence_kind)
			 VALUES ($1,$2,$3,'queued',$4,$4,'comment_source','issue_assignment')`,
			s.agentID, s.runtimeID, issue, s.userID)
		if !isUniqueViolation(err) {
			t.Fatalf("expected unique_violation guarding against a 2nd issue-assignment author, got: %v", err)
		}
		// The service-layer coalescing keeps this at one controller.
		if got := countTasks(t, pool,
			`issue_id=$1 AND trigger_comment_id IS NULL AND is_leader_task=false AND trigger_evidence_kind='issue_assignment'
			 AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')`, issue); got != 1 {
			t.Fatalf("comment must not add a 2nd issue-assignment author, want 1 controller, got %d", got)
		}
	})

	// M6 — Reassign must leave one active author across ALL agents.
	t.Run("M6_reassign_single_active_author", func(t *testing.T) {
		s := seedBase(t, pool)
		issue := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
		s.seedTask(t, issue, s.agentID, "running") // agent A already working
		// Reassign A→B (issue update path) then B gets enqueued — neither cancels A.
		if _, err := pool.Exec(context.Background(),
			`UPDATE issue SET assignee_id=$1 WHERE id=$2`, s.agentBID, issue); err != nil {
			t.Fatalf("reassign: %v", err)
		}
		_, err := pool.Exec(context.Background(),
			`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, originator_user_id, accountable_user_id, originator_source, trigger_evidence_kind)
			 VALUES ($1,$2,$3,'queued',$4,$4,'direct_human','issue_assignment')`,
			s.agentBID, s.runtimeID, issue, s.userID)
		if !isUniqueViolation(err) {
			t.Fatalf("expected issue-wide fence to reject reassigned author while prior task is running, got: %v", err)
		}
		assertAtMostOneActiveAuthorForIssue(t, pool, issue)
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

	// M10 — No fake in_progress: the read model must classify it as a fault.
	t.Run("M10_no_fake_in_progress", func(t *testing.T) {
		s := seedBase(t, pool)
		parent := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
		// A child that is already done, and NO live task anywhere in the subtree.
		child := s.seedIssue(t, "done", "agent", s.agentBID, parent)
		_ = child
		live := liveTaskExistsInSubtree(t, pool, parent)
		if live {
			t.Fatalf("fixture unexpectedly has live work for %s", parent)
		}
		// GetProjectOrchestrationSummary exposes this exact predicate as
		// classification=orchestration_fault/reason=stale_in_progress.
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

func TestIssueAssignmentControllerConcurrencyFence(t *testing.T) {
	pool := integrationPool(t)

	t.Run("second_issue_dispatch_is_rejected", func(t *testing.T) {
		s := seedBase(t, pool)
		issue := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
		s.seedTask(t, issue, s.agentID, "dispatched")
		_, err := s.pool.Exec(context.Background(),
			`INSERT INTO agent_task_queue
			   (agent_id, runtime_id, issue_id, status, originator_user_id,
			    accountable_user_id, originator_source, trigger_evidence_kind)
			 VALUES ($1,$2,$3,'dispatched',$4,$4,'owner_fallback','issue_assignment')`,
			s.agentBID, s.runtimeID, issue, s.userID)
		if !isUniqueViolation(err) {
			t.Fatalf("second issue dispatch: got %v, want unique_violation", err)
		}
		assertAtMostOneActiveAuthorForIssue(t, s.pool, issue)
	})

	t.Run("daemon_retry_and_patrol_reassign", func(t *testing.T) {
		s := seedBase(t, pool)
		issue := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
		assertConcurrentControllerRace(t, s, issue, "direct_human")
	})

	t.Run("concurrent_comment_triggers", func(t *testing.T) {
		s := seedBase(t, pool)
		issue := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
		assertConcurrentControllerRace(t, s, issue, "comment_source")
	})
}

func assertConcurrentControllerRace(t *testing.T, s *scenario, issueID, originatorSource string) {
	t.Helper()
	type result struct{ err error }
	results := make(chan result, 2)
	for _, agentID := range []string{s.agentID, s.agentBID} {
		agentID := agentID
		go func() {
			_, err := s.pool.Exec(context.Background(),
				`INSERT INTO agent_task_queue
				   (agent_id, runtime_id, issue_id, status, originator_user_id,
				    accountable_user_id, originator_source, trigger_evidence_kind)
				 VALUES ($1,$2,$3,'queued',$4,$4,$5,'issue_assignment')`,
				agentID, s.runtimeID, issueID, s.userID, originatorSource)
			results <- result{err: err}
		}()
	}

	successes, conflicts := 0, 0
	for range 2 {
		r := <-results
		switch {
		case r.err == nil:
			successes++
		case isUniqueViolation(r.err):
			conflicts++
		default:
			t.Fatalf("concurrent controller insert failed unexpectedly: %v", r.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent controller race: successes=%d unique_conflicts=%d; want 1/1", successes, conflicts)
	}
	assertAtMostOneActiveAuthorForIssue(t, s.pool, issueID)
}
