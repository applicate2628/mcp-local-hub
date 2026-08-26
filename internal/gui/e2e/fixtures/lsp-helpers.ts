// internal/gui/e2e/fixtures/lsp-helpers.ts
//
// v0.5.x Task 4.4 e2e helpers — synthetic responses + page-route
// wiring that the LSP / env-overlay / coexistence-anomaly specs
// share. Keeping these out of the spec files lets the per-test
// route setup stay declarative.
//
// Two helper families:
//
//   1. `*Response` builders return wire-shape JSON for /api/scan,
//      /api/status, /api/workspaces — consumed by Playwright
//      page.route to fulfill requests with synthetic bodies.
//
//   2. `route*` wrappers attach the routes to a Page instance and
//      record any POST bodies into a shared log so a single test
//      assertion can verify both the request shape AND the response
//      handling.

import type { Page } from "@playwright/test";

export interface WorkspacePair {
  workspace_key: string;
  workspace_path: string;
}

export interface WorkspaceEntryDTO {
  workspace_key: string;
  workspace_path: string;
  language: string;
  backend: string;
  port: number;
  task_name: string;
  client_entries?: Record<string, string>;
  lifecycle?: string;
  last_error?: string;
}

export interface WorkspacesResponse {
  workspaces: WorkspacePair[];
  entries: WorkspaceEntryDTO[];
}

// emptyScanResult is the wire-shape body the GUI receives on a fresh
// install with no client configs — entries=null is the api.ScanResult
// JSON form when the embedded scanner returns no rows.
export const emptyScanResult = {
  at: "2026-05-20T00:00:00Z",
  entries: null,
};

// routeScanFixture is the narrow, opt-in scan-consumer seam for E2E screens
// whose acceptance criteria are rendering or client-side interpretation of a
// documented ScanResult wire shape. It deliberately routes only /api/scan;
// callers that verify the backend scanner itself must use the API's Go tests
// instead of this browser fixture.
export async function routeScanFixture(
  page: Page,
  scan: unknown | (() => unknown) = emptyScanResult,
): Promise<void> {
  await page.route("**/api/scan", async (route) => {
    const body = typeof scan === "function" ? scan() : scan;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(body),
    });
  });
}

export interface PersistentSupervisorDownRoute {
  emitPollerError(): Promise<void>;
}

// routePersistentSupervisorDown provides the Dashboard's observable outage
// sequence without waiting on the real five-second poller cadence. It replaces
// EventSource only in the document under test, so the test can emit the same
// named event only after the initial successful status state has rendered.
// Direct supervisor-down endpoint tests continue to exercise the real backend.
export async function routePersistentSupervisorDown(
  page: Page,
): Promise<PersistentSupervisorDownRoute> {
  let statusCalls = 0;
  let statusShouldFail = false;
  await page.addInitScript(() => {
    type Listener = (event: MessageEvent) => void;
    const listeners = new Map<string, Set<Listener>>();
    class DashboardEventSource {
      onopen: ((event: Event) => void) | null = null;
      onerror: ((event: Event) => void) | null = null;

      constructor(_url: string) {
        setTimeout(() => this.onopen?.(new Event("open")), 0);
      }

      addEventListener(type: string, listener: Listener) {
        const typeListeners = listeners.get(type) ?? new Set<Listener>();
        typeListeners.add(listener);
        listeners.set(type, typeListeners);
      }

      removeEventListener(type: string, listener: Listener) {
        listeners.get(type)?.delete(listener);
      }

      close() {}
    }
    Object.defineProperty(window, "EventSource", { value: DashboardEventSource });
    (window as typeof window & { __e2eEmitPollerError?: () => void }).__e2eEmitPollerError = () => {
      const event = new MessageEvent("poller-error", {
        data: JSON.stringify({ err: "supervisor unreachable — restart the hub" }),
      });
      for (const listener of listeners.get("poller-error") ?? []) listener(event);
    };
  });
  await page.route("**/api/status", async (route) => {
    statusCalls += 1;
    if (!statusShouldFail) {
      await route.fulfill({ status: 200, contentType: "application/json", body: "[]" });
      return;
    }
    await route.fulfill({
      status: 500,
      contentType: "application/json",
      body: JSON.stringify({
        code: "STATUS_FAILED",
        error: "supervisor unreachable — restart the hub",
      }),
    });
  });
  return {
    async emitPollerError() {
      if (statusCalls === 0) {
        throw new Error("dashboard fixture: initial /api/status has not been requested");
      }
      statusShouldFail = true;
      await page.evaluate(() => {
        const emit = (window as typeof window & { __e2eEmitPollerError?: () => void })
          .__e2eEmitPollerError;
        if (!emit) throw new Error("dashboard fixture: EventSource control is unavailable");
        emit();
      });
    },
  };
}

