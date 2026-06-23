import { describe, it, expect } from "vitest";
import { groupMigrationEntries } from "./migration-grouping";
import type { ScanResult } from "../types";

describe("groupMigrationEntries", () => {
  it("splits entries by backend-provided status into 4 groups", () => {
    const scan: ScanResult = {
      at: "2026-04-23T00:00:00Z",
      entries: [
        {name: "a", status: "via-hub", client_presence: {"claude-code": {transport: "http", endpoint: "http://localhost:9200/mcp"}}},
        {name: "b", status: "can-migrate", client_presence: {"claude-code": {transport: "stdio", endpoint: "npx"}}},
        {name: "c", status: "unknown", client_presence: {"codex-cli": {transport: "stdio", endpoint: "uvx"}}},
        {name: "d", status: "per-session", client_presence: {}},
      ],
    };
    const g = groupMigrationEntries(scan, new Set());
    expect(g.viaHub.map(e => e.name)).toEqual(["a"]);
    expect(g.canMigrate.map(e => e.name)).toEqual(["b"]);
    expect(g.unknown.map(e => e.name)).toEqual(["c"]);
    expect(g.perSession.map(e => e.name)).toEqual(["d"]);
  });

  it("surfaces status='via-hub-inherited' in its own read-only bucket (visible, NOT dropped, NOT demigratable)", () => {
    const scan: ScanResult = {
      at: "2026-04-23T00:00:00Z",
      entries: [
        // Import-/below-write-target-sourced hub-loopback entry (backend classify
        // "via-hub-inherited"): hub-routed but not hub-ownable.
        {name: "time", status: "via-hub-inherited", managed: false, client_presence: {"mimocode": {transport: "http", endpoint: "http://localhost:9200/mcp", inherited: true}}},
        // A genuinely owned hub entry stays in the demigratable bucket.
        {name: "memory", status: "via-hub", managed: true, client_presence: {"claude-code": {transport: "http", endpoint: "http://localhost:9200/mcp"}}},
      ],
    };
    const g = groupMigrationEntries(scan, new Set());
    // VISIBLE: the inherited row is in its own bucket, not dropped.
    expect(g.viaHubInherited.map(e => e.name)).toEqual(["time"]);
    // NOT in the demigratable viaHub bucket.
    expect(g.viaHub.map(e => e.name)).toEqual(["memory"]);
    expect(g.viaHub.map(e => e.name)).not.toContain("time");
    // Not leaked into any other bucket.
    expect(g.canMigrate.length + g.unknown.length + g.external.length + g.perSession.length + g.dismissed.length).toBe(0);
  });

  it("drops entries with status='not-installed' (no client has them)", () => {
    const scan: ScanResult = {
      at: "2026-04-23T00:00:00Z",
      entries: [
        {name: "ghost", status: "not-installed", client_presence: {}},
        {name: "real", status: "via-hub", client_presence: {"claude-code": {transport: "http", endpoint: "http://localhost:9200/mcp"}}},
      ],
    };
    const g = groupMigrationEntries(scan, new Set());
    expect(g.viaHub.map(e => e.name)).toEqual(["real"]);
    expect(g.canMigrate.length + g.unknown.length + g.external.length + g.perSession.length).toBe(0);
  });

  it("surfaces status='external' in the external bucket (real remote MCP, not dropped)", () => {
    const scan: ScanResult = {
      at: "2026-04-23T00:00:00Z",
      entries: [
        {name: "context7", status: "external", managed: false, client_presence: {"claude-code": {transport: "http", endpoint: "https://mcp.context7.com/mcp"}}},
        {name: "qt-docs", status: "external", managed: false, client_presence: {"cursor": {transport: "http", endpoint: "https://qt.io/mcp"}}},
        {name: "memory", status: "via-hub", managed: true, client_presence: {"claude-code": {transport: "http", endpoint: "http://localhost:9200/mcp"}}},
      ],
    };
    const g = groupMigrationEntries(scan, new Set());
    expect(g.external.map(e => e.name)).toEqual(["context7", "qt-docs"]);
    // Managed flag rides through on the entry for badge rendering.
    expect(g.viaHub[0].managed).toBe(true);
    expect(g.external[0].managed).toBe(false);
  });

  it("dismiss-filters external like unknown, parking both in the dismissed bucket", () => {
    const scan: ScanResult = {
      at: "2026-04-23T00:00:00Z",
      entries: [
        {name: "ext-noisy", status: "external", client_presence: {"claude-code": {transport: "http", endpoint: "https://noisy.example.com/mcp"}}},
        {name: "ext-kept", status: "external", client_presence: {"claude-code": {transport: "http", endpoint: "https://kept.example.com/mcp"}}},
        {name: "stdio-dismissed", status: "unknown", client_presence: {"codex-cli": {transport: "stdio", endpoint: "uvx"}}},
      ],
    };
    const dismissed = new Set<string>(["ext-noisy", "stdio-dismissed"]);
    const g = groupMigrationEntries(scan, dismissed);
    expect(g.external.map(e => e.name)).toEqual(["ext-kept"]);
    expect(g.unknown.map(e => e.name)).toEqual([]);
    // Dismissed entries are NOT lost — they go to the collapsed section.
    expect(g.dismissed.map(e => e.name)).toEqual(["ext-noisy", "stdio-dismissed"]);
  });

  it("drops entries without a status (defensive: malformed backend response)", () => {
    const scan: ScanResult = {
      at: "2026-04-23T00:00:00Z",
      entries: [
        {name: "no-status", client_presence: {}},
        {name: "real", status: "via-hub", client_presence: {}},
      ],
    };
    const g = groupMigrationEntries(scan, new Set());
    expect(g.viaHub.map(e => e.name)).toEqual(["real"]);
  });

  it("sorts each group alphabetically by name for stable UI order", () => {
    const scan: ScanResult = {
      at: "2026-04-23T00:00:00Z",
      entries: [
        {name: "z", status: "can-migrate", client_presence: {}},
        {name: "a", status: "can-migrate", client_presence: {}},
        {name: "m", status: "can-migrate", client_presence: {}},
      ],
    };
    const g = groupMigrationEntries(scan, new Set());
    expect(g.canMigrate.map(e => e.name)).toEqual(["a", "m", "z"]);
  });

  it("filters dismissedUnknown from the unknown group ONLY, not other groups", () => {
    const scan: ScanResult = {
      at: "2026-04-23T00:00:00Z",
      entries: [
        {name: "fetch", status: "unknown", client_presence: {"claude-code": {transport: "stdio"}}},
        {name: "also-dismissed", status: "can-migrate", client_presence: {}},
        {name: "kept", status: "unknown", client_presence: {}},
      ],
    };
    const dismissed = new Set<string>(["fetch", "also-dismissed"]);
    const g = groupMigrationEntries(scan, dismissed);
    // Dismissed unknown is hidden.
    expect(g.unknown.map(e => e.name)).toEqual(["kept"]);
    // A name in the dismissed set but classified as can-migrate must
    // NOT be filtered — dismissal is unknown-group only.
    expect(g.canMigrate.map(e => e.name)).toEqual(["also-dismissed"]);
  });

  it("handles a scan with null/undefined entries (fresh hub, no configs)", () => {
    const g1 = groupMigrationEntries({at: "x", entries: null}, new Set());
    expect(g1.viaHub).toEqual([]);
    expect(g1.canMigrate).toEqual([]);
    expect(g1.unknown).toEqual([]);
    expect(g1.perSession).toEqual([]);

    const g2 = groupMigrationEntries({at: "x", entries: []}, new Set());
    expect(g2.viaHub).toEqual([]);
    expect(g2.canMigrate).toEqual([]);
    expect(g2.unknown).toEqual([]);
    expect(g2.perSession).toEqual([]);
  });
});
