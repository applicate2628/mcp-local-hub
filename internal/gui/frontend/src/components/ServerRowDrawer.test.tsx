import { describe, expect, it, afterEach, beforeEach, vi } from "vitest";
import { render, screen, cleanup, fireEvent, waitFor } from "@testing-library/preact";
import { ServerRowDrawer } from "./ServerRowDrawer";
import type { DaemonStatus } from "../types";

// getManifest is mocked so the drawer's manifest-preview pane resolves
// deterministically without a real /api/manifest/get round-trip.
vi.mock("../api", () => ({
  getManifest: vi.fn(),
}));
import { getManifest } from "../api";

// flowbite's Drawer touches window.matchMedia / focus-trap behavior that
// happy-dom only partially provides. Mock it to a no-op constructor so the
// component's show()/hide() lifecycle is exercised without the real DOM
// plumbing — the markup + handlers are what we assert.
const hideSpy = vi.hoisted(() => vi.fn());
vi.mock("flowbite", () => ({
  Drawer: class {
    private opts: { onHide?: () => void };
    constructor(_el: HTMLElement, opts: { onHide?: () => void }) {
      this.opts = opts;
    }
    show() {}
    hide() {
      hideSpy();
      this.opts.onHide?.();
    }
  },
}));

const RUNNING_ROW: DaemonStatus = {
  server: "memory",
  daemon: "default",
  port: 9123,
  pid: 4242,
  state: "Running",
  uptime_sec: 2 * 3600 + 14 * 60, // 2h 14m
  ram_bytes: 48 * 1024 * 1024, // 48 MB
};

