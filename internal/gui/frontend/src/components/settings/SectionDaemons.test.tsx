import { describe, expect, it, vi, afterEach } from "vitest";
import { render, cleanup } from "@testing-library/preact";
import { SectionDaemons } from "./SectionDaemons";
import type { SettingsSnapshot, SettingsEnvelope } from "../../lib/settings-types";

const env: SettingsEnvelope = {
  actual_port: 9125,
  settings: [
    { key: "daemons.weekly_schedule", section: "daemons", type: "string",
      default: "weekly Sun 03:00", value: "weekly Sun 03:00", deferred: true, help: "" },
    { key: "daemons.retry_policy", section: "daemons", type: "enum",
      default: "exponential", value: "exponential", enum: ["none","linear","exponential"], deferred: true, help: "" },
  ],
};
const snap: SettingsSnapshot = { status: "ok", data: env, error: null, refresh: vi.fn(async () => {}) };

describe("SectionDaemons", () => {
  afterEach(() => cleanup());

  it("renders 'Configured schedule' label (NOT 'Current schedule' — Codex r1 P1.7)", () => {
    const { getByText, queryByText } = render(<SectionDaemons snapshot={snap} />);
    expect(getByText(/Configured schedule:/)).toBeTruthy();
    expect(queryByText(/^Current schedule:/)).toBeNull();
  });

  it("renders '(effective in A4-b)' suffix on each row", () => {
    const { getAllByText } = render(<SectionDaemons snapshot={snap} />);
    expect(getAllByText("(effective in A4-b)").length).toBeGreaterThanOrEqual(2);
  });

  it("renders 'edit coming in A4-b' affordance", () => {
    const { getByText } = render(<SectionDaemons snapshot={snap} />);
    expect(getByText("edit coming in A4-b")).toBeTruthy();
  });

  it("has no Save button (read-only)", () => {
    const { container } = render(<SectionDaemons snapshot={snap} />);
    expect(container.querySelectorAll("button")).toHaveLength(0);
  });

  it("shows 'Schedule unavailable' on snapshot error", () => {
    const errSnap: SettingsSnapshot = { status: "error", data: null, error: new Error("boom"), refresh: vi.fn(async () => {}) };
    const { getByText } = render(<SectionDaemons snapshot={errSnap} />);
    expect(getByText(/Schedule unavailable/)).toBeTruthy();
  });
});
