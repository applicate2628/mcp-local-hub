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
  act,
} from "@testing-library/preact";
import { ServersScreen } from "./Servers";
import { ALL_CLIENTS, CORE_CLIENTS } from "../lib/routing";
import { installMemoryLocalStorage } from "../lib/test-local-storage";
import type { ScanResult, DaemonStatus } from "../types";

// happy-dom 20.9.0's globalThis.localStorage is a bare object with no
// Storage methods; the column-visibility feature persists through it, so
// install a Map-backed shim for these tests.
const ls = installMemoryLocalStorage();

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

function scanWith(presence: Record<string, string>, name = "memory"): ScanResult {
  // visibleClients() gates an auto-detected non-core column on the SCANNABLE
  // capability, and effectiveVisibleClients() additionally drops a PINNED
  // (pref === true) non-core column unless that client is scannable. Mark the
  // ENTIRE client registry scannable (not just the core set) so these tests
  // exercise the file-present detection + pref gate, not the scannable gate
  // (that is unit-tested in routing.test.ts) — and so a pinned non-core client
  // (e.g. hermes/kiro) renders its column instead of being dropped by the
  // scannable re-check. direct_installable / remote_http_capable are irrelevant
  // to column visibility, so default both to false.
  const capabilities: NonNullable<ScanResult["client_capabilities"]> = {};
  for (const c of [...ALL_CLIENTS, ...Object.keys(presence)]) {
    capabilities[c] = { scannable: true, direct_installable: false, remote_http_capable: false };
  }
  return {
    at: "2026-05-16T00:00:00Z",
    entries: [
      {
        name,
        manifest_exists: true,
        can_migrate: true,
        client_presence: {},
      },
    ],
    client_config_presence: presence as ScanResult["client_config_presence"],
    client_capabilities: capabilities,
  };
}

