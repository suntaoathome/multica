package orchestrationqa

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// Stage 3 (AI-109) regression harness. Where the M1..M10 acceptance matrix
// asserts one invariant per row on a seeded scenario, these tests exercise the
// concurrent and recovery paths the Stage 3 charter calls out explicitly:
// real daemon-restart recovery, 6-slot concurrency, preemption/cancellation +
// re-dispatch, and deduplicated next-round generation under a race.
//
// They run against the same DATABASE_URL pool and skip cleanly without Postgres
// (integrationPool). They add tests only — no production logic is touched.

// insertIssueAuthor tries to enqueue one issue-controller author (the exact row
// shape the trigger path inserts) and returns the error so callers can count
// how many concurrent inserts the single-active fence admitted.
func insertIssueAuthor(pool interface {
	Exec(context.Context, string, ...any) (interface{ RowsAffected() int64 }, error)
}, s *scenario, issueID, agentID, status string) error {
	// Not used directly — kept as documentation of the row shape. See the
	// inline INSERTs below which use the concrete pgxpool signature.
	return nil
}

// TestStage3_RestartRecoverySingleAuthorAcrossSources proves that after a
// daemon-restart recovery (the running parent is marked failed with
// runtime_recovery and a single retry child is queued), the issue-wide fence
// still admits exactly one active issue_assignment author — and a second
// recovery/patrol retry racing in is rejected by the unique backstop, so a
// restart cannot fan out into duplicate authors.
func TestStage3_RestartRecoverySingleAuthorAcrossSources(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	s := seedBase(t, pool)
	issue := s.seedIssue(t, "in_progress", "agent", s.agentID, "")

	// A run was live when the runtime died.
	parent := s.seedTask(t, issue, s.agentID, "running")
	if _, err := pool.Exec(ctx,
		`UPDATE agent_task_queue SET status='failed', failure_reason='runtime_recovery', completed_at=now() WHERE id=$1`,
		parent); err != nil {
		t.Fatalf("mark parent failed(runtime_recovery): %v", err)
	}
	// Recovery enqueues exactly one retry child.
	s.seedTask(t, issue, s.agentID, "queued")
	assertAtMostOneActiveAuthorForIssue(t, pool, issue)

	// A second retry racing in (patrol reassign + restart auto-retry both firing)
	// must be rejected by idx_one_active_task_per_issue.
	_, err := pool.Exec(ctx,
		`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, originator_user_id, accountable_user_id, originator_source, trigger_evidence_kind)
		 VALUES ($1,$2,$3,'queued',$4,$4,'direct_human','issue_assignment')`,
		s.agentID, s.runtimeID, issue, s.userID)
	if !isUniqueViolation(err) {
		t.Fatalf("restart+patrol double-retry must be rejected by the single-active fence, got: %v", err)
	}
	assertAtMostOneActiveAuthorForIssue(t, pool, issue)
}

// TestStage3_ConcurrentEnqueueOneWinnerPerIssue is the "6-slot concurrency"
// invariant at the fence layer: N goroutines racing to enqueue an
// issue_assignment author for the SAME issue must yield exactly one committed
// row; the losers get a unique_violation, never a second active author. This is
// the DB backstop under the max-concurrent-slots dispatcher, complementing the
// service-level TestClaimTaskConcurrentCapacityRespected.
func TestStage3_ConcurrentEnqueueOneWinnerPerIssue(t *testing.T) {
	pool := integrationPool(t)
	s := seedBase(t, pool)
	issue := s.seedIssue(t, "todo", "agent", s.agentID, "")

	const racers = 6
	var wg sync.WaitGroup
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = pool.Exec(context.Background(),
				`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, originator_user_id, accountable_user_id, originator_source, trigger_evidence_kind)
				 VALUES ($1,$2,$3,'queued',$4,$4,'direct_human','issue_assignment')`,
				s.agentID, s.runtimeID, issue, s.userID)
		}(i)
	}
	close(start)
	wg.Wait()

	winners, losers := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			winners++
		case isUniqueViolation(e):
			losers++
		default:
			t.Fatalf("unexpected non-unique error from concurrent enqueue: %v", e)
		}
	}
	if winners != 1 {
		t.Fatalf("6-slot concurrent enqueue: want exactly 1 winner, got %d (losers=%d)", winners, losers)
	}
	if losers != racers-1 {
		t.Fatalf("want %d rejected racers, got %d", racers-1, losers)
	}
	assertAtMostOneActiveAuthorForIssue(t, pool, issue)
}

