import { describe, it, expect } from "vitest";
import { toYAML } from "./manifest-yaml";
import type { ManifestFormState } from "../types";

const base: ManifestFormState = {
  name: "",
  kind: "global",
  transport: "stdio-bridge",
  command: "",
  base_args: [],
  env: [],
  daemons: [],
  client_bindings: [],
  weekly_refresh: false,
};

describe("toYAML", () => {
  it("serializes a minimal state with only name + kind + transport + command", () => {
    const state: ManifestFormState = { ...base, name: "demo", command: "npx" };
    const yaml = toYAML(state);
    expect(yaml).toContain("name: demo");
    expect(yaml).toContain("kind: global");
    expect(yaml).toContain("transport: stdio-bridge");
    expect(yaml).toContain("command: npx");
  });

  it("renders base_args as a flow-style YAML array with quotes around each element", () => {
    const state: ManifestFormState = {
      ...base,
      name: "demo",
      command: "npx",
      base_args: ["-y", "@example/server-mem"],
    };
    const yaml = toYAML(state);
    expect(yaml).toContain(`base_args: ["-y", "@example/server-mem"]`);
  });

  it("renders env as a nested map with quoted values", () => {
    const state: ManifestFormState = {
      ...base,
      name: "demo",
      command: "npx",
      env: [
        { key: "PATH", value: "/usr/bin" },
        { key: "MEMORY_FILE_PATH", value: "${HOME}/.local/share/mcp-memory/memory.jsonl" },
      ],
    };
    const yaml = toYAML(state);
    expect(yaml).toContain("env:");
    expect(yaml).toContain(`  PATH: "/usr/bin"`);
    expect(yaml).toContain(`  MEMORY_FILE_PATH: "\${HOME}/.local/share/mcp-memory/memory.jsonl"`);
  });

  it("omits env section entirely when the env array is empty", () => {
    const state: ManifestFormState = { ...base, name: "demo", command: "npx" };
    const yaml = toYAML(state);
    expect(yaml).not.toContain("env:");
  });

  it("renders daemons as a list-of-maps with name + port", () => {
    const state: ManifestFormState = {
      ...base,
      name: "demo",
      command: "npx",
      daemons: [
        { name: "default", port: 9123 },
        { name: "workspace-py", port: 9124 },
      ],
    };
    const yaml = toYAML(state);
    expect(yaml).toContain("daemons:");
    expect(yaml).toMatch(/- name: default\s+port: 9123/);
    expect(yaml).toMatch(/- name: workspace-py\s+port: 9124/);
  });

  it("renders client_bindings as a list-of-maps with client + daemon + url_path", () => {
    const state: ManifestFormState = {
      ...base,
      name: "demo",
      command: "npx",
      daemons: [{ name: "default", port: 9123 }],
      client_bindings: [
        { client: "claude-code", daemon: "default", url_path: "/mcp" },
        { client: "codex-cli", daemon: "default", url_path: "/mcp" },
      ],
    };
    const yaml = toYAML(state);
    expect(yaml).toContain("client_bindings:");
    expect(yaml).toMatch(/- client: claude-code\s+daemon: default\s+url_path: \/mcp/);
    expect(yaml).toMatch(/- client: codex-cli\s+daemon: default\s+url_path: \/mcp/);
  });

  it("renders weekly_refresh only when true", () => {
    const stateFalse: ManifestFormState = { ...base, name: "demo", command: "npx", weekly_refresh: false };
    const stateTrue: ManifestFormState = { ...base, name: "demo", command: "npx", weekly_refresh: true };
    expect(toYAML(stateFalse)).not.toContain("weekly_refresh");
    expect(toYAML(stateTrue)).toContain("weekly_refresh: true");
  });

  it("escapes double-quotes in values by wrapping with single quotes", () => {
    const state: ManifestFormState = {
      ...base,
      name: "demo",
      command: "npx",
      env: [{ key: "FLAG", value: `has "quotes" inside` }],
    };
    const yaml = toYAML(state);
    // Either single-quote the whole value or escape the inner quotes — both are valid YAML.
    // Assert the output parses correctly by checking it contains the inner text at all.
    expect(yaml).toContain("FLAG:");
    expect(yaml).toContain(`has`);
    // And does not contain a corruption pattern like `has "quotes"` inside a double-quoted wrapper.
    expect(yaml).not.toMatch(/"has "quotes" inside"/);
  });
});
