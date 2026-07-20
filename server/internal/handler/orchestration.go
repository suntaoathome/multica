package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

type OrchestrationReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type IssueOrchestrationSummary struct {
	IssueID        string              `json:"issue_id"`
	IssueStatus    string              `json:"issue_status"`
	ExecutionState string              `json:"execution_state"`
	Reason         OrchestrationReason `json:"reason"`
	ActiveTasks    int64               `json:"active_tasks"`
	ReadyTasks     int64               `json:"ready_tasks"`
}

type SelfIterationCandidateResponse struct {
	ID            string `json:"id"`
	SnapshotHash  string `json:"snapshot_hash"`
	PolicyVersion int32  `json:"policy_version"`
	State         string `json:"state"`
	Title         string `json:"title"`
	Reason        string `json:"reason"`
	CreatedAt     string `json:"created_at"`
}

type ProjectOrchestrationSummary struct {
	ProjectID      string                           `json:"project_id"`
	Classification string                           `json:"classification"`
	Reason         OrchestrationReason              `json:"reason"`
	Issues         []IssueOrchestrationSummary      `json:"issues"`
	Candidates     []SelfIterationCandidateResponse `json:"self_iteration_candidates"`
}

// GetProjectOrchestrationSummary derives execution truth from task rows. It
// deliberately keeps issue_status as a separate field: in_progress without an
// active/ready/wait/review signal is reported as orchestration_fault.
func (h *Handler) GetProjectOrchestrationSummary(w http.ResponseWriter, r *http.Request) {
	projectID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "project id")
	if !ok {
		return
	}
	workspaceID, ok := parseUUIDOrBadRequest(w, h.resolveWorkspaceID(r), "workspace id")
	if !ok {
		return
	}
	if _, err := h.Queries.GetProjectInWorkspace(r.Context(), db.GetProjectInWorkspaceParams{ID: projectID, WorkspaceID: workspaceID}); err != nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	rows, err := h.DB.Query(r.Context(), `
		SELECT i.id::text, i.status,
		       count(t.id) FILTER (WHERE t.status IN ('queued','dispatched','running','waiting_local_directory','deferred')),
		       count(t.id) FILTER (WHERE t.status IN ('queued','deferred')),
		       bool_or(t.status IN ('dispatched','running','waiting_local_directory')),
		       bool_or(t.status = 'deferred')
		FROM issue i
		LEFT JOIN agent_task_queue t ON t.issue_id = i.id
		WHERE i.workspace_id=$1 AND i.project_id=$2
		GROUP BY i.id, i.status
		ORDER BY i.created_at, i.id`, workspaceID, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to derive project orchestration")
		return
	}
	defer rows.Close()

	resp := ProjectOrchestrationSummary{ProjectID: uuidToString(projectID), Issues: []IssueOrchestrationSummary{}, Candidates: []SelfIterationCandidateResponse{}}
	hasFault, hasExternalWait, hasTemporary, hasReady := false, false, false, false
	for rows.Next() {
		var item IssueOrchestrationSummary
		var hasExecution, hasDeferred *bool
		if err := rows.Scan(&item.IssueID, &item.IssueStatus, &item.ActiveTasks, &item.ReadyTasks, &hasExecution, &hasDeferred); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read project orchestration")
			return
		}
		switch {
		case item.IssueStatus == "done" || item.IssueStatus == "cancelled":
			item.ExecutionState, item.Reason = "complete", OrchestrationReason{"terminal", "Issue is terminal"}
		case hasExecution != nil && *hasExecution:
			item.ExecutionState, item.Reason = "running", OrchestrationReason{"active_execution", "An execution is dispatched or running"}
		case item.ReadyTasks > 0 && hasDeferred != nil && *hasDeferred:
			item.ExecutionState, item.Reason = "temporarily_not_ready", OrchestrationReason{"retry_backoff", "A deferred retry has a durable wake time"}
			hasTemporary = true
		case item.ReadyTasks > 0:
			item.ExecutionState, item.Reason = "ready", OrchestrationReason{"queued", "A durable task is ready for dispatch"}
			hasReady = true
		case item.IssueStatus == "in_review":
			item.ExecutionState, item.Reason = "waiting", OrchestrationReason{"review", "Waiting for independent review"}
			hasExternalWait = true
		case item.IssueStatus == "blocked":
			item.ExecutionState, item.Reason = "waiting", OrchestrationReason{"external_input", "Waiting for an explicit unblock action"}
			hasExternalWait = true
		case item.IssueStatus == "backlog":
			item.ExecutionState, item.Reason = "temporarily_not_ready", OrchestrationReason{"stage_gate", "Issue is parked behind a stage gate"}
			hasTemporary = true
		default:
			item.ExecutionState, item.Reason = "faulted", OrchestrationReason{"stale_in_progress", "Nonterminal issue has no active task, ready task, or declared wait"}
			hasFault = true
		}
		resp.Issues = append(resp.Issues, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read project orchestration")
		return
	}

	switch {
	case hasFault:
		resp.Classification, resp.Reason = "orchestration_fault", OrchestrationReason{"stale_in_progress", "At least one nonterminal issue has no durable execution path"}
	case hasExternalWait:
		resp.Classification, resp.Reason = "waiting_external", OrchestrationReason{"declared_wait", "All remaining work is waiting for review or external input"}
	case hasReady:
		resp.Classification, resp.Reason = "ready", OrchestrationReason{"ready_work", "At least one issue has durable ready work"}
	case hasTemporary:
		resp.Classification, resp.Reason = "temporarily_not_ready", OrchestrationReason{"scheduled_wake", "Remaining work has a stage gate or retry wake"}
	default:
		resp.Classification, resp.Reason = "complete", OrchestrationReason{"all_terminal", "All project issues are terminal"}
	}

	candidateRows, err := h.DB.Query(r.Context(), `
		SELECT id::text, snapshot_hash, policy_version, state, title, reason, created_at
		FROM self_iteration_candidate WHERE workspace_id=$1 AND project_id=$2
		ORDER BY created_at DESC, id DESC`, workspaceID, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list self-iteration candidates")
		return
	}
	defer candidateRows.Close()
	for candidateRows.Next() {
		var c SelfIterationCandidateResponse
		var createdAt pgtype.Timestamptz
		if err := candidateRows.Scan(&c.ID, &c.SnapshotHash, &c.PolicyVersion, &c.State, &c.Title, &c.Reason, &createdAt); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read self-iteration candidates")
			return
		}
		if createdAt.Valid {
			c.CreatedAt = createdAt.Time.UTC().Format("2006-01-02T15:04:05.999999Z07:00")
		}
		resp.Candidates = append(resp.Candidates, c)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ensureSelfIterationCandidate proposes one candidate when every issue in the
