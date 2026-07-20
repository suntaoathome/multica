package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

// TestMemberCommentDoesNotCreateSecondTaskWhileDirectRunIsActive captures the
// orchestration gap behind AI-104. A plain member comment is additional input
// for the issue's current execution, not authority to start a second controller
// for the same (issue, agent) pair.
//
// This intentionally fails before the orchestration fix: pending-task routing
// only sees queued/dispatched rows, so a running row is missed and the comment
// path inserts another queued task. Keeping the assertion at the public comment
// trigger boundary makes the regression deterministic without sleeps.
func TestMemberCommentDoesNotCreateSecondTaskWhileDirectRunIsActive(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "AI-104 active direct run", nil)

	var runtimeID, issueID, taskID, commentID string
	if err := testPool.QueryRow(ctx, `SELECT runtime_id FROM agent WHERE id = $1`, agentID).Scan(&runtimeID); err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position, assignee_type, assignee_id)
		VALUES ($1, 'AI-104 duplicate dispatch fixture', 'in_progress', 'none', $2, 'member', 910104, 0, 'agent', $3)
		RETURNING id`, testWorkspaceID, testUserID, agentID).Scan(&issueID); err != nil {
		t.Fatalf("create issue: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID) })
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, status, priority, started_at)
		VALUES ($1, $2, $3, 'running', 0, now()) RETURNING id`, agentID, runtimeID, issueID).Scan(&taskID); err != nil {
		t.Fatalf("create running task: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID) })
	if err := testPool.QueryRow(ctx, `
		INSERT INTO comment (issue_id, workspace_id, author_type, author_id, content, type)
		VALUES ($1, $2, 'member', $3, 'additional context, do not start another controller', 'comment')
		RETURNING id`, issueID, testWorkspaceID, testUserID).Scan(&commentID); err != nil {
		t.Fatalf("create comment: %v", err)
	}
	t.Cleanup(func() { _, _ = testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id = $1`, issueID) })

	issue, err := testHandler.Queries.GetIssue(ctx, util.MustParseUUID(issueID))
	if err != nil {
		t.Fatalf("load issue: %v", err)
	}
	comment, err := testHandler.Queries.GetComment(ctx, util.MustParseUUID(commentID))
	if err != nil {
		t.Fatalf("load comment: %v", err)
	}
	testHandler.triggerTasksForComment(ctx, issue, comment, nil, "member", testUserID, "", "", nil)

	var active int
	if err := testPool.QueryRow(ctx, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND agent_id = $2 AND status IN ('queued', 'dispatched', 'running')`,
		issueID, agentID).Scan(&active); err != nil {
		t.Fatalf("count active tasks: %v", err)
	}
	if active != 1 {
		t.Fatalf("plain member comment created a second controller: active tasks = %d, want 1 (running task %s)", active, taskID)
	}
}
