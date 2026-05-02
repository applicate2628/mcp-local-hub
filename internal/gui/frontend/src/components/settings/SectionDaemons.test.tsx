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

beforeEach(() => {
  mockFetch.mockReset();
  // Default: empty membership — keeps tests focused on the field-row UI
  // and avoids cross-coupling failures with WeeklyMembershipTable internals.
  mockFetch.mockResolvedValue(jsonResponse({ rows: [] }));
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

  it("Reset clears membership edits and resets dirty state (P2-A)", async () => {
    const onDirty = vi.fn();
    // Seed one membership row so the table renders a checkbox.
    mockFetch.mockResolvedValue(
      jsonResponse({
        rows: [
          { workspace_key: "ws1", workspace_path: "/ws1", language: "python", weekly_refresh: false },
        ],
      })
    );
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
    mockFetch.mockResolvedValue(
      jsonResponse({
        rows: [
          { workspace_key: "ws1", workspace_path: "/ws1", language: "python", weekly_refresh: false },
        ],
      })
    );

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
