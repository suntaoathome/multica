package main

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/blocker"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/handler"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// registerBlockerResolutionListeners routes an agent-reported blocker to the
// leader of the agent's one active squad. The issue remains assigned to the
// original worker: the leader resolves the environmental/capability problem,
// then promotes blocked -> in_progress to wake the worker again.
func registerBlockerResolutionListeners(bus *events.Bus, queries *db.Queries, taskSvc *service.TaskService) {
	ctx := context.Background()
	bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok || e.ActorType != "agent" {
			return
		}
		statusChanged, _ := payload["status_changed"].(bool)
		if !statusChanged {
			return
		}
		response, ok := payload["issue"].(handler.IssueResponse)
		if !ok || response.Status != "blocked" || response.AssigneeType == nil || response.AssigneeID == nil {
			return
		}
		if *response.AssigneeType != "agent" || *response.AssigneeID != e.ActorID {
			return
		}

		issue, err := queries.GetIssue(ctx, parseUUID(response.ID))
		if err != nil {
			slog.Warn("blocker resolver: failed to load issue", "issue_id", response.ID, "error", err)
			return
		}
		squads, err := queries.ListSquadsByMember(ctx, db.ListSquadsByMemberParams{
			WorkspaceID: issue.WorkspaceID,
			MemberType:  "agent",
			MemberID:    issue.AssigneeID,
		})
		if err != nil {
			slog.Warn("blocker resolver: failed to load squads", "issue_id", response.ID, "error", err)
			return
		}

		squad, ok := uniqueResolverSquad(squads, e.ActorID)
		if !ok {
			slog.Info("blocker resolver: automatic escalation requires one active squad",
				"issue_id", response.ID, "agent_id", e.ActorID, "squad_count", len(squads))
			return
		}
		leader, err := queries.GetAgent(ctx, squad.LeaderID)
		if err != nil {
			slog.Warn("blocker resolver: failed to load squad leader", "issue_id", response.ID, "error", err)
			return
		}
		ready, reason, err := service.AgentReadiness(ctx, queries, leader)
		if err != nil || !ready {
			slog.Info("blocker resolver: squad leader is not runnable",
				"issue_id", response.ID, "leader_id", util.UUIDToString(squad.LeaderID), "reason", reason, "error", err)
			return
		}

		// Mark pending before enqueue so the later autopilot listener does not
		// turn this recoverable wait into a terminal failure.
		if !setBlockerMetadata(ctx, queries, issue, blocker.ResolverAgentKey, util.UUIDToString(squad.LeaderID)) ||
			!setBlockerMetadata(ctx, queries, issue, blocker.ResolverSquadKey, util.UUIDToString(squad.ID)) ||
			!setBlockerMetadata(ctx, queries, issue, blocker.ResolutionStateKey, blocker.ResolutionPending) {
			return
		}

		handoff := blocker.ResolverHandoff(blocker.Reason(issue.Metadata))
		var accountableUserID pgtype.UUID
		if issue.CreatorType == "member" {
			accountableUserID = issue.CreatorID
		}
		if _, err := taskSvc.EnqueueTaskForSquadLeaderWithHandoff(
			ctx, issue, squad.LeaderID, squad.ID, handoff, accountableUserID,
		); err != nil {
			_ = setBlockerMetadata(ctx, queries, issue, blocker.ResolutionStateKey, blocker.ResolutionTerminal)
			slog.Warn("blocker resolver: failed to enqueue squad leader",
				"issue_id", response.ID, "leader_id", util.UUIDToString(squad.LeaderID), "error", err)
			return
		}
		slog.Info("blocker resolver: escalated recoverable blocker",
			"issue_id", response.ID, "agent_id", e.ActorID,
			"leader_id", util.UUIDToString(squad.LeaderID), "squad_id", util.UUIDToString(squad.ID))
	})
}

func uniqueResolverSquad(squads []db.Squad, blockedAgentID string) (db.Squad, bool) {
	var selected db.Squad
	count := 0
	for _, squad := range squads {
		if squad.ArchivedAt.Valid || util.UUIDToString(squad.LeaderID) == blockedAgentID {
			continue
		}
		selected = squad
		count++
	}
	return selected, count == 1
}

func setBlockerMetadata(ctx context.Context, queries *db.Queries, issue db.Issue, key, value string) bool {
	raw, _ := json.Marshal(value)
	if _, err := queries.SetIssueMetadataKey(ctx, db.SetIssueMetadataKeyParams{
		Key: key, Value: raw, ID: issue.ID, WorkspaceID: issue.WorkspaceID,
	}); err != nil {
		slog.Warn("blocker resolver: failed to persist resolution metadata",
			"issue_id", util.UUIDToString(issue.ID), "key", key, "error", err)
		return false
	}
	return true
}
