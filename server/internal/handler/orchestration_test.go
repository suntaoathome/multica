package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

func TestEnsureSelfIterationCandidateIsSnapshotIdempotent(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	var projectID, issueID string
	if err := testPool.QueryRow(ctx, `INSERT INTO project (workspace_id,title) VALUES ($1,'orchestration candidate test') RETURNING id`, testWorkspaceID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM orchestration_audit_event WHERE project_id=$1`, projectID)
		_, _ = testPool.Exec(ctx, `DELETE FROM self_iteration_candidate WHERE project_id=$1`, projectID)
		_, _ = testPool.Exec(ctx, `DELETE FROM issue WHERE project_id=$1`, projectID)
		_, _ = testPool.Exec(ctx, `DELETE FROM project WHERE id=$1`, projectID)
	})
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id,project_id,title,status,priority,creator_type,creator_id,number,position)
		VALUES ($1,$2,'terminal work','done','none','member',$3,920107,0) RETURNING id`, testWorkspaceID, projectID, testUserID).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	testHandler.ensureSelfIterationCandidate(ctx, issue)
	testHandler.ensureSelfIterationCandidate(ctx, issue)
	var candidates, events int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM self_iteration_candidate WHERE project_id=$1`, projectID).Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM orchestration_audit_event WHERE project_id=$1 AND event_type='project.iteration_candidate_proposed'`, projectID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if candidates != 1 || events != 1 {
		t.Fatalf("candidate/event counts = %d/%d, want 1/1", candidates, events)
	}
}

func TestProjectOrchestrationSummaryReportsStaleInProgress(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	var projectID string
	if err := testPool.QueryRow(ctx, `INSERT INTO project (workspace_id,title) VALUES ($1,'orchestration summary test') RETURNING id`, testWorkspaceID).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM issue WHERE project_id=$1`, projectID)
		_, _ = testPool.Exec(ctx, `DELETE FROM project WHERE id=$1`, projectID)
	})
	if _, err := testPool.Exec(ctx, `
		INSERT INTO issue (workspace_id,project_id,title,status,priority,creator_type,creator_id,number,position)
		VALUES ($1,$2,'stale work','in_progress','none','member',$3,920108,0)`, testWorkspaceID, projectID, testUserID); err != nil {
		t.Fatal(err)
	}
	req := withURLParam(newRequest("GET", "/api/projects/"+projectID+"/orchestration-summary", nil), "id", projectID)
	w := httptest.NewRecorder()
	testHandler.GetProjectOrchestrationSummary(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got ProjectOrchestrationSummary
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Classification != "orchestration_fault" || len(got.Issues) != 1 || got.Issues[0].Reason.Code != "stale_in_progress" {
		t.Fatalf("unexpected summary: %+v", got)
	}
}