// TestStage3_ConcurrentDistinctIssuesAllEnqueue confirms the fence is
// per-issue, not a global lock: 6 distinct issues each accept their author
// concurrently (no false contention that would starve the 6 slots).
func TestStage3_ConcurrentDistinctIssuesAllEnqueue(t *testing.T) {
	pool := integrationPool(t)
	s := seedBase(t, pool)

	const n = 6
	issues := make([]string, n)
	for i := range n {
		issues[i] = s.seedIssue(t, "todo", "agent", s.agentID, "")
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = pool.Exec(context.Background(),
				`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, originator_user_id, accountable_user_id, originator_source, trigger_evidence_kind)
				 VALUES ($1,$2,$3,'queued',$4,$4,'direct_human','issue_assignment')`,
				s.agentID, s.runtimeID, issues[i], s.userID)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("distinct-issue enqueue %d must succeed (per-issue fence), got: %v", i, e)
		}
	}
}

// TestStage3_PreemptThenRedispatchNoOrphanNoLoss models cancel/preemption: an
// active author is cancelled (status-gated, idempotent), leaving no in-flight
// row (不丢任务 = no orphan). A fresh author can then be enqueued and is admitted
// — proving cancellation truly releases the fence rather than wedging the issue.
func TestStage3_PreemptThenRedispatchNoOrphanNoLoss(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	s := seedBase(t, pool)
	issue := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
	task := s.seedTask(t, issue, s.agentID, "running")

	// While the task is live, the fence blocks a competing author.
	_, blocked := pool.Exec(ctx,
		`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, originator_user_id, accountable_user_id, originator_source, trigger_evidence_kind)
		 VALUES ($1,$2,$3,'queued',$4,$4,'direct_human','issue_assignment')`,
		s.agentID, s.runtimeID, issue, s.userID)
	if !isUniqueViolation(blocked) {
		t.Fatalf("fence must block a new author while one is running, got: %v", blocked)
	}

	// Preempt/cancel the active task (status-gated across the five active states).
	ct, err := pool.Exec(ctx,
		`UPDATE agent_task_queue SET status='cancelled', completed_at=now()
		 WHERE id=$1 AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')`,
		task)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if ct.RowsAffected() != 1 {
		t.Fatalf("cancel should affect exactly 1 row, got %d", ct.RowsAffected())
	}
	if got := countTasks(t, pool, `issue_id=$1 AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')`, issue); got != 0 {
		t.Fatalf("no active/orphan task must remain after preemption, got %d", got)
	}

	// Re-dispatch now succeeds — the fence was released, not permanently held.
	if _, err := pool.Exec(ctx,
		`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, originator_user_id, accountable_user_id, originator_source, trigger_evidence_kind)
		 VALUES ($1,$2,$3,'queued',$4,$4,'direct_human','issue_assignment')`,
		s.agentID, s.runtimeID, issue, s.userID); err != nil {
		t.Fatalf("re-dispatch after cancel must be admitted, got: %v", err)
	}
	assertAtMostOneActiveAuthorForIssue(t, pool, issue)
}

