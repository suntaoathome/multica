export function projectOrchestrationSummaryPath(projectId: string): string {
  return `/api/projects/${projectId}/orchestration-summary`;
}

export function projectOrchestrationRecoveryRequest(
  projectId: string,
  issueId: string,
): {
  path: string;
  init: Omit<RequestInit, "signal"> & { signal?: AbortSignal };
} {
  return {
    path: `/api/projects/${projectId}/orchestration-recovery`,
    init: {
      method: "POST",
      body: JSON.stringify({
        issue_id: issueId,
        action: "resume_stale_issue",
      }),
    },
  };
}
