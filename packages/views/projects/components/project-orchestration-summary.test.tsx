import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { ProjectOrchestrationSummary } from "@multica/core/types";
import { ProjectOrchestrationSummaryCard } from "./project-orchestration-summary";

const getSummary = vi.fn();
const recover = vi.fn();
vi.mock("@multica/core/api", () => ({ api: {
  getProjectOrchestrationSummary: (...args: unknown[]) => getSummary(...args),
  recoverProjectOrchestration: (...args: unknown[]) => recover(...args),
} }));
vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));
vi.mock("../../i18n", () => ({ useT: () => ({ t: (selector: (value: any) => string) => selector({ orchestration: {
  title: "Orchestration", loading: "Loading", error: "Could not load", retry: "Try again", refresh: "Check again", refresh_hint: "No side effects",
  active: "Active", ready: "Ready tasks", running_slots: "Running slots", empty: "No issues", issue_states: "Issue states", candidates: "Candidates",
  last_event: "Last event", no_event: "No event", issue_slots: "Slots", recover: "Resume", recovering: "Resuming", admin_only: "Admin only",
  recovery_title: "Resume stale issue?", confirm_recovery: "Resume issue", cancel: "Cancel", recovery_applied: "Queued", recovery_already_active: "Already active", recovery_failed: "Failed",
  classification: { running: "Running", ready: "Ready", complete: "Completed", waiting_external: "Waiting", temporarily_not_ready: "Scheduled", orchestration_fault: "Needs attention" },
} }) }) }));

const base: ProjectOrchestrationSummary = {
  project_id: "project-1", classification: "orchestration_fault", reason: { code: "stale_in_progress", message: "Work has no durable execution path" },
  running_slots: 2, capacity: 6, last_event: { type: "task.failed", reason_code: "runtime_offline", created_at: "2026-07-20T00:00:00Z" },
  issues: [{ issue_id: "issue-1", issue_status: "in_progress", execution_state: "faulted", reason: { code: "stale_in_progress", message: "No active task" }, active_tasks: 0, ready_tasks: 0, running_slots: 0, capacity: 1, last_event: null, recovery_action: { action: "resume_stale_issue", allowed: true, reason: "allowed", side_effect: "Queues one assignment execution." } }],
  self_iteration_candidates: [],
};

function renderCard(isWorkspaceAdmin = true) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ProjectOrchestrationSummaryCard projectId="project-1" wsId="workspace-1" isWorkspaceAdmin={isWorkspaceAdmin} /></QueryClientProvider>);
}

describe("ProjectOrchestrationSummaryCard Web/Desktop shared flow", () => {
  beforeEach(() => { getSummary.mockReset(); recover.mockReset(); });

  it("renders loading, project slots, last event, stale reason, and candidate", async () => {
    let resolve!: (value: ProjectOrchestrationSummary) => void;
    getSummary.mockReturnValue(new Promise<ProjectOrchestrationSummary>((done) => { resolve = done; }));
    renderCard();
    expect(screen.getByRole("status", { name: "Loading" })).toBeInTheDocument();
    resolve({ ...base, self_iteration_candidates: [{ id: "candidate-1", snapshot_hash: "hash", policy_version: 1, state: "proposed", title: "Plan next iteration", reason: "All work is terminal", created_at: "2026-07-20T00:00:00Z" }] });
    expect(await screen.findByText("2 / 6")).toBeInTheDocument();
    expect(screen.getByText("Last event")).toBeInTheDocument();
    expect(screen.getByText("No active task")).toBeInTheDocument();
    expect(screen.getByText("Plan next iteration")).toBeInTheDocument();
  });

  it.each([
    ["running", "Running"], ["ready", "Ready"], ["waiting_external", "Waiting"], ["temporarily_not_ready", "Scheduled"], ["complete", "Completed"],
  ] as const)("renders %s classification", async (classification, label) => {
    getSummary.mockResolvedValue({ ...base, classification, issues: [] });
    renderCard();
    expect(await screen.findByText(label)).toBeInTheDocument();
    expect(screen.getByText("No issues")).toBeInTheDocument();
  });

  it("shows an error and retries with a keyboard-accessible button", async () => {
    getSummary.mockRejectedValueOnce(new Error("offline")).mockResolvedValueOnce({ ...base, issues: [] });
    const user = userEvent.setup();
    renderCard();
    const retry = await screen.findByRole("button", { name: "Try again" });
    retry.focus();
    await user.keyboard("{Enter}");
    expect(await screen.findByText("No issues")).toBeInTheDocument();
    expect(getSummary).toHaveBeenCalledTimes(2);
  });

  it("explains permission denial and never calls recovery for a non-admin", async () => {
    getSummary.mockResolvedValue(base);
    renderCard(false);
    const button = await screen.findByRole("button", { name: "Resume" });
    expect(button).toBeDisabled();
    expect(screen.getByText("Admin only")).toBeInTheDocument();
    expect(recover).not.toHaveBeenCalled();
  });

  it("confirms the side effect and prevents duplicate recovery submissions", async () => {
    getSummary.mockResolvedValue(base);
    let resolveRecovery!: (value: { applied: boolean; reason: string }) => void;
    recover.mockReturnValue(new Promise((done) => { resolveRecovery = done; }));
    const user = userEvent.setup();
    renderCard();
    await user.click(await screen.findByRole("button", { name: "Resume" }));
    expect(screen.getByText("Queues one assignment execution.")).toBeInTheDocument();
    const confirm = screen.getByRole("button", { name: "Resume issue" });
    await user.click(confirm);
    expect(screen.getByRole("button", { name: "Resuming" })).toBeDisabled();
    expect(recover).toHaveBeenCalledTimes(1);
    expect(recover).toHaveBeenCalledWith("project-1", "issue-1");
    resolveRecovery({ applied: true, reason: "assignment_run_queued" });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });
});