// TestStage3_NextRoundCandidateDedupUnderConcurrency proves the self-iteration
// generator cannot double-produce a next round: N concurrent inserts for the
// same (project, snapshot_hash, policy_version) collapse to exactly one live
// 'proposed' candidate via idx_self_iteration_candidate_live_snapshot.
func TestStage3_NextRoundCandidateDedupUnderConcurrency(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	s := seedBase(t, pool)

	var projectID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO project (workspace_id,title) VALUES ($1,'Stage3 dedup project') RETURNING id`,
		s.workspaceID).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM self_iteration_candidate WHERE project_id=$1`, projectID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM project WHERE id=$1`, projectID)
	})

	const racers = 6
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// ON CONFLICT DO NOTHING is the generator's real insert path.
			_, _ = pool.Exec(context.Background(), `
				INSERT INTO self_iteration_candidate (workspace_id,project_id,snapshot_hash,title,reason)
				VALUES ($1,$2,'stage3-stable-snapshot','Next iteration','all terminal')
				ON CONFLICT (project_id,snapshot_hash,policy_version) WHERE state IN ('proposed','accepted') DO NOTHING`,
				s.workspaceID, projectID)
		}()
	}
	close(start)
	wg.Wait()

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM self_iteration_candidate WHERE project_id=$1 AND state IN ('proposed','accepted')`,
		projectID).Scan(&count); err != nil {
		t.Fatalf("count candidates: %v", err)
	}
	if count != 1 {
		t.Fatalf("concurrent next-round generation: want exactly 1 live candidate, got %d", count)
	}

	// A rejected candidate frees the snapshot for a genuinely new round (the
	// partial index is scoped to live states) — proves dedup is not a permanent lock.
	if _, err := pool.Exec(ctx,
		`UPDATE self_iteration_candidate SET state='rejected' WHERE project_id=$1`, projectID); err != nil {
		t.Fatalf("reject candidate: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO self_iteration_candidate (workspace_id,project_id,snapshot_hash,title,reason)
		VALUES ($1,$2,'stage3-stable-snapshot','Next iteration retry','prior rejected')
		ON CONFLICT (project_id,snapshot_hash,policy_version) WHERE state IN ('proposed','accepted') DO NOTHING`,
		s.workspaceID, projectID); err != nil {
		t.Fatalf("re-propose after reject must be admitted: %v", err)
	}
}

// TestStage3_FenceUsesTriggerEvidenceNotOriginatorSource pins the C1 contract:
// attribution source describes who initiated a task, while trigger evidence
// identifies issue-assignment controllers participating in the DB fence.
func TestStage3_FenceUsesTriggerEvidenceNotOriginatorSource(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	s := seedBase(t, pool)

	// 1) The shipped index predicate is scoped as designed.
	var predicate string
	if err := pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname='idx_one_active_task_per_issue'`).Scan(&predicate); err != nil {
		t.Fatalf("read index def (fence must exist): %v", err)
	}
	for _, want := range []string{
		"trigger_evidence_kind = 'issue_assignment'",
		"trigger_comment_id IS NULL",
		"is_leader_task = false",
	} {
		if !strings.Contains(predicate, want) {
			t.Fatalf("single-active index lost its scope clause %q; predicate=%s", want, predicate)
		}
	}
	if strings.Contains(predicate, "originator_source = 'issue_assignment'") {
		t.Fatalf("single-active index must not use attribution source as controller evidence: %s", predicate)
	}

	// 2) An issue_assignment controller is active.
	issue := s.seedIssue(t, "in_progress", "agent", s.agentID, "")
	s.seedTask(t, issue, s.agentID, "running")

	// 3) A comment-sourced task carrying issue-assignment evidence is a second
	//    controller and must be rejected. originator_source cannot bypass C1.
	_, err := pool.Exec(ctx,
		`INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, originator_user_id, accountable_user_id, originator_source, trigger_evidence_kind)
		 VALUES ($1,$2,$3,'queued',$4,$4,'comment_source','issue_assignment')`,
		s.agentID, s.runtimeID, issue, s.userID)
	if !isUniqueViolation(err) {
		t.Fatalf("comment source with issue-assignment evidence must hit the controller fence, got: %v", err)
	}

	// 4) The rejected contender leaves exactly one active controller.
	n := countTasks(t, pool,
		`issue_id=$1 AND trigger_comment_id IS NULL AND is_leader_task=false AND trigger_evidence_kind='issue_assignment'
		 AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')`, issue)
	if n != 1 {
		t.Fatalf("exactly one issue_assignment controller expected, got %d", n)
	}
}
