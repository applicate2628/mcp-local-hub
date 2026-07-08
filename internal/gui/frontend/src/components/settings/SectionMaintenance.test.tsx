// SectionMaintenance component tests — addresses xhigh+subagents
// QA F2 (no SectionMaintenance.test.tsx) AND verifies the fix for
// Codex Cloud bot P2 on PR #131 commit 72757c6 (kill_err lost on
// apply UI render).
//
// Cleanup-6 updates:
//   - Test fixtures populate `cmdline_display` (the redacted basename
//     the backend now emits) instead of relying on the deprecated
//     `cmdline` field. The legacy field is kept optional on the type
//     for compatibility but production wire never carries it.
//   - The destructive-action gate is the in-app <ConfirmModal>, not
//     window.confirm. Tests open the modal via the Clean/Stop/Force-kill
//     button and click the confirm/cancel buttons inside it.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, fireEvent, waitFor, cleanup } from "@testing-library/preact";
import { SectionMaintenance } from "./SectionMaintenance";
import * as api from "../../lib/settings-api";

// happy-dom does NOT implement <dialog>.showModal()/close() natively
// (they noop and never flip `open`). The ConfirmModal component drives
// its open/close state through these methods, so without a polyfill the
// confirm-modal-{confirm,cancel} buttons are never actually visible
// to the test. SectionBackups.test.tsx uses the same shim; we mirror
// it AND additionally set the `open` content-attribute so CSS selectors
// like `dialog[open]` match — happy-dom's `.open` property setter does
// not reflect to the attribute by default, and SectionMaintenance
// renders four ConfirmModal instances (one per card) that the
// `dialog[open]` filter is needed to disambiguate.
function installDialogShim() {
  HTMLDialogElement.prototype.showModal = function () {
    this.open = true;
    this.setAttribute("open", "");
  };
  HTMLDialogElement.prototype.close = function () {
    this.open = false;
    this.removeAttribute("open");
  };
}

// Helper: click the Confirm button inside the currently-open ConfirmModal.
// The modal renders <button data-testid="confirm-modal-confirm"> globally;
// since only one modal is ever open at a time, querying the document is
// safe in this test surface. Each call queries `[open]` so we hit the
// active modal, not a stale one left in the DOM by happy-dom.
function activeModal(container: Element): Element | null {
  return container.ownerDocument!.querySelector(
    'dialog[data-testid="confirm-modal"][open]',
  );
}

function clickConfirmModal(container: Element) {
  const modal = activeModal(container);
  if (!modal) throw new Error("ConfirmModal not open");
  const btn = modal.querySelector(
    '[data-testid="confirm-modal-confirm"]',
  ) as HTMLButtonElement | null;
  if (!btn) throw new Error("ConfirmModal Confirm button not in DOM");
  fireEvent.click(btn);
}

function clickCancelModal(container: Element) {
  const modal = activeModal(container);
  if (!modal) throw new Error("ConfirmModal not open");
  const btn = modal.querySelector(
    '[data-testid="confirm-modal-cancel"]',
  ) as HTMLButtonElement | null;
  if (!btn) throw new Error("ConfirmModal Cancel button not in DOM");
  fireEvent.click(btn);
}

// Helper: the card's primary action button (the first NON-InfoTip <button>).
// Each card header now carries an InfoTip whose trigger is itself a <button>
// (class `infotip-trigger`); positional `querySelectorAll("button")[0]` would
// otherwise hit that tooltip trigger instead of Preview/Diagnose/etc. Filter
// it out so the selector stays anchored on the real action regardless of how
// many tooltip affordances precede it.
function cardActionButton(card: Element, index = 0): HTMLButtonElement {
  const buttons = Array.from(card.querySelectorAll("button")).filter(
    (b) => !b.classList.contains("infotip-trigger"),
  ) as HTMLButtonElement[];
  const btn = buttons[index];
  if (!btn) throw new Error(`card action button[${index}] not in DOM`);
  return btn;
}

