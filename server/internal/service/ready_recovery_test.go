package service

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// readyRecoveryPool is intentionally non-skipping: this package is the
// production coordinator's PostgreSQL acceptance suite, and CI must fail if
// its required database is absent rather than silently dropping concurrency
// coverage.
func readyRecoveryPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Fatal("DATABASE_URL is required for ready recovery PostgreSQL tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect ready recovery PostgreSQL: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping ready recovery PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestRecoverReadyIssueAssignments_IdempotentServicePath(t *testing.T) {
	pool := readyRecoveryPool(t)
	ctx := context.Background()
	_, _, _, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE issue SET status='in_progress' WHERE id=$1`, issueID); err != nil {
		t.Fatalf("activate issue: %v", err)
	}
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	result, err := svc.recoverReadyIssueAssignments(ctx, 10, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("recover ready issue: %v", err)
	}
	if result.Recovered != 1 || result.Contended != 0 || result.Failed != 0 {
		t.Fatalf("first pass = %+v, want one recovery", result)
	}
	result, err = svc.recoverReadyIssueAssignments(ctx, 10, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("repeat recovery: %v", err)
	}
	if result != (ReadyRecoveryResult{}) {
		t.Fatalf("repeat pass = %+v, want no-op", result)
	}
	var controllers, audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND trigger_evidence_kind='issue_assignment' AND status='queued'`, issueID).Scan(&controllers); err != nil {
		t.Fatalf("count controllers: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orchestration_audit_event WHERE issue_id=$1 AND event_type='execution.recovered'`, issueID).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if controllers != 1 || audits != 1 {
		t.Fatalf("controllers=%d audits=%d, want 1/1", controllers, audits)
	}
}

func TestRecoverReadyIssueAssignments_ConcurrentCoordinatorsLeaveOneController(t *testing.T) {
	pool := readyRecoveryPool(t)
	ctx := context.Background()
	_, _, _, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE issue SET status='in_progress' WHERE id=$1`, issueID); err != nil {
		t.Fatalf("activate issue: %v", err)
	}
	svc := NewTaskService(db.New(pool), pool, nil, events.New())

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.recoverReadyIssueAssignments(ctx, 10, util.MustParseUUID(issueID))
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent recovery: %v", err)
		}
	}
	var controllers int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND trigger_evidence_kind='issue_assignment' AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')`, issueID).Scan(&controllers); err != nil {
		t.Fatalf("count controllers: %v", err)
	}
	if controllers != 1 {
		t.Fatalf("active controllers=%d, want exactly one", controllers)
	}
}

func TestRecoverReadyIssueAssignments_ExcludesDeclaredWaits(t *testing.T) {
	for _, status := range []string{"backlog", "blocked", "in_review", "done", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			pool := readyRecoveryPool(t)
			ctx := context.Background()
			_, _, _, issueID := seedAttributionFixture(t, pool)
			if _, err := pool.Exec(ctx, `UPDATE issue SET status=$1 WHERE id=$2`, status, issueID); err != nil {
				t.Fatalf("set status: %v", err)
			}
			svc := NewTaskService(db.New(pool), pool, nil, events.New())
			result, err := svc.recoverReadyIssueAssignments(ctx, 10, util.MustParseUUID(issueID))
			if err != nil {
				t.Fatalf("recover declared wait: %v", err)
			}
			if result != (ReadyRecoveryResult{}) {
				t.Fatalf("status %s recovered unexpectedly: %+v", status, result)
			}
		})
	}
	t.Run("metadata_waiting_on", func(t *testing.T) {
		pool := readyRecoveryPool(t)
		ctx := context.Background()
		_, _, _, issueID := seedAttributionFixture(t, pool)
		if _, err := pool.Exec(ctx, `UPDATE issue SET status='in_progress', metadata=jsonb_build_object('waiting_on','external approval') WHERE id=$1`, issueID); err != nil {
			t.Fatalf("set external wait: %v", err)
		}
		svc := NewTaskService(db.New(pool), pool, nil, events.New())
		result, err := svc.recoverReadyIssueAssignments(ctx, 10, util.MustParseUUID(issueID))
		if err != nil {
			t.Fatalf("recover external wait: %v", err)
		}
		if result != (ReadyRecoveryResult{}) {
			t.Fatalf("external wait recovered unexpectedly: %+v", result)
		}
	})
}

