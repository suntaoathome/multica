package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/blocker"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestUniqueResolverSquadSafety(t *testing.T) {
	worker := parseUUID("11111111-1111-4111-8111-111111111111")
	otherLeader := parseUUID("22222222-2222-4222-8222-222222222222")
	active := db.Squad{ID: parseUUID("33333333-3333-4333-8333-333333333333"), LeaderID: otherLeader}
	selfLed := db.Squad{ID: parseUUID("44444444-4444-4444-8444-444444444444"), LeaderID: worker}
	archived := db.Squad{
		ID: parseUUID("55555555-5555-4555-8555-555555555555"), LeaderID: otherLeader,
		ArchivedAt: pgtype.Timestamptz{Valid: true},
	}

	tests := []struct {
		name   string
		squads []db.Squad
		want   bool
	}{
		{name: "one active", squads: []db.Squad{active}, want: true},
		{name: "no squad"},
		{name: "multiple active", squads: []db.Squad{active, {ID: parseUUID("66666666-6666-4666-8666-666666666666"), LeaderID: otherLeader}}},
		{name: "self leader", squads: []db.Squad{selfLed}},
		{name: "archived ignored", squads: []db.Squad{active, archived}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, got := uniqueResolverSquad(tc.squads, util.UUIDToString(worker))
			if got != tc.want {
				t.Fatalf("uniqueResolverSquad() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAgentBlockerEscalatesToUniqueSquadLeader(t *testing.T) {
	ctx := context.Background()
	queries := db.New(testPool)
	bus := events.New()
	taskSvc := service.NewTaskService(queries, testPool, nil, bus)
	registerBlockerResolutionListeners(bus, queries, taskSvc)

	var leaderID string
	if err := testPool.QueryRow(ctx, `
		SELECT id::text FROM agent
		WHERE workspace_id = $1 AND runtime_id IS NOT NULL
		ORDER BY created_at ASC LIMIT 1
	`, testWorkspaceID).Scan(&leaderID); err != nil {
		t.Fatalf("load leader fixture: %v", err)
	}

	var runtimeID, workerID, squadID string
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent_runtime (
			workspace_id, daemon_id, name, runtime_mode, provider, status,
			device_info, metadata, last_seen_at
		) VALUES ($1, NULL, 'Blocker worker runtime', 'local',
			'blocker_worker_test', 'online', '{}'::jsonb, '{}'::jsonb, now())
		RETURNING id::text
	`, testWorkspaceID).Scan(&runtimeID); err != nil {
		t.Fatalf("create worker runtime: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO agent (
			workspace_id, name, description, runtime_mode, runtime_config,
			runtime_id, visibility, max_concurrent_tasks, owner_id
		) VALUES ($1, 'blocker-worker-test', '', 'local', '{}'::jsonb,
			$2, 'workspace', 1, $3)
		RETURNING id::text
	`, testWorkspaceID, runtimeID, testUserID).Scan(&workerID); err != nil {
		t.Fatalf("create worker: %v", err)
	}
	if err := testPool.QueryRow(ctx, `
		INSERT INTO squad (workspace_id, name, description, leader_id, creator_id)
		VALUES ($1, 'Blocker resolution test squad', '', $2, $3)
		RETURNING id::text
	`, testWorkspaceID, leaderID, testUserID).Scan(&squadID); err != nil {
		t.Fatalf("create squad: %v", err)
	}
	if _, err := testPool.Exec(ctx, `
		INSERT INTO squad_member (squad_id, member_type, member_id, role)
		VALUES ($1, 'agent', $2, 'member')
	`, squadID, workerID); err != nil {
		t.Fatalf("add worker to squad: %v", err)
	}

	issueID := createTestIssue(t, testWorkspaceID, testUserID)
	if _, err := testPool.Exec(ctx, `
		UPDATE issue SET status = 'blocked', assignee_type = 'agent', assignee_id = $2,
			metadata = '{"blocked_reason":"GitHub credentials unavailable"}'::jsonb
		WHERE id = $1
	`, issueID, workerID); err != nil {
		t.Fatalf("seed blocked issue: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_task_queue WHERE issue_id = $1`, issueID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, issueID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM squad_member WHERE squad_id = $1`, squadID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM squad WHERE id = $1`, squadID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent WHERE id = $1`, workerID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM agent_runtime WHERE id = $1`, runtimeID)
	})

	assigneeType := "agent"
	bus.Publish(events.Event{
		Type: protocol.EventIssueUpdated, WorkspaceID: testWorkspaceID,
		ActorType: "agent", ActorID: workerID,
		Payload: map[string]any{
			"status_changed": true,
			"issue": handler.IssueResponse{
				ID: issueID, WorkspaceID: testWorkspaceID, Status: "blocked",
				AssigneeType: &assigneeType, AssigneeID: &workerID,
			},
		},
	})

	var taskAgentID, taskSquadID, handoff string
	if err := testPool.QueryRow(ctx, `
		SELECT agent_id::text, squad_id::text, COALESCE(handoff_note, '')
		FROM agent_task_queue WHERE issue_id = $1
	`, issueID).Scan(&taskAgentID, &taskSquadID, &handoff); err != nil {
		t.Fatalf("load resolver task: %v", err)
	}
	if taskAgentID != leaderID || taskSquadID != squadID {
		t.Fatalf("resolver routed to agent=%s squad=%s, want agent=%s squad=%s", taskAgentID, taskSquadID, leaderID, squadID)
	}
	if !blocker.IsResolverHandoff(handoff) {
		t.Fatalf("resolver task missing internal handoff: %q", handoff)
	}

	var metadata []byte
	if err := testPool.QueryRow(ctx, `SELECT metadata FROM issue WHERE id = $1`, issueID).Scan(&metadata); err != nil {
		t.Fatalf("load blocker metadata: %v", err)
	}
	if !blocker.IsResolutionPending(metadata) {
		t.Fatalf("resolver state is not pending: %s", metadata)
	}
	var values map[string]any
	if err := json.Unmarshal(metadata, &values); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if values[blocker.ResolverAgentKey] != leaderID || values[blocker.ResolverSquadKey] != squadID {
		t.Fatalf("resolver metadata mismatch: %+v", values)
	}
}