// fetchRouter dispatches each fetch call to the appropriate response
// based on the request URL. Lets tests describe the wire surface
// declaratively instead of chaining mockResolvedValueOnce calls in a
// brittle order.
function fetchRouter(routes: Record<string, (init?: RequestInit) => Response | Promise<Response>>) {
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

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
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

  // G17 (2026-06-18): "missing-init-creatable" (config dir absent but
  // securely creatable under the user home) also surfaces the Initialize
  // affordance, with a tooltip that names the directory-creation
  // semantic so the operator knows a config DIR is being made for a
  // not-yet-installed client.
  it("renders Initialize button in vscode header when presence is missing-init-creatable", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () =>
          jsonResponse(200, scanWith({ vscode: "missing-init-creatable" })),
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
    // Tooltip must describe the create-directory semantic, distinct
    // from the missing-init-possible (stub-only) wording.
    expect(btn.title).toContain("config directory does not exist yet");
    expect(btn.title).toContain("create the config directory");
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

  it("unchecking a routed language-server client posts the per-client LSP router disable endpoint", async () => {
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
              endpoint: "http://127.0.0.1:9200/lsp/python/mcp",
            },
          },
        },
      ],
      client_config_presence: { "codex-cli": "ok" },
    };
    const disableBodies: unknown[] = [];

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
        "/api/lsp-router/disable": (init?: RequestInit) => {
          disableBodies.push(JSON.parse(String(init?.body ?? "{}")));
          return jsonResponse(200, {
            client: "codex-cli",
            enabled: false,
            report: {
              removed: [
                {
                  client: "codex-cli",
                  language: "python",
                  entry_name: "mcp-language-server-python",
                },
              ],
            },
          });
        },
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);

    const toggle = (await screen.findByTestId(
      "lsp-toggle-python-codex-cli",
    )) as HTMLInputElement;
    expect(toggle.checked).toBe(true);
    expect(screen.getByTestId("lsp-chip-primary-python-codex-cli").textContent).toBe(
      "via-hub",
    );
    expect(screen.getByTestId("lsp-edit-env-python")).toBeTruthy();

    fireEvent.click(toggle);

    await waitFor(() => {
      expect(disableBodies).toHaveLength(1);
    });
    expect(disableBodies[0]).toEqual({ client: "codex-cli" });
  });

  it("checking an absent language-server client posts the per-client LSP router enable endpoint", async () => {
    const scan: ScanResult = {
      at: "2026-06-03T00:00:00Z",
      entries: [],
      client_config_presence: { "codex-cli": "ok" },
    };
    const enableBodies: unknown[] = [];

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
                client_entries: {},
              },
            ],
          }),
        "/api/lsp-router/enable": (init?: RequestInit) => {
          enableBodies.push(JSON.parse(String(init?.body ?? "{}")));
          return jsonResponse(200, {
            client: "codex-cli",
            enabled: true,
            report: {
              applied: [
                {
                  client: "codex-cli",
                  language: "python",
                  entry_name: "mcp-language-server-python",
                },
              ],
            },
          });
        },
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);

    const toggle = (await screen.findByTestId(
      "lsp-toggle-python-codex-cli",
    )) as HTMLInputElement;
    expect(toggle.checked).toBe(false);

    fireEvent.click(toggle);

    await waitFor(() => {
      expect(enableBodies).toHaveLength(1);
    });
    expect(enableBodies[0]).toEqual({ client: "codex-cli" });
  });

  it("enables an unregistered language-server row through the selected workspace", async () => {
    const scan: ScanResult = {
      at: "2026-06-03T00:00:00Z",
      entries: [],
      client_config_presence: { "codex-cli": "ok" },
    };
    const registerBodies: unknown[] = [];

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
            entries: [],
          }),
        "/api/lsp/register": (init?: RequestInit) => {
          registerBodies.push(JSON.parse(String(init?.body ?? "{}")));
          return jsonResponse(200, {
            workspace: "D:/dev/project",
            workspace_key: "default",
            entries: [
              {
                workspace_key: "default",
                workspace_path: "D:/dev/project",
                language: "go",
                backend: "gopls-mcp",
                port: 9201,
                task_name: "\\mcp-local-hub-lsp-default-go",
              },
            ],
          });
        },
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);

    const enable = await screen.findByTestId("lsp-enable-go");
    expect(enable.textContent).toBe("Enable");

    fireEvent.click(enable);

    await waitFor(() => {
      expect(registerBodies).toHaveLength(1);
    });
    expect(registerBodies[0]).toEqual({
      workspace_path: "D:/dev/project",
      language: "go",
    });
  });

  // Fresh-machine connect path: a host with ZERO registered workspaces must be
  // able to register the FIRST workspace from the GUI (no CLI). The
  // RegisterWorkspacePanel in the LSP daemons section takes a typed path + a
  // language and POSTs /api/lsp/register directly.
  it("registers the first workspace from the GUI when no workspace exists yet", async () => {
    const scan: ScanResult = {
      at: "2026-06-03T00:00:00Z",
      entries: [],
      client_config_presence: { "codex-cli": "ok" },
    };
    const registerBodies: unknown[] = [];
    let workspacesCallCount = 0;

    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scan),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/workspaces": () => {
          workspacesCallCount += 1;
          // First load: empty registry (fresh machine). After register: the
          // new workspace appears so the selector + rows reflect it.
          return workspacesCallCount === 1
            ? jsonResponse(200, { workspaces: [], entries: [] })
            : jsonResponse(200, {
                workspaces: [{ workspace_key: "my-project", workspace_path: "D:/dev/my-project" }],
                entries: [
                  {
                    workspace_key: "my-project",
                    workspace_path: "D:/dev/my-project",
                    language: "python",
                    backend: "mcp-language-server",
                    port: 9202,
                    task_name: "\\mcp-local-hub-lsp-my-project-python",
                  },
                ],
              });
        },
        "/api/lsp/register": (init?: RequestInit) => {
          registerBodies.push(JSON.parse(String(init?.body ?? "{}")));
          return jsonResponse(200, {
            workspace: "D:/dev/my-project",
            workspace_key: "my-project",
            entries: [
              {
                workspace_key: "my-project",
                workspace_path: "D:/dev/my-project",
                language: "python",
                backend: "mcp-language-server",
                port: 9202,
                task_name: "\\mcp-local-hub-lsp-my-project-python",
              },
            ],
            results: [{ language: "python", status: "ok" }],
          });
        },
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);

    // The first-workspace register panel is present even with zero workspaces.
    const pathInput = (await screen.findByTestId(
      "lsp-register-workspace-path",
    )) as HTMLInputElement;
    const langSelect = screen.getByTestId(
      "lsp-register-workspace-language",
    ) as HTMLSelectElement;
    const submit = screen.getByTestId(
      "lsp-register-workspace-submit",
    ) as HTMLButtonElement;

    // The empty-state intro nudges the operator to register here (not the CLI).
    expect(screen.getByTestId("lsp-register-workspace-intro").textContent).toContain(
      "No workspace registered yet",
    );
    // Submit is disabled until a path is entered.
    expect(submit.disabled).toBe(true);

    fireEvent.input(pathInput, { target: { value: "D:/dev/my-project" } });
    fireEvent.change(langSelect, { target: { value: "python" } });
    expect(submit.disabled).toBe(false);

    fireEvent.click(submit);

    await waitFor(() => {
      expect(registerBodies).toHaveLength(1);
    });
    expect(registerBodies[0]).toEqual({
      workspace_path: "D:/dev/my-project",
      language: "python",
    });
    // After a successful register the workspace list reloads (selector reflects it).
    await waitFor(() => {
      expect(workspacesCallCount).toBeGreaterThan(1);
    });
  });

  // Finding 1 (Codex P2): a legacy per-workspace HTTP entry
  // (http://127.0.0.1:<port>/mcp) is NOT the shared /lsp/<lang>/mcp router, so
  // its toggle must render UNCHECKED — pre-fix it rendered checked because the
  // transport happened to be "http". Checking it then enables the shared router.
  it("renders a legacy per-workspace HTTP LSP entry as unchecked (not the shared router)", async () => {
    const scan: ScanResult = {
      at: "2026-07-09T00:00:00Z",
      entries: [
        {
          name: "mcp-language-server-python",
          manifest_exists: false,
          can_migrate: false,
          status: "via-hub",
          client_presence: {
            "codex-cli": {
              transport: "http",
              // Legacy per-workspace daemon URL (path /mcp), NOT the shared
              // /lsp/python/mcp router this toggle owns.
              endpoint: "http://127.0.0.1:9200/mcp",
            },
          },
        },
      ],
      client_config_presence: { "codex-cli": "ok" },
    };
    const enableBodies: unknown[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scan),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/workspaces": () => jsonResponse(200, { workspaces: [], entries: [] }),
        "/api/lsp-router/enable": (init?: RequestInit) => {
          enableBodies.push(JSON.parse(String(init?.body ?? "{}")));
          return jsonResponse(200, { client: "codex-cli", enabled: true, report: {} });
        },
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);

    const toggle = (await screen.findByTestId(
      "lsp-toggle-python-codex-cli",
    )) as HTMLInputElement;
    // Legacy /mcp entry → unchecked (finding 1) and still interactive (config ok).
    expect(toggle.checked).toBe(false);
    expect(toggle.disabled).toBe(false);

    // Checking it enables the shared LSP router for the client.
    fireEvent.click(toggle);
    await waitFor(() => expect(enableBodies).toHaveLength(1));
    expect(enableBodies[0]).toEqual({ client: "codex-cli" });
  });

  // Finding 2 (Codex P2): a client whose config is missing/error has no usable
  // target, so its LSP toggle must be DISABLED — pre-fix only `busy` gated it.
  // A sibling "ok" client stays enabled, proving the gate is the SAME
  // client_config_presence usability the main matrix uses.
  it("disables the LSP toggle for a client with an error config and enables it for an ok client", async () => {
    const scan: ScanResult = {
      at: "2026-07-09T00:00:00Z",
      entries: [],
      client_config_presence: { "codex-cli": "error", "gemini-cli": "ok" },
    };
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scan),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/workspaces": () => jsonResponse(200, { workspaces: [], entries: [] }),
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);

    const errored = (await screen.findByTestId(
      "lsp-toggle-python-codex-cli",
    )) as HTMLInputElement;
    const usable = screen.getByTestId(
      "lsp-toggle-python-gemini-cli",
    ) as HTMLInputElement;
    // No usable config → disabled; usable "ok" config → enabled.
    expect(errored.disabled).toBe(true);
    expect(usable.disabled).toBe(false);
  });

  // Finding 3 (Codex P2): an inherited shared-router entry (a config layer
  // mcphub never wrote, e.g. ~/.claude.json) is hub-routed but READ-ONLY —
  // checked yet disabled, mirroring the main matrix's via-hub-inherited cell.
  // Pre-fix it was an enabled switch that would always fail closed.
  it("renders an inherited LSP router cell as checked-but-disabled (read-only)", async () => {
    const scan: ScanResult = {
      at: "2026-07-09T00:00:00Z",
      entries: [
        {
          name: "mcp-language-server-python",
          manifest_exists: false,
          can_migrate: false,
          status: "via-hub-inherited",
          client_presence: {
            "codex-cli": {
              transport: "http",
              endpoint: "http://127.0.0.1:9200/lsp/python/mcp",
              inherited: true,
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
        "/api/workspaces": () => jsonResponse(200, { workspaces: [], entries: [] }),
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);

    const toggle = (await screen.findByTestId(
      "lsp-toggle-python-codex-cli",
    )) as HTMLInputElement;
    // Inherited → checked (it IS hub-routed) but disabled (read-only).
    expect(toggle.checked).toBe(true);
    expect(toggle.disabled).toBe(true);
    const cell = screen.getByTestId("lsp-cell-python-codex-cli");
    expect(cell.getAttribute("data-inherited")).toBe("true");
    const label = toggle.closest("label");
    expect(label?.getAttribute("title") ?? "").toContain("inherits");
  });
});

// The non-core opt-in clients are detection-gated as matrix columns. A
// non-core client surfaces as a column header only when the scan reports it
// present on the host; an undetected one adds no column. The non-core
// universe is derived live from client_config_presence (all backend clients),
// not a hardcoded list, so all supported clients can surface when detected.
describe("ServersScreen — detection-gated non-core client columns", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    window.location.hash = "#/servers";
  });
  afterEach(() => {
    cleanup();
  });

  function headerLabels(): string[] {
    return screen
      .queryAllByRole("columnheader")
      .map((th) => th.textContent?.trim() ?? "");
  }

  it("does NOT render a zed column on a host with no zed config", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scanWith({ "claude-code": "ok" })),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryAllByRole("columnheader").length).toBeGreaterThan(0);
    });
    // Core columns present, zed (and other wave-2) absent.
    expect(headerLabels().some((t) => t.includes("claude-code"))).toBe(true);
    expect(headerLabels().some((t) => t.includes("zed"))).toBe(false);
    expect(headerLabels().some((t) => t.includes("openclaw"))).toBe(false);
  });

  it("renders a zed column when scan detects zed (relay-stdio client)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () =>
          jsonResponse(200, scanWith({ "claude-code": "ok", zed: "ok" })),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await waitFor(() => {
      expect(headerLabels().some((t) => t.includes("zed"))).toBe(true);
    });
    // Other undetected wave-2 clients still absent.
    expect(headerLabels().some((t) => t.includes("kiro"))).toBe(false);
  });

  it("renders multiple detected wave-2 columns when their config FILES are present (kiro http-direct + zed relay)", async () => {
    // Both have an actual config file ("ok"), so both earn a column. (A
    // "missing-init-*" parent-exists-but-file-absent state would NOT — that is
    // the anti-overflow gate, covered in routing.test.ts.)
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () =>
          jsonResponse(
            200,
            scanWith({ "claude-code": "ok", zed: "ok", kiro: "ok" }),
          ),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await waitFor(() => {
      expect(headerLabels().some((t) => t.includes("kiro"))).toBe(true);
    });
    expect(headerLabels().some((t) => t.includes("zed"))).toBe(true);
  });

  it("HIDES a wave-2 client whose config file is absent but parent dir exists (no overflow on fresh profile)", async () => {
    // Finding 1 anti-overflow: a "missing-init-possible" non-core client does
    // NOT earn a column even though it is scannable — only a present config
    // FILE does. Pre-fix this state counted as detected and overflowed the
    // matrix on a fresh profile.
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () =>
          jsonResponse(
            200,
            scanWith({ "claude-code": "ok", zed: "missing-init-possible", kiro: "missing-init-creatable" }),
          ),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryAllByRole("columnheader").length).toBeGreaterThan(0);
    });
    expect(headerLabels().some((t) => t.includes("zed"))).toBe(false);
    expect(headerLabels().some((t) => t.includes("kiro"))).toBe(false);
  });
});

