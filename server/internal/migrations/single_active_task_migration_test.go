package migrations

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigration202SupersedesDuplicateIssueAssignmentControllers(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to Postgres: %v", err)
	}
	defer pool.Close()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire Postgres connection: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `
		CREATE TEMP TABLE agent_task_queue (
			id UUID PRIMARY KEY,
			issue_id UUID,
			status TEXT,
			trigger_comment_id UUID,
			is_leader_task BOOLEAN,
			originator_source TEXT,
			trigger_evidence_kind TEXT,
			created_at TIMESTAMPTZ,
			completed_at TIMESTAMPTZ,
			failure_reason TEXT,
			error TEXT,
			prepare_lease_expires_at TIMESTAMPTZ
		)
	`); err != nil {
		t.Fatalf("create temporary queue table: %v", err)
	}

	const issueID = "00000000-0000-0000-0000-000000000201"
	if _, err := conn.Exec(ctx, `
		INSERT INTO agent_task_queue
			(id, issue_id, status, is_leader_task, originator_source,
			 trigger_evidence_kind, created_at)
		VALUES
			('00000000-0000-0000-0000-000000000211', $1, 'running', false,
			 'direct_human', 'issue_assignment', now() - interval '2 minutes'),
			('00000000-0000-0000-0000-000000000212', $1, 'queued', false,
			 'owner_fallback', 'issue_assignment', now() - interval '1 minute'),
			('00000000-0000-0000-0000-000000000213', $1, 'queued', false,
			 'comment_source', 'comment', now())
	`, issueID); err != nil {
		t.Fatalf("seed duplicate controllers: %v", err)
	}

	migration := readMigrationFile(t, "202_drop_per_agent_active_task_index.up.sql")
	backfill, _, ok := strings.Cut(migration, "\n\nDROP INDEX")
	if !ok {
		t.Fatal("migration 202 no longer has an independently testable backfill statement")
	}
	if strings.Contains(backfill, "originator_source = 'issue_assignment'") ||
		!strings.Contains(backfill, "trigger_evidence_kind = 'issue_assignment'") {
		t.Fatalf("migration 202 backfill uses the wrong issue-assignment discriminator:\n%s", backfill)
	}
	if _, err := conn.Exec(ctx, backfill); err != nil {
		t.Fatalf("apply migration 202 backfill: %v", err)
	}

	var activeControllers, superseded, unrelated int
	if err := conn.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE trigger_evidence_kind='issue_assignment' AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')),
			count(*) FILTER (WHERE trigger_evidence_kind='issue_assignment' AND status='cancelled' AND failure_reason='orchestration_superseded'),
			count(*) FILTER (WHERE trigger_evidence_kind='comment' AND status='queued')
		FROM agent_task_queue WHERE issue_id=$1
	`, issueID).Scan(&activeControllers, &superseded, &unrelated); err != nil {
		t.Fatalf("inspect migration 202 result: %v", err)
	}
	if activeControllers != 1 || superseded != 1 || unrelated != 1 {
		t.Fatalf("migration 202 result: active controllers=%d superseded=%d unrelated=%d; want 1/1/1",
			activeControllers, superseded, unrelated)
	}
}