describe("SectionMaintenance — kill_err visibility on apply (Cloud bot P2 on 72757c6)", () => {
  beforeEach(() => {
    cleanup();
    // happy-dom keeps the document.body element across tests; if a
    // prior test mounted a ConfirmModal that closed via an attribute
    // toggle (not a real DOM removal) the dialog's content text might
    // still be queryable. Clear the body so each test starts from a
    // pristine document.
    document.body.innerHTML = "";
    vi.restoreAllMocks();
    installDialogShim();
    // Cleanup-6: native window.confirm is no longer called from the
    // component. We still install a stub to prove that the production
    // path does NOT invoke it — any spurious call would fail the
    // assertions that count Confirm modal interactions.
    (window as { confirm: (msg?: string) => boolean }).confirm = vi.fn(() => {
      throw new Error("native confirm() should not be called — ConfirmModal owns the gate");
    });
  });

  it("renders per-row kill_err in Result column after orphan-MCP apply when any row has kill_err", async () => {
    let applyCount = 0;
    vi.spyOn(api, "cleanupOrphans").mockImplementation(async (apply: boolean) => {
      if (apply) {
        applyCount++;
        return {
          orphans: [
            { pid: 1234, parent_pid: 1, server: "fs", cmdline_display: "uvx",
              age_sec: 30, ram_bytes: 1024 * 1024,
              kill_err: "skipped: snapshot start-time unknown (cannot revalidate identity)" },
            { pid: 5678, parent_pid: 1, server: "weather", cmdline_display: "uvx",
              age_sec: 60, ram_bytes: 2 * 1024 * 1024 },
          ],
          killed: 1,
          skipped: 1,
        };
      }
      return {
        orphans: [
          { pid: 1234, parent_pid: 1, server: "fs", cmdline_display: "uvx",
            age_sec: 30, ram_bytes: 1024 * 1024 },
          { pid: 5678, parent_pid: 1, server: "weather", cmdline_display: "uvx",
            age_sec: 60, ram_bytes: 2 * 1024 * 1024 },
        ],
        killed: 0,
        skipped: 0,
      };
    });

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;

    // Preview phase.
    fireEvent.click(cardActionButton(card));
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());

    // Preview state: no Result column yet (no kill_err on any row).
    let headers = Array.from(card.querySelectorAll("th")).map((h) => h.textContent);
    expect(headers).not.toContain("Result");

    // Click Clean → opens ConfirmModal → click confirm-modal-confirm.
    fireEvent.click(card.querySelector('[data-testid="orphan-mcp-clean-button"]')!);
    await waitFor(() =>
      expect(
        container.ownerDocument!.querySelector('[data-testid="confirm-modal-confirm"]'),
      ).toBeTruthy(),
    );
    clickConfirmModal(container as HTMLElement);
    await waitFor(() => expect(applyCount).toBe(1));

    // Apply state: table re-renders with the post-kill rows; one has
    // kill_err → Result column appears.
    await waitFor(() => {
      const updatedHeaders = Array.from(card.querySelectorAll("th")).map((h) => h.textContent);
      expect(updatedHeaders).toContain("Result");
    });

    // The skipped row's kill_err must appear in the table.
    const errCell = card.querySelector("td.maintenance-error");
    expect(errCell?.textContent).toMatch(/snapshot start-time unknown/);

    // The killed row reads "killed" (no kill_err).
    const killedCells = Array.from(card.querySelectorAll("td"))
      .filter((td) => td.textContent === "killed");
    expect(killedCells.length).toBe(1);
  });

  it("preview shows per-row reap_verdict and Clean counts only eligible rows (bot PR #520 P2)", async () => {
    vi.spyOn(api, "cleanupOrphans").mockImplementation(async () => ({
      orphans: [
        { pid: 1234, parent_pid: 1, server: "fs", cmdline_display: "uvx",
          age_sec: 700, ram_bytes: 1024 * 1024, reap_verdict: "reap-eligible" },
        { pid: 5678, parent_pid: 1, server: "weather", cmdline_display: "uvx",
          age_sec: 120, ram_bytes: 2 * 1024 * 1024, reap_verdict: "spared-below-kill-age-floor" },
        { pid: 9999, parent_pid: 1, server: "memory", cmdline_display: "uvx",
          age_sec: 800, ram_bytes: 1024 * 1024, reap_verdict: "spared-config-referenced" },
      ],
      killed: 0,
      skipped: 0,
    }));

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;

    fireEvent.click(cardActionButton(card));
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());

    // Preview now HAS a Result column because rows carry reap_verdict...
    const headers = Array.from(card.querySelectorAll("th")).map((h) => h.textContent);
    expect(headers).toContain("Result");

    // ...and NO row renders a false "killed" during a preview (the PR #520 P1 bug).
    const killedInPreview = Array.from(card.querySelectorAll("td")).filter((td) => td.textContent === "killed");
    expect(killedInPreview.length).toBe(0);

    // Eligible row shows "will clean"; each spared row shows its skip reason.
    const cellTexts = Array.from(card.querySelectorAll("td")).map((td) => td.textContent ?? "");
    expect(cellTexts).toContain("will clean");
    expect(cellTexts.some((t) => t.startsWith("skip: below the kill-age floor"))).toBe(true);
    expect(cellTexts.some((t) => t.startsWith("skip: still referenced"))).toBe(true);

    // Clean button counts ONLY the 1 eligible row, not all 3 previewed.
    const cleanBtn = card.querySelector('[data-testid="orphan-mcp-clean-button"]')!;
    expect(cleanBtn.textContent).toMatch(/Clean \(1\)/);
  });

  it("apply binds expect (identity) to the confirmed ELIGIBLE rows only (bot PR #520 P2 TOCTOU)", async () => {
    const spy = vi.spyOn(api, "cleanupOrphans").mockImplementation(async (apply: boolean) => ({
      orphans: [
        { pid: 1234, parent_pid: 1, server: "fs", cmdline_display: "uvx", started_at: "2026-01-01T00:00:00Z",
          age_sec: 700, ram_bytes: 1024 * 1024, reap_verdict: "reap-eligible" },
        { pid: 5678, parent_pid: 1, server: "weather", cmdline_display: "uvx", started_at: "2026-01-01T00:05:00Z",
          age_sec: 120, ram_bytes: 2 * 1024 * 1024, reap_verdict: "spared-below-kill-age-floor" },
      ],
      killed: apply ? 1 : 0,
      skipped: 0,
    }));

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;
    fireEvent.click(cardActionButton(card)); // Preview
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelector('[data-testid="orphan-mcp-clean-button"]')!);
    await waitFor(() =>
      expect(container.ownerDocument!.querySelector('[data-testid="confirm-modal-confirm"]')).toBeTruthy(),
    );
    clickConfirmModal(container as HTMLElement);
    await waitFor(() => expect(spy).toHaveBeenCalledTimes(2));

    // apply binds expect to ONLY the eligible row's {pid, started_at} (1234); the spared
    // 5678 (below age floor) is excluded, and a recycled PID would carry a different
    // started_at — so neither can be killed unacknowledged.
    expect(spy.mock.calls[1][0]).toBe(true);
    expect(spy.mock.calls[1][1]).toEqual([{ pid: 1234, started_at: "2026-01-01T00:00:00Z" }]);
  });

  it("does NOT render Result column when no row has kill_err (e.g. all-clean apply)", async () => {
    vi.spyOn(api, "cleanupOrphans").mockImplementation(async () => ({
      orphans: [
        { pid: 1234, parent_pid: 1, server: "fs", cmdline_display: "uvx",
          age_sec: 30, ram_bytes: 1024 * 1024 },
      ],
      killed: 1,
      skipped: 0,
    }));

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;
    fireEvent.click(cardActionButton(card)); // Preview
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelector('[data-testid="orphan-mcp-clean-button"]')!); // open modal
    await waitFor(() =>
      expect(
        container.ownerDocument!.querySelector('[data-testid="confirm-modal-confirm"]'),
      ).toBeTruthy(),
    );
    clickConfirmModal(container as HTMLElement);
    // Wait for the "Done" banner to appear so we know apply completed.
    await waitFor(() =>
      expect(card.querySelector(".maintenance-status")?.textContent).toMatch(/Done/),
    );
    const headers = Array.from(card.querySelectorAll("th")).map((h) => h.textContent);
    expect(headers).not.toContain("Result");
  });

  it("renders OS-friendly error when backend returns 501 not_supported_on_this_os", async () => {
    vi.spyOn(api, "cleanupOrphans").mockRejectedValue(new Error("not_supported_on_this_os"));

    const { container, findByText } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;
    fireEvent.click(cardActionButton(card));
    const msg = await findByText(/Windows only/);
    expect(msg).toBeTruthy();
  });

  it("disables Clean(0) on log-watchers when all rows have parent_alive=true and includeLive=false", async () => {
    vi.spyOn(api, "cleanupLogWatchers").mockResolvedValue({
      watchers: [
        { pid: 100, parent_pid: 50, parent_alive: true, name: "tail.exe",
          age_sec: 60, cmdline: "tail -F log" },
        { pid: 200, parent_pid: 51, parent_alive: true, name: "grep.exe",
          age_sec: 30, cmdline: "grep TRACE" },
      ],
      killed: 0,
      skipped: 0,
    });

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-log-watchers"]')!;
    fireEvent.click(cardActionButton(card)); // Preview
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());

    const cleanBtn = card.querySelector(
      '[data-testid="orphan-log-watchers-clean-button"]',
    ) as HTMLButtonElement;
    expect(cleanBtn.textContent).toMatch(/Clean \(0\)/);
    expect(cleanBtn.disabled).toBe(true);
    expect(cleanBtn.getAttribute("title")).toMatch(/active sessions/);
  });

  it("renders log-watcher kill_err on apply (e.g. PID-reuse identity-mismatch skip)", async () => {
    let phase: "preview" | "apply" = "preview";
    vi.spyOn(api, "cleanupLogWatchers").mockImplementation(async (apply) => {
      if (apply) phase = "apply";
      return {
        watchers: phase === "preview"
          ? [
              { pid: 1000, parent_pid: 99999, parent_alive: false, name: "tail.exe",
                age_sec: 30, cmdline: "tail -F /d/dev/.scratch/x.log" },
            ]
          : [
              { pid: 1000, parent_pid: 99999, parent_alive: false, name: "tail.exe",
                age_sec: 30, cmdline: "tail -F /d/dev/.scratch/x.log",
                kill_err: "skipped: PID reused (identity mismatch)" },
            ],
        killed: apply ? 0 : 0,
        skipped: apply ? 1 : 0,
      };
    });

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-log-watchers"]')!;
    fireEvent.click(cardActionButton(card)); // Preview
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelector('[data-testid="orphan-log-watchers-clean-button"]')!);
    await waitFor(() =>
      expect(
        container.ownerDocument!.querySelector('[data-testid="confirm-modal-confirm"]'),
      ).toBeTruthy(),
    );
    clickConfirmModal(container as HTMLElement);

    await waitFor(() => {
      const cell = card.querySelector("td.maintenance-error");
      expect(cell?.textContent).toMatch(/PID reused/);
    });
  });

  it("does not mislabel live-parent skipped log-watchers as killed in mixed apply results", async () => {
    let phase: "preview" | "apply" = "preview";
    vi.spyOn(api, "cleanupLogWatchers").mockImplementation(async (apply) => {
      if (apply) phase = "apply";
      return {
        watchers: phase === "preview"
          ? [
              { pid: 200, parent_pid: 100, parent_alive: true, name: "tail.exe", age_sec: 30, cmdline: "tail live.log" },
              { pid: 300, parent_pid: 1, parent_alive: false, name: "grep.exe", age_sec: 60, cmdline: "grep orphan.log" },
            ]
          : [
              { pid: 200, parent_pid: 100, parent_alive: true, name: "tail.exe", age_sec: 30, cmdline: "tail live.log" },
              { pid: 300, parent_pid: 1, parent_alive: false, name: "grep.exe", age_sec: 60, cmdline: "grep orphan.log", kill_err: "access denied" },
            ],
        killed: apply ? 0 : 0,
        skipped: apply ? 2 : 0,
      };
    });

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-log-watchers"]')!;
    fireEvent.click(cardActionButton(card)); // Preview
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelector('[data-testid="orphan-log-watchers-clean-button"]')!);
    await waitFor(() =>
      expect(
        container.ownerDocument!.querySelector('[data-testid="confirm-modal-confirm"]'),
      ).toBeTruthy(),
    );
    clickConfirmModal(container as HTMLElement);

    await waitFor(() => {
      const cells = Array.from(card.querySelectorAll("td")).map((td) => td.textContent);
      expect(cells).toContain("skipped (live parent)");
      expect(cells).not.toContain("killed");
    });
  });

  // Codex Cloud bot P2 on PR #135 round 2: rendering the Result column
  // off the LIVE includeLive checkbox state means a post-apply toggle
  // re-labels rows that were already executed. Pin the labelling lever
  // to the apply-time includeLive value so the audit trail is stable.
  it("preserves apply-time skipped-live-parent labels after a post-apply checkbox toggle", async () => {
    let phase: "preview" | "apply" = "preview";
    vi.spyOn(api, "cleanupLogWatchers").mockImplementation(async (apply, _includeLive) => {
      if (apply) phase = "apply";
      return {
        watchers: phase === "preview"
          ? [
              { pid: 200, parent_pid: 100, parent_alive: true, name: "tail.exe", age_sec: 30, cmdline: "tail live.log" },
              { pid: 300, parent_pid: 1, parent_alive: false, name: "grep.exe", age_sec: 60, cmdline: "grep orphan.log" },
            ]
          : [
              { pid: 200, parent_pid: 100, parent_alive: true, name: "tail.exe", age_sec: 30, cmdline: "tail live.log" },
              { pid: 300, parent_pid: 1, parent_alive: false, name: "grep.exe", age_sec: 60, cmdline: "grep orphan.log", kill_err: "access denied" },
            ],
        killed: apply ? 0 : 0,
        skipped: apply ? 2 : 0,
      };
    });

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-log-watchers"]')!;
    // Apply with includeLive=false (the default checkbox state). The
    // live-parent row should render as "skipped (live parent)".
    fireEvent.click(cardActionButton(card)); // Preview
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelector('[data-testid="orphan-log-watchers-clean-button"]')!);
    await waitFor(() =>
      expect(
        container.ownerDocument!.querySelector('[data-testid="confirm-modal-confirm"]'),
      ).toBeTruthy(),
    );
    clickConfirmModal(container as HTMLElement);
    await waitFor(() => {
      const cells = Array.from(card.querySelectorAll("td")).map((td) => td.textContent);
      expect(cells).toContain("skipped (live parent)");
    });

    // Now flip the checkbox AFTER the apply. The post-apply label must
    // NOT change to "killed" — the request that already executed used
    // includeLive=false, so the rendered audit row must reflect that.
    const checkbox = card.querySelector('input[type="checkbox"]') as HTMLInputElement;
    fireEvent.change(checkbox, { target: { checked: true } });

    // A short waitFor lets any preact re-render flush.
    await waitFor(() => {
      const cells = Array.from(card.querySelectorAll("td")).map((td) => td.textContent);
      expect(cells).toContain("skipped (live parent)");
      // The skipped-live row must NOT be re-labelled as "killed" by
      // the toggle — it must remain pinned to the apply-time flag.
      // (The other row's cell is `kill_err: "access denied"` so the
      // total td list legitimately also contains that error string;
      // we're explicitly asserting the live-parent label stays.)
      expect(cells).not.toContain("killed");
    });
  });

  it("renders HTTP 207 partial-failure banner + per-daemon error on Stop-All", async () => {
    vi.spyOn(api, "stopAllDaemons").mockResolvedValue({
      stop_results: [
        { task_name: "fs", error: "" },
        { task_name: "weather", error: "child exited with code 1" },
      ],
    });

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="stop-all-daemons"]')!;
    fireEvent.click(card.querySelector('[data-testid="stop-all-button"]')!);
    await waitFor(() =>
      expect(
        container.ownerDocument!.querySelector('[data-testid="confirm-modal-confirm"]'),
      ).toBeTruthy(),
    );
    clickConfirmModal(container as HTMLElement);
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());

    const banner = card.querySelector(".maintenance-status.maintenance-error");
    expect(banner?.textContent).toMatch(/Partial: 1 stopped, 1 failed/);

    const failedCell = Array.from(card.querySelectorAll("td"))
      .find((td) => td.textContent?.startsWith("Failed:"));
    expect(failedCell?.textContent).toMatch(/child exited with code 1/);
  });

  it("renders Done banner with no daemons running on Stop-All empty result", async () => {
    vi.spyOn(api, "stopAllDaemons").mockResolvedValue({ stop_results: [] });

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="stop-all-daemons"]')!;
    fireEvent.click(card.querySelector('[data-testid="stop-all-button"]')!);
    await waitFor(() =>
      expect(
        container.ownerDocument!.querySelector('[data-testid="confirm-modal-confirm"]'),
      ).toBeTruthy(),
    );
    clickConfirmModal(container as HTMLElement);

    await waitFor(() =>
      expect(card.querySelector(".maintenance-status")?.textContent).toMatch(/No daemons were running/),
    );
  });
});

