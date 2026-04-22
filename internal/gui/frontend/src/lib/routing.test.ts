import { describe, expect, it } from "vitest";
import { isHubLoopback, perClientRouting, collectServers } from "./routing";
import type { ScanResult } from "../types";

describe("isHubLoopback", () => {
  it("accepts 127.0.0.1 URLs", () => {
    expect(isHubLoopback("http://127.0.0.1:9123/mcp")).toBe(true);
  });
  it("accepts localhost URLs", () => {
    expect(isHubLoopback("http://localhost:9123/mcp")).toBe(true);
  });
  it("accepts [::1] URLs", () => {
    expect(isHubLoopback("http://[::1]:9123/mcp")).toBe(true);
  });
  it("rejects subdomain-as-path spoofs like 127.0.0.1.evil.com", () => {
    expect(isHubLoopback("https://127.0.0.1.evil.com/foo")).toBe(false);
  });
  it("rejects query-param spoofs like ?host=127.0.0.1", () => {
    expect(isHubLoopback("https://example.com/?host=127.0.0.1")).toBe(false);
  });
  it("rejects non-http schemes like stdio://", () => {
    expect(isHubLoopback("stdio:///memory")).toBe(false);
  });
  it("rejects empty string", () => {
    expect(isHubLoopback("")).toBe(false);
  });
});

describe("perClientRouting", () => {
  it("tags hub loopback http as via-hub", () => {
    const r = perClientRouting({
      "claude-code": { transport: "http", endpoint: "http://127.0.0.1:9100/mcp" },
    });
    expect(r["claude-code"]).toBe("via-hub");
  });
  it("tags relay transport as via-hub", () => {
    const r = perClientRouting({ "codex-cli": { transport: "relay" } });
    expect(r["codex-cli"]).toBe("via-hub");
  });
  it("tags remote http as direct", () => {
    const r = perClientRouting({
      "gemini-cli": { transport: "http", endpoint: "https://example.com/mcp" },
    });
    expect(r["gemini-cli"]).toBe("direct");
  });
  it("tags stdio as direct", () => {
    const r = perClientRouting({ "antigravity": { transport: "stdio" } });
    expect(r["antigravity"]).toBe("direct");
  });
  it("tags absent transport as not-installed", () => {
    const r = perClientRouting({ "claude-code": { transport: "absent" } });
    expect(r["claude-code"]).toBe("not-installed");
  });
  it("tags missing transport as not-installed", () => {
    const r = perClientRouting({ "claude-code": {} });
    expect(r["claude-code"]).toBe("not-installed");
  });
});

describe("collectServers", () => {
  it("sorts by name ascending", () => {
    const scan: ScanResult = {
      at: "",
      entries: [
        { name: "zulu", client_presence: {} },
        { name: "alpha", client_presence: {} },
      ],
    };
    const out = collectServers(scan);
    expect(out.map((s) => s.name)).toEqual(["alpha", "zulu"]);
  });
  it("handles null entries gracefully", () => {
    const out = collectServers({ at: "", entries: null });
    expect(out).toEqual([]);
  });
  it("derives routing from client_presence", () => {
    const scan: ScanResult = {
      at: "",
      entries: [
        {
          name: "serena",
          client_presence: {
            "claude-code": { transport: "http", endpoint: "http://127.0.0.1:9100/mcp" },
          },
        },
      ],
    };
    const out = collectServers(scan);
    expect(out[0].routing["claude-code"]).toBe("via-hub");
  });
});
