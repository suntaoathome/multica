package orchestrationqa

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// integrationPool returns a pool against DATABASE_URL, or skips the test when
// Postgres is unreachable. Same contract as internal/scheduler and
// internal/handler so the whole suite behaves identically in CI.
//
// SAFETY: these tests INSERT and mutate rows. They must only ever point at a
// disposable test database. Never set DATABASE_URL to a production or Staging
// DSN when running this package.
func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://multica:multica@localhost:5432/multica?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Skipf("orchestrationqa integration tests require Postgres: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("orchestrationqa integration tests require Postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// scenario holds the ids seeded for one matrix run. All rows live under a
// unique workspace slug so concurrent CI runs and -race stay hermetic.
type scenario struct {
	pool          *pgxpool.Pool
	workspaceID   string
	workspaceSlug string
	userID        string
	runtimeID     string
	agentID       string // primary assignee agent
	agentBID      string // secondary agent (reassign / mention scenarios)
	nextIssueNum  int
}

// seedBase creates workspace + owner + one online runtime + two agents bound to
// it. Mirrors handler/handler_test.go:setupHandlerTestFixture but scoped to a
// unique slug per test and returning both agents for the reassign matrix rows.
func seedBase(t *testing.T, pool *pgxpool.Pool) *scenario {
	t.Helper()
	ctx := context.Background()
	slug := "qa-orch-" + uuid.NewString()[:12]
	email := slug + "@multica-tests.invalid"

	s := &scenario{pool: pool, workspaceSlug: slug, nextIssueNum: 1}

	if err := pool.QueryRow(ctx,
		`INSERT INTO "user" (name, email) VALUES ($1, $2) RETURNING id`,
		"QA Orchestration User", email,
	).Scan(&s.userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspace (name, slug, description, issue_prefix)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		"QA Orchestration", slug, "orchestrationqa acceptance harness", "QAO",
	).Scan(&s.workspaceID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() {
		// ON DELETE CASCADE from workspace cleans runtime/agent/issue/task;
		// the user row is independent, so drop it explicitly.
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspace WHERE id = $1`, s.workspaceID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM "user" WHERE id = $1`, s.userID)
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO member (workspace_id, user_id, role) VALUES ($1, $2, 'owner')`,
		s.workspaceID, s.userID,
	); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO agent_runtime
		   (workspace_id, daemon_id, name, runtime_mode, provider, status, device_info, metadata, owner_id, last_seen_at)
		 VALUES ($1, NULL, $2, 'cloud', $3, 'online', $4, '{}'::jsonb, $5, now())
		 RETURNING id`,
		s.workspaceID, "QA Runtime", "qa_orch_runtime", "qa runtime", s.userID,
	).Scan(&s.runtimeID); err != nil {
		t.Fatalf("seed runtime: %v", err)
	}
	s.agentID = s.seedAgent(t, "QA Agent A")
	s.agentBID = s.seedAgent(t, "QA Agent B")
	return s
}

func (s *scenario) seedAgent(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := s.pool.QueryRow(context.Background(),
		`INSERT INTO agent
		   (workspace_id, name, description, runtime_mode, runtime_config,
		    runtime_id, visibility, permission_mode, max_concurrent_tasks, owner_id)
		 VALUES ($1, $2, '', 'cloud', '{}'::jsonb, $3, 'workspace', 'public_to', 1, $4)
		 RETURNING id`,
		s.workspaceID, name, s.runtimeID, s.userID,
	).Scan(&id); err != nil {
		t.Fatalf("seed agent %q: %v", name, err)
	}
	return id
}