// emptyWorkspaces is the body /api/workspaces returns when the
// registry file is missing or empty. The selector renders its
// placeholder; the LSP matrix renders 9 placeholder rows.
export const emptyWorkspaces: WorkspacesResponse = {
  workspaces: [],
  entries: [],
};

// buildWorkspace produces a single (key, language) registry entry with
// the canonical leading-backslash task_name form. Helpers can spread
// multiple of these into the `entries` array of a WorkspacesResponse.
export function buildWorkspace(opts: {
  key: string;
  path: string;
  language: string;
  backend?: string;
  port?: number;
  clientEntries?: Record<string, string>;
}): WorkspaceEntryDTO {
  return {
    workspace_key: opts.key,
    workspace_path: opts.path,
    language: opts.language,
    backend: opts.backend ?? "mcp-language-server",
    port: opts.port ?? 9200,
    task_name: `\\mcp-local-hub-lsp-${opts.key}-${opts.language}`,
    client_entries: opts.clientEntries ?? {},
  };
}

// seedCoexistence produces the dual-entry ScanResult body that the
// coexistence-anomaly spec asserts against — one hub-routed http
// presence for the named language under (clientHub) AND one legacy
// stdio presence under (clientLegacy). The `legacy_conflict` side-
// channel mirrors what scan.go would emit when its three-rule
// classifier detects both entries for the same (language, client).
//
// Returns a JSON-serializable object directly (not a string) so the
// caller can JSON.stringify it inside page.route.fulfill.
export function seedCoexistence(opts: {
  language: string;
  clientHub: string;
  clientLegacy: string;
  workspaceKey?: string;
  hubPort?: number;
}) {
  const wsKey = opts.workspaceKey ?? "default";
  const port = opts.hubPort ?? 9200;
  return {
    at: "2026-05-20T00:00:00Z",
    entries: [
      {
        name: `mcp-language-server-${opts.language}`,
        manifest_exists: false,
        can_migrate: false,
        status: "via-hub",
        client_presence: {
          [opts.clientHub]: {
            transport: "http",
            endpoint: `http://127.0.0.1:${port}/lsp/${opts.language}`,
          },
        },
        legacy_conflict: {
          [opts.clientLegacy]: {
            transport: "stdio",
            endpoint: "mcp-language-server",
          },
        },
      },
    ],
    workspace_key: wsKey,
  };
}

// routeStandardLspMocks wires the three endpoints the LSP matrix
// reads on mount: /api/scan, /api/status, /api/workspaces. Returns a
// record-of-bodies object so tests can extend it per-spec without
// fighting the route order.
export async function routeStandardLspMocks(
  page: Page,
  opts: {
    scan?: unknown;
    status?: unknown;
    workspaces?: WorkspacesResponse;
  } = {},
): Promise<void> {
  const scan = opts.scan ?? emptyScanResult;
  const status = opts.status ?? [];
  const workspaces = opts.workspaces ?? emptyWorkspaces;
  await routeScanFixture(page, scan);
  await page.route("**/api/status", (r) =>
    r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(status) }),
  );
  await page.route("**/api/workspaces", (r) =>
    r.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(workspaces),
    }),
  );
}
