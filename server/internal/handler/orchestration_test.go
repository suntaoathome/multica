package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
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

func TestProjectOrchestrationSummaryReportsRunningProject(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	projectID, agentID, issueID := createOrchestrationRecoveryFixture(t, 920109, false)
	if _, err := testPool.Exec(ctx, `
		INSERT INTO agent_task_queue (agent_id,runtime_id,issue_id,status,priority,trigger_evidence_kind)
		VALUES ($1,$2,$3,'running',0,'issue_assignment')`, agentID, testRuntimeID, issueID); err != nil {
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
	if got.Classification != "running" || got.Reason.Code != "active_execution" || got.RunningSlots != 1 {
		t.Fatalf("running summary = %+v, want running/active_execution with one slot", got)
	}
}

func TestRecoverProjectOrchestrationAuthorizationAndIdempotency(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	t.Run("owner agent recovery is idempotent", func(t *testing.T) {
		projectID, _, issueID := createOrchestrationRecoveryFixture(t, 920110, false)
		first := recoverOrchestration(t, testUserID, projectID, issueID)
		if first.Code != http.StatusOK {
			t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
		}
		var firstBody OrchestrationRecoveryResponse
		if err := json.NewDecoder(first.Body).Decode(&firstBody); err != nil {
			t.Fatal(err)
		}
		if !firstBody.Applied || firstBody.Reason != "assignment_run_queued" {
			t.Fatalf("first response=%+v", firstBody)
		}

		second := recoverOrchestration(t, testUserID, projectID, issueID)
		var secondBody OrchestrationRecoveryResponse
		if err := json.NewDecoder(second.Body).Decode(&secondBody); err != nil {
			t.Fatal(err)
		}
		if second.Code != http.StatusOK || secondBody.Applied || secondBody.Reason != "active_execution_exists" {
			t.Fatalf("second status=%d response=%+v", second.Code, secondBody)
		}
		assertOrchestrationTaskCount(t, issueID, 1)
	})

	t.Run("admin squad recovery succeeds", func(t *testing.T) {
		adminID := createPermissionTestAdmin(t, "orchestration-admin@multica.test")
		projectID, _, issueID := createOrchestrationRecoveryFixture(t, 920111, true)
		w := recoverOrchestration(t, adminID, projectID, issueID)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var body OrchestrationRecoveryResponse
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !body.Applied {
			t.Fatalf("response=%+v, want applied", body)
		}
		assertOrchestrationTaskCount(t, issueID, 1)
	})

	t.Run("plain member is forbidden", func(t *testing.T) {
		memberID := createPermissionTestMember(t, "orchestration-member@multica.test")
		projectID, _, issueID := createOrchestrationRecoveryFixture(t, 920112, false)
		w := recoverOrchestration(t, memberID, projectID, issueID)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		assertOrchestrationTaskCount(t, issueID, 0)
	})

	t.Run("cross project issue is rejected", func(t *testing.T) {
		projectID, _, _ := createOrchestrationRecoveryFixture(t, 920113, false)
		_, _, otherIssueID := createOrchestrationRecoveryFixture(t, 920114, false)
		w := recoverOrchestration(t, testUserID, projectID, otherIssueID)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		assertOrchestrationTaskCount(t, otherIssueID, 0)
	})

	t.Run("concurrent requests converge", func(t *testing.T) {
		projectID, _, issueID := createOrchestrationRecoveryFixture(t, 920115, false)
		const callers = 8
		codes := make(chan int, callers)
		var wg sync.WaitGroup
		for range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes <- recoverOrchestration(t, testUserID, projectID, issueID).Code
			}()
		}
		wg.Wait()
		close(codes)
		for code := range codes {
			if code != http.StatusOK {
				t.Errorf("concurrent status=%d, want 200", code)
			}
		}
		assertOrchestrationTaskCount(t, issueID, 1)
	})
}

func createOrchestrationRecoveryFixture(t *testing.T, number int, squadAssigned bool) (projectID, agentID, issueID string) {
	t.Helper()
	ctx := context.Background()
	if err := testPool.QueryRow(ctx, `INSERT INTO project (workspace_id,title) VALUES ($1,$2) RETURNING id`, testWorkspaceID, "orchestration recovery fixture").Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (workspace_id,name,description,runtime_mode,runtime_config,runtime_id,visibility,permission_mode,max_concurrent_tasks,owner_id)
		VALUES ($1,$2,'','cloud','{}'::jsonb,$3,'workspace','public_to',6,$4) RETURNING id`,
		testWorkspaceID, fmt.Sprintf("orchestration recovery agent %d", number), testRuntimeID, testUserID).Scan(&agentID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO agent_invocation_target (agent_id,target_type,target_id) VALUES ($1,'workspace',$2)`, agentID, testWorkspaceID); err != nil {
		t.Fatal(err)
	}
	assigneeType, assigneeID := "agent", agentID
	var squadID string
	if squadAssigned {
		if err := testPool.QueryRow(ctx, `INSERT INTO squad (workspace_id,name,description,leader_id,creator_id) VALUES ($1,$2,'',$3,$4) RETURNING id`,
			testWorkspaceID, "orchestration recovery squad", agentID, testUserID).Scan(&squadID); err != nil {
			t.Fatal(err)
		}
		assigneeType, assigneeID = "squad", squadID
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id,project_id,title,status,priority,creator_type,creator_id,assignee_type,assignee_id,number,position)
		VALUES ($1,$2,'stale assigned work','in_progress','none','member',$3,$4,$5,$6,0) RETURNING id`,
		testWorkspaceID, projectID, testUserID, assigneeType, assigneeID, number).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM orchestration_audit_event WHERE project_id=$1`, projectID)
		_, _ = testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id=$1`, issueID)
		_, _ = testPool.Exec(ctx, `DELETE FROM issue WHERE id=$1`, issueID)
		if squadID != "" {
			_, _ = testPool.Exec(ctx, `DELETE FROM squad WHERE id=$1`, squadID)
		}
		_, _ = testPool.Exec(ctx, `DELETE FROM agent_invocation_target WHERE agent_id=$1`, agentID)
		_, _ = testPool.Exec(ctx, `DELETE FROM agent WHERE id=$1`, agentID)
		_, _ = testPool.Exec(ctx, `DELETE FROM project WHERE id=$1`, projectID)
	})
	return projectID, agentID, issueID
}

func recoverOrchestration(t *testing.T, userID, projectID, issueID string) *httptest.ResponseRecorder {
	t.Helper()
	req := newRequest("POST", "/api/projects/"+projectID+"/orchestration-recovery", OrchestrationRecoveryRequest{
		IssueID: issueID, Action: "resume_stale_issue",
	})
	req.Header.Set("X-User-ID", userID)
	req = withURLParam(req, "id", projectID)
	w := httptest.NewRecorder()
	testHandler.RecoverProjectOrchestration(w, req)
	return w
}

func assertOrchestrationTaskCount(t *testing.T, issueID string, want int) {
	t.Helper()
	var got int
	if err := testPool.QueryRow(context.Background(), `SELECT count(*) FROM agent_task_queue WHERE issue_id=$1`, issueID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("task count=%d, want %d", got, want)
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
	action := got.Issues[0].RecoveryAction
	if action == nil || action.Action != "resume_stale_issue" || action.Allowed {
		t.Fatalf("recovery action = %+v, want disabled resume_stale_issue for unassigned issue", action)
	}
	if got.RunningSlots != 0 || got.Capacity != 0 {
		t.Fatalf("slots = %d/%d, want 0/0", got.RunningSlots, got.Capacity)
	}
}
