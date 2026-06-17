import { describe, expect, it } from "vitest";
import { aggregateStatus, stateShape } from "./status";
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


  it("handles server names that collide with Object prototype keys", () => {
    const out = aggregateStatus([row({ server: "constructor", port: 9100 })]);
    expect(out["constructor"]).toEqual({
      server: "constructor",
      state: "Running",
      port: 9100,
      daemonCount: 1,
    });
  });

  it("tolerates null input", () => {
    expect(aggregateStatus(null)).toEqual({});
  });
});

describe("stateShape", () => {
  it("returns ● for Running", () => {
    expect(stateShape("Running")).toBe("●");
  });

  it("returns ◐ for Partial (mixed multi-daemon aggregate)", () => {
    expect(stateShape("Partial")).toBe("◐");
  });

  it("returns ◓ for the recovering group (Starting/Restarting/Backoff/Spawning)", () => {
    for (const s of ["Starting", "Restarting", "Backoff", "Spawning"]) {
      expect(stateShape(s)).toBe("◓");
    }
  });

  it("returns ✕ for the terminal-failure group (Failed/Quarantined)", () => {
    for (const s of ["Failed", "Quarantined"]) {
      expect(stateShape(s)).toBe("✕");
    }
  });

  it("returns ○ for the benign-idle group (Ready/Scheduled/Stopped)", () => {
    for (const s of ["Ready", "Scheduled", "Stopped"]) {
      expect(stateShape(s)).toBe("○");
    }
  });

  it("returns ○ for unrecognized / blank vocabulary", () => {
    // Unknown states are benign, not errors — they must not borrow the
    // failure cross. They fall back to the open circle.
    for (const s of ["Gone", "Disabled", "Queued", ""]) {
      expect(stateShape(s)).toBe("○");
    }
  });

  it("gives the three most-confused states distinct silhouettes", () => {
    // Color-blind regression guard: Running, Stopped, Failed, and Partial
    // must NOT collapse to the same glyph (the pre-fix binary helper made
    // Stopped/Failed/Partial all render ○, defeating the shape encoding).
    const glyphs = new Set([
      stateShape("Running"),
      stateShape("Stopped"),
      stateShape("Failed"),
      stateShape("Partial"),
      stateShape("Starting"),
    ]);
    expect(glyphs.size).toBe(5);
  });
});
