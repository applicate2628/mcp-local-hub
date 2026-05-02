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
//                4=KilledRecovered, 5=KillRefused, 6=KillFailed, 7=RaceLost)
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
