// internal/gui/frontend/src/screens/Servers.test.tsx
//
// v0.4.5 init-button: per-column "Initialize <client>" affordance
// surfaces in the matrix header when client_config_presence reports
// "missing-init-possible". Clicking the button POSTs to
// /api/init-client-config and refreshes /api/scan so the now-present
// file flips the column to "available" without a manual reload.
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import {
  render,
  waitFor,
  cleanup,
  fireEvent,
  screen,
} from "@testing-library/preact";
import { ServersScreen } from "./Servers";
import type { ScanResult, DaemonStatus } from "../types";

// happy-dom doesn't ship EventSource. ServersScreen subscribes to
// /api/events for the tray rescan-clients event; the stub matches
// other screen tests' minimal shape.
class StubEventSource {
  url: string;
  constructor(url: string) {
    this.url = url;
  }
  addEventListener(_t: string, _l: (ev: MessageEvent) => void): void {}
  removeEventListener(_t: string, _l: (ev: MessageEvent) => void): void {}
  close(): void {}
}
(globalThis as unknown as { EventSource: typeof StubEventSource }).EventSource =
  StubEventSource;

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function scanWith(presence: Record<string, string>): ScanResult {
  return {
    at: "2026-05-16T00:00:00Z",
    entries: [
      {
        name: "memory",
        manifest_exists: true,
        can_migrate: true,
        client_presence: {},
      },
    ],
    client_config_presence: presence as ScanResult["client_config_presence"],
  };
}

// fetchRouter dispatches each fetch call to the appropriate response
// based on the request URL. Lets tests describe the wire surface
// declaratively instead of chaining mockResolvedValueOnce calls in a
// brittle order.
function fetchRouter(routes: Record<string, (init?: RequestInit) => Response>) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    for (const prefix of Object.keys(routes)) {
      if (url.startsWith(prefix)) {
        return routes[prefix](init);
      }
    }
    throw new Error(`unexpected fetch: ${url}`);
  });
}

