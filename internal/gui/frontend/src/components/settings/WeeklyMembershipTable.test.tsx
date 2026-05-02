import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, cleanup, fireEvent, waitFor } from "@testing-library/preact";
import { WeeklyMembershipTable } from "./WeeklyMembershipTable";

// Stub global fetch to model GET /api/daemons/weekly-refresh-membership.
// Each test installs its own response via mockFetch.mockResolvedValueOnce.
const mockFetch = vi.fn();

function jsonResponse(body: unknown, status = 200): Response {
  // Minimal Response shim sufficient for the api-daemons.ts usage:
  // - r.ok / r.status drive the parse path
  // - r.json() returns the configured body
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response;
}

beforeEach(() => {
  mockFetch.mockReset();
  vi.stubGlobal("fetch", mockFetch);
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("WeeklyMembershipTable", () => {
  it("renders one row per registry entry with correct initial state", async () => {
    mockFetch.mockResolvedValueOnce(
      jsonResponse({
        rows: [
          { workspace_key: "k1", workspace_path: "D:/p1", language: "python", weekly_refresh: true },
          { workspace_key: "k1", workspace_path: "D:/p1", language: "rust", weekly_refresh: false },
          { workspace_key: "k2", workspace_path: "/p2", language: "go", weekly_refresh: true },
        ],
      }),
    );

    const { findByTestId } = render(
      <WeeklyMembershipTable onDirtyChange={() => {}} onDeltasChange={() => {}} />,
    );

    const py = (await findByTestId("membership-k1-python")) as HTMLInputElement;
    const rs = (await findByTestId("membership-k1-rust")) as HTMLInputElement;
    const go = (await findByTestId("membership-k2-go")) as HTMLInputElement;
    expect(py.checked).toBe(true);
    expect(rs.checked).toBe(false);
    expect(go.checked).toBe(true);
  });

  it("toggling a row dirties the section via onDirtyChange(true) and emits the delta", async () => {
    mockFetch.mockResolvedValueOnce(
      jsonResponse({
        rows: [
          { workspace_key: "k1", workspace_path: "D:/p1", language: "python", weekly_refresh: true },
        ],
      }),
    );
    const onDirty = vi.fn();
    const onDeltas = vi.fn();
    const { findByTestId } = render(
      <WeeklyMembershipTable onDirtyChange={onDirty} onDeltasChange={onDeltas} />,
    );

    const cb = (await findByTestId("membership-k1-python")) as HTMLInputElement;
    // Initial post-load: clean.
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
    fireEvent.click(cb);
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(true));
    // Last delta payload contains the toggled row with enabled=false (it
    // was true server-side, now unchecked locally).
    const lastDeltas = onDeltas.mock.calls.at(-1)?.[0] ?? [];
    expect(lastDeltas).toEqual([
      { workspace_key: "k1", language: "python", enabled: false },
    ]);
  });

  it("Select all flips every row to enabled (only divergent rows count as deltas)", async () => {
    mockFetch.mockResolvedValueOnce(
      jsonResponse({
        rows: [
          { workspace_key: "k1", workspace_path: "D:/p1", language: "python", weekly_refresh: false },
          { workspace_key: "k1", workspace_path: "D:/p1", language: "rust", weekly_refresh: true },
          { workspace_key: "k2", workspace_path: "/p2", language: "go", weekly_refresh: false },
        ],
      }),
    );
    const onDeltas = vi.fn();
    const { findByTestId } = render(
      <WeeklyMembershipTable onDirtyChange={() => {}} onDeltasChange={onDeltas} />,
    );

    const selectAll = await findByTestId("weekly-membership-select-all");
    fireEvent.click(selectAll);

    // Every visible checkbox is now checked.
    const py = (await findByTestId("membership-k1-python")) as HTMLInputElement;
    const rs = (await findByTestId("membership-k1-rust")) as HTMLInputElement;
    const go = (await findByTestId("membership-k2-go")) as HTMLInputElement;
    await waitFor(() => {
      expect(py.checked).toBe(true);
      expect(rs.checked).toBe(true);
      expect(go.checked).toBe(true);
    });

    // Deltas only include the two rows whose server value was false. The
    // already-true rust row matches the server and is NOT a delta — that's
    // the D5 idempotent partial-update contract.
    const lastDeltas = onDeltas.mock.calls.at(-1)?.[0] ?? [];
    expect(lastDeltas).toHaveLength(2);
    expect(lastDeltas).toEqual(
      expect.arrayContaining([
        { workspace_key: "k1", language: "python", enabled: true },
        { workspace_key: "k2", language: "go", enabled: true },
      ]),
    );
  });

  it("Clear all flips every row to disabled (only divergent rows count as deltas)", async () => {
    mockFetch.mockResolvedValueOnce(
      jsonResponse({
        rows: [
          { workspace_key: "k1", workspace_path: "D:/p1", language: "python", weekly_refresh: true },
          { workspace_key: "k2", workspace_path: "/p2", language: "go", weekly_refresh: false },
        ],
      }),
    );
    const onDeltas = vi.fn();
    const { findByTestId } = render(
      <WeeklyMembershipTable onDirtyChange={() => {}} onDeltasChange={onDeltas} />,
    );

    const clearAll = await findByTestId("weekly-membership-clear-all");
    fireEvent.click(clearAll);

    const py = (await findByTestId("membership-k1-python")) as HTMLInputElement;
    const go = (await findByTestId("membership-k2-go")) as HTMLInputElement;
    await waitFor(() => {
      expect(py.checked).toBe(false);
      expect(go.checked).toBe(false);
    });
    const lastDeltas = onDeltas.mock.calls.at(-1)?.[0] ?? [];
    expect(lastDeltas).toEqual([
      { workspace_key: "k1", language: "python", enabled: false },
    ]);
  });

  it("empty registry renders the empty-state copy and stays clean", async () => {
    mockFetch.mockResolvedValueOnce(jsonResponse({ rows: [] }));
    const onDirty = vi.fn();
    const { findByText } = render(
      <WeeklyMembershipTable onDirtyChange={onDirty} onDeltasChange={() => {}} />,
    );
    expect(await findByText(/No workspaces registered yet/i)).toBeTruthy();
    // No checkbox rendered → no deltas → never dirty.
    await waitFor(() => expect(onDirty).toHaveBeenLastCalledWith(false));
  });
});
