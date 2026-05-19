import { describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/preact";
import { ALL_WORKSPACES_KEY, WorkspaceSelector } from "./WorkspaceSelector";
import type { WorkspacePair } from "../api";

describe("WorkspaceSelector", () => {
  it("renders empty-state placeholder when workspaces is empty", () => {
    cleanup();
    const onChange = vi.fn();
    render(
      <WorkspaceSelector
        workspaces={[]}
        selectedKey={ALL_WORKSPACES_KEY}
        onChange={onChange}
      />,
    );
    expect(screen.getByTestId("workspace-selector")).toBeTruthy();
    // The placeholder text must read as a single sentence — split into
    // text + <code>, joined here as plain textContent so the assertion
    // doesn't fight the inline-code styling.
    const text = screen.getByTestId("workspace-selector").textContent ?? "";
    expect(text).toContain("register a workspace first");
    expect(text).toContain("mcphub register");
    expect(screen.queryByTestId("workspace-selector-select")).toBeNull();
  });

  it("renders option per workspace + (all workspaces) sentinel + invokes onChange on selection", () => {
    cleanup();
    const ws: WorkspacePair[] = [
      { workspace_key: "alpha", workspace_path: "/proj/alpha" },
      { workspace_key: "beta", workspace_path: "/proj/beta" },
    ];
    const onChange = vi.fn();
    render(
      <WorkspaceSelector
        workspaces={ws}
        selectedKey={ALL_WORKSPACES_KEY}
        onChange={onChange}
      />,
    );
    const select = screen.getByTestId("workspace-selector-select") as HTMLSelectElement;
    // 1 sentinel + 2 workspace options.
    expect(select.options.length).toBe(3);
    expect(select.options[0].value).toBe(ALL_WORKSPACES_KEY);
    expect(select.options[1].value).toBe("alpha");
    expect(select.options[2].value).toBe("beta");
    fireEvent.change(select, { target: { value: "beta" } });
    expect(onChange).toHaveBeenCalledWith("beta");
  });

  it("shows the selected workspace path when a key is picked", () => {
    cleanup();
    const ws: WorkspacePair[] = [
      { workspace_key: "alpha", workspace_path: "/proj/alpha" },
    ];
    render(
      <WorkspaceSelector workspaces={ws} selectedKey="alpha" onChange={() => {}} />,
    );
    const pathSpan = screen.getByTestId("workspace-selector-path");
    expect(pathSpan.textContent).toBe("/proj/alpha");
    expect(pathSpan.getAttribute("title")).toBe("/proj/alpha");
  });

  it("does NOT render path preview when (all workspaces) is selected", () => {
    cleanup();
    const ws: WorkspacePair[] = [
      { workspace_key: "alpha", workspace_path: "/proj/alpha" },
    ];
    render(
      <WorkspaceSelector
        workspaces={ws}
        selectedKey={ALL_WORKSPACES_KEY}
        onChange={() => {}}
      />,
    );
    expect(screen.queryByTestId("workspace-selector-path")).toBeNull();
  });
});