describe("ServersScreen — Initialize button (v0.4.5)", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    window.location.hash = "#/servers";
  });
  afterEach(() => {
    cleanup();
  });

  it("renders Initialize button in vscode header when presence is missing-init-possible", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () =>
          jsonResponse(200, scanWith({ vscode: "missing-init-possible" })),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("init-client-vscode")).toBeTruthy();
    });
    const btn = screen.getByTestId("init-client-vscode") as HTMLButtonElement;
    expect(btn.textContent).toBe("Initialize");
    expect(btn.disabled).toBe(false);
  });

  it("does NOT render Initialize button when presence is ok", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scanWith({ vscode: "ok" })),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);
    // Wait for matrix to render at all
    await waitFor(() => {
      expect(screen.queryAllByRole("columnheader").length).toBeGreaterThan(0);
    });
    expect(screen.queryByTestId("init-client-vscode")).toBeNull();
  });

  it("does NOT render Initialize button when presence is missing (no parent dir)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scanWith({ vscode: "missing" })),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryAllByRole("columnheader").length).toBeGreaterThan(0);
    });
    expect(screen.queryByTestId("init-client-vscode")).toBeNull();
  });

  // v0.4.5 PR #208 codex r1 F2: the hardened init pipeline refuses
  // to follow parent symlinks (POSIX O_NOFOLLOW, Windows
  // FILE_FLAG_OPEN_REPARSE_POINT). Scan emits the new
  // "missing-init-blocked-symlink" state for that case; the matrix
  // header must suppress the Initialize affordance so the operator
  // doesn't click a button that would deterministically fail.
  it("does NOT render Initialize button when presence is missing-init-blocked-symlink", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () =>
          jsonResponse(200, scanWith({ vscode: "missing-init-blocked-symlink" })),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryAllByRole("columnheader").length).toBeGreaterThan(0);
    });
    expect(screen.queryByTestId("init-client-vscode")).toBeNull();
  });

  it("on click POSTs to /api/init-client-config, shows success banner, and refreshes scan", async () => {
    let scanCallCount = 0;
    const initBodies: string[] = [];

    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => {
          scanCallCount += 1;
          // First scan: vscode missing-init-possible (button shows).
          // Subsequent scans (after init): vscode ok (button gone).
          const presence =
            scanCallCount === 1
              ? { vscode: "missing-init-possible" }
              : { vscode: "ok" };
          return jsonResponse(200, scanWith(presence));
        },
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/init-client-config": (init) => {
          if (init?.body) initBodies.push(init.body as string);
          return jsonResponse(200, {
            client: "vscode",
            path: "C:/Users/foo/AppData/Roaming/Code/User/mcp.json",
            created: true,
          });
        },
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("init-client-vscode")).toBeTruthy();
    });
    fireEvent.click(screen.getByTestId("init-client-vscode"));

    await waitFor(() => {
      expect(screen.queryByTestId("init-client-msg")).toBeTruthy();
    });
    const msg = screen.getByTestId("init-client-msg");
    expect(msg.textContent).toContain("Initialized vscode config");
    expect(msg.className).not.toContain("error");

    // Verify the POST body carried the client name verbatim.
    expect(initBodies.length).toBe(1);
    expect(JSON.parse(initBodies[0])).toEqual({ client: "vscode" });

    // Scan refetched at least twice (initial + after init). The
    // refetch is triggered by setReloadToken inside initializeClient
    // and happens in the next useEffect tick — waitFor lets the
    // microtask queue drain.
    await waitFor(() => {
      expect(scanCallCount).toBeGreaterThanOrEqual(2);
    });

    // Banner clears on the refresh that flips presence to "ok" (success
    // banner is non-sticky).
    await waitFor(() => {
      expect(screen.queryByTestId("init-client-msg")).toBeNull();
    });
  });

  it("shows error banner on PARENT_MISSING, preserves operational code, and refreshes scan (deep-sec Lane B round 3+4)", async () => {
    let scanCallCount = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => {
          scanCallCount += 1;
          // First scan: vscode missing-init-possible. Subsequent
          // scans (after PARENT_MISSING-triggered refresh): vscode
          // genuinely missing so the Initialize button disappears.
          const presence =
            scanCallCount === 1
              ? { vscode: "missing-init-possible" }
              : { vscode: "missing" };
          return jsonResponse(200, scanWith(presence));
        },
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/init-client-config": () =>
          jsonResponse(412, {
            error: "client config parent directory missing",
            code: "PARENT_MISSING",
          }),
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("init-client-vscode")).toBeTruthy();
    });
    fireEvent.click(screen.getByTestId("init-client-vscode"));

    await waitFor(() => {
      const banner = screen.queryByTestId("init-client-msg");
      expect(banner).toBeTruthy();
      expect(banner!.textContent).toContain("parent directory missing");
    });
    const banner = screen.getByTestId("init-client-msg");
    expect(banner.className).toContain("error");
    // Lane B round 3 P2 fix: the operational code MUST appear in
    // the banner so operators reading docs that reference
    // PARENT_MISSING / INIT_FAILED can map the banner back to them.
    expect(banner.textContent).toContain("PARENT_MISSING");

    // Lane B round 4 P2 fix: PARENT_MISSING triggers a scan refresh
    // so the stale Initialize button disappears (presence flips
    // from "missing-init-possible" to "missing"). Without this,
    // the operator would keep clicking the same failing button.
    await waitFor(() => {
      expect(scanCallCount).toBeGreaterThanOrEqual(2);
    });
    await waitFor(() => {
      expect(screen.queryByTestId("init-client-vscode")).toBeNull();
    });
    // Error banner persists across the scan refresh (sticky-on-error
    // semantic) — the operator must still see the failure context.
    const persistedBanner = screen.queryByTestId("init-client-msg");
    expect(persistedBanner).toBeTruthy();
    expect(persistedBanner!.textContent).toContain("PARENT_MISSING");
  });

  it("renders Initialize buttons for every missing-init-possible client independently", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () =>
          jsonResponse(
            200,
            scanWith({
              vscode: "missing-init-possible",
              cursor: "missing-init-possible",
              "qwen-cli": "ok",
              "claude-code": "missing",
            }),
          ),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("init-client-vscode")).toBeTruthy();
      expect(screen.queryByTestId("init-client-cursor")).toBeTruthy();
    });
    // qwen-cli is "ok" (no button), claude-code is "missing" (no button).
    expect(screen.queryByTestId("init-client-qwen-cli")).toBeNull();
    expect(screen.queryByTestId("init-client-claude-code")).toBeNull();
  });
});