// Manual column-visibility: the "Manage columns" popover lets the operator
// override the detection-gated default — hide a noisy column or pin an
// undetected one. Choices persist in localStorage and re-render the matrix
// live. Pure view filter; apply/migrate logic untouched.
describe("ServersScreen — manual column visibility", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    ls.reset();
    window.location.hash = "#/servers";
  });
  afterEach(() => {
    cleanup();
    ls.reset();
  });

  function headerLabels(): string[] {
    return screen
      .queryAllByRole("columnheader")
      .map((th) => th.textContent?.trim() ?? "");
  }

  function bareScanRouter() {
    return fetchRouter({
      "/api/scan": () => jsonResponse(200, scanWith({ "claude-code": "ok" })),
      "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
    }) as unknown as typeof fetch;
  }

  it("renders the Columns button labelled with the visible/total count", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(bareScanRouter());
    render(<ServersScreen />);
    const btn = await screen.findByTestId("matrix-columns-button");
    // Bare host: 7 core clients visible out of the full registry total.
    expect(btn.textContent).toContain(
      `Clients (${CORE_CLIENTS.length}/${ALL_CLIENTS.length})`,
    );
  });

  it("opens the popover with a checkbox for every supported client on click", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(bareScanRouter());
    render(<ServersScreen />);
    const btn = await screen.findByTestId("matrix-columns-button");
    expect(screen.queryByTestId("matrix-columns-popover")).toBeNull();
    fireEvent.click(btn);
    expect(screen.queryByTestId("matrix-columns-popover")).toBeTruthy();
    // aria-expanded reflects state.
    expect(btn.getAttribute("aria-expanded")).toBe("true");
    // A toggle exists for every known client (the full CORE + NON_CORE
    // registry mirror), including the mimocode OpenCode fork and newer
    // non-core clients beyond the original wave-2 set (e.g. warp, goose,
    // zencoder).
    for (const c of ["claude-code", "zed", "mimocode", "hermes", "openclaw", "warp", "goose", "zencoder"]) {
      expect(screen.queryByTestId(`matrix-columns-toggle-${c}`)).toBeTruthy();
    }
    // The popover lists one checkbox per ALL_CLIENTS entry.
    for (const c of ALL_CLIENTS) {
      expect(screen.queryByTestId(`matrix-columns-toggle-${c}`)).toBeTruthy();
    }
  });

  it("hiding a detected core column removes it from the matrix header and persists", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(bareScanRouter());
    render(<ServersScreen />);
    const btn = await screen.findByTestId("matrix-columns-button");
    expect(headerLabels().some((t) => t.includes("claude-code"))).toBe(true);

    fireEvent.click(btn);
    const toggle = screen.getByTestId(
      "matrix-columns-toggle-claude-code",
    ) as HTMLInputElement;
    expect(toggle.checked).toBe(true);
    fireEvent.click(toggle); // uncheck → hide

    await waitFor(() => {
      expect(headerLabels().some((t) => t.includes("claude-code"))).toBe(false);
    });
    // Count drops by one (one core column hidden).
    expect(btn.textContent).toContain(
      `Clients (${CORE_CLIENTS.length - 1}/${ALL_CLIENTS.length})`,
    );
    // Persisted to localStorage under the documented key.
    const stored = JSON.parse(
      localStorage.getItem("mcphub.servers.column-visibility") ?? "{}",
    );
    expect(stored["claude-code"]).toBe(false);
  });

  it("pinning an undetected wave-2 column adds it to the matrix header", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(bareScanRouter());
    render(<ServersScreen />);
    const btn = await screen.findByTestId("matrix-columns-button");
    // hermes is NOT detected on this bare host → no column.
    expect(headerLabels().some((t) => t.includes("hermes"))).toBe(false);

    fireEvent.click(btn);
    const toggle = screen.getByTestId(
      "matrix-columns-toggle-hermes",
    ) as HTMLInputElement;
    expect(toggle.checked).toBe(false);
    fireEvent.click(toggle); // check → pin visible

    await waitFor(() => {
      expect(headerLabels().some((t) => t.includes("hermes"))).toBe(true);
    });
  });

  it("restores persisted overrides on next mount", async () => {
    // Seed an override BEFORE first render: hide claude-code, pin kiro.
    localStorage.setItem(
      "mcphub.servers.column-visibility",
      JSON.stringify({ "claude-code": false, kiro: true }),
    );
    vi.spyOn(globalThis, "fetch").mockImplementation(bareScanRouter());
    render(<ServersScreen />);
    await screen.findByTestId("matrix-columns-button");
    await waitFor(() => {
      expect(headerLabels().some((t) => t.includes("kiro"))).toBe(true);
    });
    expect(headerLabels().some((t) => t.includes("claude-code"))).toBe(false);
  });

  it("Reset to auto clears overrides and reverts the matrix to detection default", async () => {
    localStorage.setItem(
      "mcphub.servers.column-visibility",
      JSON.stringify({ "claude-code": false, kiro: true }),
    );
    vi.spyOn(globalThis, "fetch").mockImplementation(bareScanRouter());
    render(<ServersScreen />);
    const btn = await screen.findByTestId("matrix-columns-button");
    await waitFor(() => {
      expect(headerLabels().some((t) => t.includes("kiro"))).toBe(true);
    });

    fireEvent.click(btn);
    fireEvent.click(screen.getByTestId("matrix-columns-reset"));

    await waitFor(() => {
      // claude-code back (auto-detected core), kiro gone (undetected).
      expect(headerLabels().some((t) => t.includes("claude-code"))).toBe(true);
      expect(headerLabels().some((t) => t.includes("kiro"))).toBe(false);
    });
    // Persisted record removed.
    expect(localStorage.getItem("mcphub.servers.column-visibility")).toBeNull();
  });

  it("Escape closes the popover", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(bareScanRouter());
    render(<ServersScreen />);
    const btn = await screen.findByTestId("matrix-columns-button");
    fireEvent.click(btn);
    expect(screen.queryByTestId("matrix-columns-popover")).toBeTruthy();
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => {
      expect(screen.queryByTestId("matrix-columns-popover")).toBeNull();
    });
  });
});

