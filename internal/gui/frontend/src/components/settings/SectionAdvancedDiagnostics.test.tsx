// internal/gui/frontend/src/components/settings/SectionAdvancedDiagnostics.test.tsx
//
// Bridge-translation tests for the two-click force-kill flow. The
// fictional Verdict shape in plan Task 14 (PascalCase keys, "Stuck"
// string class, PIDCmdline:[]string) does NOT match the actual wire
// shape from /api/force-kill/probe — encoding/json marshals the C1
// Verdict with snake_case tags, numeric VerdictClass iota, and
// PIDCmdline:`json:"-"` (excluded for security; pid_subcommand carries
// only argv[1]).
//
// These tests use the real wire shape:
//   - class:int (0=Healthy, 1=LiveUnreachable, 2=DeadPID, 3=Malformed,
//                4=KilledRecovered, 5=KillRefused, 6=KillFailed, 7=RaceLost,
//                8=Indeterminate)
//   - "Stuck" predicate = pid_alive===true && ping_match===false
//   - cmdline guard = pid_subcommand === "gui" || pid_subcommand === ""
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/preact";
import { SectionAdvancedDiagnostics } from "./SectionAdvancedDiagnostics";

beforeEach(() => {
  cleanup(); // happy-dom: prior renders linger in document.body without explicit cleanup
  HTMLDialogElement.prototype.showModal = function () { (this as any).open = true; };
  HTMLDialogElement.prototype.close = function () { (this as any).open = false; };
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// Stuck = VerdictLiveUnreachable (class:1) — pid_alive but no ping match.
// This is the only kill-eligible classification in C1's iota.
const stuckVerdict = {
  class: 1, // VerdictLiveUnreachable
  pid: 1234,
  port: 9125,
  mtime: "2026-05-01T03:00:00Z",
  pid_alive: true,
  pid_image: "C:/path/mcphub.exe",
  pid_subcommand: "gui",
  pid_start: "2026-05-01T02:59:00Z",
  ping_match: false,
};

// Healthy = class:0, pid_alive:true, ping_match:true.
const healthyVerdict = {
  class: 0, // VerdictHealthy
  pid: 1234,
  port: 9125,
  mtime: "2026-05-01T03:00:00Z",
  pid_alive: true,
  pid_image: "C:/path/mcphub.exe",
  pid_subcommand: "gui",
  pid_start: "2026-05-01T02:59:00Z",
  ping_match: true,
};

// Indeterminate = VerdictIndeterminate (class:8). The server force-sets
// pid_alive:false because the identity probe produced NO liveness fact — an
// ambiguous platform error that is explicitly NOT proof of death. The
// (pid_alive, ping_match) tuple therefore does not summarize this class, which
// is why classLabel must branch on the numeric class first.
const indeterminateVerdict = {
  class: 8, // VerdictIndeterminate
  pid: 1234,
  port: 9125,
  mtime: "2026-05-01T03:00:00Z",
  pid_alive: false,
  pid_image: "",
  pid_subcommand: "",
  pid_start: "0001-01-01T00:00:00Z",
  ping_match: false,
};

function stubFetchOnce(body: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn().mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: () => Promise.resolve(body),
    } as unknown as Response),
  );
}

