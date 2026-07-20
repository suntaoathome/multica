package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

const (
	readyRecoveryMaxAttempts = 5
	readyRecoveryBackoffSecs = 30
)

// ReadyRecoveryResult describes one coordinator pass. Contended is not a
// failure: another producer won the issue-assignment controller fence.
type ReadyRecoveryResult struct {
	Attempted int
	Recovered int
	Contended int
	Failed    int
}

// RecoverReadyIssueAssignments repairs executable agent-assigned issues that
// have lost their durable issue-assignment controller. The existing partial
// unique index is the commit-time authority, so daemon recovery, comments,
// Autopilot patrol, and multiple server replicas may race this scan safely.
// Mention and squad-leader tasks use different evidence and are never touched.
func (s *TaskService) RecoverReadyIssueAssignments(ctx context.Context, limit int) (ReadyRecoveryResult, error) {
	return s.recoverReadyIssueAssignments(ctx, limit, pgtype.UUID{})
}

func (s *TaskService) recoverReadyIssueAssignments(ctx context.Context, limit int, onlyIssueID pgtype.UUID) (ReadyRecoveryResult, error) {
	if s == nil || s.TxStarter == nil || s.Queries == nil {
		return ReadyRecoveryResult{}, errors.New("ready recovery requires database access")
	}
	if limit <= 0 {
		limit = 100
	}

	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		return ReadyRecoveryResult{}, fmt.Errorf("begin ready recovery scan: %w", err)
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, readyRecoveryCandidateSQL, readyRecoveryMaxAttempts, readyRecoveryBackoffSecs, limit, onlyIssueID)
	if err != nil {
		return ReadyRecoveryResult{}, fmt.Errorf("list ready recovery candidates: %w", err)
	}
	type candidate struct {
		id          string
		key         string
		workspaceID pgtype.UUID
		projectID   pgtype.UUID
	}
	var candidates []candidate
	for rows.Next() {
		var candidate candidate
		if err := rows.Scan(&candidate.id, &candidate.key, &candidate.workspaceID, &candidate.projectID); err != nil {
			rows.Close()
			return ReadyRecoveryResult{}, fmt.Errorf("scan ready recovery candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ReadyRecoveryResult{}, fmt.Errorf("iterate ready recovery candidates: %w", err)
	}
	rows.Close()
	// Persist the retry-budget debit before any enqueue. If this transaction
	// cannot write the durable attempt, the coordinator fails closed and does
	// not dispatch. Outcome audit below is observability only; bounded retry
	// correctness does not depend on that best-effort write.
	for _, candidate := range candidates {
		issueID, parseErr := util.ParseUUID(candidate.id)
		if parseErr != nil {
			return ReadyRecoveryResult{}, fmt.Errorf("parse locked recovery candidate: %w", parseErr)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO orchestration_audit_event
			  (workspace_id, project_id, issue_id, event_type, reason_code, payload)
			VALUES ($1,$2,$3,'execution.recovery_attempt','missing_issue_assignment',
			  jsonb_build_object('recovery_key',$4::text))`,
			candidate.workspaceID, candidate.projectID, issueID, candidate.key); err != nil {
			return ReadyRecoveryResult{}, fmt.Errorf("persist ready recovery attempt: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ReadyRecoveryResult{}, fmt.Errorf("commit ready recovery scan: %w", err)
	}

	result := ReadyRecoveryResult{Attempted: len(candidates)}
	for _, candidate := range candidates {
		issueID, err := util.ParseUUID(candidate.id)
		if err != nil {
			result.Failed++
			continue
		}
		issue, err := s.Queries.GetIssue(ctx, issueID)
		if err != nil {
			result.Failed++
			continue
		}
		task, err := s.EnqueueTaskForIssue(ctx, issue)
		if err == nil {
			result.Recovered++
			s.recordReadyRecoveryAudit(ctx, issue, candidate.key, "execution.recovered", "missing_issue_assignment", util.UUIDToString(task.ID), "")
			continue
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			result.Contended++
			s.recordReadyRecoveryAudit(ctx, issue, candidate.key, "execution.recovery_contended", "controller_exists", "", "")
			continue
		}
		result.Failed++
		s.recordReadyRecoveryAudit(ctx, issue, candidate.key, "execution.recovery_failed", "enqueue_failed", "", err.Error())
	}
	return result, nil
}

func (s *TaskService) recordReadyRecoveryAudit(ctx context.Context, issue db.Issue, recoveryKey, eventType, reasonCode, taskID, detail string) {
	if s.TxStarter == nil {
		return
	}
	tx, err := s.TxStarter.Begin(ctx)
	if err != nil {
		slog.Warn("ready recovery: begin audit failed", "issue_id", util.UUIDToString(issue.ID), "error", err)
		return
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `
		INSERT INTO orchestration_audit_event
		  (workspace_id, project_id, issue_id, event_type, reason_code, payload)
		VALUES ($1,$2,$3,$4,$5,jsonb_strip_nulls(jsonb_build_object(
		  'recovery_key',$6::text,'task_id',NULLIF($7::text,''),'detail',NULLIF($8::text,''))))`,
		issue.WorkspaceID, issue.ProjectID, issue.ID, eventType, reasonCode, recoveryKey, taskID, detail)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err != nil {
		slog.Warn("ready recovery: audit failed", "issue_id", util.UUIDToString(issue.ID), "event_type", eventType, "error", err)
	}
}

// readyRecoveryCandidateSQL deliberately admits only direct agent assignees.
// Squad recovery must continue through its leader authorization path.
const readyRecoveryCandidateSQL = `
WITH attempts AS (
  SELECT issue_id, payload->>'recovery_key' AS recovery_key,
         count(*) AS attempt_count,
         max(created_at) AS last_attempt
  FROM orchestration_audit_event
  WHERE event_type='execution.recovery_attempt'
  GROUP BY issue_id, payload->>'recovery_key'
)
SELECT i.id::text,
       md5(i.id::text || ':' || i.assignee_id::text || ':' || i.updated_at::text) AS recovery_key,
       i.workspace_id, i.project_id
FROM issue i
JOIN agent a ON a.id=i.assignee_id AND a.workspace_id=i.workspace_id
JOIN agent_runtime r ON r.id=a.runtime_id AND r.workspace_id=i.workspace_id
LEFT JOIN issue p ON p.id=i.parent_issue_id
LEFT JOIN attempts x ON x.issue_id=i.id
  AND x.recovery_key=md5(i.id::text || ':' || i.assignee_id::text || ':' || i.updated_at::text)
WHERE i.assignee_type='agent'
  AND ($4::uuid IS NULL OR i.id=$4::uuid)
  AND i.status IN ('todo','in_progress')
  AND NOT (i.metadata ? 'waiting_on')
  AND a.archived_at IS NULL
  AND r.status='online'
  AND (p.id IS NULL OR p.status NOT IN ('backlog','blocked','in_review','done','cancelled'))
  AND NOT EXISTS (
    SELECT 1 FROM issue lower_stage
    WHERE lower_stage.parent_issue_id=i.parent_issue_id
      AND i.stage IS NOT NULL
      AND lower_stage.stage < i.stage
      AND lower_stage.status NOT IN ('done','cancelled')
  )
  AND NOT EXISTS (
    SELECT 1 FROM agent_task_queue t
    WHERE t.issue_id=i.id
      AND t.trigger_comment_id IS NULL
      AND t.is_leader_task=FALSE
      AND t.trigger_evidence_kind='issue_assignment'
      AND t.status IN ('queued','dispatched','running','waiting_local_directory','deferred')
  )
  AND COALESCE(x.attempt_count,0) < $1
  AND (x.last_attempt IS NULL OR x.last_attempt < now() - ($2 * interval '1 second'))
ORDER BY i.updated_at, i.id
FOR UPDATE OF i SKIP LOCKED
LIMIT $3`