func TestRecoverReadyIssueAssignments_DurableAttemptBudgetStopsFailures(t *testing.T) {
	pool := readyRecoveryPool(t)
	ctx := context.Background()
	_, _, _, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE issue SET status='in_progress' WHERE id=$1`, issueID); err != nil {
		t.Fatalf("activate issue: %v", err)
	}
	fn := fmt.Sprintf("ready_recovery_fail_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.issue_id = '%s'::uuid THEN RAISE EXCEPTION 'injected enqueue failure'; END IF; RETURN NEW; END $$`, fn, issueID)); err != nil {
		t.Fatalf("create enqueue failure function: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON agent_task_queue FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn)); err != nil {
		t.Fatalf("create enqueue failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON agent_task_queue`, fn))
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})

	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	for attempt := 1; attempt <= readyRecoveryMaxAttempts; attempt++ {
		result, err := svc.recoverReadyIssueAssignments(ctx, 1, util.MustParseUUID(issueID))
		if err != nil {
			t.Fatalf("attempt %d scan: %v", attempt, err)
		}
		if result.Attempted != 1 || result.Failed != 1 {
			t.Fatalf("attempt %d result=%+v, want one durable failed attempt", attempt, result)
		}
		if _, err := pool.Exec(ctx, `UPDATE orchestration_audit_event SET created_at=now()-interval '1 minute' WHERE issue_id=$1 AND event_type='execution.recovery_attempt'`, issueID); err != nil {
			t.Fatalf("age retry attempt: %v", err)
		}
	}
	result, err := svc.recoverReadyIssueAssignments(ctx, 1, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("exhausted scan: %v", err)
	}
	if result != (ReadyRecoveryResult{}) {
		t.Fatalf("exhausted budget retried unexpectedly: %+v", result)
	}
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orchestration_audit_event WHERE issue_id=$1 AND event_type='execution.recovery_attempt'`, issueID).Scan(&attempts); err != nil {
		t.Fatalf("count durable attempts: %v", err)
	}
	if attempts != readyRecoveryMaxAttempts {
		t.Fatalf("durable attempts=%d, want %d", attempts, readyRecoveryMaxAttempts)
	}
}

func TestRecoverReadyIssueAssignments_AttemptAuditFailureFailsClosed(t *testing.T) {
	pool := readyRecoveryPool(t)
	ctx := context.Background()
	_, _, _, issueID := seedAttributionFixture(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE issue SET status='in_progress' WHERE id=$1`, issueID); err != nil {
		t.Fatalf("activate issue: %v", err)
	}
	fn := fmt.Sprintf("ready_recovery_audit_fail_%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.issue_id = '%s'::uuid AND NEW.event_type = 'execution.recovery_attempt' THEN RAISE EXCEPTION 'injected audit failure'; END IF; RETURN NEW; END $$`, fn, issueID)); err != nil {
		t.Fatalf("create audit failure function: %v", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON orchestration_audit_event FOR EACH ROW EXECUTE FUNCTION %s()`, fn, fn)); err != nil {
		t.Fatalf("create audit failure trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON orchestration_audit_event`, fn))
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})

	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	if _, err := svc.recoverReadyIssueAssignments(ctx, 1, util.MustParseUUID(issueID)); err == nil {
		t.Fatal("recovery succeeded despite durable attempt audit failure")
	}
	var tasks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, issueID).Scan(&tasks); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if tasks != 0 {
		t.Fatalf("audit failure dispatched %d tasks; want fail-closed zero", tasks)
	}
}
