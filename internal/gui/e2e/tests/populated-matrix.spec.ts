import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { seededHubFor, expect } from "../fixtures/seeded-hub";
import { test as baseTest } from "../fixtures/hub";
import { routeScanFixture } from "../fixtures/lsp-helpers";

// populated-matrix.spec.ts — closes work-items/bugs/2026-05-08-g3-populated-e2e-coverage.md.
//
// The clean-tmpHome e2e suite renders only the EMPTY capability matrix
// (/api/scan finds no client configs) and the empty Capabilities screen
// (/api/status is [] under MCPHUB_E2E_SCHEDULER=none). Two render paths
// therefore had zero e2e exercise:
//
//   1. Populated Servers matrix rows produced by a REAL client config that
//      the live Go /api/scan backend reads off disk — the full embed-bundle
//      + backend roundtrip the bug doc names as the value over the mocked
//      unit tests (wire-shape mismatches, presence-probe + classify wiring).
//   2. The Capabilities partial-failures banner + redacted error text +
//      synthetic-source pill, which fire only when a daemon probe fails.
//
// Path 1 uses a SEED FIXTURE: a real ~/.cursor/mcp.json is written into the
// per-test temp home BEFORE the binary starts, so the live scanner observes
// it exactly as a real installed client config. Cursor is the simplest
// reliable scanner — a standalone mcp.json with the canonical
// `{"mcpServers": {...}}` shape (internal/clients/cursor.go +
// internal/api/scan.go scanCursor).
//
// Path 2 cannot use the seed fixture: capabilities/probes iterate live
// DaemonStatus rows from the scheduler, and the e2e fixture pins
// MCPHUB_E2E_SCHEDULER=none so /api/status is always []. A seeded CLIENT
// config does not start a hub DAEMON, so no probe ever runs and the
// real-backend banner is unreachable in e2e. Path 2 therefore injects a
// populated /api/health response via page.route to exercise the banner +
// pill + card render the real backend cannot reach here. The real-backend
// redaction-banner path is noted as deferred at the bottom of this file.

// --- Path 1: populated Servers matrix consumer wiring ---------------------

// MANIFESTED_SEEDED matches a bundled manifest (servers/memory/manifest.yaml)
// so its row lands in the MAIN matrix (table.servers-matrix). A row only
// joins the main matrix when `manifested` is true; non-manifested rows fall
// to the read-only "Other MCP entries" expander (Servers.tsx splits on
// s.manifested). See servers/ for the bundled manifest set.
const MANIFESTED_SEEDED = "memory";
// THIRD_PARTY_SEEDED has NO bundled manifest, so it must surface in the
// "Other MCP entries" read-only expander rather than vanish.
const THIRD_PARTY_SEEDED = "legacy-thirdparty";

// seedCursorConfig writes ~/.cursor/mcp.json with two stdio servers. The
// fixture wires HOME=home, and internal/clients/clients.go resolves the
// cursor config path to <home>/.cursor/mcp.json, so this is exactly the path
// the live scanner reads.
function seedCursorConfig(home: string): void {
  const cursorDir = join(home, ".cursor");
  mkdirSync(cursorDir, { recursive: true });
  const config = {
    mcpServers: {
      // stdio entry (a `command`, no `url`) → scan.go shapeCursorEntry tags
      // transport:"stdio" → routing.ts perClientRouting → "direct"
      // (interactive cell). With a bundled manifest the row is can-migrate.
      [MANIFESTED_SEEDED]: {
        command: "npx",
        args: ["-y", "@modelcontextprotocol/server-memory"],
      },
      [THIRD_PARTY_SEEDED]: {
        command: "node",
        args: ["/opt/legacy/server.js"],
      },
    },
  };
  writeFileSync(
    join(cursorDir, "mcp.json"),
    JSON.stringify(config, null, 2) + "\n",
    "utf8",
  );
}

const seededTest = seededHubFor(seedCursorConfig);

const populatedScanResult = {
  at: "2026-08-26T00:00:00Z",
  entries: [
    {
      name: MANIFESTED_SEEDED,
      manifest_exists: true,
      can_migrate: true,
      status: "can-migrate",
      client_presence: { cursor: { transport: "stdio", endpoint: "" } },
    },
    {
      name: THIRD_PARTY_SEEDED,
      manifest_exists: false,
      can_migrate: false,
      status: "unknown",
      client_presence: { cursor: { transport: "stdio", endpoint: "" } },
    },
  ],
  client_config_presence: { cursor: "ok" },
};

