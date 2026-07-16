// screens/Groups.test.tsx — component tests for the Groups authoring screen
// (groups Phase 5b-2). Mirrors Catalog.test.tsx's fetch-router idiom: a
// declarative URL→response map so each test describes the wire surface
// without brittle mockResolvedValueOnce ordering. The pure draft/dirty/
// validation logic is unit-tested in lib/groups-draft.test.ts; these tests
// cover the screen's network glue + the operator-visible behaviors:
//   - list render + empty-state;
//   - create-via-form → POST body shape (servers + tools_hidden);
//   - validation code → inline field error (unknown server, invalid name);
//   - the restart_required banner;
//   - delete via the ConfirmModal.
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor, cleanup, fireEvent, screen, act } from "@testing-library/preact";
import { GroupsScreen } from "./Groups";

type StubListener = (ev: MessageEvent) => void;
const stubInstances = new Set<StubEventSource>();
class StubEventSource {
  url: string;
  listeners = new Map<string, Set<StubListener>>();
  onopen: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  constructor(url: string) {
    this.url = url;
    stubInstances.add(this);
  }
  addEventListener(name: string, handler: StubListener): void {
    let bucket = this.listeners.get(name);
    if (!bucket) {
      bucket = new Set();
      this.listeners.set(name, bucket);
    }
    bucket.add(handler);
  }
  removeEventListener(name: string, handler: StubListener): void {
    this.listeners.get(name)?.delete(handler);
  }
  close(): void {
    stubInstances.delete(this);
  }
  triggerOpen(): void {
    this.onopen?.(new Event("open"));
  }
}
(globalThis as unknown as { EventSource: typeof StubEventSource }).EventSource = StubEventSource;

function dispatchSse(eventName: string, data: unknown): void {
  const ev = new MessageEvent(eventName, { data: JSON.stringify(data) });
  for (const inst of stubInstances) {
    inst.listeners.get(eventName)?.forEach((handler) => handler(ev));
  }
}

function activeStubEventSource(): StubEventSource {
  const [source] = stubInstances;
  if (!source) throw new Error("no active StubEventSource");
  return source;
}

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

// fetchRouter dispatches each fetch call to the matching response based on
// the request URL prefix (same helper shape as Catalog.test.tsx). The
// per-route fn receives the RequestInit so a test can capture POST bodies.
function fetchRouter(
  routes: Record<string, (init?: RequestInit, url?: string) => Response | Promise<Response>>,
) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    // Longest-prefix match so "/api/groups?name=x" can route distinctly if
    // a test ever needs it; default falls back to the plain "/api/groups".
    const keys = Object.keys(routes).sort((a, b) => b.length - a.length);
    for (const prefix of keys) {
      if (url.startsWith(prefix)) {
        return routes[prefix](init, url);
      }
    }
    throw new Error(`unexpected fetch: ${url}`);
  });
}

const AVAILABLE = ["memory", "serena", "time"];

// listBody builds the GET /api/groups response.
function listBody(groups: unknown[], available: string[] = AVAILABLE) {
  return { groups, available_servers: available };
}

