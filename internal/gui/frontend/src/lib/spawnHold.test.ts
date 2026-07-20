import { describe, expect, it } from "vitest";
import {
  MISSING_BINARY,
  MISSING_WORKSPACE,
  UNAVAILABLE_BINARY,
  UNAVAILABLE_WORKSPACE,
  deriveSpawnHoldBanners,
  spawnHoldBadge,
  spawnHoldMessage,
} from "./spawnHold";
import type { DaemonStatus } from "../types";

const held = (path: string, reason = MISSING_BINARY): DaemonStatus =>
  ({ server: "s", daemon: "d", state: "Stopped", spawn_hold_reason: reason, spawn_hold_path: path }) as DaemonStatus;
const healthy = (): DaemonStatus => ({ server: "s", daemon: "d", state: "Running" }) as DaemonStatus;

const BIN = "C:\\Users\\dev\\.local\\bin\\mcphub.exe";

describe("spawn-hold copy", () => {
  it("names the remedy, not the internal state", () => {
    // The whole point: the operator must be told what to DO. A reason id or a
    // threshold message is what failed them in the incident.
    expect(spawnHoldMessage(MISSING_BINARY, BIN).toLowerCase()).toContain("reinstall");
    expect(spawnHoldMessage(MISSING_BINARY, BIN)).toContain(BIN);
  });

  it("promises automatic recovery and never asks for a recovery command", () => {
    // A hold is not a quarantine. Telling the operator to run `mcphub daemon
    // recover` here would be both wrong and more frightening than the truth.
    for (const reason of [MISSING_BINARY, MISSING_WORKSPACE, "some-future-id"]) {
      const msg = spawnHoldMessage(reason, BIN).toLowerCase();
      expect(msg).toMatch(/by (itself|themselves)|automatic/);
      expect(msg).not.toContain("mcphub daemon recover");
      expect(msg).not.toContain("quarantin");
    }
  });

  it("degrades gracefully on an unknown reason id from a newer supervisor", () => {
    expect(spawnHoldBadge("brand-new-id")).toBe("Required file missing");
    expect(spawnHoldMessage("brand-new-id", BIN)).toContain(BIN);
    // Never leak the raw id into operator-facing copy.
    expect(spawnHoldBadge("brand-new-id")).not.toContain("brand-new-id");
  });

  it("omits the parenthetical when no path is known", () => {
    expect(spawnHoldMessage(MISSING_BINARY, "")).not.toContain("()");
  });
});

describe("spawn-hold copy — unavailable volume gives a DIFFERENT remedy", () => {
  // Go folds ERROR_BAD_NETPATH into fs.ErrNotExist, so a disconnected mapped
  // drive looks exactly like a deleted file. Holding is right either way, but
  // "reinstall mcphub" is the wrong instruction for an offline share.
  it("never tells the operator to reinstall when the drive is offline", () => {
    const msg = spawnHoldMessage(UNAVAILABLE_BINARY, "Z:\\bin\\mcphub.exe").toLowerCase();
    expect(msg).toContain("not available");
    expect(msg).toContain("reinstalling will not help");
  });

  it("still promises automatic recovery for the unavailable variants", () => {
    for (const reason of [UNAVAILABLE_BINARY, UNAVAILABLE_WORKSPACE]) {
      const msg = spawnHoldMessage(reason, "Z:\\x").toLowerCase();
      expect(msg).toMatch(/by (itself|themselves)/);
      expect(msg).not.toContain("mcphub daemon recover");
    }
  });

  it("gives the unavailable variants their own badges", () => {
    expect(spawnHoldBadge(UNAVAILABLE_BINARY)).not.toBe(spawnHoldBadge(MISSING_BINARY));
    expect(spawnHoldBadge(UNAVAILABLE_BINARY).toLowerCase()).toContain("unavailable");
  });
});

describe("deriveSpawnHoldBanners", () => {
  it("collapses the incident shape: many daemons, one missing binary", () => {
    const banners = deriveSpawnHoldBanners(Array.from({ length: 12 }, () => held(BIN)));
    expect(banners).toHaveLength(1);
    expect(banners[0].count).toBe(12);
    expect(banners[0].path).toBe(BIN);
    expect(banners[0].headline).toContain("12 servers cannot start");
    expect(banners[0].headline.toLowerCase()).toContain("reinstall");
  });

  it("still collapses when healthy daemons sit alongside the held ones", () => {
    const banners = deriveSpawnHoldBanners([held(BIN), held(BIN), healthy()]);
    expect(banners).toHaveLength(1);
    expect(banners[0].count).toBe(2);
  });

  // FIX-3: the remedy lives in the banner; the card only has a hover tooltip.
  // A one-server host must therefore STILL get a banner, or the operator can
  // see the cause but never the fix without hovering.
  it("banners a SINGLE held daemon so the remedy is visible without hover", () => {
    const banners = deriveSpawnHoldBanners([held(BIN), healthy()]);
    expect(banners).toHaveLength(1);
    expect(banners[0].count).toBe(1);
    expect(banners[0].headline).toContain("1 server cannot start");
    expect(banners[0].headline.toLowerCase()).toContain("reinstall");
  });

  it("emits one banner PER distinct cause in a mixed fleet", () => {
    const banners = deriveSpawnHoldBanners([
      held(BIN),
      held(BIN),
      held("D:\\other.exe"),
      held(BIN, MISSING_WORKSPACE),
    ]);
    expect(banners).toHaveLength(3);
    expect(banners.map((b) => b.count)).toEqual([2, 1, 1]);
    // Every held daemon is accounted for by exactly one banner — nobody is
    // left with a cause on screen and no remedy.
    expect(banners.reduce((n, b) => n + b.count, 0)).toBe(4);
  });

  it("produces nothing for a healthy fleet", () => {
    expect(deriveSpawnHoldBanners([healthy(), healthy()])).toEqual([]);
    expect(deriveSpawnHoldBanners([])).toEqual([]);
  });
});
