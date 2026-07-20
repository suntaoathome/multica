package handler

import (
	"context"
	"encoding/json"
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
	RunningSlots   int64               `json:"running_slots"`
	Capacity       int64               `json:"capacity"`
	LastEvent      *OrchestrationEvent `json:"last_event"`
	RecoveryAction *RecoveryAction     `json:"recovery_action"`
}

type OrchestrationEvent struct {
	Type       string `json:"type"`
	ReasonCode string `json:"reason_code"`
	CreatedAt  string `json:"created_at"`
}

type RecoveryAction struct {
	Action     string `json:"action"`
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason"`
	SideEffect string `json:"side_effect"`
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
	RunningSlots   int64                            `json:"running_slots"`
	Capacity       int64                            `json:"capacity"`
	LastEvent      *OrchestrationEvent              `json:"last_event"`
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
	member, err := h.Queries.GetMemberByUserAndWorkspace(r.Context(), db.GetMemberByUserAndWorkspaceParams{
		UserID: parseUUID(requestUserID(r)), WorkspaceID: workspaceID,
	})
	canRecover := err == nil && roleAllowed(member.Role, "owner", "admin")

	rows, err := h.DB.Query(r.Context(), `
		SELECT i.id::text, i.status,
		       count(t.id) FILTER (WHERE t.status IN ('queued','dispatched','running','waiting_local_directory','deferred')),
		       count(t.id) FILTER (WHERE t.status IN ('queued','deferred')),
		       bool_or(t.status IN ('dispatched','running','waiting_local_directory')),
		       bool_or(t.status = 'deferred'),
		       count(t.id) FILTER (WHERE t.status IN ('dispatched','running','waiting_local_directory')),
		       COALESCE(max(a.max_concurrent_tasks), 0),
		       le.event_type, le.reason_code, le.created_at,
		       (i.assignee_type IN ('agent','squad') AND i.assignee_id IS NOT NULL)
		FROM issue i
		LEFT JOIN agent_task_queue t ON t.issue_id = i.id
		LEFT JOIN agent a ON a.id = COALESCE(t.agent_id, CASE WHEN i.assignee_type = 'agent' THEN i.assignee_id END)
		LEFT JOIN LATERAL (
			SELECT event_type, reason_code, created_at FROM (
				SELECT 'task.' || t2.status AS event_type, COALESCE(t2.failure_reason, t2.status) AS reason_code,
				       COALESCE(t2.completed_at, t2.started_at, t2.dispatched_at, t2.created_at) AS created_at
				FROM agent_task_queue t2 WHERE t2.issue_id = i.id
				UNION ALL
				SELECT event_type, reason_code, created_at FROM orchestration_audit_event e WHERE e.issue_id = i.id
			) events ORDER BY created_at DESC LIMIT 1
		) le ON true
		WHERE i.workspace_id=$1 AND i.project_id=$2
		GROUP BY i.id, i.status, i.assignee_type, i.assignee_id, le.event_type, le.reason_code, le.created_at
		ORDER BY i.created_at, i.id`, workspaceID, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to derive project orchestration")
		return
	}
	defer rows.Close()

	resp := ProjectOrchestrationSummary{ProjectID: uuidToString(projectID), Issues: []IssueOrchestrationSummary{}, Candidates: []SelfIterationCandidateResponse{}}
	hasFault, hasExternalWait, hasTemporary, hasReady, hasActive := false, false, false, false, false
	for rows.Next() {
		var item IssueOrchestrationSummary
		var hasExecution, hasDeferred *bool
		var eventType, eventReason *string
		var eventAt pgtype.Timestamptz
		var hasRunnableAssignee bool
		if err := rows.Scan(&item.IssueID, &item.IssueStatus, &item.ActiveTasks, &item.ReadyTasks, &hasExecution, &hasDeferred,
			&item.RunningSlots, &item.Capacity, &eventType, &eventReason, &eventAt, &hasRunnableAssignee); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to read project orchestration")
			return
		}
		if eventAt.Valid && eventType != nil && eventReason != nil {
			item.LastEvent = &OrchestrationEvent{Type: *eventType, ReasonCode: *eventReason, CreatedAt: eventAt.Time.UTC().Format("2006-01-02T15:04:05.999999Z07:00")}
		}
		switch {
		case item.IssueStatus == "done" || item.IssueStatus == "cancelled":
			item.ExecutionState, item.Reason = "complete", OrchestrationReason{"terminal", "Issue is terminal"}
		case hasExecution != nil && *hasExecution:
			item.ExecutionState, item.Reason = "running", OrchestrationReason{"active_execution", "An execution is dispatched or running"}
			hasActive = true
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
			allowed := canRecover && hasRunnableAssignee
			actionReason := "Workspace owner or admin permission is required"
			if canRecover && !hasRunnableAssignee {
				actionReason = "A runnable agent or squad assignee is required"
			} else if allowed {
				actionReason = "Issue can be resumed"
			}
			item.RecoveryAction = &RecoveryAction{
				Action: "resume_stale_issue", Allowed: allowed,
				Reason:     actionReason,
				SideEffect: "Queues one assignment run for the current assignee; concurrent and repeated requests are deduplicated",
			}
			hasFault = true
		}
		if item.LastEvent != nil && (resp.LastEvent == nil || item.LastEvent.CreatedAt > resp.LastEvent.CreatedAt) {
			resp.LastEvent = item.LastEvent
		}
		resp.Issues = append(resp.Issues, item)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read project orchestration")
		return
	}
	if err := h.DB.QueryRow(r.Context(), `
		WITH project_agents AS (
			SELECT DISTINCT t.agent_id AS id FROM agent_task_queue t JOIN issue i ON i.id=t.issue_id
			WHERE i.workspace_id=$1 AND i.project_id=$2
			UNION
			SELECT DISTINCT i.assignee_id FROM issue i
			WHERE i.workspace_id=$1 AND i.project_id=$2 AND i.assignee_type='agent' AND i.assignee_id IS NOT NULL
		)
		SELECT
			(SELECT count(*) FROM agent_task_queue t JOIN issue i ON i.id=t.issue_id WHERE i.workspace_id=$1 AND i.project_id=$2 AND t.status IN ('dispatched','running','waiting_local_directory')),
			COALESCE((SELECT sum(a.max_concurrent_tasks) FROM agent a JOIN project_agents pa ON pa.id=a.id),0)`, workspaceID, projectID).Scan(&resp.RunningSlots, &resp.Capacity); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to derive project capacity")
		return
	}

	switch {
	case hasFault:
		resp.Classification, resp.Reason = "orchestration_fault", OrchestrationReason{"stale_in_progress", "At least one nonterminal issue has no durable execution path"}
	case hasActive:
		resp.Classification, resp.Reason = "running", OrchestrationReason{"active_execution", "At least one issue has an active execution"}
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

type OrchestrationRecoveryRequest struct {
	IssueID string `json:"issue_id"`
	Action  string `json:"action"`
}

type OrchestrationRecoveryResponse struct {
	Applied bool   `json:"applied"`
	Reason  string `json:"reason"`
}

// RecoverProjectOrchestration resumes a stale assignment. The assignment-task
// unique index is the concurrency backstop: a request racing with another
// recovery or a normal trigger converges on one active assignment run.
func (h *Handler) RecoverProjectOrchestration(w http.ResponseWriter, r *http.Request) {
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
	if _, ok := h.requireWorkspaceRole(w, r, uuidToString(workspaceID), "project not found", "owner", "admin"); !ok {
		return
	}
	var req OrchestrationRecoveryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Action != "resume_stale_issue" {
		writeError(w, http.StatusBadRequest, "invalid recovery request")
		return
	}
	issueID, ok := parseUUIDOrBadRequest(w, req.IssueID, "issue_id")
	if !ok {
		return
	}
	issue, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{ID: issueID, WorkspaceID: workspaceID})
	if err != nil || !issue.ProjectID.Valid || issue.ProjectID != projectID {
		writeError(w, http.StatusNotFound, "issue not found in project")
		return
	}
	if issue.Status == "done" || issue.Status == "cancelled" || issue.Status == "blocked" || issue.Status == "backlog" || issue.Status == "in_review" || !issue.AssigneeType.Valid || !issue.AssigneeID.Valid {
		writeError(w, http.StatusConflict, "issue is not recoverable")
		return
	}
	var active int64
	if err := h.DB.QueryRow(r.Context(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')`, issue.ID).Scan(&active); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to inspect recovery state")
		return
	}
	if active > 0 {
		writeJSON(w, http.StatusOK, OrchestrationRecoveryResponse{Applied: false, Reason: "active_execution_exists"})
		return
	}
	actorType, actorID := h.resolveActor(r, requestUserID(r), uuidToString(workspaceID))
	switch issue.AssigneeType.String {
	case "agent":
		if _, err := h.TaskService.EnqueueTaskForIssueWithHandoff(r.Context(), issue, "Resume stale project orchestration", memberActorUserID(actorType, actorID)); err != nil {
			// A concurrent insert that won the active-assignment unique index is
			// an idempotent success from the caller's perspective.
			if h.DB.QueryRow(r.Context(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')`, issue.ID).Scan(&active) == nil && active > 0 {
				writeJSON(w, http.StatusOK, OrchestrationRecoveryResponse{Applied: false, Reason: "active_execution_exists"})
				return
			}
			writeError(w, http.StatusConflict, "assignee cannot be resumed")
			return
		}
	case "squad":
		h.enqueueSquadLeaderTask(r.Context(), issue, pgtype.UUID{}, actorType, actorID, "Resume stale project orchestration")
	default:
		writeError(w, http.StatusConflict, "issue is not recoverable")
		return
	}
	if h.DB.QueryRow(r.Context(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1 AND status IN ('queued','dispatched','running','waiting_local_directory','deferred')`, issue.ID).Scan(&active) != nil || active == 0 {
		writeError(w, http.StatusConflict, "assignee cannot be resumed")
		return
	}
	_, _ = h.DB.Exec(r.Context(), `INSERT INTO orchestration_audit_event (workspace_id,project_id,issue_id,event_type,reason_code,payload) VALUES ($1,$2,$3,'execution.recovery_requested','stale_in_progress',jsonb_build_object('actor_type',$4::text,'actor_id',$5::text))`, workspaceID, projectID, issue.ID, actorType, actorID)
	writeJSON(w, http.StatusOK, OrchestrationRecoveryResponse{Applied: true, Reason: "assignment_run_queued"})
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