// Auto-refresh + Rescan control + dirty-state no-clobber. The matrix is
// scan-driven; useAutoScan(loadScan, dirty.size > 0) polls /api/scan every
// SCAN_POLL_MS while no edits are pending, and PAUSES while there are
// unsaved dirty cells so a poll tick can't discard them.
describe("ServersScreen — auto-refresh + Rescan no-clobber", () => {
  // A manifested entry routed via-hub on claude-code → the matrix renders
  // one interactive (checked) checkbox the operator can uncheck to make a
  // pending "demigrate" dirty cell.
  function viaHubScan(name = "memory"): ScanResult {
    return {
      at: "2026-06-15T00:00:00Z",
      entries: [
        {
          name,
          manifest_exists: true,
          can_migrate: true,
          status: "via-hub",
          // Port-aware via-hub: the loopback cell at :9200 is only "via-hub"
          // when 9200 is a declared daemon port for this server.
          daemon_ports: [9200],
          client_presence: {
            "claude-code": {
              transport: "http",
              endpoint: "http://127.0.0.1:9200/memory",
            },
          },
        },
      ],
      client_config_presence: { "claude-code": "ok" },
    };
  }

  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useFakeTimers();
    // Reset visibility to "visible" each test (a prior test may have left
    // the redefined getter in place).
    Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
    window.location.hash = "#/servers";
  });
  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  // SCAN_POLL_MS is 10_000 (see useAutoScan.ts). Hardcode here to avoid
  // an import; if the const changes, this test must be updated alongside.
  const POLL_MS = 10_000;

  it("renders the Rescan control with an 'updated …' indicator", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, viaHubScan()),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await vi.waitFor(() => {
      expect(screen.queryByTestId("scan-rescan-btn")).toBeTruthy();
    });
    const ago = screen.getByTestId("scan-updated-ago");
    expect(ago.textContent).toMatch(/updated|scanning/);
  });

  it("re-fetches /api/scan after SCAN_POLL_MS while no edits are pending", async () => {
    let scanCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => {
          scanCalls += 1;
          return jsonResponse(200, viaHubScan());
        },
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await vi.waitFor(() => expect(scanCalls).toBeGreaterThanOrEqual(1));
    const initial = scanCalls;

    await vi.advanceTimersByTimeAsync(POLL_MS);
    await vi.waitFor(() => expect(scanCalls).toBe(initial + 1));
  });

  it("Rescan now triggers an immediate /api/scan refetch", async () => {
    let scanCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => {
          scanCalls += 1;
          return jsonResponse(200, viaHubScan());
        },
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await vi.waitFor(() => expect(screen.queryByTestId("scan-rescan-btn")).toBeTruthy());
    const before = scanCalls;
    fireEvent.click(screen.getByTestId("scan-rescan-btn"));
    await vi.waitFor(() => expect(scanCalls).toBe(before + 1));
  });


  it("ignores an older overlapping refresh that resolves after a newer one", async () => {
    const olderScan = deferred<Response>();
    const newerScan = deferred<Response>();
    let scanCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => {
          scanCalls += 1;
          return scanCalls === 1 ? olderScan.promise : newerScan.promise;
        },
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await vi.waitFor(() => expect(scanCalls).toBe(1));

    await vi.advanceTimersByTimeAsync(POLL_MS);
    await vi.waitFor(() => expect(scanCalls).toBe(2));

    await act(async () => {
      newerScan.resolve(jsonResponse(200, viaHubScan("fresh-memory")));
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });
    await vi.waitFor(() => expect(screen.queryByText("fresh-memory")).toBeTruthy());

    await act(async () => {
      olderScan.resolve(jsonResponse(200, viaHubScan("stale-memory")));
      await Promise.resolve();
      await Promise.resolve();
      await Promise.resolve();
    });
    await vi.waitFor(() => expect(screen.queryByText("stale-memory")).toBeNull());
    expect(screen.queryByText("fresh-memory")).toBeTruthy();
  });

  it("pauses auto-refresh and disables Rescan while a dirty cell is pending — and does NOT clobber the edit", async () => {
    let scanCalls = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => {
          scanCalls += 1;
          return jsonResponse(200, viaHubScan());
        },
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);

    // The via-hub memory/claude-code cell renders one checked, interactive
    // checkbox. Uncheck it → pending "demigrate" dirty cell.
    const box = await vi.waitFor(() => {
      const inputs = document.querySelectorAll<HTMLInputElement>(
        'table.servers-matrix tbody input[type="checkbox"]',
      );
      const checked = Array.from(inputs).find((i) => i.checked && !i.disabled);
      if (!checked) throw new Error("no interactive checked cell yet");
      return checked;
    });
    const scansAtEdit = scanCalls;
    fireEvent.click(box);
    expect(box.checked).toBe(false); // edit visible

    // Apply button is now enabled (dirty.size > 0); the paused note + a
    // disabled Rescan button appear.
    await vi.waitFor(() => {
      expect(screen.queryByTestId("scan-paused-note")).toBeTruthy();
    });
    const rescanBtn = screen.getByTestId("scan-rescan-btn") as HTMLButtonElement;
    expect(rescanBtn.disabled).toBe(true);

    // Advance well past several poll periods — NO auto refetch fires while
    // dirty, and the unchecked edit survives (not clobbered by a baseline
    // reload).
    await vi.advanceTimersByTimeAsync(POLL_MS * 3);
    expect(scanCalls).toBe(scansAtEdit); // paused → zero new scans
    expect(box.checked).toBe(false); // edit preserved across the paused window
  });

  it("pauses auto-refresh while an LSP router toggle POST is in flight", async () => {
    let scanCalls = 0;
    const disable = deferred<Response>();
    const disableBodies: unknown[] = [];
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
              endpoint: "http://127.0.0.1:9200/lsp/python/mcp",
            },
          },
        },
      ],
      client_config_presence: { "codex-cli": "ok" },
    };

    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => {
          scanCalls += 1;
          return jsonResponse(200, scan);
        },
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
        "/api/lsp-router/disable": (init?: RequestInit) => {
          disableBodies.push(JSON.parse(String(init?.body ?? "{}")));
          return disable.promise;
        },
      }) as unknown as typeof fetch,
    );

    render(<ServersScreen />);

    const toggle = (await screen.findByTestId(
      "lsp-toggle-python-codex-cli",
    )) as HTMLInputElement;
    expect(toggle.checked).toBe(true);

    const scansAtToggle = scanCalls;
    fireEvent.click(toggle);
    await vi.waitFor(() => expect(disableBodies).toHaveLength(1));
    await vi.waitFor(() => expect(toggle.checked).toBe(false));

    await vi.advanceTimersByTimeAsync(POLL_MS * 2);
    expect(scanCalls).toBe(scansAtToggle);
    expect(toggle.checked).toBe(false);

    await act(async () => {
      disable.resolve(
        jsonResponse(200, {
          client: "codex-cli",
          enabled: false,
          report: {},
        }),
      );
      await Promise.resolve();
      await Promise.resolve();
    });
  });
});

