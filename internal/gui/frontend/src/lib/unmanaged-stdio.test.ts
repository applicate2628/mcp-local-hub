import { describe, expect, it } from "vitest";
import { unmanagedStdioCount } from "./unmanaged-stdio";
import type { ScanEntry } from "../types";

describe("unmanagedStdioCount", () => {
  const cases: Array<{ name: string; entry: ScanEntry; want: number }> = [
    {
      name: "unknown stdio is unmanaged drift",
      entry: {
        name: "local-stdio",
        status: "unknown",
        client_presence: { "claude-code": { transport: "stdio", endpoint: "npx" } },
      },
      want: 1,
    },
    {
      name: "unknown with any stdio presence is unmanaged drift",
      entry: {
        name: "mixed",
        status: "unknown",
        client_presence: {
          "claude-code": { transport: "http", endpoint: "https://example.test/mcp" },
          "codex-cli": { transport: "stdio", endpoint: "uvx" },
        },
      },
      want: 1,
    },
    {
      name: "disabled unknown stdio is not unmanaged drift",
      entry: {
        name: "parked",
        status: "unknown",
        client_presence: { "codex-cli": { transport: "stdio", endpoint: "npx", disabled: true } },
      },
      want: 0,
    },
    {
      name: "enabled unknown stdio remains unmanaged drift",
      entry: {
        name: "active",
        status: "unknown",
        client_presence: { "codex-cli": { transport: "stdio", endpoint: "npx", disabled: false } },
      },
      want: 1,
    },
    {
      name: "via hub is managed",
      entry: {
        name: "memory",
        status: "via-hub",
        client_presence: { "claude-code": { transport: "http", endpoint: "http://127.0.0.1:9123/mcp" } },
      },
      want: 0,
    },
    {
      name: "can migrate is not unmanaged drift",
      entry: {
        name: "fetch",
        status: "can-migrate",
        client_presence: { "claude-code": { transport: "stdio", endpoint: "npx" } },
      },
      want: 0,
    },
    {
      name: "external http is not unmanaged stdio",
      entry: {
        name: "context7",
        status: "external",
        client_presence: { "claude-code": { transport: "http", endpoint: "https://mcp.context7.com/mcp" } },
      },
      want: 0,
    },
    {
      name: "unknown http only is not unmanaged stdio",
      entry: {
        name: "odd-remote",
        status: "unknown",
        client_presence: { "claude-code": { transport: "http", endpoint: "https://example.test/mcp" } },
      },
      want: 0,
    },
    {
      name: "not installed is not client drift",
      entry: { name: "manifest-only", status: "not-installed", client_presence: {} },
      want: 0,
    },
  ];

  for (const tc of cases) {
    it(tc.name, () => {
      expect(unmanagedStdioCount([tc.entry])).toBe(tc.want);
    });
  }
});
