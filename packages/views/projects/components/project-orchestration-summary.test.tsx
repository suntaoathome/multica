import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ProjectOrchestrationSummaryCard } from "./project-orchestration-summary";

const getSummary = vi.fn();
vi.mock("@multica/core/api", () => ({ api: { getProjectOrchestrationSummary: (...args: unknown[]) => getSummary(...args) } }));
vi.mock("../../i18n", () => ({ useT: () => ({ t: (selector: (value: any) => string) => selector({ orchestration: {
  title: "Orchestration", loading: "Loading", error: "Could not load", retry: "Try again", refresh: "Check again", refresh_hint: "No side effects",
  active: "Active executions", ready: "Ready tasks", empty: "No issues", issue_states: "Issue states", candidates: "Candidates",
  classification: { ready: "Ready", complete: "Completed", waiting_external: "Waiting", temporarily_not_ready: "Scheduled", orchestration_fault: "Needs attention" },
} }) }) }));

function renderCard() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ProjectOrchestrationSummaryCard projectId="project-1" wsId="workspace-1" /></QueryClientProvider>);
}

describe("ProjectOrchestrationSummaryCard", () => {
  beforeEach(() => getSummary.mockReset());

  it("renders execution counts, reasons, and a deduplicated candidate", async () => {
    getSummary.mockResolvedValue({
      project_id: "project-1", classification: "orchestration_fault", reason: { code: "stale_in_progress", message: "Work has no durable execution path" },
      issues: [{ issue_id: "issue-1", issue_status: "in_progress", execution_state: "faulted", reason: { code: "stale_in_progress", message: "No active task" }, active_tasks: 0, ready_tasks: 0 }],
      self_iteration_candidates: [{ id: "candidate-1", snapshot_hash: "hash", policy_version: 1, state: "proposed", title: "Plan next iteration", reason: "All work is terminal", created_at: "2026-07-20T00:00:00Z" }],
    });
    renderCard();
    expect(await screen.findByText("Needs attention")).toBeInTheDocument();
    expect(screen.getByText("No active task")).toBeInTheDocument();
    expect(screen.getByText("Plan next iteration")).toBeInTheDocument();
    expect(getSummary).toHaveBeenCalledWith("project-1");
  });

  it("shows an empty state when the project has no issues", async () => {
    getSummary.mockResolvedValue({ project_id: "project-1", classification: "complete", reason: { code: "all_terminal", message: "All work complete" }, issues: [], self_iteration_candidates: [] });
    renderCard();
    expect(await screen.findByText("No issues")).toBeInTheDocument();
  });
});