// Whole-row toggle: clicking the server-name row affordance flips EVERY
// interactive cell in that row at once — the row analog of the column header
// toggle, fed through the same flipCellGroup owner.
describe("ServersScreen — whole-row toggle", () => {
  // One manifested server with TWO interactive cells in mixed state:
  // claude-code via-hub (checked) + codex-cli "available" (config ok, no
  // entry → unchecked). The row is therefore NOT all-checked initially.
  function mixedRowScan(): ScanResult {
    return {
      at: "2026-06-15T00:00:00Z",
      entries: [
        {
          name: "memory",
          manifest_exists: true,
          can_migrate: true,
          status: "via-hub",
          // Port-aware via-hub: 9200 must be a declared daemon port for the
          // loopback claude-code cell to render checked/via-hub.
          daemon_ports: [9200],
          client_presence: {
            "claude-code": {
              transport: "http",
              endpoint: "http://127.0.0.1:9200/memory",
            },
          },
        },
      ],
      client_config_presence: { "claude-code": "ok", "codex-cli": "ok" },
    };
  }

  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
    window.location.hash = "#/servers";
  });
  afterEach(() => cleanup());

  const interactiveRowInputs = () =>
    Array.from(
      document.querySelectorAll<HTMLInputElement>(
        'table.servers-matrix:not(.lsp-matrix) tbody tr input[type="checkbox"]:not(:disabled)',
      ),
    );

  it("flips every interactive cell in the row on click, and back on a second click", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, mixedRowScan()),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);

    const rowToggle = await screen.findByTestId("matrix-row-toggle-memory");

    // Two interactive cells; only the via-hub one is checked → not all-checked.
    await vi.waitFor(() => expect(interactiveRowInputs().length).toBe(2));
    expect(interactiveRowInputs().filter((i) => i.checked).length).toBe(1);

    // Click row toggle → not-all-checked → flip every interactive cell ON.
    fireEvent.click(rowToggle);
    await vi.waitFor(() => expect(interactiveRowInputs().every((i) => i.checked)).toBe(true));
    // Bulk edit is dirty → Apply enabled.
    expect((screen.getByRole("button", { name: /Apply changes/ }) as HTMLButtonElement).disabled).toBe(false);

    // Click again → all-checked → flip every interactive cell OFF.
    fireEvent.click(rowToggle);
    await vi.waitFor(() => expect(interactiveRowInputs().every((i) => !i.checked)).toBe(true));
  });
});