describe("SectionAdvancedDiagnostics", () => {
  it("first click runs Probe and shows result strip", async () => {
    stubFetchOnce(healthyVerdict);
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("verdict-strip"));
    expect(screen.getByText(/healthy/i)).toBeTruthy();
    vi.unstubAllGlobals();
  });

  it("Stuck + identity gate pass → Kill button appears with PID baked in", async () => {
    stubFetchOnce(stuckVerdict);
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("kill-button"));
    expect(screen.getByText(/Kill stuck PID 1234/)).toBeTruthy();
    vi.unstubAllGlobals();
  });

  // Review finding: classLabel branched only on the (pid_alive, ping_match)
  // tuple, so VerdictIndeterminate's pid_alive:false rendered
  // "Vacant — lock file present but no live holder" — the exact opposite of
  // the class's defining guarantee. Telling an operator the lock is vacant
  // when a live GUI may still hold it invites them to force a second instance.
  //
  // MUTATION: delete the `v.class === VERDICT_INDETERMINATE` branch at the top
  // of classLabel — this test then fails on the "Vacant" assertion.
  it("Indeterminate → strip says Indeterminate, never Vacant, and no Kill button", async () => {
    stubFetchOnce(indeterminateVerdict);
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("verdict-strip"));
    const strip = screen.getByTestId("verdict-strip");
    expect(strip.textContent).toMatch(/Indeterminate/i);
    expect(strip.textContent).not.toMatch(/Vacant/i);
    expect(strip.textContent).not.toMatch(/no live holder/i);
    // Ambiguous liveness is never kill-eligible.
    expect(screen.queryByTestId("kill-button")).toBeNull();
    vi.unstubAllGlobals();
  });

  // The 503 companion of the same finding: force_kill.go returns
  // {error:"liveness_indeterminate"} when the kill was SKIPPED. The generic
  // non-ok path said "Kill failed", implying a kill was attempted.
  //
  // MUTATION: remove the `body.error === "liveness_indeterminate"` branch in
  // doKill — this test then fails because the banner reads "Kill failed".
  it("kill 503 liveness_indeterminate → banner says skipped, not failed", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        // 1st call: probe returns a Stuck verdict so the Kill button renders.
        .mockResolvedValueOnce({
          ok: true,
          status: 200,
          json: () => Promise.resolve(stuckVerdict),
        } as unknown as Response)
        // 2nd call: the kill is skipped because liveness went indeterminate
        // between the probe click and the kill click.
        .mockResolvedValueOnce({
          ok: false,
          status: 503,
          json: () =>
            Promise.resolve({
              error: "liveness_indeterminate",
              detail: "kill skipped: Indeterminate",
              verdict: indeterminateVerdict,
            }),
        } as unknown as Response),
    );
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("kill-button"));
    fireEvent.click(screen.getByTestId("kill-button"));
    await waitFor(() => screen.getByTestId("confirm-modal"));
    fireEvent.click(screen.getByTestId("confirm-modal-confirm"));

    await waitFor(() => {
      const alert = screen.getByRole("alert");
      expect(alert.textContent).toMatch(/skipped/i);
    });
    expect(screen.getByRole("alert").textContent).not.toMatch(/Kill failed/i);
    // The strip refreshes from the returned verdict.
    expect(screen.getByTestId("verdict-strip").textContent).toMatch(/Indeterminate/i);
    vi.unstubAllGlobals();
  });

  it("Healthy → Kill button does NOT render", async () => {
    stubFetchOnce(healthyVerdict);
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("verdict-strip"));
    expect(screen.queryByTestId("kill-button")).toBeNull();
    vi.unstubAllGlobals();
  });

  it("Explorer launch (empty pid_subcommand) still passes the cmdline guard", async () => {
    // Memo D12 cmdline guard translates to pid_subcommand: empty subcmd
    // corresponds to len(argv) <= 1 (Explorer/Start-menu launches default
    // to gui via cmd/mcphub/main.go:32). Kill MUST still be allowed.
    const explorerLaunch = { ...stuckVerdict, pid_subcommand: "" };
    stubFetchOnce(explorerLaunch);
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("kill-button"));
    expect(screen.getByText(/Kill stuck PID 1234/)).toBeTruthy();
    vi.unstubAllGlobals();
  });

  it("Mismatched image (e.g. cmd.exe) → Kill button does NOT render", async () => {
    const mismatched = {
      ...stuckVerdict,
      pid_image: "C:/Windows/System32/cmd.exe",
      pid_subcommand: "/c",
    };
    stubFetchOnce(mismatched);
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("verdict-strip"));
    expect(screen.queryByTestId("kill-button")).toBeNull();
    vi.unstubAllGlobals();
  });

  it("pid_start >= mtime → Kill button does NOT render (clock semantics fail-closed)", async () => {
    const startAfterMtime = { ...stuckVerdict, pid_start: "2026-05-01T03:01:00Z" };
    stubFetchOnce(startAfterMtime);
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("verdict-strip"));
    expect(screen.queryByTestId("kill-button")).toBeNull();
    vi.unstubAllGlobals();
  });

  it("Kill button click opens ConfirmModal", async () => {
    stubFetchOnce(stuckVerdict);
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("kill-button"));
    fireEvent.click(screen.getByTestId("kill-button"));
    await waitFor(() => screen.getByTestId("confirm-modal"));
    // Scope the assertion to the modal so it does not collide with the
    // outer kill-button label which also contains "PID 1234".
    const modal = screen.getByTestId("confirm-modal");
    expect(modal.textContent).toMatch(/PID 1234/);
  });

  it("rejects PascalCase Verdict (wire is snake_case)", async () => {
    // Regression guard for memo D12: the original plan reference shape
    // used PascalCase keys + a "Stuck" string class, which does NOT
    // match the actual /api/force-kill/probe wire shape. If a future
    // refactor accidentally re-introduces PascalCase parsing, the kill
    // button must NOT activate against this misshapen payload.
    const wrongShape = {
      Class: "Stuck",
      PID: 1234,
      PIDImage: "C:/path/mcphub.exe",
      PIDCmdline: ["mcphub.exe", "gui"],
      PIDStart: "2026-05-01T02:59:00Z",
      Mtime: "2026-05-01T03:00:00Z",
      PingMatch: false,
    };
    stubFetchOnce(wrongShape);
    render(<SectionAdvancedDiagnostics />);
    fireEvent.click(screen.getByText("Diagnose lock state"));
    await waitFor(() => screen.getByTestId("verdict-strip"));
    expect(screen.queryByTestId("kill-button")).toBeNull();
    vi.unstubAllGlobals();
  });
});
