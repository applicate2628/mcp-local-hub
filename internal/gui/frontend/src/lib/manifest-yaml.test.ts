import { describe, it, expect } from "vitest";
import { toYAML, parseYAMLToForm, BLANK_FORM } from "./manifest-yaml";
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

describe("BLANK_FORM constant", () => {
  it("has all fields with sensible defaults", () => {
    expect(BLANK_FORM.name).toBe("");
    expect(BLANK_FORM.kind).toBe("global");
    expect(BLANK_FORM.transport).toBe("stdio-bridge");
    expect(BLANK_FORM.command).toBe("");
    expect(BLANK_FORM.base_args).toEqual([]);
    expect(BLANK_FORM.env).toEqual([]);
    expect(BLANK_FORM.daemons).toEqual([]);
    expect(BLANK_FORM.client_bindings).toEqual([]);
    expect(BLANK_FORM.weekly_refresh).toBe(false);
  });
});

describe("parseYAMLToForm", () => {
  it("parses a complete manifest (memory example) round-trip-cleanly", () => {
    const yaml = `name: memory
kind: global
transport: stdio-bridge
command: npx
base_args:
  - "-y"
  - "@modelcontextprotocol/server-memory"
env:
  MEMORY_FILE_PATH: "\${HOME}/.local/share/mcp-memory/memory.jsonl"
daemons:
  - name: default
    port: 9123
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
  - client: codex-cli
    daemon: default
    url_path: /mcp
`;
    const form = parseYAMLToForm(yaml);
    expect(form.name).toBe("memory");
    expect(form.kind).toBe("global");
    expect(form.transport).toBe("stdio-bridge");
    expect(form.command).toBe("npx");
    expect(form.base_args).toEqual(["-y", "@modelcontextprotocol/server-memory"]);
    expect(form.env).toEqual([
      { key: "MEMORY_FILE_PATH", value: "${HOME}/.local/share/mcp-memory/memory.jsonl" },
    ]);
    expect(form.daemons).toEqual([{ name: "default", port: 9123 }]);
    expect(form.client_bindings).toHaveLength(2);
    expect(form.client_bindings[0]).toEqual({ client: "claude-code", daemon: "default", url_path: "/mcp" });
  });

  it("treats missing kind as 'global' (default)", () => {
    const form = parseYAMLToForm(`name: demo\ntransport: stdio-bridge\ncommand: npx\n`);
    expect(form.kind).toBe("global");
  });

  it("treats missing transport as 'stdio-bridge' (default)", () => {
    const form = parseYAMLToForm(`name: demo\nkind: global\ncommand: npx\n`);
    expect(form.transport).toBe("stdio-bridge");
  });

  it("coerces missing arrays to []", () => {
    const form = parseYAMLToForm(`name: demo\nkind: global\ntransport: stdio-bridge\ncommand: npx\n`);
    expect(form.base_args).toEqual([]);
    expect(form.daemons).toEqual([]);
    expect(form.client_bindings).toEqual([]);
  });

  it("coerces missing env map to empty array", () => {
    const form = parseYAMLToForm(`name: demo\ncommand: npx\n`);
    expect(form.env).toEqual([]);
  });

  it("coerces missing weekly_refresh to false", () => {
    const form = parseYAMLToForm(`name: demo\ncommand: npx\n`);
    expect(form.weekly_refresh).toBe(false);
  });

  it("normalizes env map into array-of-{key,value} pairs", () => {
    const form = parseYAMLToForm(`name: demo\ncommand: npx\nenv:\n  A: "1"\n  B: "two"\n`);
    expect(form.env).toEqual([
      { key: "A", value: "1" },
      { key: "B", value: "two" },
    ]);
  });

  it("throws on malformed YAML", () => {
    expect(() => parseYAMLToForm(`name: demo\n  this: is: nested: wrong`)).toThrow();
  });

  it("round-trips via toYAML without losing required fields", () => {
    const input: ManifestFormState = {
      ...base,
      name: "memory",
      command: "npx",
      base_args: ["-y", "@pkg/srv"],
      env: [{ key: "K", value: "v" }],
      daemons: [{ name: "default", port: 9100 }],
      client_bindings: [{ client: "claude-code", daemon: "default", url_path: "/mcp" }],
    };
    const yaml = toYAML(input);
    const parsed = parseYAMLToForm(yaml);
    expect(parsed).toEqual(input);
  });

  it("BLANK_FORM round-trips to minimal YAML and back to BLANK_FORM shape", () => {
    const yaml = toYAML(BLANK_FORM);
    const parsed = parseYAMLToForm(yaml);
    expect(parsed).toEqual(BLANK_FORM);
  });
});