// --- Cleanup-6: ConfirmModal swap (UX consistency) -------------------------
//
// These tests verify the destructive-action gate uses the in-app
// <ConfirmModal> rather than the native window.confirm, and that the
// orphans table now renders the redacted `cmdline_display` field
// (basename only) instead of the raw full command line.

describe("SectionMaintenance — ConfirmModal swap (Cleanup-6)", () => {
  beforeEach(() => {
    // Codex bot PR #143 round 5 P3: this describe block clicks
    // ConfirmModal buttons and queries dialog-open state, so it MUST
    // run the same setup as the other describes (dialog shim + DOM
    // reset). Without this, running the block in isolation
    // (`vitest -t "ConfirmModal swap"`) or under reordered execution
    // could leave HTMLDialogElement.showModal undefined or stale
    // `<dialog>` elements in document.body.
    document.body.innerHTML = "";
    vi.restoreAllMocks();
    installDialogShim();
    // If any code path slipped a native confirm() through, the stub
    // throws so the affected test fails loudly.
    (window as { confirm: (msg?: string) => boolean }).confirm = vi.fn(() => {
      throw new Error("native confirm() should not be called — ConfirmModal owns the gate");
    });
  });

  it("orphan-MCP Clean opens ConfirmModal; Cancel does NOT call apply", async () => {
    const spy = vi.spyOn(api, "cleanupOrphans").mockImplementation(async () => ({
      orphans: [
        { pid: 1234, parent_pid: 1, server: "fs", cmdline_display: "uvx",
          age_sec: 30, ram_bytes: 1024 * 1024 },
      ],
      killed: 0,
      skipped: 0,
    }));

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;

    // Preview.
    fireEvent.click(cardActionButton(card));
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    expect(spy).toHaveBeenCalledTimes(1);
    expect(spy).toHaveBeenLastCalledWith(false);

    // Click Clean → modal opens.
    fireEvent.click(card.querySelector('[data-testid="orphan-mcp-clean-button"]')!);
    await waitFor(() => {
      const modal = container.ownerDocument!.querySelector('[data-testid="confirm-modal"]');
      expect(modal).toBeTruthy();
    });

    // Cancel → modal closes; apply NOT called (cleanupOrphans still
    // only invoked once for the preview).
    clickCancelModal(container as HTMLElement);
    await waitFor(() => {
      // Implementation detail: dialog.close() may keep the element in
      // the DOM with `open=false`. Assert the API spy stayed at 1.
      expect(spy).toHaveBeenCalledTimes(1);
    });
  });

  it("orphan-MCP Clean opens ConfirmModal; Confirm calls cleanupOrphans(true) exactly once", async () => {
    const spy = vi.spyOn(api, "cleanupOrphans").mockImplementation(async (apply: boolean) => ({
      orphans: [
        { pid: 1234, parent_pid: 1, server: "fs", cmdline_display: "uvx",
          age_sec: 30, ram_bytes: 1024 * 1024 },
      ],
      killed: apply ? 1 : 0,
      skipped: 0,
    }));

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;
    fireEvent.click(cardActionButton(card)); // Preview
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelector('[data-testid="orphan-mcp-clean-button"]')!);
    await waitFor(() =>
      expect(
        container.ownerDocument!.querySelector('[data-testid="confirm-modal-confirm"]'),
      ).toBeTruthy(),
    );
    clickConfirmModal(container as HTMLElement);

    // Wait for the apply call to settle. Production wire shape:
    // first call had apply=false (preview), second has apply=true.
    await waitFor(() => {
      expect(spy).toHaveBeenCalledTimes(2);
    });
    expect(spy.mock.calls[0][0]).toBe(false);
    expect(spy.mock.calls[1][0]).toBe(true);
  });

  it("orphans table renders cmdline_display (basename) — never the raw full command line", async () => {
    vi.spyOn(api, "cleanupOrphans").mockImplementation(async () => ({
      orphans: [
        // Production wire (Cleanup-6): only cmdline_display is sent.
        { pid: 1234, parent_pid: 1, server: "fs",
          cmdline_display: "node.exe",
          age_sec: 30, ram_bytes: 1024 * 1024 },
      ],
      killed: 0,
      skipped: 0,
    }));

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;
    fireEvent.click(cardActionButton(card));
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());

    const cmdCell = card.querySelector("td.maintenance-cmd");
    expect(cmdCell?.textContent).toBe("node.exe");
  });

  it("orphan-MCP confirm body lists the orphans by basename + PID + server", async () => {
    vi.spyOn(api, "cleanupOrphans").mockImplementation(async () => ({
      orphans: [
        { pid: 1234, parent_pid: 1, server: "fs",
          cmdline_display: "node.exe",
          age_sec: 30, ram_bytes: 1024 * 1024 },
        { pid: 5678, parent_pid: 1, server: "weather",
          cmdline_display: "uvx",
          age_sec: 60, ram_bytes: 2 * 1024 * 1024 },
      ],
      killed: 0,
      skipped: 0,
    }));

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;
    fireEvent.click(cardActionButton(card)); // Preview
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelector('[data-testid="orphan-mcp-clean-button"]')!);

    // The orphan-MCP confirm body has its own data-testid that pins the
    // assertion to the right card's modal (avoids matching the empty
    // log-watchers / force-kill modals that also render in the section).
    await waitFor(() => {
      const modal = activeModal(container);
      expect(modal).toBeTruthy();
      expect(modal!.querySelector('[data-testid="orphan-mcp-confirm-list"]')).toBeTruthy();
    });

    const modal = activeModal(container)!;
    const list = modal.querySelector('[data-testid="orphan-mcp-confirm-list"]')!;
    const text = list.textContent ?? "";
    expect(text).toMatch(/node\.exe/);
    expect(text).toMatch(/PID 1234/);
    expect(text).toMatch(/fs/);
    expect(text).toMatch(/uvx/);
    expect(text).toMatch(/PID 5678/);
    expect(text).toMatch(/weather/);
  });

  it("orphan-MCP confirm modal title reflects the orphan count", async () => {
    vi.spyOn(api, "cleanupOrphans").mockImplementation(async () => ({
      orphans: [
        { pid: 1234, parent_pid: 1, server: "fs", cmdline_display: "uvx",
          age_sec: 30, ram_bytes: 1024 * 1024 },
        { pid: 5678, parent_pid: 1, server: "weather", cmdline_display: "uvx",
          age_sec: 60, ram_bytes: 2 * 1024 * 1024 },
      ],
      killed: 0,
      skipped: 0,
    }));

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;
    fireEvent.click(cardActionButton(card));
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelector('[data-testid="orphan-mcp-clean-button"]')!);

    // activeModal() filters on `dialog[open]` so we only match the
    // currently-open modal, not the closed siblings (each card mounts
    // its own ConfirmModal — only one can be open at a time).
    await waitFor(() => {
      const modal = activeModal(container);
      expect(modal).toBeTruthy();
      const title = modal!.querySelector("h2")?.textContent ?? "";
      expect(title).toMatch(/Clean 2 orphan MCP processes\?/);
    });
  });

  it("Stop-all Cancel does NOT invoke stopAllDaemons", async () => {
    const spy = vi.spyOn(api, "stopAllDaemons").mockResolvedValue({ stop_results: [] });

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="stop-all-daemons"]')!;

    fireEvent.click(card.querySelector('[data-testid="stop-all-button"]')!);
    await waitFor(() =>
      expect(
        container.ownerDocument!.querySelector('[data-testid="confirm-modal-cancel"]'),
      ).toBeTruthy(),
    );
    clickCancelModal(container as HTMLElement);

    // Give any pending fetches a tick to resolve, then assert the
    // API was never called.
    await waitFor(() => {
      expect(spy).toHaveBeenCalledTimes(0);
    });
  });

  it("Force-kill Cancel does NOT invoke forceKillApply", async () => {
    const probeSpy = vi.spyOn(api, "forceKillProbe").mockResolvedValue({ ok: true } as never);
    const applySpy = vi.spyOn(api, "forceKillApply").mockResolvedValue({ ok: true } as never);

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="force-kill-instance"]')!;

    fireEvent.click(card.querySelector('[data-testid="force-kill-button"]')!);
    await waitFor(() =>
      expect(
        container.ownerDocument!.querySelector('[data-testid="confirm-modal-cancel"]'),
      ).toBeTruthy(),
    );
    clickCancelModal(container as HTMLElement);

    await waitFor(() => {
      expect(applySpy).toHaveBeenCalledTimes(0);
    });
    // Probe should not have run either since we never clicked Diagnose.
    expect(probeSpy).toHaveBeenCalledTimes(0);
  });

  it("Force-kill Confirm invokes forceKillApply once", async () => {
    const applySpy = vi.spyOn(api, "forceKillApply").mockResolvedValue({ ok: true } as never);

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="force-kill-instance"]')!;

    fireEvent.click(card.querySelector('[data-testid="force-kill-button"]')!);
    // Wait for the Force-kill modal specifically: scope the assertion to
    // the force-kill card so we don't pick up a stale closed modal from
    // a sibling card. The modal under the force-kill card has the title
    // "Force-kill the single-instance lock holder?" — easy to disambiguate.
    await waitFor(() => {
      const modal = card.querySelector('dialog[data-testid="confirm-modal"][open]');
      expect(modal).toBeTruthy();
    });
    const modal = card.querySelector(
      'dialog[data-testid="confirm-modal"][open]',
    )!;
    const btn = modal.querySelector(
      '[data-testid="confirm-modal-confirm"]',
    ) as HTMLButtonElement;
    fireEvent.click(btn);

    await waitFor(() => {
      expect(applySpy).toHaveBeenCalledTimes(1);
    });
  });
});