// G15 a11y: the matrix tables must carry table semantics so a screen-reader
// user can associate each toggle cell with its server (row) and client
// (column). These assertions are additive — they do not change any existing
// testid, text, or role count, only verify the new scope= + <caption>.
describe("ServersScreen — matrix table a11y semantics (G15)", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    window.location.hash = "#/servers";
  });
  afterEach(() => {
    cleanup();
  });

  it("renders a <caption> and scope= on the servers matrix headers", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scanWith({ vscode: "ok" })),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );

    const { container } = render(<ServersScreen />);
    await waitFor(() => {
      expect(container.querySelector("table.servers-matrix")).toBeTruthy();
    });

    const table = container.querySelector("table.servers-matrix") as HTMLTableElement;
    // <caption> present and non-empty (describes the matrix for AT).
    const caption = table.querySelector("caption");
    expect(caption).toBeTruthy();
    expect((caption!.textContent ?? "").trim().length).toBeGreaterThan(0);

    // Every column header (<thead th>) carries scope="col".
    const colHeaders = table.querySelectorAll("thead th");
    expect(colHeaders.length).toBeGreaterThan(0);
    colHeaders.forEach((th) => {
      expect(th.getAttribute("scope")).toBe("col");
    });

    // The per-server row header cell is a <th scope="row"> (associates every
    // client toggle in the row with the server name).
    const rowHeaders = table.querySelectorAll("tbody th[scope='row']");
    expect(rowHeaders.length).toBeGreaterThan(0);
  });

  it("renders a <caption> and scope= on the LSP matrix headers", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scanWith({ vscode: "ok" })),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );

    const { container } = render(<ServersScreen />);
    await waitFor(() => {
      expect(container.querySelector("table.lsp-matrix")).toBeTruthy();
    });

    const lsp = container.querySelector("table.lsp-matrix") as HTMLTableElement;
    const caption = lsp.querySelector("caption");
    expect(caption).toBeTruthy();
    expect((caption!.textContent ?? "").trim().length).toBeGreaterThan(0);

    const colHeaders = lsp.querySelectorAll("thead th");
    expect(colHeaders.length).toBeGreaterThan(0);
    colHeaders.forEach((th) => {
      expect(th.getAttribute("scope")).toBe("col");
    });

    // The LSP matrix always renders 9 placeholder language rows, each a
    // <th scope="row">, even on a clean home.
    const rowHeaders = lsp.querySelectorAll("tbody th[scope='row']");
    expect(rowHeaders.length).toBeGreaterThan(0);
  });
});