seededTest.describe("populated matrix — documented /api/scan consumer shape", () => {
  seededTest("manifested seeded server renders a populated main-matrix row with a cursor cell", async ({ page, hub }) => {
    // This test owns the Servers consumer rendering. The Go ScanFrom tests
    // own the real client-config producer contract; route its documented
    // wire shape here so supervisor availability is not an accidental gate.
    await page.route("**/api/status", async (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }),
    );
    await routeScanFixture(page, populatedScanResult);
    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector("table.servers-matrix");

    // The seeded manifested server appears as a real populated row. Anchor
    // on data-testids the row renders (not brittle text): the edit-server
    // link carries the server name, and the Details button is keyed by it.
    const row = page
      .locator("table.servers-matrix tbody tr")
      .filter({ has: page.locator(`[data-testid="server-row-details-${MANIFESTED_SEEDED}"]`) });
    await expect(row).toHaveCount(1);

    // The server-name edit link is present (matrix Server column).
    await expect(
      row.locator('a[data-action="edit-server"]'),
    ).toHaveText(MANIFESTED_SEEDED);

    // The cursor column cell for this row carries an interactive checkbox.
    // Cursor is column index 2 (Server=0, claude-code=1, codex-cli=2? — the
    // header order is Server, claude-code, codex-cli, cursor, ...). Rather
    // than hard-code the index, locate the checkbox whose state reflects
    // the seeded direct (unchecked, enabled) cursor entry: the row has at
    // least one enabled checkbox produced by the seeded stdio entry.
    const enabledCheckbox = row
      .locator('input[type="checkbox"]:not([disabled])')
      .first();
    await expect(enabledCheckbox).toBeVisible();
    // A direct (non-hub) stdio entry renders unchecked — check + Apply would
    // migrate it through the hub.
    await expect(enabledCheckbox).not.toBeChecked();

    // Port/State columns render the no-daemon placeholder ("—") because the
    // noop scheduler reports no running daemon for this server.
    const cells = row.locator("td");
    await expect(cells.last()).toContainText("—");
  });

  seededTest("non-manifested seeded server surfaces in the read-only Other MCP entries expander", async ({ page, hub }) => {
    await page.route("**/api/status", async (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }),
    );
    await routeScanFixture(page, populatedScanResult);
    await page.goto(`${hub.url}/#/servers`);
    await page.waitForSelector("table.servers-matrix");

    // The third-party (manifest-less) server is NOT in the main matrix —
    // it is read-only because migrate/demigrate are no-ops without a
    // manifest. It must instead appear inside the "Other MCP entries"
    // <details> expander (Servers.tsx OtherMCPEntriesSection).
    const other = page.locator("details.other-mcp-entries");
    await expect(other).toBeVisible();
    await expect(other).toContainText(THIRD_PARTY_SEEDED);

    // Negative: the manifest-less server has NO main-matrix Details button.
    await expect(
      page.locator(`[data-testid="server-row-details-${THIRD_PARTY_SEEDED}"]`),
    ).toHaveCount(0);
  });
});

// --- Path 2: Capabilities populated cards + redacted partial-failures -----

// The Capabilities partial-failures banner, the per-daemon section-error
// list (which carries the BACKEND-REDACTED error text), and the
// synthetic-source pill all render only when /api/health?include=capabilities
// returns probe/capability failures alongside successful rows. The live
// backend cannot produce this under MCPHUB_E2E_SCHEDULER=none (no daemons →
// no probes), so inject a populated fixture via page.route. This is the e2e
// exercise of the render wiring the existing capabilities.spec.ts (empty +
// nav + refresh only) and the mocked unit tests do not cover end-to-end
// against the real embed bundle.
const capTest = baseTest;

// A health snapshot with three daemon outcomes: one OK card, one
// synthetic-source card, and one probe FAILURE whose error text is the
// already-redacted message the backend emits (scrubbedURLError strips the
// raw URL). The banner + error list reflect the redacted text verbatim — the
// frontend never re-redacts, so asserting the redacted form is on the wire
// is the meaningful contract.
const REDACTED_PROBE_ERR =
  "probe failed: dial tcp [redacted]: connect: connection refused";