// 2026-05-19 message-accuracy fix
// (work-items/bugs/2026-05-19-codex-config-symlink-blocked-by-pr209.md):
// a symlinked client config reports "error-symlink" in default/strict
// mode. The matrix cell must render a symlink-specific tooltip instead
// of the misleading generic "stat error — check permissions and disk
// health" message, and the cell stays disabled.
// 2026-06-02 opt-in-accuracy fix: the tooltip leads with the supported
// MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK opt-in (under which scan would
// instead report "ok") rather than claiming an unconditional refusal.
describe("ServersScreen — symlinked-config tooltip (2026-05-19)", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });
  afterEach(() => {
    cleanup();
  });

  it("renders the symlink-specific tooltip and disables the cell for error-symlink presence", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () =>
          jsonResponse(200, scanWith({ "codex-cli": "error-symlink" })),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryAllByRole("columnheader").length).toBeGreaterThan(0);
    });

    // The codex-cli cell carries the symlink tooltip; find it by its
    // load-bearing phrasing rather than column index.
    const cell = await screen.findByTitle(/config file is a symlink/);
    expect(cell).toBeTruthy();
    expect(cell.getAttribute("title")).toContain("confused-deputy");
    expect(cell.getAttribute("title")).toContain("PR #209");
    // The tooltip must surface the supported opt-in so dotfile-symlink
    // operators are pointed at the remediation, not away from it.
    expect(cell.getAttribute("title")).toContain(
      "MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK",
    );
    // 2026-06-03 opt-in-qualification fix (Codex PR #258 P3): the opt-in
    // flips a symlink to "ok" ONLY when its target is a regular file
    // (probeClientConfigPresence rst.Mode().IsRegular()). A dangling /
    // directory-target symlink stays error-symlink even with the env var
    // set, so the tooltip must qualify the opt-in on a regular-file
    // target rather than presenting it as an unconditional remedy.
    expect(cell.getAttribute("title")).toContain(
      "if the symlink points at a regular file",
    );
    // 2026-06-03 opt-in-restart fix (Codex PR #258 P3):
    // OperatorAllowsClientConfigSymlink() reads os.Getenv per-process at
    // runtime, so a running mcphub never observes an env var exported
    // after startup — a browser refresh keeps returning error-symlink.
    // The remediation must say to RESTART mcphub, not merely refresh.
    expect(cell.getAttribute("title")).toContain("restart mcphub");
    expect(cell.getAttribute("title")).not.toContain(
      "and refresh",
    );
    // Disabled because in default/strict mode the symlinked config
    // can't be written through.
    expect((cell as HTMLInputElement).disabled).toBe(true);
    // The misleading generic stat-error wording must NOT be used here.
    expect(cell.getAttribute("title")).not.toContain("stat error");
  });

  it("keeps the generic stat-error tooltip for plain 'error' presence", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () =>
          jsonResponse(200, scanWith({ "codex-cli": "error" })),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryAllByRole("columnheader").length).toBeGreaterThan(0);
    });

    const cell = await screen.findByTitle(/stat error/);
    expect(cell).toBeTruthy();
    // The generic error must NOT borrow the symlink wording.
    expect(cell.getAttribute("title")).not.toContain("symlink");
    expect((cell as HTMLInputElement).disabled).toBe(true);
  });
});

describe("ServersScreen — LSP matrix rows", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });
  afterEach(() => {
    cleanup();
  });

  it("renders language-server rows as badge-only cells without ineffective checkboxes", async () => {
    const scan: ScanResult = {
      at: "2026-06-03T00:00:00Z",
      entries: [
        {
          name: "mcp-language-server-python",
          manifest_exists: false,
          can_migrate: false,
          status: "via-hub",
          client_presence: {
            "codex-cli": {
              transport: "http",
              endpoint: "http://127.0.0.1:9200/lsp/python",
            },
          },
        },
      ],
      client_config_presence: { "codex-cli": "ok" },
    };

    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scan),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/workspaces": () =>
          jsonResponse(200, {
            workspaces: [
              {
                workspace_key: "default",
                workspace_path: "D:/dev/project",
              },
            ],
            entries: [
              {
                workspace_key: "default",
                workspace_path: "D:/dev/project",
                language: "python",
                backend: "mcp-language-server",
                port: 9200,
                task_name: "\\mcp-local-hub-lsp-default-python",
                client_entries: {
                  "codex-cli": "mcp-language-server-python",
                },
              },
            ],
          }),
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);

    const row = await screen.findByTestId("lsp-row-python");
    expect(row.querySelectorAll('input[type="checkbox"]')).toHaveLength(0);
    expect(screen.getByTestId("lsp-chip-primary-python-codex-cli").textContent).toBe(
      "via-hub",
    );
    expect(screen.getByTestId("lsp-edit-env-python")).toBeTruthy();
  });
});