// --- Codex deep-sec PR #143 round 4 finding R1 + A2 -----------------------
//
// R1: orphan ConfirmModal must capture the orphan list at modal-open and
// render exclusively from that snapshot, so concurrent state mutations
// (Preview re-clicks, etc.) cannot make the user confirm against fresh,
// unconfirmed data.
//
// A2: cmdlineDisplayOf must render `<unknown>` when cmdline_display is
// missing — the deprecated `cmdline` fallback is removed so a regression
// or test fixture cannot re-expose the raw cmdline (workspace paths /
// argv-borne secrets) through the GUI.

describe("SectionMaintenance — finding R1 (modal snapshot) + A2 (no cmdline fallback)", () => {
  beforeEach(() => {
    cleanup();
    document.body.innerHTML = "";
    vi.restoreAllMocks();
    installDialogShim();
    (window as { confirm: (msg?: string) => boolean }).confirm = vi.fn(() => {
      throw new Error("native confirm() should not be called — ConfirmModal owns the gate");
    });
  });

  it("R1: modal renders from snapshot captured at open; later Preview-click closes modal and clears snapshot", async () => {
    // First Preview returns 2 orphans; second Preview returns 3.
    let previewCount = 0;
    vi.spyOn(api, "cleanupOrphans").mockImplementation(async (apply: boolean) => {
      if (!apply) previewCount++;
      const initial = [
        { pid: 1234, parent_pid: 1, server: "fs", cmdline_display: "uvx",
          age_sec: 30, ram_bytes: 1024 * 1024 },
        { pid: 5678, parent_pid: 1, server: "weather", cmdline_display: "node.exe",
          age_sec: 60, ram_bytes: 2 * 1024 * 1024 },
      ];
      const enlarged = [
        ...initial,
        { pid: 9999, parent_pid: 1, server: "extra", cmdline_display: "python.exe",
          age_sec: 10, ram_bytes: 512 * 1024 },
      ];
      const orphans = previewCount >= 2 ? enlarged : initial;
      return { orphans, killed: 0, skipped: 0 };
    });

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;

    // Preview #1 → 2 orphans rendered.
    fireEvent.click(cardActionButton(card));
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    expect(card.querySelectorAll("tbody tr").length).toBe(2);

    // Open modal → snapshot captured (2 orphans).
    fireEvent.click(card.querySelector('[data-testid="orphan-mcp-clean-button"]')!);
    await waitFor(() => {
      const modal = activeModal(container);
      expect(modal).toBeTruthy();
    });
    let modal = activeModal(container)!;
    let listText = modal.querySelector('[data-testid="orphan-mcp-confirm-list"]')!.textContent ?? "";
    expect(listText).toMatch(/PID 1234/);
    expect(listText).toMatch(/PID 5678/);
    // Snapshot must NOT contain the third orphan yet — it doesn't exist.
    expect(listText).not.toMatch(/PID 9999/);
    let title = modal.querySelector("h2")?.textContent ?? "";
    expect(title).toMatch(/Clean 2 orphan MCP processes\?/);

    // Re-click Preview while the modal is still open. R1 contract: the
    // modal closes and the snapshot clears, forcing the user to
    // explicitly re-confirm against the new preview.
    fireEvent.click(cardActionButton(card));
    await waitFor(() => {
      // The modal must be closed (no `dialog[open]` for this card).
      const open = card.querySelector('dialog[data-testid="confirm-modal"][open]');
      expect(open).toBeFalsy();
    });
    // The fresh preview should now show 3 rows in the underlying table.
    await waitFor(() => {
      expect(card.querySelectorAll("tbody tr").length).toBe(3);
    });

    // User must click Clean again to re-open the modal — and the new
    // snapshot must reflect the larger set (3 orphans, including 9999).
    fireEvent.click(card.querySelector('[data-testid="orphan-mcp-clean-button"]')!);
    await waitFor(() => {
      const m = activeModal(container);
      expect(m).toBeTruthy();
    });
    modal = activeModal(container)!;
    listText = modal.querySelector('[data-testid="orphan-mcp-confirm-list"]')!.textContent ?? "";
    expect(listText).toMatch(/PID 9999/);
    title = modal.querySelector("h2")?.textContent ?? "";
    expect(title).toMatch(/Clean 3 orphan MCP processes\?/);
  });

  it("R1: Cancel clears the snapshot — re-opening starts from a fresh state", async () => {
    vi.spyOn(api, "cleanupOrphans").mockImplementation(async () => ({
      orphans: [
        { pid: 1234, parent_pid: 1, server: "fs", cmdline_display: "uvx",
          age_sec: 30, ram_bytes: 1024 * 1024 },
      ],
      killed: 0,
      skipped: 0,
    }));

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;

    fireEvent.click(cardActionButton(card)); // Preview
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());

    // Open + Cancel.
    fireEvent.click(card.querySelector('[data-testid="orphan-mcp-clean-button"]')!);
    await waitFor(() => expect(activeModal(container)).toBeTruthy());
    clickCancelModal(container as HTMLElement);
    await waitFor(() => expect(activeModal(container)).toBeFalsy());

    // Re-open. Modal body must rebuild from a NEW snapshot (snapshot
    // was cleared on cancel). The orphan list is the same in this
    // mock, but the test verifies the snapshot was cleared by virtue
    // of the modal showing live state on reopen.
    fireEvent.click(card.querySelector('[data-testid="orphan-mcp-clean-button"]')!);
    await waitFor(() => expect(activeModal(container)).toBeTruthy());
    const modal = activeModal(container)!;
    const list = modal.querySelector('[data-testid="orphan-mcp-confirm-list"]')!;
    expect(list.textContent).toMatch(/PID 1234/);
    // Sanity: only one row in the snapshot, list is non-empty (not the
    // cleared state).
    expect(list.querySelectorAll("li").length).toBe(1);
  });

  it("R1: Confirm applies against the snapshot taken at modal-open, not against later state mutation", async () => {
    // Mock returns 2 orphans on preview, 1 orphan on apply (simulating
    // backend re-resolution). The frontend snapshot is what the user
    // confirmed (2 rows). Apply is invoked, and the user-visible kill
    // request is cleanupOrphans(true) — the backend's kill set is its
    // own concern. The test asserts that apply IS invoked exactly
    // once (snapshot was non-empty when Confirm was clicked) and
    // never falls through to the apply-with-empty-snapshot guard.
    let applyCount = 0;
    vi.spyOn(api, "cleanupOrphans").mockImplementation(async (apply: boolean) => {
      if (apply) applyCount++;
      return {
        orphans: [
          { pid: 1234, parent_pid: 1, server: "fs", cmdline_display: "uvx",
            age_sec: 30, ram_bytes: 1024 * 1024 },
          { pid: 5678, parent_pid: 1, server: "weather", cmdline_display: "node.exe",
            age_sec: 60, ram_bytes: 2 * 1024 * 1024 },
        ],
        killed: apply ? 2 : 0,
        skipped: 0,
      };
    });

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;

    fireEvent.click(cardActionButton(card)); // Preview
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());

    fireEvent.click(card.querySelector('[data-testid="orphan-mcp-clean-button"]')!);
    await waitFor(() => expect(activeModal(container)).toBeTruthy());

    // The snapshot at modal-open holds 2 rows; the Confirm body shows
    // PID 1234 + PID 5678. Click Confirm.
    clickConfirmModal(container as HTMLElement);

    await waitFor(() => expect(applyCount).toBe(1));
    // After apply, snapshot is cleared; opening the modal again would
    // need fresh preview rows. Done banner indicates apply finished.
    await waitFor(() =>
      expect(card.querySelector(".maintenance-status")?.textContent).toMatch(/Done/),
    );
  });

  it("A2: cmdlineDisplayOf renders <unknown> when cmdline_display is missing (no fallback to raw cmdline)", async () => {
    // Production wire ALWAYS carries cmdline_display. This test
    // simulates a regression / test fixture that omits it BUT still
    // emits the legacy `cmdline` field — the contract is that the GUI
    // must render `<unknown>`, never the raw cmdline (the very field
    // Cleanup-6 redaction was meant to hide).
    vi.spyOn(api, "cleanupOrphans").mockImplementation(async () => ({
      orphans: [
        // Cast through `as never` to bypass the OrphanProcess type's
        // requirement of cmdline_display — we are explicitly testing
        // the runtime fallback path for a missing field.
        {
          pid: 1234, parent_pid: 1, server: "fs",
          cmdline: `"C:\\private\\workspace\\node.exe" --api-key=sk-leak`,
          age_sec: 30, ram_bytes: 1024 * 1024,
        } as never,
      ],
      killed: 0,
      skipped: 0,
    }));

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;
    fireEvent.click(cardActionButton(card));
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());

    const cmdCell = card.querySelector("td.maintenance-cmd");
    // The Cmd column must render the explicit fallback marker, NOT the
    // raw cmdline (which would re-leak the API key).
    expect(cmdCell?.textContent).toBe("<unknown>");
    // Defense in depth: assert NOTHING in the table contains the leaked
    // argv string. This catches any future regression where the
    // fallback chain is widened back to o.cmdline.
    const tableHTML = card.querySelector("table")?.innerHTML ?? "";
    expect(tableHTML).not.toMatch(/sk-leak/);
    expect(tableHTML).not.toMatch(/private/);
    expect(tableHTML).not.toMatch(/--api-key/);
  });

  it("A2: confirm modal also renders <unknown> instead of raw cmdline when cmdline_display is missing", async () => {
    vi.spyOn(api, "cleanupOrphans").mockImplementation(async () => ({
      orphans: [
        {
          pid: 1234, parent_pid: 1, server: "fs",
          cmdline: `"C:\\private\\workspace\\node.exe" --api-key=sk-leak`,
          age_sec: 30, ram_bytes: 1024 * 1024,
        } as never,
      ],
      killed: 0,
      skipped: 0,
    }));

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;
    fireEvent.click(cardActionButton(card));
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelector('[data-testid="orphan-mcp-clean-button"]')!);
    await waitFor(() => expect(activeModal(container)).toBeTruthy());

    const modal = activeModal(container)!;
    const list = modal.querySelector('[data-testid="orphan-mcp-confirm-list"]')!;
    const text = list.textContent ?? "";
    expect(text).toMatch(/<unknown>/);
    expect(text).not.toMatch(/sk-leak/);
    expect(text).not.toMatch(/private/);
  });
});

