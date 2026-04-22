import { describe, expect, it } from "vitest";
import { aggregateStatus } from "./status";
import type { DaemonStatus } from "../types";

function row(overrides: Partial<DaemonStatus>): DaemonStatus {
  return { server: "s", daemon: "default", state: "Running", port: 9100, ...overrides };
}

describe("aggregateStatus", () => {
  it("collapses single daemon into one aggregate", () => {
    const out = aggregateStatus([row({ server: "foo", port: 9100 })]);
    expect(out["foo"]).toEqual({ server: "foo", state: "Running", port: 9100, daemonCount: 1 });
  });

  it("returns shared state when every daemon agrees", () => {
    const out = aggregateStatus([
      row({ server: "serena", daemon: "claude", port: 9121, state: "Running" }),
      row({ server: "serena", daemon: "codex", port: 9122, state: "Running" }),
    ]);
    expect(out["serena"].state).toBe("Running");
    expect(out["serena"].daemonCount).toBe(2);
  });

  it("returns 'Partial' when daemons disagree", () => {
    const out = aggregateStatus([
      row({ server: "serena", daemon: "claude", state: "Running" }),
      row({ server: "serena", daemon: "codex", state: "Stopped" }),
    ]);
    expect(out["serena"].state).toBe("Partial");
  });

  it("picks the lowest non-zero port as representative", () => {
    const out = aggregateStatus([
      row({ server: "s", daemon: "a", port: 9300 }),
      row({ server: "s", daemon: "b", port: 9100 }),
      row({ server: "s", daemon: "c", port: 9200 }),
    ]);
    expect(out["s"].port).toBe(9100);
  });

  it("ignores zero ports when picking representative", () => {
    const out = aggregateStatus([
      row({ server: "s", daemon: "a", port: 0 }),
      row({ server: "s", daemon: "b", port: 9100 }),
    ]);
    expect(out["s"].port).toBe(9100);
  });

  it("returns null port when every daemon has port 0", () => {
    const out = aggregateStatus([row({ server: "s", port: 0 })]);
    expect(out["s"].port).toBeNull();
  });

  it("filters out is_maintenance rows", () => {
    const out = aggregateStatus([
      row({ server: "s", port: 9100 }),
      row({ server: "weekly", is_maintenance: true, port: 0 }),
    ]);
    expect(Object.keys(out)).toEqual(["s"]);
  });

  it("tolerates null input", () => {
    expect(aggregateStatus(null)).toEqual({});
  });
});
