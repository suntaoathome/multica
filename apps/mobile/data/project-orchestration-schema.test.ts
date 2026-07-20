import { describe, expect, it } from "vitest";
import {
  OrchestrationRecoveryResponseSchema,
  ProjectOrchestrationSummarySchema,
} from "./schemas";

describe("mobile project orchestration contract", () => {
  it("preserves counts, states, last event, recovery and candidates", () => {
    const parsed = ProjectOrchestrationSummarySchema.parse({
      project_id: "project-1",
      classification: "orchestration_fault",
      reason: { code: "stale_in_progress", message: "Stale work" },
      issues: [{
        issue_id: "issue-1",
        issue_status: "in_progress",
        execution_state: "faulted",
        reason: { code: "stale_in_progress", message: "No live execution" },
        active_tasks: 0,
        ready_tasks: 1,
        running_slots: 0,
        capacity: 6,
        last_event: { type: "task:failed", reason_code: "lost", created_at: "2026-07-21T00:00:00Z" },
        recovery_action: { action: "resume_stale_issue", allowed: true, reason: "admin", side_effect: "Queue one run" },
      }],
      self_iteration_candidates: [{ id: "candidate-1", snapshot_hash: "hash", policy_version: 1, state: "proposed", title: "Next", reason: "All complete", created_at: "2026-07-21T00:00:00Z" }],
      running_slots: 0,
      capacity: 6,
    });

    expect(parsed.issues[0]?.recovery_action?.allowed).toBe(true);
    expect(parsed.issues[0]?.last_event?.type).toBe("task:failed");
    expect(parsed.self_iteration_candidates[0]?.state).toBe("proposed");
  });

  it("keeps recovery idempotency visible", () => {
    expect(OrchestrationRecoveryResponseSchema.parse({ applied: false, reason: "active_execution_exists" })).toEqual({ applied: false, reason: "active_execution_exists" });
  });
});
