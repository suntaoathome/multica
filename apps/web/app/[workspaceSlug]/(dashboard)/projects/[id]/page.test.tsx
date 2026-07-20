import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

vi.mock("react", async (importOriginal) => ({ ...(await importOriginal<typeof import("react")>()), use: () => ({ id: "project-web" }) }));
vi.mock("@multica/views/projects/components", () => ({ ProjectDetail: ({ projectId }: { projectId: string }) => <div>shared-project-{projectId}</div> }));

import ProjectDetailPage from "./page";

describe("web project orchestration flow wiring", () => {
  it("renders the shared project detail view", () => {
    render(<ProjectDetailPage params={Promise.resolve({ id: "ignored" })} />);
    expect(screen.getByText("shared-project-project-web")).toBeInTheDocument();
  });
});
