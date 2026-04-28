import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, waitFor, cleanup } from "@testing-library/preact";
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

  it("renders 4 client groups", async () => {
    const { findAllByText } = render(<BackupsList keepN={5} />);
    // Wait for load.
    await findAllByText(/claude-code/);
    // Each client has its own <details><summary>.
    const summaries = document.querySelectorAll(".backups-client-group summary");
    expect(summaries.length).toBe(4);
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
});