describe("GroupsScreen", () => {
  beforeEach(() => {
    cleanup();
    stubInstances.clear();
    vi.restoreAllMocks();
    window.location.hash = "#/groups";
  });
  afterEach(() => {
    cleanup();
  });

  it("renders the empty-state on a clean install (no groups)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": () => jsonResponse(200, listBody([])),
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("groups-empty")).toBeTruthy();
    });
    expect(screen.getByTestId("groups-empty").textContent).toContain("No groups yet");
    // h1 present for nav/shell smoke parity.
    expect(screen.getByText("Groups")).toBeTruthy();
  });

  it("lists existing groups with their servers", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": () =>
          jsonResponse(
            200,
            listBody([
              { name: "frontend", description: "JS tools", servers: ["serena", "time"] },
              { name: "infra", servers: ["memory"] },
            ]),
          ),
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("groups-row-frontend")).toBeTruthy();
    });
    expect(screen.getByTestId("groups-row-servers-frontend").textContent).toContain("serena, time");
    expect(screen.getByTestId("groups-row-infra").textContent).toContain("infra");
  });

  it("refetches on hub-health SSE and hides a now-unavailable group URL", async () => {
    let getCount = 0;
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": () => {
          getCount += 1;
          const connection = getCount === 1
            ? {
                available: true,
                url: "http://127.0.0.1:9201/g/frontend/mcp",
                token: "group-token",
                instance_id: "instance-1",
              }
            : { available: false, hint: "The aggregated hub is down." };
          return jsonResponse(
            200,
            listBody([{ name: "frontend", servers: ["serena"], connection }]),
          );
        },
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    expect((await screen.findByTestId("groups-conn-url-frontend")).textContent).toContain(
      "/g/frontend/mcp",
    );
    expect(getCount).toBe(1);

    act(() => {
      dispatchSse("hub-health", { state: "down", degraded: true });
    });

    await waitFor(() => expect(getCount).toBe(2));
    expect(await screen.findByTestId("groups-connection-hint-frontend")).toBeTruthy();
    expect(screen.queryByTestId("groups-conn-url-frontend")).toBeNull();
    expect(fetchSpy).toHaveBeenCalledTimes(2);
  });

  it("keeps rendered groups during a failed hub-health background refresh", async () => {
    let rejectRefresh!: (reason?: unknown) => void;
    const refreshResponse = new Promise<Response>((_resolve, reject) => {
      rejectRefresh = reject;
    });
    let getCount = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": () => {
          getCount += 1;
          return getCount === 1
            ? jsonResponse(200, listBody([{ name: "frontend", servers: ["serena"] }]))
            : refreshResponse;
        },
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    expect(await screen.findByTestId("groups-row-frontend")).toBeTruthy();

    act(() => {
      dispatchSse("hub-health", { state: "recovering", degraded: true });
    });
    await waitFor(() => expect(getCount).toBe(2));
    const stayedVisibleWhileRefreshing = screen.queryByTestId("groups-row-frontend");
    const showedLoadingWhileRefreshing = screen.queryByTestId("groups-loading");

    await act(async () => {
      rejectRefresh(new Error("transient refresh failure"));
    });

    expect(stayedVisibleWhileRefreshing).toBeTruthy();
    expect(showedLoadingWhileRefreshing).toBeNull();
    expect(screen.queryByTestId("groups-row-frontend")).toBeTruthy();
    expect(screen.queryByTestId("groups-load-error")).toBeNull();
  });

  it("applies an earlier foreground success after a newer background load fails", async () => {
    let resolveForeground!: (response: Response) => void;
    const foregroundResponse = new Promise<Response>((resolve) => {
      resolveForeground = resolve;
    });
    let rejectBackground!: (reason?: unknown) => void;
    const backgroundResponse = new Promise<Response>((_resolve, reject) => {
      rejectBackground = reject;
    });
    let getCount = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": () => {
          getCount += 1;
          return getCount === 1 ? foregroundResponse : backgroundResponse;
        },
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    await waitFor(() => {
      expect(getCount).toBe(1);
      expect(stubInstances.size).toBe(1);
    });

    act(() => {
      dispatchSse("hub-health", { state: "recovering", degraded: true });
    });
    await waitFor(() => expect(getCount).toBe(2));

    await act(async () => {
      rejectBackground(new Error("transient refresh failure"));
    });
    await act(async () => {
      resolveForeground(jsonResponse(200, listBody([{ name: "foreground", servers: ["serena"] }])));
    });

    expect(await screen.findByTestId("groups-row-foreground")).toBeTruthy();
    expect(screen.queryByTestId("groups-loading")).toBeNull();
    expect(screen.queryByTestId("groups-load-error")).toBeNull();
  });

  it("refetches groups when EventSource opens after the initial load", async () => {
    let getCount = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": () => {
          getCount += 1;
          return jsonResponse(200, listBody([]));
        },
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    await waitFor(() => {
      expect(getCount).toBe(1);
      expect(stubInstances.size).toBe(1);
    });

    act(() => {
      activeStubEventSource().triggerOpen();
    });

    await waitFor(() => expect(getCount).toBe(2));
  });

  it("keeps the newer hub-health load when the initial load resolves last", async () => {
    let resolveInitial!: (response: Response) => void;
    const initialResponse = new Promise<Response>((resolve) => {
      resolveInitial = resolve;
    });
    let getCount = 0;
    const fetchSpy = vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": () => {
          getCount += 1;
          return getCount === 1
            ? initialResponse
            : jsonResponse(200, listBody([{ name: "fresh", servers: ["serena"] }]));
        },
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    await waitFor(() => {
      expect(fetchSpy).toHaveBeenCalledTimes(1);
      expect(stubInstances.size).toBe(1);
    });

    act(() => {
      dispatchSse("hub-health", { state: "up", degraded: false });
    });

    await waitFor(() => expect(fetchSpy).toHaveBeenCalledTimes(2));
    expect(await screen.findByTestId("groups-row-fresh")).toBeTruthy();

    await act(async () => {
      resolveInitial(jsonResponse(200, listBody([{ name: "stale", servers: ["memory"] }])));
    });

    expect(screen.queryByTestId("groups-row-fresh")).toBeTruthy();
    expect(screen.queryByTestId("groups-row-stale")).toBeNull();
  });

  it("renders an error banner with Retry when GET /api/groups fails", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": () =>
          jsonResponse(500, { error: "groups list failed", code: "GROUPS_LIST_FAILED" }),
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    await waitFor(() => {
      expect(screen.queryByTestId("groups-load-error")).toBeTruthy();
    });
    expect(screen.getByTestId("groups-load-error").textContent).toContain("groups list failed");
  });

  it("creates a group via the form → POSTs {name, servers, tools_hidden} and shows it", async () => {
    const bodies: unknown[] = [];
    let getCount = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": (init) => {
          if (init?.method === "POST") {
            bodies.push(JSON.parse(String(init.body)));
            return jsonResponse(200, {
              group: {
                name: "frontend",
                servers: ["serena"],
                tools_hidden: { serena: ["find_symbol"] },
              },
              restart_required: false,
              hub_live: true,
            });
          }
          // GET: first load empty, second load (after save) shows the row.
          getCount += 1;
          return getCount === 1
            ? jsonResponse(200, listBody([]))
            : jsonResponse(
                200,
                listBody([
                  {
                    name: "frontend",
                    servers: ["serena"],
                    tools_hidden: { serena: ["find_symbol"] },
                  },
                ]),
              );
        },
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    // Open the editor.
    fireEvent.click(await screen.findByTestId("groups-new"));
    // Fill the name.
    fireEvent.input(screen.getByTestId("groups-name-input"), {
      target: { value: "frontend" },
    });
    // Select a server.
    fireEvent.click(screen.getByTestId("groups-server-checkbox-serena"));
    // Hide a tool on that server (the fine-grained filter).
    const hiddenInput = await screen.findByTestId("groups-hidden-input-serena");
    fireEvent.input(hiddenInput, { target: { value: "find_symbol" } });
    // Save.
    fireEvent.click(screen.getByTestId("groups-save"));

    await waitFor(() => {
      expect(screen.queryByTestId("groups-row-frontend")).toBeTruthy();
    });
    // The POST carried the projected body shape.
    expect(bodies[0]).toMatchObject({
      name: "frontend",
      servers: ["serena"],
      tools_hidden: { serena: ["find_symbol"] },
    });
  });

  it("shows the restart banner when the save returns restart_required (gate-OFF)", async () => {
    let getCount = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": (init) => {
          if (init?.method === "POST") {
            return jsonResponse(200, {
              group: { name: "infra", servers: ["memory"] },
              restart_required: true,
              hub_live: false,
            });
          }
          getCount += 1;
          return getCount === 1
            ? jsonResponse(200, listBody([]))
            : jsonResponse(200, listBody([{ name: "infra", servers: ["memory"] }]));
        },
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    fireEvent.click(await screen.findByTestId("groups-new"));
    fireEvent.input(screen.getByTestId("groups-name-input"), { target: { value: "infra" } });
    fireEvent.click(screen.getByTestId("groups-server-checkbox-memory"));
    fireEvent.click(screen.getByTestId("groups-save"));

    await waitFor(() => {
      expect(screen.queryByTestId("groups-restart-notice")).toBeTruthy();
    });
    expect(screen.getByTestId("groups-restart-notice").textContent).toContain("restart the hub");
  });

  it("maps GROUPS_UNKNOWN_SERVER to an inline servers error and keeps the editor open", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": (init) => {
          if (init?.method === "POST") {
            return jsonResponse(400, {
              error: 'unknown server "ghost"',
              code: "GROUPS_UNKNOWN_SERVER",
            });
          }
          return jsonResponse(200, listBody([]));
        },
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    fireEvent.click(await screen.findByTestId("groups-new"));
    fireEvent.input(screen.getByTestId("groups-name-input"), { target: { value: "g" } });
    fireEvent.click(screen.getByTestId("groups-server-checkbox-memory"));
    fireEvent.click(screen.getByTestId("groups-save"));

    await waitFor(() => {
      expect(screen.queryByTestId("groups-servers-error")).toBeTruthy();
    });
    expect(screen.getByTestId("groups-servers-error").textContent).toContain("unknown server");
    // The editor stays open so the operator can correct + retry.
    expect(screen.queryByTestId("groups-editor")).toBeTruthy();
  });

  it("blocks Save on a client-invalid name (':' separator) without a POST", async () => {
    const posts: string[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": (init) => {
          if (init?.method === "POST") posts.push("POST");
          return jsonResponse(200, listBody([]));
        },
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    fireEvent.click(await screen.findByTestId("groups-new"));
    fireEvent.input(screen.getByTestId("groups-name-input"), { target: { value: "g:bad" } });
    fireEvent.click(screen.getByTestId("groups-server-checkbox-memory"));

    // The inline name error renders and Save is disabled.
    await waitFor(() => {
      expect(screen.queryByTestId("groups-name-error")).toBeTruthy();
    });
    const saveBtn = screen.getByTestId("groups-save") as HTMLButtonElement;
    expect(saveBtn.disabled).toBe(true);
    // No POST ever fired (guarded client-side).
    expect(posts).toEqual([]);
  });

  it("deletes a group via the ConfirmModal and removes it from the list", async () => {
    const deletes: string[] = [];
    let getCount = 0;
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": (init, url) => {
          if (init?.method === "DELETE") {
            deletes.push(url ?? "");
            return jsonResponse(200, { restart_required: false, hub_live: true });
          }
          getCount += 1;
          // First load: one group. After delete: empty.
          return getCount === 1
            ? jsonResponse(200, listBody([{ name: "frontend", servers: ["serena"] }]))
            : jsonResponse(200, listBody([]));
        },
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    fireEvent.click(await screen.findByTestId("groups-delete-frontend"));
    // ConfirmModal appears; confirm.
    fireEvent.click(await screen.findByTestId("groups-confirm-delete-confirm"));

    await waitFor(() => {
      expect(screen.queryByTestId("groups-empty")).toBeTruthy();
    });
    // The DELETE carried the group name on the query string.
    expect(deletes.some((u) => u.includes("name=frontend"))).toBe(true);
  });

  it("cancelling the delete ConfirmModal fires no DELETE and keeps the group", async () => {
    const deletes: string[] = [];
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": (init) => {
          if (init?.method === "DELETE") {
            deletes.push("DELETE");
            return jsonResponse(200, { restart_required: false, hub_live: true });
          }
          return jsonResponse(200, listBody([{ name: "frontend", servers: ["serena"] }]));
        },
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    fireEvent.click(await screen.findByTestId("groups-delete-frontend"));
    fireEvent.click(await screen.findByTestId("groups-confirm-delete-cancel"));

    // Still present; no DELETE issued.
    await waitFor(() => {
      expect(screen.queryByTestId("groups-row-frontend")).toBeTruthy();
    });
    expect(deletes).toEqual([]);
  });

  it("editing an existing group hydrates the form with its members + locks the name", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": () =>
          jsonResponse(
            200,
            listBody([
              {
                name: "frontend",
                description: "JS tools",
                servers: ["serena", "time"],
                tools_hidden: { serena: ["find_symbol"] },
              },
            ]),
          ),
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    fireEvent.click(await screen.findByTestId("groups-edit-frontend"));

    // Name is hydrated + locked (rename = delete + create).
    const nameInput = (await screen.findByTestId("groups-name-input")) as HTMLInputElement;
    expect(nameInput.value).toBe("frontend");
    expect(nameInput.disabled).toBe(true);
    // Members are pre-checked.
    expect((screen.getByTestId("groups-server-checkbox-serena") as HTMLInputElement).checked).toBe(
      true,
    );
    expect((screen.getByTestId("groups-server-checkbox-time") as HTMLInputElement).checked).toBe(
      true,
    );
    expect((screen.getByTestId("groups-server-checkbox-memory") as HTMLInputElement).checked).toBe(
      false,
    );
    // Hidden tools round-trip back into the tag input.
    expect((screen.getByTestId("groups-hidden-input-serena") as HTMLInputElement).value).toBe(
      "find_symbol",
    );
    // No change yet → Save is disabled (not dirty).
    expect((screen.getByTestId("groups-save") as HTMLButtonElement).disabled).toBe(true);
  });

  it("guards a dirty editor when switching to a different group's Edit (confirm-to-discard)", async () => {
    vi.spyOn(globalThis, "fetch").mockImplementation(
      fetchRouter({
        "/api/groups": () =>
          jsonResponse(
            200,
            listBody([
              { name: "frontend", description: "", servers: ["serena"] },
              { name: "backend", description: "", servers: ["time"] },
            ]),
          ),
      }) as unknown as typeof fetch,
    );

    render(<GroupsScreen />);
    // Open the frontend editor and make it dirty (toggle a member off).
    fireEvent.click(await screen.findByTestId("groups-edit-frontend"));
    fireEvent.click(await screen.findByTestId("groups-server-checkbox-serena"));
    expect((screen.getByTestId("groups-name-input") as HTMLInputElement).value).toBe("frontend");

    // jsdom does not implement window.confirm, so install a mock directly
    // (vi.spyOn requires an existing function). CANCEL the discard prompt →
    // the dirty frontend draft is kept (the editor stays on frontend; the
    // App-level route guard never fired because the route stayed #/groups).
    const confirmMock = vi.fn().mockReturnValue(false);
    const origConfirm = window.confirm;
    window.confirm = confirmMock as unknown as typeof window.confirm;
    try {
      fireEvent.click(screen.getByTestId("groups-edit-backend"));
      expect(confirmMock).toHaveBeenCalledTimes(1);
      expect((screen.getByTestId("groups-name-input") as HTMLInputElement).value).toBe("frontend");

      // CONFIRM the discard prompt → the editor switches to backend.
      confirmMock.mockReturnValue(true);
      fireEvent.click(screen.getByTestId("groups-edit-backend"));
      expect(confirmMock).toHaveBeenCalledTimes(2);
      await waitFor(() =>
        expect((screen.getByTestId("groups-name-input") as HTMLInputElement).value).toBe("backend"),
      );
    } finally {
      window.confirm = origConfirm;
    }
  });
});