// setRuntimeStatus flips the seeded runtime online/offline for the
// external-blocker matrix row (M2).
func (s *scenario) setRuntimeStatus(t *testing.T, status string) {
	t.Helper()
	last := "now()"
	if status == "offline" {
		last = "now() - interval '10 minutes'"
	}
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE agent_runtime SET status = $1, last_seen_at = `+last+` WHERE id = $2`,
		status, s.runtimeID,
	); err != nil {
		t.Fatalf("set runtime status: %v", err)
	}
}

// seedIssue inserts an issue with a per-workspace unique number.
func (s *scenario) seedIssue(t *testing.T, status, assigneeType, assigneeID, parentID string) string {
	t.Helper()
	num := s.nextIssueNum
	s.nextIssueNum++
	var (
		id     string
		aType  any = nil
		aID    any = nil
		parent any = nil
	)
	if assigneeType != "" {
		aType = assigneeType
		aID = assigneeID
	}
	if parentID != "" {
		parent = parentID
	}
	if err := s.pool.QueryRow(context.Background(),
		`INSERT INTO issue
		   (workspace_id, title, status, priority, assignee_type, assignee_id,
		    creator_type, creator_id, parent_issue_id, number, position)
		 VALUES ($1, $2, $3, 'high', $4, $5, 'member', $6, $7, $8, $9)
		 RETURNING id`,
		s.workspaceID, fmt.Sprintf("QA issue #%d", num), status,
		aType, aID, s.userID, parent, num, float64(num),
	).Scan(&id); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	return id
}

// seedTask inserts an agent_task_queue row in a chosen state. Attribution
// columns are stamped like the production issue-assignment enqueue path:
// attribution source describes the accountable actor, while evidence kind
// identifies the controller trigger used by the issue-wide active-task fence.
func (s *scenario) seedTask(t *testing.T, issueID, agentID, status string) string {
	t.Helper()
	var id string
	err := s.pool.QueryRow(context.Background(),
		`INSERT INTO agent_task_queue
		   (agent_id, runtime_id, issue_id, status,
		    originator_user_id, accountable_user_id, originator_source,
		    trigger_evidence_kind)
		 VALUES ($1, $2, $3, $4, $5, $5, 'direct_human', 'issue_assignment')
		 RETURNING id`,
		agentID, s.runtimeID, issueID, status, s.userID,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed task (status=%s): %v", status, err)
	}
	return id
}

// ---- Invariant assertions (the durable contract each matrix row checks) ----

func countTasks(t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM agent_task_queue WHERE `+where, args...,
	).Scan(&n); err != nil {
		t.Fatalf("count tasks (%s): %v", where, err)
	}
	return n
}

// assertSinglePendingAuthorPerIssueAgent enforces idx_one_pending_task_per_issue_agent:
// at most one queued|dispatched task per (issue, agent). This is the backstop
// M4 (retry) and M5 (comment coalescing) lean on.
func assertSinglePendingAuthorPerIssueAgent(t *testing.T, pool *pgxpool.Pool, issueID, agentID string) {
	t.Helper()
	n := countTasks(t, pool,
		`issue_id = $1 AND agent_id = $2 AND status IN ('queued','dispatched')`,
		issueID, agentID)
	if n > 1 {
		t.Errorf("single-pending-author violated for (issue=%s, agent=%s): got %d queued/dispatched rows, want <=1",
			issueID, agentID, n)
	}
}

// assertAtMostOneActiveAuthorForIssue is the issue-wide execution invariant.
func assertAtMostOneActiveAuthorForIssue(t *testing.T, pool *pgxpool.Pool, issueID string) {
	t.Helper()
	n := countTasks(t, pool,
		`issue_id = $1 AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')`,
		issueID)
	if n > 1 {
		t.Errorf("single-active-author-per-issue violated for issue=%s: got %d in-flight rows across agents, want <=1",
			issueID, n)
	}
}

// liveTaskExistsInSubtree reports whether the issue or any descendant (via
// parent_issue_id) has an in-flight task. Backs the M10 "no fake in_progress"
// liveness predicate: an issue presented as actively-working must have a live
// task somewhere in its subtree.
func liveTaskExistsInSubtree(t *testing.T, pool *pgxpool.Pool, rootIssueID string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(context.Background(),
		`WITH RECURSIVE subtree AS (
		     SELECT id FROM issue WHERE id = $1
		     UNION ALL
		     SELECT c.id FROM issue c JOIN subtree p ON c.parent_issue_id = p.id
		 )
		 SELECT EXISTS (
		     SELECT 1 FROM agent_task_queue q
		     JOIN subtree s ON q.issue_id = s.id
		     WHERE q.status IN ('queued','dispatched','running','waiting_local_directory')
		 )`, rootIssueID,
	).Scan(&exists)
	if err != nil {
		t.Fatalf("liveTaskExistsInSubtree: %v", err)
	}
	return exists
}

// backdate moves a timestamp column into the past so a staleness/backoff
// threshold is crossed without sleeping (fault-injection principle #2).
func backdate(t *testing.T, pool *pgxpool.Pool, table, col, idCol, id string, d time.Duration) {
	t.Helper()
	secs := int(d.Seconds())
	sql := fmt.Sprintf(
		`UPDATE %s SET %s = now() - make_interval(secs => $1) WHERE %s = $2`,
		table, col, idCol)
	if _, err := pool.Exec(context.Background(), sql, secs, id); err != nil {
		t.Fatalf("backdate %s.%s: %v", table, col, err)
	}
}