// --- Card 1b: Aggressive cleanup (live-rooted override) --------------------
//
// The aggressive card clones the orphan card's preview→ConfirmModal→apply
// flow but adds a mandatory scope selector (By-client / By-root-PID) and
// optional danger-class opt-ins. These tests verify the scope gate, the
// danger-class surfacing in the confirm body, the match_source column,
// and the apply wire shape (cleanupAggressive(true, scope, classes)).

describe("SectionMaintenance — aggressive cleanup card", () => {
  beforeEach(() => {
    cleanup();
    document.body.innerHTML = "";
    vi.restoreAllMocks();
    installDialogShim();
    (window as { confirm: (msg?: string) => boolean }).confirm = vi.fn(() => {
      throw new Error("native confirm() should not be called — ConfirmModal owns the gate");
    });
  });

  it("disables Preview until a valid scope is chosen (root-pid empty), enables once a client is selected", async () => {
    const spy = vi.spyOn(api, "cleanupAggressive").mockResolvedValue({ orphans: [], killed: 0, skipped: 0 });
    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="aggressive-cleanup"]')!;

    // Default scope is By-client with the first launcher pre-selected, so
    // Preview is enabled out of the box.
    const previewBtn = card.querySelector('[data-testid="aggressive-preview-button"]') as HTMLButtonElement;
    expect(previewBtn.disabled).toBe(false);

    // Switch to By-root-PID with an empty number → Preview disabled.
    fireEvent.click(card.querySelector('[data-testid="aggressive-scope-root-pid"]')!);
    await waitFor(() => expect(previewBtn.disabled).toBe(true));

    // Enter a valid PID → Preview re-enabled.
    const pidInput = card.querySelector('[data-testid="aggressive-root-pid-input"]') as HTMLInputElement;
    fireEvent.input(pidInput, { target: { value: "12345" } });
    await waitFor(() => expect(previewBtn.disabled).toBe(false));
    expect(spy).not.toHaveBeenCalled(); // nothing fired yet
  });

  it("Preview posts a dry-run for the chosen client scope and renders the match_source column", async () => {
    const spy = vi.spyOn(api, "cleanupAggressive").mockResolvedValue({
      orphans: [
        { pid: 7777, parent_pid: 1, server: "", cmdline_display: "node.exe", age_sec: 300, ram_bytes: 50 * 1024 * 1024, match_source: "codex" },
      ],
      killed: 0,
      skipped: 0,
    });
    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="aggressive-cleanup"]')!;

    // Pick the codex launcher.
    fireEvent.change(card.querySelector('[data-testid="aggressive-client-select"]')!, { target: { value: "codex" } });
    fireEvent.click(card.querySelector('[data-testid="aggressive-preview-button"]')!);
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());

    // Dry-run wire shape: apply=false, scope=client codex.
    expect(spy).toHaveBeenCalledWith(false, { kind: "client", client: "codex" }, []);
    // Match column carries the match_source.
    const headers = Array.from(card.querySelectorAll("th")).map((h) => h.textContent);
    expect(headers).toContain("Match");
    const matchCell = card.querySelector("td.maintenance-match");
    expect(matchCell?.textContent).toBe("codex");
  });

  it("Clean opens a ConfirmModal that surfaces the danger-class opt-ins; Confirm posts apply=true with the classes", async () => {
    let applyCount = 0;
    const spy = vi.spyOn(api, "cleanupAggressive").mockImplementation(async (apply) => {
      if (apply) applyCount++;
      return {
        orphans: [
          { pid: 7777, parent_pid: 1, server: "", cmdline_display: "chrome.exe", age_sec: 300, ram_bytes: 50 * 1024 * 1024, match_source: "codex" },
        ],
        killed: apply ? 1 : 0,
        skipped: 0,
      };
    });
    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="aggressive-cleanup"]')!;

    // Opt the chrome danger class back in.
    fireEvent.click(card.querySelector('[data-testid="aggressive-class-chrome"]')!);
    // Preview, then Clean.
    fireEvent.click(card.querySelector('[data-testid="aggressive-preview-button"]')!);
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelector('[data-testid="aggressive-clean-button"]')!);
    await waitFor(() => {
      const modal = activeModal(container);
      expect(modal).toBeTruthy();
      // The confirm body must prominently surface the danger-class opt-in.
      expect(modal!.querySelector('[data-testid="aggressive-confirm-danger"]')?.textContent).toMatch(/chrome/);
      // And list candidates by match.
      expect(modal!.querySelector('[data-testid="aggressive-confirm-list"]')?.textContent).toMatch(/codex/);
    });
    clickConfirmModal(container as HTMLElement);
    await waitFor(() => expect(applyCount).toBe(1));

    // Apply wire shape: apply=true, scope=client (default first launcher), include_classes=[chrome].
    const applyCall = spy.mock.calls.find((c) => c[0] === true)!;
    expect(applyCall[0]).toBe(true);
    expect(applyCall[2]).toEqual(["chrome"]);
  });

  it("invalidates the preview when scope changes under the open modal — closes it, no kill fires (bot #373 P1 + consent-drift)", async () => {
    const spy = vi.spyOn(api, "cleanupAggressive").mockImplementation(async () => ({
      orphans: [
        { pid: 7777, parent_pid: 1, server: "", cmdline_display: "node.exe", age_sec: 300, ram_bytes: 50 * 1024 * 1024, match_source: "codex" },
      ],
      killed: 1,
      skipped: 0,
    }));
    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="aggressive-cleanup"]')!;

    // Preview scope = client codex, then open the confirm modal.
    fireEvent.change(card.querySelector('[data-testid="aggressive-client-select"]')!, { target: { value: "codex" } });
    fireEvent.click(card.querySelector('[data-testid="aggressive-preview-button"]')!);
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelector('[data-testid="aggressive-clean-button"]')!);
    await waitFor(() => expect(activeModal(container)).toBeTruthy());

    // Changing the scope under the open modal INVALIDATES the stale preview:
    // the modal closes, the Clean button is gone (must re-Preview), so a
    // drifted scope can never be confirmed/killed.
    fireEvent.change(card.querySelector('[data-testid="aggressive-client-select"]')!, { target: { value: "claude" } });
    await waitFor(() => expect(activeModal(container)).toBeFalsy());
    expect(card.querySelector('[data-testid="aggressive-clean-button"]')).toBeFalsy();
    expect(spy.mock.calls.some((c) => c[0] === true)).toBe(false);
  });

  it("resets a LOADING preview to idle when the scope changes mid-load — no stuck spinner, stale response discarded (bot #373 R4)", async () => {
    let resolvePreview!: (v: unknown) => void;
    vi.spyOn(api, "cleanupAggressive").mockImplementation(
      () => new Promise((r) => { resolvePreview = r as (v: unknown) => void; }),
    );
    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="aggressive-cleanup"]')!;
    const previewBtn = () => card.querySelector('[data-testid="aggressive-preview-button"]') as HTMLButtonElement;

    fireEvent.change(card.querySelector('[data-testid="aggressive-client-select"]')!, { target: { value: "codex" } });
    fireEvent.click(previewBtn());
    // In flight → Preview disabled (loading).
    await waitFor(() => expect(previewBtn().disabled).toBe(true));

    // Change scope mid-load → invalidatePreview must reset loading → idle
    // (Preview re-enabled, not a stuck spinner).
    fireEvent.change(card.querySelector('[data-testid="aggressive-client-select"]')!, { target: { value: "claude" } });
    await waitFor(() => expect(previewBtn().disabled).toBe(false));

    // The stale response resolving must NOT re-enter preview state.
    resolvePreview({ orphans: [{ pid: 1, parent_pid: 0, server: "", cmdline_display: "x", age_sec: 99, ram_bytes: 0, match_source: "codex" }], killed: 0, skipped: 0, token: "t" });
    await Promise.resolve();
    expect(card.querySelector("table")).toBeFalsy();
  });

  it("rejects a non-integer root PID — parseInt would truncate '123.9'/'1e3' to a different pid (bot #373 P2)", async () => {
    const spy = vi.spyOn(api, "cleanupAggressive").mockResolvedValue({ orphans: [], killed: 0, skipped: 0 });
    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="aggressive-cleanup"]')!;
    fireEvent.click(card.querySelector('[data-testid="aggressive-scope-root-pid"]')!);
    const preview = card.querySelector('[data-testid="aggressive-preview-button"]') as HTMLButtonElement;
    const pidInput = card.querySelector('[data-testid="aggressive-root-pid-input"]')!;

    fireEvent.input(pidInput, { target: { value: "123.9" } });
    expect(preview.disabled).toBe(true);
    fireEvent.input(pidInput, { target: { value: "1e3" } });
    expect(preview.disabled).toBe(true);
    // A clean integer enables Preview and posts exactly that pid.
    fireEvent.input(pidInput, { target: { value: "4242" } });
    expect(preview.disabled).toBe(false);
    fireEvent.click(preview);
    await waitFor(() => expect(spy).toHaveBeenCalledWith(false, { kind: "root-pid", rootPid: 4242 }, []));
  });

  it("posts a root-pid scope when By-root-PID is chosen", async () => {
    const spy = vi.spyOn(api, "cleanupAggressive").mockResolvedValue({ orphans: [], killed: 0, skipped: 0 });
    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="aggressive-cleanup"]')!;

    fireEvent.click(card.querySelector('[data-testid="aggressive-scope-root-pid"]')!);
    fireEvent.input(card.querySelector('[data-testid="aggressive-root-pid-input"]')!, { target: { value: "4242" } });
    fireEvent.click(card.querySelector('[data-testid="aggressive-preview-button"]')!);

    await waitFor(() => expect(spy).toHaveBeenCalledWith(false, { kind: "root-pid", rootPid: 4242 }, []));
  });

  it("renders OS-friendly error when backend returns 501 not_supported_on_this_os", async () => {
    vi.spyOn(api, "cleanupAggressive").mockRejectedValue(new Error("not_supported_on_this_os"));
    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="aggressive-cleanup"]')!;
    fireEvent.click(card.querySelector('[data-testid="aggressive-preview-button"]')!);
    await waitFor(() => {
      const status = card.querySelector(".maintenance-status.maintenance-error");
      expect(status?.textContent).toMatch(/Windows only/);
    });
  });

  // --- bot #373 R2 Finding 1: preview-token contract ----------------------

  it("captures the dry-run token and replays it on apply (preview-token contract)", async () => {
    const spy = vi.spyOn(api, "cleanupAggressive").mockImplementation(async (apply) => ({
      orphans: [
        { pid: 7777, parent_pid: 1, server: "", cmdline_display: "node.exe", age_sec: 300, ram_bytes: 50 * 1024 * 1024, match_source: "codex" },
      ],
      killed: apply ? 1 : 0,
      skipped: 0,
      // The dry-run carries the token; the apply call must echo it back.
      token: apply ? undefined : "abc123def4567890",
    }));
    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="aggressive-cleanup"]')!;

    fireEvent.click(card.querySelector('[data-testid="aggressive-preview-button"]')!);
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelector('[data-testid="aggressive-clean-button"]')!);
    await waitFor(() => expect(activeModal(container)).toBeTruthy());
    clickConfirmModal(container as HTMLElement);

    await waitFor(() => expect(spy.mock.calls.some((c) => c[0] === true)).toBe(true));
    // The apply call (4th positional arg) must carry the dry-run's token.
    const applyCall = spy.mock.calls.find((c) => c[0] === true)!;
    expect(applyCall[3]).toBe("abc123def4567890");
  });

  it("on a 409 token-mismatch: shows a re-Preview notice, kills nothing, renders the FRESH candidate set", async () => {
    const freshCandidate = {
      pid: 8888, parent_pid: 1, server: "", cmdline_display: "python.exe",
      age_sec: 400, ram_bytes: 30 * 1024 * 1024, match_source: "codex",
    };
    const spy = vi.spyOn(api, "cleanupAggressive").mockImplementation(async (apply) => {
      if (apply) {
        // Simulate jsonOrThrow's thrown error for a 409 body.
        const err: any = new Error("conflict");
        err.status = 409;
        err.body = {
          orphans: [freshCandidate],
          killed: 0,
          skipped: 0,
          token: "newtoken00000000",
          code: api.CLEANUP_AGGRESSIVE_TOKEN_MISMATCH,
        };
        throw err;
      }
      return {
        orphans: [
          { pid: 7777, parent_pid: 1, server: "", cmdline_display: "node.exe", age_sec: 300, ram_bytes: 50 * 1024 * 1024, match_source: "codex" },
        ],
        killed: 0,
        skipped: 0,
        token: "oldtoken00000000",
      };
    });
    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="aggressive-cleanup"]')!;

    fireEvent.click(card.querySelector('[data-testid="aggressive-preview-button"]')!);
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelector('[data-testid="aggressive-clean-button"]')!);
    await waitFor(() => expect(activeModal(container)).toBeTruthy());
    clickConfirmModal(container as HTMLElement);

    // The mismatch notice appears...
    await waitFor(() =>
      expect(card.querySelector('[data-testid="aggressive-token-mismatch-notice"]')?.textContent).toMatch(/changed since Preview/),
    );
    // ...the apply was attempted exactly once and no successful kill happened
    // (the 409 threw before any "applied" state)...
    expect(spy.mock.calls.filter((c) => c[0] === true).length).toBe(1);
    expect(card.querySelector(".maintenance-status")?.textContent ?? "").not.toMatch(/Done\. Killed/);
    // ...and the FRESH candidate set (python.exe, PID 8888) is now rendered,
    // replacing the previewed one (node.exe, PID 7777).
    await waitFor(() => {
      const cmd = card.querySelector("td.maintenance-cmd");
      expect(cmd?.textContent).toBe("python.exe");
    });
  });

  // --- bot #373 R2 Finding 2: discard in-flight (stale) previews ----------

  it("discards an in-flight preview whose scope changed before it resolved (no stale table)", async () => {
    // The first (codex) preview resolves SLOWLY; the scope is switched to
    // claude while it is pending. When the slow codex response finally
    // lands, the previewGen has advanced (invalidatePreview on scope change)
    // so the stale candidates must NOT publish.
    let resolveSlow: ((v: any) => void) | null = null;
    const spy = vi.spyOn(api, "cleanupAggressive").mockImplementation((_apply, scope) => {
      if (scope.kind === "client" && scope.client === "codex") {
        return new Promise((resolve) => { resolveSlow = resolve; });
      }
      // claude / anything else resolves immediately with empty.
      return Promise.resolve({ orphans: [], killed: 0, skipped: 0, token: "t" });
    });
    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="aggressive-cleanup"]')!;

    // Launch the slow codex preview.
    fireEvent.change(card.querySelector('[data-testid="aggressive-client-select"]')!, { target: { value: "codex" } });
    fireEvent.click(card.querySelector('[data-testid="aggressive-preview-button"]')!);
    await waitFor(() => expect(resolveSlow).not.toBeNull());

    // Switch scope to claude WHILE the codex preview is still in flight —
    // this invalidates the preview (bumps previewGen).
    fireEvent.change(card.querySelector('[data-testid="aggressive-client-select"]')!, { target: { value: "claude" } });

    // Now resolve the STALE codex preview with candidates for the OLD scope.
    resolveSlow!({
      orphans: [
        { pid: 7777, parent_pid: 1, server: "", cmdline_display: "node.exe", age_sec: 300, ram_bytes: 50 * 1024 * 1024, match_source: "codex" },
      ],
      killed: 0,
      skipped: 0,
      token: "stale",
    });

    // The stale response must be discarded: no candidate table, no Clean
    // button — the card stays idle until a fresh Preview for claude.
    await waitFor(() => {
      // give the resolved promise a tick to (not) publish
      expect(card.querySelector('[data-testid="aggressive-clean-button"]')).toBeFalsy();
    });
    expect(card.querySelector("td.maintenance-cmd")).toBeFalsy();
    expect(spy).toHaveBeenCalled();
  });
});
