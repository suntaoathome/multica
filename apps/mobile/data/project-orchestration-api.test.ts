import { describe, expect, it } from "vitest";
import {
  projectOrchestrationRecoveryRequest,
  projectOrchestrationSummaryPath,
} from "./project-orchestration-api";

describe("mobile project orchestration API contract", () => {
  it("uses the backend summary route", () => {
    expect(projectOrchestrationSummaryPath("project-1")).toBe(
      "/api/projects/project-1/orchestration-summary",
    );
  });

  it("uses the backend recovery route and required idempotent action", () => {
    const request = projectOrchestrationRecoveryRequest(
      "project-1",
      "issue-1",
    );

    expect(request.path).toBe(
      "/api/projects/project-1/orchestration-recovery",
    );
    expect(request.init.method).toBe("POST");
    expect(JSON.parse(String(request.init.body))).toEqual({
      issue_id: "issue-1",
      action: "resume_stale_issue",
    });
  });
});
