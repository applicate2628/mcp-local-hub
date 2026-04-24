import { describe, it, expect } from "vitest";
import { toYAML, parseYAMLToForm, BLANK_FORM, hasNestedUnknown } from "./manifest-yaml";
import type { ManifestFormState } from "../types";

// A2b base: must include loadedHash + _preservedRaw to satisfy the new
// ManifestFormState shape. Daemons and bindings use A2b shapes (_id / daemonId).
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
  loadedHash: "",
  _preservedRaw: {},
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
        { _id: "id-1", name: "default", port: 9123 },
        { _id: "id-2", name: "workspace-py", port: 9124 },
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
      daemons: [{ _id: "id-1", name: "default", port: 9123 }],
      client_bindings: [
        { client: "claude-code", daemonId: "id-1", url_path: "/mcp" },
        { client: "codex-cli", daemonId: "id-1", url_path: "/mcp" },
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

  it("wraps Windows-path values (backslashes) in single quotes to avoid YAML escapes (PR #5 Codex R1)", () => {
    const state: ManifestFormState = {
      ...base,
      name: "demo",
      command: "npx",
      env: [{ key: "PATH", value: "C:\\Users\\dima_\\.local\\bin" }],
    };
    const yaml = toYAML(state);
    // Single-quoted branch keeps the backslashes literal.
    expect(yaml).toContain(`PATH: 'C:\\Users\\dima_\\.local\\bin'`);
    // Must NOT double-quote — that would let YAML interpret \U as a hex escape.
    expect(yaml).not.toMatch(/PATH: "C:\\\\Users/);
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
    expect(BLANK_FORM.loadedHash).toBe("");
    expect(BLANK_FORM._preservedRaw).toEqual({});
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
    // A2b: daemons have _id
    expect(form.daemons).toHaveLength(1);
    expect(form.daemons[0].name).toBe("default");
    expect(form.daemons[0].port).toBe(9123);
    expect(form.daemons[0]._id).toBeDefined();
    // A2b: client_bindings use daemonId
    expect(form.client_bindings).toHaveLength(2);
    expect(form.client_bindings[0].client).toBe("claude-code");
    expect(form.client_bindings[0].daemonId).toBe(form.daemons[0]._id);
    expect(form.client_bindings[0].url_path).toBe("/mcp");
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
    const form = parseYAMLToForm(`name: memory
kind: global
transport: stdio-bridge
command: npx
base_args: ["-y", "@pkg/srv"]
env:
  K: "v"
daemons:
  - name: default
    port: 9100
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`);
    const yaml = toYAML(form);
    const parsed2 = parseYAMLToForm(yaml);
    // Core fields must survive the round-trip.
    expect(parsed2.name).toBe(form.name);
    expect(parsed2.kind).toBe(form.kind);
    expect(parsed2.command).toBe(form.command);
    expect(parsed2.base_args).toEqual(form.base_args);
    expect(parsed2.env).toEqual(form.env);
    expect(parsed2.daemons[0].name).toBe("default");
    expect(parsed2.daemons[0].port).toBe(9100);
    expect(parsed2.client_bindings[0].client).toBe("claude-code");
    expect(parsed2.client_bindings[0].url_path).toBe("/mcp");
    // daemonId re-keyed on each parse — just verify it links to the new daemon.
    expect(parsed2.client_bindings[0].daemonId).toBe(parsed2.daemons[0]._id);
  });

  it("BLANK_FORM round-trips to minimal YAML and back to BLANK_FORM shape", () => {
    const yaml = toYAML(BLANK_FORM);
    const parsed = parseYAMLToForm(yaml);
    // Core fields must match.
    expect(parsed.name).toBe(BLANK_FORM.name);
    expect(parsed.kind).toBe(BLANK_FORM.kind);
    expect(parsed.transport).toBe(BLANK_FORM.transport);
    expect(parsed.command).toBe(BLANK_FORM.command);
    expect(parsed.base_args).toEqual(BLANK_FORM.base_args);
    expect(parsed.env).toEqual(BLANK_FORM.env);
    expect(parsed.daemons).toEqual(BLANK_FORM.daemons);
    expect(parsed.client_bindings).toEqual(BLANK_FORM.client_bindings);
    expect(parsed.weekly_refresh).toBe(BLANK_FORM.weekly_refresh);
    expect(parsed.loadedHash).toBe("");
    expect(parsed._preservedRaw).toEqual({});
  });
});

