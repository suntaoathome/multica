import { describe, expect, it } from "vitest";
import type { AgentTask } from "../types";
import { deriveExecutionSummary } from "./execution-observability";

function task(overrides: Partial<AgentTask>): AgentTask {
  return {
    id: "task-1", agent_id: "agent-1", runtime_id: "runtime-1", issue_id: "issue-1",
    status: "queued", priority: 0, dispatched_at: null, started_at: null,
    completed_at: null, result: null, error: null, created_at: "2026-07-20T00:00:00Z",
    ...overrides,
  };
}

describe("deriveExecutionSummary", () => {
  it("reports real running slots and capacity independently of workflow status", () => {
    expect(deriveExecutionSummary([
      task({ status: "running", effective_max_concurrent_tasks: 2 }),
      task({ id: "task-2", status: "queued" }),
    ])).toMatchObject({ state: "running", running: 1, queued: 1, capacity: 2 });
  });

  it("distinguishes ready, internal waiting, and external waiting", () => {
    expect(deriveExecutionSummary([task({ claimable: true })]).state).toBe("ready");
    expect(deriveExecutionSummary([task({ claimable: false, execution_reason: { code: "runtime_offline" } })]).state).toBe("waiting");
    expect(deriveExecutionSummary([task({ claimable: false, execution_reason: { code: "human_approval" } })]).state).toBe("external");
  });

  it("falls back unknown reason codes to waiting while preserving the message", () => {
    expect(deriveExecutionSummary([task({ execution_reason: { code: "future_reason", message: "New server reason" } })]))
      .toMatchObject({ state: "waiting", reasonCode: "future_reason", reasonMessage: "New server reason" });
  });
});