// A3 PR-2 — "Resolve symlink → write to real target" affordance on a
// config-error-symlink cell. A client whose config is a symlink renders the
// cell disabled with a symlink tooltip; the affordance is the explicit
// per-config ENABLE. Clicking opens a confirm modal that shows the PINNED real
// path; confirming POSTs the two-phase resolve→write and rescans.
describe("ServersScreen — symlink-consent affordance (A3 PR-2)", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    window.location.hash = "#/servers";
  });
  afterEach(() => {
    cleanup();
  });

  // A scan with the memory server present and codex-cli's config a symlink
  // (top-level client_config_presence "error-symlink", codex-cli NOT in the
  // server's client_presence → routing maps the cell to config-error-symlink).
  function scanSymlink(): ScanResult {
    return {
      at: "2026-06-21T00:00:00Z",
      entries: [
        {
          name: "memory",
          manifest_exists: true,
          can_migrate: true,
          client_presence: {},
        },
      ],
      client_config_presence: {
        "codex-cli": "error-symlink",
      } as ScanResult["client_config_presence"],
    };
  }

  it("renders the Resolve symlink button on a config-error-symlink cell", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scanSymlink()),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("resolve-symlink-codex-cli")).toBeTruthy();
    });
    const btn = screen.getByTestId("resolve-symlink-codex-cli") as HTMLButtonElement;
    expect(btn.textContent).toContain("Resolve symlink");
  });

  it("clicking opens a confirm modal showing the PINNED real path", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scanSymlink()),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/resolve-symlink-and-write": () =>
          jsonResponse(200, {
            client: "codex-cli",
            original_path: "/home/u/.codex/config.toml",
            resolved_target: "/e/env/Agents/.codex/config.toml",
            pinned_real_path: "/e/env/Agents/.codex",
            content_hash: "deadbeef",
            is_symlink: true,
          }),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("resolve-symlink-codex-cli")).toBeTruthy();
    });
    fireEvent.click(screen.getByTestId("resolve-symlink-codex-cli"));
    await waitFor(() => {
      expect(screen.queryByTestId("resolve-symlink-modal-codex-cli")).toBeTruthy();
    });
    const pinned = screen.getByTestId("resolve-symlink-pinned-codex-cli");
    // The modal renders the resolved real target so the operator sees exactly
    // what they consent to.
    expect(pinned.textContent).toBe("/e/env/Agents/.codex/config.toml");
  });

  it("confirming the modal POSTs the write with the pinned path + hash and rescans", async () => {
    let scanCount = 0;
    const bodies: string[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => {
          scanCount += 1;
          // After a successful resolve-write the cell flips to ok.
          return jsonResponse(200, scanCount === 1 ? scanSymlink() : scanWith({ "codex-cli": "ok" }));
        },
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/resolve-symlink-and-write": (init) => {
          const body = JSON.parse((init?.body as string) ?? "{}");
          bodies.push(init?.body as string);
          if (body.confirm) {
            return jsonResponse(200, {
              client: "codex-cli",
              original_path: "/home/u/.codex/config.toml",
              written_path: "/e/env/Agents/.codex/config.toml",
              written: true,
            });
          }
          return jsonResponse(200, {
            client: "codex-cli",
            original_path: "/home/u/.codex/config.toml",
            resolved_target: "/e/env/Agents/.codex/config.toml",
            pinned_real_path: "/e/env/Agents/.codex",
            content_hash: "deadbeef",
            is_symlink: true,
          });
        },
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("resolve-symlink-codex-cli")).toBeTruthy();
    });
    fireEvent.click(screen.getByTestId("resolve-symlink-codex-cli"));
    await waitFor(() => {
      expect(screen.queryByTestId("resolve-symlink-confirm-codex-cli")).toBeTruthy();
    });
    fireEvent.click(screen.getByTestId("resolve-symlink-confirm-codex-cli"));

    // The WRITE phase POST must carry confirm:true + the pinned path + hash the
    // RESOLVE phase returned (the operator-confirmed pin).
    await waitFor(() => {
      const writeBody = bodies.map((b) => JSON.parse(b)).find((b) => b.confirm === true);
      expect(writeBody).toEqual({
        client: "codex-cli",
        confirm: true,
        pinned_real_path: "/e/env/Agents/.codex",
        content_hash: "deadbeef",
      });
    });
    // A rescan fired after the successful write.
    await waitFor(() => {
      expect(scanCount).toBeGreaterThanOrEqual(2);
    });
  });

  it("cancel closes the modal without a write POST", async () => {
    const bodies: string[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scanSymlink()),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/resolve-symlink-and-write": (init) => {
          bodies.push(init?.body as string);
          return jsonResponse(200, {
            client: "codex-cli",
            original_path: "/home/u/.codex/config.toml",
            resolved_target: "/e/env/Agents/.codex/config.toml",
            pinned_real_path: "/e/env/Agents/.codex",
            content_hash: "deadbeef",
            is_symlink: true,
          });
        },
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("resolve-symlink-codex-cli")).toBeTruthy();
    });
    fireEvent.click(screen.getByTestId("resolve-symlink-codex-cli"));
    await waitFor(() => {
      expect(screen.queryByTestId("resolve-symlink-cancel-codex-cli")).toBeTruthy();
    });
    fireEvent.click(screen.getByTestId("resolve-symlink-cancel-codex-cli"));
    await waitFor(() => {
      expect(screen.queryByTestId("resolve-symlink-modal-codex-cli")).toBeNull();
    });
    // Only the RESOLVE phase fired; no confirm:true write was sent.
    expect(bodies.map((b) => JSON.parse(b)).some((b) => b.confirm === true)).toBe(false);
  });

  it("surfaces a WRITE_REFUSED error (e.g. strict mode) without rescanning away the cell", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scanSymlink()),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
        "/api/resolve-symlink-and-write": (init) => {
          const body = JSON.parse((init?.body as string) ?? "{}");
          if (body.confirm) {
            return jsonResponse(500, {
              error:
                "write codex-cli via consent: secure write: strict mode is active (via MCPHUB_REQUIRE_SINGLE_USER_HOME=1)",
              code: "WRITE_REFUSED",
            });
          }
          return jsonResponse(200, {
            client: "codex-cli",
            original_path: "/home/u/.codex/config.toml",
            resolved_target: "/e/env/Agents/.codex/config.toml",
            pinned_real_path: "/e/env/Agents/.codex",
            content_hash: "deadbeef",
            is_symlink: true,
          });
        },
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("resolve-symlink-codex-cli")).toBeTruthy();
    });
    fireEvent.click(screen.getByTestId("resolve-symlink-codex-cli"));
    await waitFor(() => {
      expect(screen.queryByTestId("resolve-symlink-confirm-codex-cli")).toBeTruthy();
    });
    fireEvent.click(screen.getByTestId("resolve-symlink-confirm-codex-cli"));
    await waitFor(() => {
      expect(screen.queryByTestId("resolve-symlink-error-codex-cli")).toBeTruthy();
    });
    const errEl = screen.getByTestId("resolve-symlink-error-codex-cli");
    expect(errEl.textContent).toContain("WRITE_REFUSED");
    expect(errEl.textContent).toContain("MCPHUB_REQUIRE_SINGLE_USER_HOME");
  });
});

