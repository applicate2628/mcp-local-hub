import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, cleanup, fireEvent, waitFor } from "@testing-library/preact";
import { SectionDaemons } from "./SectionDaemons";
import type { SettingsSnapshot, SettingsEnvelope } from "../../lib/settings-types";

// Snapshot fixture used across all tests. Mirrors the registry deltas
// landed in PR #1 — knob is editable (not deferred), schedule is editable,
// retry is editable enum.
const env: SettingsEnvelope = {
  actual_port: 9125,
  settings: [
    { key: "daemons.weekly_refresh_default", section: "daemons", type: "bool",
      default: "false", value: "false", deferred: false, help: "knob help" },
    { key: "daemons.weekly_schedule", section: "daemons", type: "string",
      default: "weekly Sun 03:00", value: "weekly Sun 03:00", deferred: false, help: "" },
    { key: "daemons.retry_policy", section: "daemons", type: "enum",
      default: "exponential", value: "exponential", enum: ["none","linear","exponential"], deferred: false, help: "" },
    { key: "daemons.auto_prune_workspaces", section: "daemons", type: "bool",
      default: "true", value: "true", deferred: false, help: "auto-prune help" },
    { key: "daemons.prune_dead_worktrees", section: "daemons", type: "bool",
      default: "true", value: "true", deferred: false, help: "dead-worktree help" },
    { key: "daemons.prune_idle_hours", section: "daemons", type: "int",
      default: "0", value: "0", min: 0, max: 8760, deferred: false, help: "idle help" },
  ],
};
const snap = (refresh = vi.fn(async () => {})): SettingsSnapshot =>
  ({ status: "ok", data: env, error: null, refresh });

// The WeeklyMembershipTable inside SectionDaemons fetches on mount; stub
// fetch with an empty rows envelope so tests don't unhandled-reject. Each
// test that needs richer membership data installs its own mockResolvedValue.
const mockFetch = vi.fn();
function jsonResponse(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response;
}

const defaultDaemonEnvResponse = {
  daemons: [
    {
      task_name: "\\mcp-local-hub-memory-default",
      server: "memory",
      daemon: "default",
      env: { MEMORY_FILE_PATH: "old.jsonl" },
    },
  ],
};

function mockSettingsFetch(membershipBody: unknown = { rows: [] }) {
  mockFetch.mockImplementation(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes("/api/daemon/env")) {
      return jsonResponse(defaultDaemonEnvResponse);
    }
    if (url.includes("/api/daemons/weekly-refresh-membership")) {
      return jsonResponse(membershipBody);
    }
    return jsonResponse({});
  });
}

