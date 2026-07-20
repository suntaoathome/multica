import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

vi.mock("react-router-dom", () => ({ useParams: () => ({ id: "project-desktop" }) }));
vi.mock("@tanstack/react-query", () => ({ useQuery: () => ({ data: { title: "Desktop project", icon: null } }) }));
vi.mock("@multica/core/hooks", () => ({ useWorkspaceId: () => "workspace-1" }));
vi.mock("@multica/core/projects/queries", () => ({ projectDetailOptions: vi.fn(() => ({})) }));
vi.mock("@/hooks/use-document-title", () => ({ useDocumentTitle: vi.fn() }));
vi.mock("@multica/views/projects/components", () => ({ ProjectDetail: ({ projectId }: { projectId: string }) => <div>shared-project-{projectId}</div> }));

import { ProjectDetailPage } from "./project-detail-page";

describe("desktop project orchestration flow wiring", () => {
  it("renders the shared project detail view", () => {
    render(<ProjectDetailPage />);
    expect(screen.getByText("shared-project-project-desktop")).toBeInTheDocument();
  });
});