function populatedHealth(): unknown {
  const okSub = { state: "ok", items: [{ name: "search", id: "ok-srv/ok-srv/tool/search", namespace: "ok-srv", kind: "tool" }] };
  const emptySub = { state: "empty", items: [] };
  return {
    schema_version: "1",
    hub: {
      version: "test",
      commit: "deadbee",
      build_date: "2026-05-08",
      started_at: "2026-05-08T00:00:00Z",
      lock: { pid: 1, port: 9125 },
      generated_at: 1715000000,
      ttl_ms: null,
    },
    daemons: { items: [], generated_at: 1715000000, ttl_ms: 1000, errors: [] },
    probes: {
      items: [
        { server: "ok-srv", daemon: "ok-srv", ok: true, tool_count: 1, err: "", source: "" },
        { server: "synth-srv", daemon: "synth-srv", ok: true, tool_count: 0, err: "", source: "proxy-synthetic" },
        { server: "bad-srv", daemon: "bad-srv", ok: false, tool_count: 0, err: REDACTED_PROBE_ERR, source: "" },
      ],
      generated_at: 1715000000,
      ttl_ms: 1000,
      errors: [],
    },
    capabilities: {
      items: [
        { server: "ok-srv", daemon: "ok-srv", tools: okSub, prompts: emptySub, resources: emptySub },
        { server: "synth-srv", daemon: "synth-srv", tools: emptySub, prompts: emptySub, resources: emptySub },
      ],
      generated_at: 1715000000,
      ttl_ms: 1000,
      errors: [],
    },
  };
}

capTest.describe("populated capabilities — cards, synthetic pill, redacted banner", () => {
  capTest("populated health renders cards, the synthetic pill, and the redacted partial-failures banner", async ({ page, hub }) => {
    await page.route("**/api/health*", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(populatedHealth()) }),
    );
    await page.goto(`${hub.url}/#/capabilities`);
    await page.waitForSelector('[data-testid="capabilities-screen"]');

    // Per-server cards render with real probe + capability data.
    await expect(page.getByTestId("capability-card-ok-srv-ok-srv")).toBeVisible();
    await expect(page.getByTestId("capability-card-synth-srv-synth-srv")).toBeVisible();

    // Synthetic-source pill renders for the proxy-synthetic daemon (and ONLY
    // that one).
    const pill = page.getByTestId("synthetic-source-pill");
    await expect(pill).toHaveCount(1);
    await expect(pill).toHaveText("synthetic");

    // Partial-failures banner renders because one probe failed while cards
    // rendered for the others (hasFailures && rows.length > 0).
    const banner = page.getByTestId("capabilities-partial-failures");
    await expect(banner).toBeVisible();
    await expect(banner).toContainText("Some daemons reported probe or capability failures");

    // The per-daemon section-error list carries the backend-REDACTED error
    // text verbatim (the [redacted] token survives to the UI; the frontend
    // never reconstructs the raw URL). This is the g3 error-text redaction
    // path's UI surface.
    const errors = banner.getByTestId("capabilities-section-errors");
    await expect(errors).toContainText("bad-srv");
    await expect(errors).toContainText("[redacted]");
    // Negative: no raw URL/host leaks past the redaction token.
    await expect(errors).not.toContainText("127.0.0.1");
  });

  capTest("all-fail health renders the failure-empty banner with redacted text and no cards", async ({ page, hub }) => {
    // Every probe fails and no capability rows render → the failure-empty
    // branch (rows.length === 0 && hasFailures) fires instead of the partial
    // banner. Still carries the redacted section-error list.
    const allFail = populatedHealth() as any;
    allFail.capabilities.items = [];
    allFail.probes.items = [
      { server: "bad-srv", daemon: "bad-srv", ok: false, tool_count: 0, err: REDACTED_PROBE_ERR, source: "" },
    ];
    await page.route("**/api/health*", (r) =>
      r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(allFail) }),
    );
    await page.goto(`${hub.url}/#/capabilities`);
    await page.waitForSelector('[data-testid="capabilities-screen"]');

    const failEmpty = page.getByTestId("capabilities-empty-failures");
    await expect(failEmpty).toBeVisible();
    await expect(failEmpty).toContainText("Capabilities not yet available");
    await expect(failEmpty).toContainText("[redacted]");
    // No success cards rendered.
    await expect(page.locator(".capability-card")).toHaveCount(0);
  });
});

// DEFERRED: a real-backend (un-mocked /api/health) redaction-banner e2e is
// not feasible under the current fixture. Capabilities/probes iterate live
// DaemonStatus rows, and MCPHUB_E2E_SCHEDULER=none pins /api/status to [], so
// no daemon probe ever runs and the banner's data source is always empty.
// Seeding a client config (Path 1 above) populates /api/scan but does NOT
// start a hub daemon, so it cannot drive a live probe failure. Exercising the
// real redaction path end-to-end would need a scheduler/supervisor test seam
// that injects a failing daemon — tracked as future work alongside the
// CLAUDE.md "Real migrate/restart flows (needs populated client configs)"
// and scheduler-test-seam backlog items. Path 2 above covers the banner +
// pill + redacted-text RENDER wiring via the injected response, which is the
// portion reachable in the headless e2e environment.
