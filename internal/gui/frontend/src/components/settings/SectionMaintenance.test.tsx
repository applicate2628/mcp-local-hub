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
    fireEvent.click(card.querySelectorAll("button")[0]);
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
    fireEvent.click(card.querySelectorAll("button")[0]); // Preview
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
    fireEvent.click(card.querySelectorAll("button")[0]);
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
    fireEvent.click(card.querySelectorAll("button")[0]); // Preview
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
    fireEvent.click(card.querySelectorAll("button")[0]); // Preview
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
    fireEvent.click(card.querySelectorAll("button")[0]); // Preview
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
    fireEvent.click(card.querySelectorAll("button")[0]); // Preview
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
    vi.restoreAllMocks();
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
    fireEvent.click(card.querySelectorAll("button")[0]);
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
    fireEvent.click(card.querySelectorAll("button")[0]); // Preview
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
    fireEvent.click(card.querySelectorAll("button")[0]);
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
    fireEvent.click(card.querySelectorAll("button")[0]); // Preview
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
    fireEvent.click(card.querySelectorAll("button")[0]);
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