// P2 scan per-client isolation — a malformed/unreadable client config no
// longer aborts the whole scan. ScanFrom sets that client's
// client_config_presence to "error" AND records the parse/read message in the
// new ScanResult.client_scan_errors map. The Servers matrix must render such a
// cell DISTINCTLY (the actual parser message) versus a bare stat-error cell
// (presence "error" with NO client_scan_errors entry → generic stat-anomaly
// tooltip, no inline note).
describe("ServersScreen — isolated config parse/read failure (P2 scan isolation)", () => {
  beforeEach(() => {
    cleanup();
    vi.restoreAllMocks();
    window.location.hash = "#/servers";
  });
  afterEach(() => {
    cleanup();
  });

  // A scan with the memory server present, codex-cli's config presence "error"
  // AND a client_scan_errors entry (the isolated PARSE failure), plus claude-code
  // presence "error" with NO client_scan_errors entry (the pre-existing bare
  // stat error). Neither client appears in the server's client_presence, so
  // both route to config-error.
  function scanParseError(): ScanResult {
    return {
      at: "2026-06-30T00:00:00Z",
      entries: [
        {
          name: "memory",
          manifest_exists: true,
          can_migrate: true,
          client_presence: {},
        },
      ],
      client_config_presence: {
        "codex-cli": "error",
        "claude-code": "error",
      } as ScanResult["client_config_presence"],
      client_scan_errors: {
        "codex-cli": "codex: invalid TOML at line 3: unexpected '}'",
      },
    };
  }

  it("renders the parse-failure message on a config-error cell that has a client_scan_errors entry", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scanParseError()),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("scan-error-memory-codex-cli")).toBeTruthy();
    });
    const note = screen.getByTestId("scan-error-memory-codex-cli");
    // The actual parser message is surfaced inline (not the generic
    // stat-anomaly hint), prefixed with the "config unreadable" framing.
    expect(note.textContent).toContain("config unreadable");
    expect(note.textContent).toContain("invalid TOML at line 3");
    // Accessible: role="note" + the message echoed in the title.
    expect(note.getAttribute("role")).toBe("note");
    expect(note.getAttribute("title")).toContain("invalid TOML at line 3");
  });

  it("does NOT render the parse-failure note on a bare stat-error cell (presence error, no client_scan_errors entry)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/scan": () => jsonResponse(200, scanParseError()),
        "/api/status": () => jsonResponse(200, [] as DaemonStatus[]),
      }) as unknown as typeof fetch,
    );
    render(<ServersScreen />);
    // Wait for the parse-error cell to confirm the matrix has rendered.
    await waitFor(() => {
      expect(screen.queryByTestId("scan-error-memory-codex-cli")).toBeTruthy();
    });
    // claude-code is presence "error" but has NO client_scan_errors entry, so
    // it keeps the generic stat-error labeling and renders no inline note.
    expect(screen.queryByTestId("scan-error-memory-claude-code")).toBeNull();
  });
});