describe("ServerRowDrawer (Flowbite Drawer)", () => {
  beforeEach(() => {
    (getManifest as ReturnType<typeof vi.fn>).mockResolvedValue({
      yaml: "name: memory\nport: 9123\n",
      hash: "abc",
    });
  });
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it("renders the Flowbite drawer shell with right-placement classes", () => {
    render(<ServerRowDrawer serverName="memory" daemons={[RUNNING_ROW]} onClose={() => {}} />);
    const drawer = screen.getByTestId("server-row-drawer");
    // Flowbite drawer vocabulary: fixed, right-anchored, off-screen start.
    expect(drawer.className).toContain("fixed");
    expect(drawer.className).toContain("right-0");
    expect(drawer.className).toContain("translate-x-full");
    expect(drawer.getAttribute("data-server")).toBe("memory");
  });

  it("projects lifetime stats (port/PID/uptime/RAM) from the passed daemon rows", () => {
    render(<ServerRowDrawer serverName="memory" daemons={[RUNNING_ROW]} onClose={() => {}} />);
    const stats = screen.getByTestId("server-row-drawer-stats");
    expect(stats.textContent).toContain("9123"); // port
    expect(stats.textContent).toContain("4242"); // PID
    expect(screen.getByTestId("server-row-drawer-uptime-default").textContent).toBe("2h 14m");
    expect(screen.getByTestId("server-row-drawer-ram-default").textContent).toBe("48 MB");
    expect(screen.getByTestId("server-row-drawer-state-default").textContent).toContain("Running");
  });

  it("omits uptime/RAM rows when those fields are absent", () => {
    const noMetrics: DaemonStatus = { server: "memory", daemon: "default", port: 9123, pid: 1, state: "Stopped" };
    render(<ServerRowDrawer serverName="memory" daemons={[noMetrics]} onClose={() => {}} />);
    expect(screen.queryByTestId("server-row-drawer-uptime-default")).toBeNull();
    expect(screen.queryByTestId("server-row-drawer-ram-default")).toBeNull();
  });

  it("renders the empty-state line when there is no live daemon", () => {
    render(<ServerRowDrawer serverName="memory" daemons={[]} onClose={() => {}} />);
    expect(screen.getByTestId("server-row-drawer-no-daemons")).not.toBeNull();
  });

  it("loads + renders the manifest YAML preview", async () => {
    render(<ServerRowDrawer serverName="memory" daemons={[RUNNING_ROW]} onClose={() => {}} />);
    const pre = await screen.findByTestId("server-row-drawer-manifest-yaml");
    expect(pre.textContent).toContain("name: memory");
    expect(getManifest).toHaveBeenCalledWith("memory");
  });

  it("surfaces a manifest load error in the preview pane", async () => {
    (getManifest as ReturnType<typeof vi.fn>).mockRejectedValueOnce(new Error("not found"));
    render(<ServerRowDrawer serverName="ghost" daemons={[]} onClose={() => {}} />);
    const err = await screen.findByTestId("server-row-drawer-manifest-err");
    expect(err.textContent).toContain("not found");
  });

  it("Restart posts to /api/servers/<name>/restart and shows a success message", async () => {
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response(JSON.stringify({ restart_results: [] }), { status: 200 }));
    render(<ServerRowDrawer serverName="memory" daemons={[RUNNING_ROW]} onClose={() => {}} />);
    fireEvent.click(screen.getByTestId("server-row-drawer-restart"));
    await waitFor(() => {
      const msg = screen.getByTestId("server-row-drawer-action-msg");
      expect(msg.textContent).toContain("Restarted memory");
    });
    expect(fetchSpy).toHaveBeenCalledWith("/api/servers/memory/restart", { method: "POST" });
    fetchSpy.mockRestore();
  });

  it("Stop posts to /api/servers/<name>/stop", async () => {
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response(JSON.stringify({ stop_results: [] }), { status: 200 }));
    render(<ServerRowDrawer serverName="memory" daemons={[RUNNING_ROW]} onClose={() => {}} />);
    fireEvent.click(screen.getByTestId("server-row-drawer-stop"));
    await waitFor(() => {
      expect(screen.getByTestId("server-row-drawer-action-msg").textContent).toContain("Stopped memory");
    });
    expect(fetchSpy).toHaveBeenCalledWith("/api/servers/memory/stop", { method: "POST" });
    fetchSpy.mockRestore();
  });

  it("Stop is disabled when no daemon is Running", () => {
    const stopped: DaemonStatus = { server: "memory", daemon: "default", port: 9123, pid: 1, state: "Stopped" };
    render(<ServerRowDrawer serverName="memory" daemons={[stopped]} onClose={() => {}} />);
    expect((screen.getByTestId("server-row-drawer-stop") as HTMLButtonElement).disabled).toBe(true);
  });

  it("surfaces a server-action error", async () => {
    const fetchSpy = vi
      .spyOn(globalThis, "fetch")
      .mockResolvedValue(new Response(JSON.stringify({ error: "boom" }), { status: 500 }));
    render(<ServerRowDrawer serverName="memory" daemons={[RUNNING_ROW]} onClose={() => {}} />);
    fireEvent.click(screen.getByTestId("server-row-drawer-restart"));
    await waitFor(() => {
      expect(screen.getByTestId("server-row-drawer-action-msg").textContent).toContain("boom");
    });
    fetchSpy.mockRestore();
  });

  it("calls onClose when the × button is clicked", () => {
    const onClose = vi.fn();
    render(<ServerRowDrawer serverName="memory" daemons={[RUNNING_ROW]} onClose={onClose} />);
    fireEvent.click(screen.getByTestId("server-row-drawer-close"));
    expect(onClose).toHaveBeenCalled();
  });

  it("keeps the Flowbite drawer mounted across onClose prop identity changes", () => {
    const firstClose = vi.fn();
    const secondClose = vi.fn();
    const { rerender } = render(
      <ServerRowDrawer serverName="memory" daemons={[RUNNING_ROW]} onClose={firstClose} />,
    );
    hideSpy.mockClear();

    rerender(<ServerRowDrawer serverName="memory" daemons={[RUNNING_ROW]} onClose={secondClose} />);

    expect(hideSpy).not.toHaveBeenCalled();
    expect(firstClose).not.toHaveBeenCalled();
    expect(secondClose).not.toHaveBeenCalled();
  });
});