beforeEach(() => {
  mockFetch.mockReset();
  // Default: empty membership — keeps tests focused on the field-row UI
  // and avoids cross-coupling failures with WeeklyMembershipTable internals.
  mockSettingsFetch();
  vi.stubGlobal("fetch", mockFetch);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("SectionDaemons (editable, A4-b PR #1 / Task 11)", () => {
  it("renders cron input, retry select, knob checkbox, and the membership table host", async () => {
    const { findByTestId } = render(<SectionDaemons snapshot={snap()} />);
    expect((await findByTestId("daemons-weekly-schedule-input")) as HTMLInputElement).toBeTruthy();
    expect((await findByTestId("daemons-retry-policy-select")) as HTMLSelectElement).toBeTruthy();
    expect((await findByTestId("daemons-weekly-refresh-default-checkbox")) as HTMLInputElement).toBeTruthy();
    expect(await findByTestId("weekly-membership-table")).toBeTruthy();
  });

  it("Save button is disabled with no edits and enabled after editing the cron field", async () => {
    const { findByTestId } = render(<SectionDaemons snapshot={snap()} />);
    const saveBtn = (await findByTestId("daemons-save")) as HTMLButtonElement;
    expect(saveBtn.disabled).toBe(true);

    const cron = (await findByTestId("daemons-weekly-schedule-input")) as HTMLInputElement;
    fireEvent.input(cron, { target: { value: "weekly Tue 14:30" } });
    await waitFor(() => expect(saveBtn.disabled).toBe(false));
  });

  it("Save button enables after toggling the knob checkbox", async () => {
    const { findByTestId } = render(<SectionDaemons snapshot={snap()} />);
    const saveBtn = (await findByTestId("daemons-save")) as HTMLButtonElement;
    expect(saveBtn.disabled).toBe(true);

    const knob = (await findByTestId("daemons-weekly-refresh-default-checkbox")) as HTMLInputElement;
    fireEvent.click(knob);
    await waitFor(() => expect(saveBtn.disabled).toBe(false));
  });

  it("renders the auto-prune master toggle (checked by default) and Save enables on toggle", async () => {
    // Bug #1 (2026-06-15): the auto_prune_workspaces master gate was missing
    // from the GUI — only the idle-hours sub-knob rendered, so the operator
    // could not see or control "autoprune". The toggle defaults checked
    // (registry default "true").
    const { findByTestId } = render(<SectionDaemons snapshot={snap()} />);
    const toggle = (await findByTestId("daemons-auto-prune-workspaces-checkbox")) as HTMLInputElement;
    expect(toggle).toBeTruthy();
    expect(toggle.checked).toBe(true);
    const saveBtn = (await findByTestId("daemons-save")) as HTMLButtonElement;
    expect(saveBtn.disabled).toBe(true);
    fireEvent.click(toggle);
    await waitFor(() => expect(saveBtn.disabled).toBe(false));
  });

  it("renders the dead-worktree toggle (checked by default) and Save enables on toggle", async () => {
    // PR-2: the daemons.prune_dead_worktrees gate controls the dead-git-worktree
    // structural orphan signal (a leftover worktree dir whose git admin dir is
    // gone). The toggle defaults checked (registry default "true").
    const { findByTestId } = render(<SectionDaemons snapshot={snap()} />);
    const toggle = (await findByTestId("daemons-prune-dead-worktrees-checkbox")) as HTMLInputElement;
    expect(toggle).toBeTruthy();
    expect(toggle.checked).toBe(true);
    const saveBtn = (await findByTestId("daemons-save")) as HTMLButtonElement;
    expect(saveBtn.disabled).toBe(true);
    fireEvent.click(toggle);
    await waitFor(() => expect(saveBtn.disabled).toBe(false));
  });

  it("Save round-trips the dead-worktree toggle to PUT /api/settings/daemons.prune_dead_worktrees", async () => {
    // Capture the PUT to the dead-worktree key and assert the wire body carries
    // the toggled value ("false"). Mock the PUT path with a header-bearing
    // JSON response so settings-api's jsonOrThrow resolves cleanly.
    let putKey: string | null = null;
    let putBody: string | null = null;
    mockFetch.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes("/api/daemon/env")) return jsonResponse(defaultDaemonEnvResponse);
      if (url.includes("/api/daemons/weekly-refresh-membership")) return jsonResponse({ rows: [] });
      if (url.includes("/api/settings/") && init?.method === "PUT") {
        putKey = decodeURIComponent(url.split("/api/settings/")[1] ?? "");
        putBody = typeof init?.body === "string" ? init.body : null;
        return {
          ok: true,
          status: 200,
          headers: { get: (h: string) => (h.toLowerCase() === "content-type" ? "application/json" : null) },
          json: async () => ({}),
        } as unknown as Response;
      }
      return jsonResponse({});
    });

    const refresh = vi.fn(async () => {});
    const { findByTestId } = render(<SectionDaemons snapshot={snap(refresh)} />);
    const toggle = (await findByTestId("daemons-prune-dead-worktrees-checkbox")) as HTMLInputElement;
    fireEvent.click(toggle); // true → false
    const saveBtn = (await findByTestId("daemons-save")) as HTMLButtonElement;
    await waitFor(() => expect(saveBtn.disabled).toBe(false));
    fireEvent.click(saveBtn);

    await waitFor(() => expect(putKey).toBe("daemons.prune_dead_worktrees"));
    expect(putBody).toBe(JSON.stringify({ value: "false" }));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("Save button enables after changing the retry policy select", async () => {
    const { findByTestId } = render(<SectionDaemons snapshot={snap()} />);
    const saveBtn = (await findByTestId("daemons-save")) as HTMLButtonElement;
    const retry = (await findByTestId("daemons-retry-policy-select")) as HTMLSelectElement;
    fireEvent.change(retry, { target: { value: "linear" } });
    await waitFor(() => expect(saveBtn.disabled).toBe(false));
  });

  it("Reset reverts edits and clears the dirty state", async () => {
    const { findByTestId } = render(<SectionDaemons snapshot={snap()} />);
    const cron = (await findByTestId("daemons-weekly-schedule-input")) as HTMLInputElement;
    fireEvent.input(cron, { target: { value: "weekly Tue 14:30" } });
    await waitFor(() => expect(cron.value).toBe("weekly Tue 14:30"));

    const resetBtn = (await findByTestId("daemons-reset")) as HTMLButtonElement;
    expect(resetBtn.disabled).toBe(false);
    fireEvent.click(resetBtn);
    await waitFor(() => expect(cron.value).toBe("weekly Sun 03:00"));

    const saveBtn = (await findByTestId("daemons-save")) as HTMLButtonElement;
    await waitFor(() => expect(saveBtn.disabled).toBe(true));
  });

  it("bubbles dirty=true via onDirtyChange after editing", async () => {
    const onDirty = vi.fn();
    const { findByTestId } = render(<SectionDaemons snapshot={snap()} onDirtyChange={onDirty} />);
    const knob = (await findByTestId("daemons-weekly-refresh-default-checkbox")) as HTMLInputElement;
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
    fireEvent.click(knob);
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
  });

  it("bubbles env dirty to the navigation guard without enabling section Save", async () => {
    const onDirty = vi.fn();
    const { findByTestId } = render(<SectionDaemons snapshot={snap()} onDirtyChange={onDirty} />);
    const saveBtn = (await findByTestId("daemons-save")) as HTMLButtonElement;
    const value = (await findByTestId("daemon-env-value")) as HTMLInputElement;

    await waitFor(() => expect(value.value).toBe("old.jsonl"));
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
    expect(saveBtn.disabled).toBe(true);

    fireEvent.input(value, { target: { value: "draft.jsonl" } });

    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    expect(saveBtn.disabled).toBe(true);
  });

  it("Reset clears membership edits and resets dirty state (P2-A)", async () => {
    const onDirty = vi.fn();
    // Seed one membership row so the table renders a checkbox.
    mockSettingsFetch({
      rows: [
        {
          workspace_key: "ws1",
          workspace_path: "/ws1",
          language: "python",
          weekly_refresh: false,
        },
      ],
    });
    render(<SectionDaemons snapshot={snap()} onDirtyChange={onDirty} />);
    // Wait for the table to finish loading.
    await waitFor(() => expect(mockFetch).toHaveBeenCalled());
    // Wait for initial dirty=false to stabilise.
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));

    // Toggle the membership checkbox → dirty=true bubbles.
    const checkbox = (await waitFor(() =>
      document.querySelector('[data-testid="membership-ws1-python"]')
    )) as HTMLInputElement;
    fireEvent.change(checkbox, { target: { checked: true } });
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));

    // Seed the re-fetch that happens after Reset remounts the table.
    mockSettingsFetch({
      rows: [
        {
          workspace_key: "ws1",
          workspace_path: "/ws1",
          language: "python",
          weekly_refresh: false,
        },
      ],
    });

    // Click Reset — bumps tableResetKey → WeeklyMembershipTable remounts → edits cleared.
    const resetBtn = document.querySelector('[data-testid="daemons-reset"]') as HTMLButtonElement;
    fireEvent.click(resetBtn);

    // After remount and re-fetch, onDirtyChange must be called with false.
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
  });

  it("shows 'Schedule unavailable' on snapshot error", () => {
    const errSnap: SettingsSnapshot = {
      status: "error",
      data: null,
      error: new Error("boom"),
      refresh: vi.fn(async () => {}),
    };
    const { getByText } = render(<SectionDaemons snapshot={errSnap} />);
    expect(getByText(/Schedule unavailable/)).toBeTruthy();
  });
});