describe("parseYAMLToForm A2b extensions", () => {
  it("assigns a unique _id to each daemon", () => {
    const form = parseYAMLToForm(`name: demo
command: npx
daemons:
  - name: a
    port: 9100
  - name: b
    port: 9101
`);
    expect(form.daemons[0]._id).toBeDefined();
    expect(form.daemons[1]._id).toBeDefined();
    expect(form.daemons[0]._id).not.toBe(form.daemons[1]._id);
  });

  it("re-keys client_bindings to the freshly-generated daemonId", () => {
    const form = parseYAMLToForm(`name: demo
command: npx
daemons:
  - name: default
    port: 9100
client_bindings:
  - client: claude-code
    daemon: default
    url_path: /mcp
`);
    const defaultId = form.daemons[0]._id;
    expect(form.client_bindings[0].daemonId).toBe(defaultId);
    expect(form.client_bindings[0].client).toBe("claude-code");
    expect(form.client_bindings[0].url_path).toBe("/mcp");
  });

  it("extracts unknown top-level YAML keys into _preservedRaw", () => {
    const form = parseYAMLToForm(`name: demo
command: npx
some_future_field: "hi"
another_ref: 42
`);
    expect(form._preservedRaw).toEqual({
      some_future_field: "hi",
      another_ref: 42,
    });
  });

  it("leaves _preservedRaw empty for a fully-recognized manifest", () => {
    const form = parseYAMLToForm(`name: demo
kind: global
transport: stdio-bridge
command: npx
`);
    expect(form._preservedRaw).toEqual({});
  });

  it("sets loadedHash to empty string (caller sets it on Load)", () => {
    const form = parseYAMLToForm(`name: demo\ncommand: npx\n`);
    expect(form.loadedHash).toBe("");
  });

  it("parses A2b Advanced fields when present", () => {
    const form = parseYAMLToForm(`name: demo
kind: workspace-scoped
transport: stdio-bridge
command: ws
idle_timeout_min: 15
base_args_template: ["--lang=$LANG"]
port_pool:
  start: 9200
  end: 9220
languages:
  - name: python
    backend: mcp-language-server
    transport: stdio
    lsp_command: pyright-langserver
    extra_flags: ["--stdio"]
`);
    expect(form.idle_timeout_min).toBe(15);
    expect(form.base_args_template).toEqual(["--lang=$LANG"]);
    expect(form.port_pool).toEqual({ start: 9200, end: 9220 });
    expect(form.languages).toHaveLength(1);
    expect(form.languages![0]._id).toBeDefined();
    expect(form.languages![0].name).toBe("python");
    expect(form.languages![0].backend).toBe("mcp-language-server");
  });
});

describe("hasNestedUnknown", () => {
  it("returns false for a vanilla global manifest", () => {
    const yaml = `name: demo
kind: global
transport: stdio-bridge
command: npx
daemons:
  - name: a
    port: 9100
`;
    expect(hasNestedUnknown(yaml)).toBe(false);
  });

  it("returns true when a daemon has an unknown field", () => {
    const yaml = `name: demo
command: npx
daemons:
  - name: a
    port: 9100
    extra_config:
      foo: 1
`;
    expect(hasNestedUnknown(yaml)).toBe(true);
  });

  it("returns true when a language has an unknown field", () => {
    const yaml = `name: demo
kind: workspace-scoped
transport: stdio-bridge
command: ws
languages:
  - name: python
    backend: mcp-language-server
    future_feature: 42
`;
    expect(hasNestedUnknown(yaml)).toBe(true);
  });

  it("returns true when a client_binding has an unknown field", () => {
    const yaml = `name: demo
command: npx
daemons:
  - name: a
    port: 9100
client_bindings:
  - client: claude-code
    daemon: a
    url_path: /mcp
    priority: high
`;
    expect(hasNestedUnknown(yaml)).toBe(true);
  });

  it("returns false for top-level unknown keys (those go to _preservedRaw)", () => {
    const yaml = `name: demo
command: npx
future_top_level_field: ok
`;
    expect(hasNestedUnknown(yaml)).toBe(false);
  });
});

describe("toYAML A2b extensions", () => {
  it("resolves daemonId back to daemon.name at serialize time", () => {
    const form: ManifestFormState = {
      ...BLANK_FORM,
      name: "demo",
      command: "npx",
      daemons: [{ _id: "uuid-1", name: "only", port: 9100 }],
      client_bindings: [{ client: "claude-code", daemonId: "uuid-1", url_path: "/mcp" }],
    };
    const yaml = toYAML(form);
    expect(yaml).toMatch(/- client: claude-code\s+daemon: only\s+url_path: \/mcp/);
  });

  it("drops client_bindings whose daemonId no longer exists (safety net)", () => {
    const form: ManifestFormState = {
      ...BLANK_FORM,
      name: "demo",
      command: "npx",
      daemons: [{ _id: "uuid-live", name: "live", port: 9100 }],
      client_bindings: [
        { client: "claude-code", daemonId: "uuid-live", url_path: "/mcp" },
        { client: "codex-cli", daemonId: "uuid-deleted", url_path: "/mcp" },
      ],
    };
    const yaml = toYAML(form);
    expect(yaml).toContain("client: claude-code");
    expect(yaml).not.toContain("client: codex-cli");
  });

  it("merges _preservedRaw top-level keys into the output", () => {
    const form: ManifestFormState = {
      ...BLANK_FORM,
      name: "demo",
      command: "npx",
      _preservedRaw: {
        custom_annotation: "hello",
        ext_config: { a: 1 },
      },
    };
    const yaml = toYAML(form);
    expect(yaml).toContain("custom_annotation:");
    expect(yaml).toContain("ext_config:");
  });

  it("emits Advanced fields only when non-empty and kind gates workspace-only ones", () => {
    const global: ManifestFormState = {
      ...BLANK_FORM,
      name: "demo",
      command: "npx",
      kind: "global",
      idle_timeout_min: 15,
      languages: [{ _id: "u", name: "python", backend: "mcp-language-server" }],
      port_pool: { start: 9200, end: 9220 },
    };
    const y1 = toYAML(global);
    expect(y1).toContain("idle_timeout_min: 15");
    // kind-gated fields dropped when kind !== workspace-scoped.
    expect(y1).not.toContain("languages:");
    expect(y1).not.toContain("port_pool:");

    const workspace: ManifestFormState = { ...global, kind: "workspace-scoped" };
    const y2 = toYAML(workspace);
    expect(y2).toContain("languages:");
    expect(y2).toContain("port_pool:");
  });
});
