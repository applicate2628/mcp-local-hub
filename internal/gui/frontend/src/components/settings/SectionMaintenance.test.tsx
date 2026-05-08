// SectionMaintenance component tests — addresses xhigh+subagents
// QA F2 (no SectionMaintenance.test.tsx) AND verifies the fix for
// Codex Cloud bot P2 on PR #131 commit 72757c6 (kill_err lost on
// apply UI render).

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, fireEvent, waitFor } from "@testing-library/preact";
import { SectionMaintenance } from "./SectionMaintenance";
import * as api from "../../lib/settings-api";

describe("SectionMaintenance — kill_err visibility on apply (Cloud bot P2 on 72757c6)", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    // happy-dom doesn't define window.confirm by default, so vi.spyOn
    // can't wrap it. Assign a stub that auto-confirms; tests use the
    // applied/preview transitions, not the confirm-prompt UX.
    (window as { confirm: (msg?: string) => boolean }).confirm = vi.fn(() => true);
  });

  it("renders per-row kill_err in Result column after orphan-MCP apply when any row has kill_err", async () => {
    let applyCount = 0;
    vi.spyOn(api, "cleanupOrphans").mockImplementation(async (apply: boolean) => {
      if (apply) {
        applyCount++;
        return {
          orphans: [
            { pid: 1234, parent_pid: 1, server: "fs", cmdline: "uvx fs",
              age_sec: 30, ram_bytes: 1024 * 1024,
              kill_err: "skipped: snapshot start-time unknown (cannot revalidate identity)" },
            { pid: 5678, parent_pid: 1, server: "weather", cmdline: "uvx weather",
              age_sec: 60, ram_bytes: 2 * 1024 * 1024 },
          ],
          killed: 1,
          skipped: 1,
        };
      }
      return {
        orphans: [
          { pid: 1234, parent_pid: 1, server: "fs", cmdline: "uvx fs",
            age_sec: 30, ram_bytes: 1024 * 1024 },
          { pid: 5678, parent_pid: 1, server: "weather", cmdline: "uvx weather",
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

    // Click Clean.
    fireEvent.click(card.querySelectorAll("button")[1]);
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
        { pid: 1234, parent_pid: 1, server: "fs", cmdline: "uvx fs",
          age_sec: 30, ram_bytes: 1024 * 1024 },
      ],
      killed: 1,
      skipped: 0,
    }));

    const { container } = render(<SectionMaintenance />);
    const card = container.querySelector('[data-card="orphan-mcp-servers"]')!;
    fireEvent.click(card.querySelectorAll("button")[0]); // Preview
    await waitFor(() => expect(card.querySelector("table")).toBeTruthy());
    fireEvent.click(card.querySelectorAll("button")[1]); // Clean
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

    const cleanBtn = card.querySelectorAll("button")[1] as HTMLButtonElement;
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
    fireEvent.click(card.querySelectorAll("button")[1]); // Clean

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
    fireEvent.click(card.querySelectorAll("button")[1]); // Clean

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
    fireEvent.click(card.querySelectorAll("button")[1]); // Clean
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
    fireEvent.click(card.querySelector("button")!);
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
    fireEvent.click(card.querySelector("button")!);

    await waitFor(() =>
      expect(card.querySelector(".maintenance-status")?.textContent).toMatch(/No daemons were running/),
    );
  });
});
