import type { AgentTask } from "../types";

export type ExecutionSummaryState =
  | "running"
  | "ready"
  | "waiting"
  | "external"
  | "failed"
  | "idle";

export interface ExecutionSummary {
  state: ExecutionSummaryState;
  running: number;
  queued: number;
  capacity?: number;
  reasonCode?: string;
  reasonMessage?: string;
  lastEventAt?: string;
}

const EXTERNAL_REASONS = new Set([
  "human_approval", "external_service", "repository_access", "credentials", "physical_resource",
]);

export function deriveExecutionSummary(tasks: readonly AgentTask[]): ExecutionSummary {
  const workflow = tasks.filter((task) => !task.chat_session_id);
  const running = workflow.filter((task) => task.status === "running");
  const queued = workflow.filter((task) =>
    task.status === "queued" || task.status === "dispatched" || task.status === "waiting_local_directory",
  );
  const unresolvedFailure = workflow.find((task) => task.status === "failed");
  const reasonTask = queued.find((task) => task.execution_reason) ?? unresolvedFailure;
  const reasonCode = reasonTask?.execution_reason?.code;
  const capacity = workflow.find((task) => task.effective_max_concurrent_tasks)?.effective_max_concurrent_tasks;
  const lastEventAt = workflow
    .flatMap((task) => [task.completed_at, task.started_at, task.dispatched_at, task.created_at])
    .filter((value): value is string => Boolean(value))
    .sort()
    .at(-1);

  if (running.length > 0) return { state: "running", running: running.length, queued: queued.length, capacity, lastEventAt };
  if (queued.length > 0) {
    const claimable = queued.some((task) => task.claimable === true);
    const state = reasonCode && EXTERNAL_REASONS.has(reasonCode) ? "external" : claimable ? "ready" : "waiting";
    return { state, running: 0, queued: queued.length, capacity, reasonCode, reasonMessage: reasonTask?.execution_reason?.message, lastEventAt };
  }
  if (unresolvedFailure) return { state: "failed", running: 0, queued: 0, capacity, reasonCode, reasonMessage: unresolvedFailure.execution_reason?.message ?? unresolvedFailure.error ?? undefined, lastEventAt };
  return { state: "idle", running: 0, queued: 0, capacity, lastEventAt };
}
