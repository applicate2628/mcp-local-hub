import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { fireEvent, render, waitFor, cleanup } from "@testing-library/preact";
import { BackupsList } from "./BackupsList";
import * as api from "../../lib/settings-api";
import { BACKUPS_COPY } from "./backups-copy";

const fixture = [
  { client: "claude-code", path: "/cc/orig.bak", kind: "original" as const,
    mod_time: "2025-12-01T00:00:00Z", size_byte: 1000 },
  { client: "claude-code", path: "/cc/2026-04-25.bak", kind: "timestamped" as const,
    mod_time: "2026-04-25T14:00:00Z", size_byte: 1234 },
  { client: "claude-code", path: "/cc/2026-04-24.bak", kind: "timestamped" as const,
    mod_time: "2026-04-24T14:00:00Z", size_byte: 1100 },
];

describe("BackupsList", () => {
  beforeEach(() => {
    cleanup(); // happy-dom: prior renders linger in document.body without explicit cleanup
    vi.restoreAllMocks();
    vi.spyOn(api, "getBackups").mockResolvedValue(fixture);
    vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue([]);
  });
  afterEach(() => cleanup());

  it("renders the 7 core client groups unconditionally (no wave-2 groups without backups)", async () => {
    const { findAllByText } = render(<BackupsList keepN={5} />);
    // Wait for load.
    await findAllByText(/claude-code/);
    // Each client has its own <details><summary>. The fixture only carries
    // claude-code backups, so only the seven always-on CORE_CLIENTS render —
    // the eight opt-in wave-2 clients are detection-gated and add no empty
    // group when they have no backups on disk.
    const summaries = document.querySelectorAll(".backups-client-group summary");
    expect(summaries.length).toBe(7);
  });

  it("renders a wave-2 client group when it has backups (detection-gated)", async () => {
    // zed (a wave-2 opt-in client) gains a group only because the payload
    // carries a backup row for it; the other seven wave-2 clients still add
    // no group. Total = 7 core + 1 detected wave-2 = 8.
    vi.spyOn(api, "getBackups").mockResolvedValue([
      ...fixture,
      { client: "zed", path: "/zed/2026-05-01.bak", kind: "timestamped" as const,
        mod_time: "2026-05-01T00:00:00Z", size_byte: 500 },
    ]);
    const { findAllByText } = render(<BackupsList keepN={5} />);
    await findAllByText(/zed/);
    const summaries = document.querySelectorAll(".backups-client-group summary");
    expect(summaries.length).toBe(8);
    // The wave-2 group sits AFTER the seven core groups (canonical order).
    const labels = Array.from(summaries).map((s) => (s.textContent ?? "").trim());
    expect(labels[7]).toContain("zed");
  });

  it("renders the locked group note (Codex copy §9.4)", async () => {
    const { findByText } = render(<BackupsList keepN={5} />);
    expect(await findByText(BACKUPS_COPY.groupNote)).toBeTruthy();
  });

  it("would-prune rows tagged with eligible badge", async () => {
    vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue(["/cc/2026-04-24.bak"]);
    const { findByTestId } = render(<BackupsList keepN={1} />);
    const badge = await findByTestId("eligible-badge");
    expect(badge.textContent).toBe(BACKUPS_COPY.rowBadge);
  });

  it("originals NEVER get the eligible badge even if path matches", async () => {
    // Defensive: simulate backend mistakenly including an original path.
    vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue(["/cc/orig.bak"]);
    const { container } = render(<BackupsList keepN={0} />);
    await waitFor(() => expect(container.querySelectorAll(".backups-row.original").length).toBeGreaterThan(0));
    const orig = Array.from(container.querySelectorAll(".backups-row.original"))[0];
    expect(orig.querySelector('[data-testid="eligible-badge"]')).toBeNull();
  });

  it("does not render a delete action for original sentinel backups", async () => {
    const { container } = render(<BackupsList keepN={5} />);
    await waitFor(() => expect(container.querySelectorAll(".backups-row.original").length).toBeGreaterThan(0));
    const original = Array.from(container.querySelectorAll(".backups-row.original"))[0];
    expect(original.querySelector(".backups-row-delete")).toBeNull();
    expect(original.querySelector(".backups-row-restore")).toBeTruthy();
  });

  it("preview failure shows 'Preview unavailable' inline + base list still visible", async () => {
    vi.spyOn(api, "getBackupsCleanPreview").mockRejectedValue(new Error("boom"));
    const { findByTestId, findAllByText } = render(<BackupsList keepN={2} />);
    expect(await findByTestId("preview-unavailable")).toBeTruthy();
    // Base list still rendered.
    await findAllByText(/claude-code/);
  });

  it("Codex pre-push P2: stale eligible badges cleared on keepN change AND on preview failure", async () => {
    // First render: keepN=1 → /cc/2026-04-24.bak eligible.
    const previewSpy = vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue(["/cc/2026-04-24.bak"]);
    const { findByTestId, queryByTestId, rerender, container } = render(<BackupsList keepN={1} />);
    expect(await findByTestId("eligible-badge")).toBeTruthy();
    // keepN bump: stale markers must clear synchronously, before the new
    // preview resolves. We capture the count BEFORE letting the timer run.
    previewSpy.mockResolvedValue([]); // new keep_n returns no eligible paths
    rerender(<BackupsList keepN={99} />);
    // The synchronous clear inside the keepN-change effect should have
    // emptied wouldRemove already; the badge must be gone.
    await waitFor(() => expect(container.querySelectorAll('[data-testid="eligible-badge"]').length).toBe(0));
    // Second transition: preview failure must also clear leftovers.
    previewSpy.mockResolvedValue(["/cc/2026-04-25.bak"]);
    rerender(<BackupsList keepN={1} />);
    expect(await findByTestId("eligible-badge")).toBeTruthy(); // re-eligible after success
    previewSpy.mockRejectedValue(new Error("backend down"));
    rerender(<BackupsList keepN={2} />);
    await findByTestId("preview-unavailable");
    // No stale eligible badges should remain alongside "Preview unavailable".
    expect(queryByTestId("eligible-badge")).toBeNull();
  });

  // Bug-bash B2 closure (#21): per-client Clean buttons.
  describe("per-client Clean now (#21)", () => {
    it("renders a Clean button per client group with the eligible count", async () => {
      vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue(["/cc/2026-04-24.bak"]);
      const { findByTestId } = render(<BackupsList keepN={1} />);
      // Wait for the debounced preview to populate wouldRemove (250ms);
      // observable via the eligible-badge rendering on the matching row.
      await findByTestId("eligible-badge");
      const btn = (await findByTestId("clean-now-claude-code")) as HTMLButtonElement;
      expect(btn.textContent).toContain("Clean claude-code only (1)");
      expect(btn.disabled).toBe(false);
      const btnEmpty = (await findByTestId("clean-now-cursor")) as HTMLButtonElement;
      // cursor has zero backups in this fixture; button must be disabled.
      expect(btnEmpty.disabled).toBe(true);
      expect(btnEmpty.textContent).toContain("Clean cursor only (0)");
    });

    it("clicking Clean for one client calls cleanBackupsForClient + invokes onClientCleaned", async () => {
      vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue(["/cc/2026-04-24.bak"]);
      const cleanSpy = vi
        .spyOn(api, "cleanBackupsForClient")
        .mockResolvedValue({ cleaned: 1, client: "claude-code" });
      const cleaned: string[] = [];
      const { findByTestId } = render(
        <BackupsList keepN={1} onClientCleaned={(c) => cleaned.push(c)} />,
      );
      // Wait for the debounced preview to populate so the button is enabled.
      await findByTestId("eligible-badge");
      const btn = (await findByTestId("clean-now-claude-code")) as HTMLButtonElement;
      await waitFor(() => expect(btn.disabled).toBe(false));
      fireEvent.click(btn);
      // Bug #2 WYSIWYG: the clean must carry the live keepN (1 here) so it
      // deletes exactly what the preview at keep_n=1 showed, not the persisted
      // setting.
      await waitFor(() => expect(cleanSpy).toHaveBeenCalledWith("claude-code", 1));
      await waitFor(() => expect(cleaned).toEqual(["claude-code"]));
    });

    it("backend error per-client renders inline error AND does NOT call onClientCleaned", async () => {
      vi.spyOn(api, "getBackupsCleanPreview").mockResolvedValue(["/cc/2026-04-24.bak"]);
      vi.spyOn(api, "cleanBackupsForClient").mockRejectedValue(new Error("disk full"));
      const cleaned: string[] = [];
      const { findByTestId, container } = render(
        <BackupsList keepN={1} onClientCleaned={(c) => cleaned.push(c)} />,
      );
      await findByTestId("eligible-badge");
      const btn = (await findByTestId("clean-now-claude-code")) as HTMLButtonElement;
      await waitFor(() => expect(btn.disabled).toBe(false));
      fireEvent.click(btn);
      await waitFor(() =>
        expect(container.textContent ?? "").toContain("disk full"),
      );
      expect(cleaned).toEqual([]);
    });
  });
});
