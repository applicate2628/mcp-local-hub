import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import {
  aggregateStatus,
  daemonStateVisual,
  isRecoveryEligibleState,
  stateShape,
} from "./status";
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

describe("daemonStateVisual", () => {
  const green = "bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-300";
  const amber = "bg-yellow-100 text-yellow-800 dark:bg-yellow-900 dark:text-yellow-300";
  const orange = "bg-orange-100 text-orange-800 dark:bg-orange-900 dark:text-orange-300";
  const red = "bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-300";
  const gray = "bg-gray-100 text-gray-800 dark:bg-gray-700 dark:text-gray-300";

  it.each([
    ["Running", "card ok", green],
    ["Partial", "card warning", amber],
    ["Starting", "card warning", amber],
    ["Restarting", "card warning", amber],
    ["Backoff", "card warning", amber],
    ["Spawning", "card warning", amber],
    ["Quarantined", "card warning", orange],
    ["Failed", "card down", red],
    ["Ready", "card idle", gray],
    ["Scheduled", "card idle", gray],
    ["Stopped", "card idle", gray],
    ["Idle", "card idle", gray],
    ["", "card idle", gray],
    ["FutureState", "card idle", gray],
  ])("maps %s through the canonical visual bucket", (state, cardClass, badgeClass) => {
    expect(daemonStateVisual(state)).toEqual({ cardClass, badgeClass });
  });

  it("is the only Dashboard owner of card and badge status colors", () => {
    const source = readFileSync(
      resolve(process.cwd(), "src/screens/Dashboard.tsx"),
      "utf8",
    );
    expect(source).toContain("daemonStateVisual(d.state)");
    expect(source).not.toContain('d.state === "Running" ? "card ok"');
    expect(source).not.toMatch(/d\.state\s*===\s*["']Running["']\s*\?\s*["']bg-green/);
  });
});

describe("isRecoveryEligibleState", () => {
  it("admits Quarantined by exact value only", () => {
    expect(isRecoveryEligibleState("Quarantined")).toBe(true);
    for (const state of ["LostChild", "quarantined", "Failed", "", "FutureState"]) {
      expect(isRecoveryEligibleState(state)).toBe(false);
    }
  });
});
