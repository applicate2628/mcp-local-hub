import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/preact";
import { EnvDrawer } from "./EnvDrawer";

const TASK = "\\mcp-local-hub-lsp-default-clangd";

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("EnvDrawer", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });
  afterEach(() => {
    cleanup();
  });

  it("renders header + initial path + no warning when initialPath includes ${parent_path}", () => {
    render(
      <EnvDrawer
        taskName={TASK}
        initialPath="/usr/local/bin;${parent_path}"
        rowLabel="clangd"
        onClose={() => {}}
      />,
    );
    expect(screen.getByTestId("env-drawer")).toBeTruthy();
    expect((screen.getByTestId("env-drawer-path") as HTMLTextAreaElement).value).toBe(
      "/usr/local/bin;${parent_path}",
    );
    expect(screen.queryByTestId("env-drawer-parent-path-warning")).toBeNull();
  });

  it("renders warning chip when PATH textarea lacks ${parent_path}", () => {
    render(
      <EnvDrawer
        taskName={TASK}
        initialPath="/usr/local/bin"
        rowLabel="clangd"
        onClose={() => {}}
      />,
    );
    expect(screen.getByTestId("env-drawer-parent-path-warning")).toBeTruthy();
    const text = screen.getByTestId("env-drawer-parent-path-warning").textContent ?? "";
    expect(text).toContain("PATH does not include");
    expect(text).toContain("DROPPED");
  });

  it("warning chip hides as soon as ${parent_path} is typed", async () => {
    render(
      <EnvDrawer
        taskName={TASK}
        initialPath="/usr/local/bin"
        rowLabel="clangd"
        onClose={() => {}}
      />,
    );
    expect(screen.getByTestId("env-drawer-parent-path-warning")).toBeTruthy();
    const ta = screen.getByTestId("env-drawer-path") as HTMLTextAreaElement;
    fireEvent.input(ta, { target: { value: "/usr/local/bin;${parent_path}" } });
    await waitFor(() => {
      expect(screen.queryByTestId("env-drawer-parent-path-warning")).toBeNull();
    });
  });

  it("Apply posts /api/daemon/env with task_name + env.PATH (uppercase)", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      jsonResponse(200, {
        task_name: TASK,
        changed_keys: ["PATH"],
      }),
    );
    render(
      <EnvDrawer
        taskName={TASK}
        initialPath="/usr/local/bin;${parent_path}"
        rowLabel="clangd"
        onClose={() => {}}
      />,
    );
    fireEvent.click(screen.getByTestId("env-drawer-apply"));
    await waitFor(() => {
      expect(screen.getByTestId("env-drawer-apply-msg").textContent).toContain("Applied");
    });
    expect(fetchSpy).toHaveBeenCalledOnce();
    const [url, init] = fetchSpy.mock.calls[0] as [string, RequestInit];
    expect(url).toBe("/api/daemon/env");
    expect(init.method).toBe("POST");
    const body = JSON.parse(init.body as string);
    expect(body).toEqual({
      task_name: TASK,
      env: { PATH: "/usr/local/bin;${parent_path}" },
    });
  });

  it("Apply with empty PATH sends env: {} (does not clobber Path to empty)", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      jsonResponse(200, {
        task_name: TASK,
        changed_keys: [],
      }),
    );
    render(
      <EnvDrawer
        taskName={TASK}
        initialPath=""
        rowLabel="clangd"
        onClose={() => {}}
      />,
    );
    fireEvent.click(screen.getByTestId("env-drawer-apply"));
    await waitFor(() => {
      expect(screen.getByTestId("env-drawer-apply-msg")).toBeTruthy();
    });
    const body = JSON.parse(
      (fetchSpy.mock.calls[0]?.[1] as RequestInit).body as string,
    );
    expect(body).toEqual({ task_name: TASK, env: {} });
  });

  it("Apply surfaces backend error message", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      jsonResponse(400, { error: "task_name not in supervisor-intent", code: "UNKNOWN_TASK" }),
    );
    render(
      <EnvDrawer
        taskName={TASK}
        initialPath="C:\\bin;${parent_path}"
        rowLabel="clangd"
        onClose={() => {}}
      />,
    );
    fireEvent.click(screen.getByTestId("env-drawer-apply"));
    await waitFor(() => {
      const msg = screen.getByTestId("env-drawer-apply-msg");
      expect(msg.className).toBe("error");
      expect(msg.textContent).toContain("UNKNOWN_TASK");
    });
  });

  it("Restart posts /api/daemon/respawn with force=false by default", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      jsonResponse(200, { task_name: TASK, force: false, state: "spawned" }),
    );
    render(
      <EnvDrawer
        taskName={TASK}
        initialPath=""
        rowLabel="clangd"
        onClose={() => {}}
      />,
    );
    fireEvent.click(screen.getByTestId("env-drawer-restart"));
    await waitFor(() => {
      expect(screen.getByTestId("env-drawer-restart-msg").textContent).toContain("respawned");
    });
    const body = JSON.parse(
      (fetchSpy.mock.calls[0]?.[1] as RequestInit).body as string,
    );
    expect(body).toEqual({ task_name: TASK, force: false });
  });

  it("Restart with force checkbox enabled posts force=true", async () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      jsonResponse(200, { task_name: TASK, force: true, state: "spawned" }),
    );
    render(
      <EnvDrawer
        taskName={TASK}
        initialPath=""
        rowLabel="clangd"
        onClose={() => {}}
      />,
    );
    fireEvent.click(screen.getByTestId("env-drawer-force"));
    fireEvent.click(screen.getByTestId("env-drawer-restart"));
    await waitFor(() => {
      expect(screen.getByTestId("env-drawer-restart-msg")).toBeTruthy();
    });
    const body = JSON.parse(
      (fetchSpy.mock.calls[0]?.[1] as RequestInit).body as string,
    );
    expect(body.force).toBe(true);
  });

  it("Restart with 409 QUARANTINED shows the force-and-retry hint", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      jsonResponse(409, {
        error: "daemon quarantined",
        code: "QUARANTINED",
      }),
    );
    render(
      <EnvDrawer
        taskName={TASK}
        initialPath=""
        rowLabel="clangd"
        onClose={() => {}}
      />,
    );
    fireEvent.click(screen.getByTestId("env-drawer-restart"));
    await waitFor(() => {
      const msg = screen.getByTestId("env-drawer-restart-msg");
      expect(msg.className).toBe("error");
      expect(msg.textContent).toContain("quarantined");
      expect(msg.textContent).toContain("force");
    });
  });

  it("Close button invokes onClose", () => {
    const onClose = vi.fn();
    render(
      <EnvDrawer
        taskName={TASK}
        initialPath=""
        rowLabel="clangd"
        onClose={onClose}
      />,
    );
    fireEvent.click(screen.getByTestId("env-drawer-close"));
    expect(onClose).toHaveBeenCalledOnce();
  });
});