// project is terminal. The snapshot hash and partial unique index make replay
// of child-terminal events idempotent.
func (h *Handler) ensureSelfIterationCandidate(ctx context.Context, issue db.Issue) {
	if !issue.ProjectID.Valid {
		return
	}
	tx, err := h.TxStarter.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)
	var snapshot string
	err = tx.QueryRow(ctx, `
		SELECT md5(string_agg(id::text || ':' || status || ':' || updated_at::text, '|' ORDER BY id))
		FROM issue WHERE workspace_id=$1 AND project_id=$2
		HAVING count(*) > 0 AND count(*) FILTER (WHERE status NOT IN ('done','cancelled')) = 0`, issue.WorkspaceID, issue.ProjectID).Scan(&snapshot)
	if errors.Is(err, pgx.ErrNoRows) || snapshot == "" {
		return
	}
	if err != nil {
		return
	}
	var candidateID string
	err = tx.QueryRow(ctx, `
		INSERT INTO self_iteration_candidate (workspace_id, project_id, snapshot_hash, title, reason)
		VALUES ($1,$2,$3,'Plan the next project iteration','All project issues are terminal; review and accept before materializing new work')
		ON CONFLICT (project_id, snapshot_hash, policy_version) WHERE state IN ('proposed','accepted') DO NOTHING
		RETURNING id::text`, issue.WorkspaceID, issue.ProjectID, snapshot).Scan(&candidateID)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		return
	}
	if _, err = tx.Exec(ctx, `
		INSERT INTO orchestration_audit_event (workspace_id, project_id, issue_id, event_type, reason_code, payload)
		VALUES ($1,$2,$3,'project.iteration_candidate_proposed','all_terminal',jsonb_build_object('candidate_id',$4::text,'snapshot_hash',$5::text))`,
		issue.WorkspaceID, issue.ProjectID, issue.ID, candidateID, snapshot); err != nil {
		return
	}
	_ = tx.Commit(ctx)
}
