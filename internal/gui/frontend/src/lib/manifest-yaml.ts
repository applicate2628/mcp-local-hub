import { parse as yamlParse } from "yaml";
import type {
  ManifestFormState,
  DaemonFormEntry,
  BindingFormEntry,
  LanguageFormEntry,
} from "../types";
import { generateUUID } from "./uuid";

// Known top-level keys — anything else lands in _preservedRaw.
const TOP_LEVEL_KNOWN = new Set([
  "name",
  "kind",
  "transport",
  "command",
  "base_args",
  "env",
  "daemons",
  "client_bindings",
  "weekly_refresh",
  "idle_timeout_min",
  "base_args_template",
  "languages",
  "port_pool",
]);

// Nested per-level known keys — used by hasNestedUnknown.
const DAEMON_KNOWN = new Set(["name", "port", "context", "extra_args"]);
const LANGUAGE_KNOWN = new Set([
  "name",
  "backend",
  "transport",
  "lsp_command",
  "extra_flags",
]);
// BINDING_KNOWN lists raw YAML wire-format keys (what hasNestedUnknown sees
// when it parses on-disk YAML), NOT form-state keys. parseYAMLToForm reads
// b.daemon (YAML) and converts it to daemonId (form state) at load time;
// toYAML does the inverse. Keep "daemon" here even though BindingFormEntry
// uses "daemonId" — this set describes the input side of the parse boundary.
const BINDING_KNOWN = new Set(["client", "daemon", "url_path"]);

// BLANK_FORM is the canonical empty ManifestFormState. Used by:
//   - AddServer.tsx fresh-create entry path (no URL params)
//   - parseYAMLToForm as the starting state that missing fields fall back to
// Keeping one named constant ensures AddServer's "clean form" and
// parseYAMLToForm's defaults do not drift apart.
export const BLANK_FORM: ManifestFormState = {
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

// quote serializes a user-supplied string as a YAML-safe scalar. We always
// quote user input because bare scalars have surprising semantics: `yes`,
// `true`, `null`, `1234`, values with leading whitespace, or names starting
// with `{` / `[` / `*` / `&` / `!` / `#` are all parsed as non-strings or
// YAML structure markers. Single-quoted form is the safer default because
// it interprets backslashes literally; we only fall back to double-quoted
// for values containing `'` or `\n` (neither of which single-quoted YAML
// can express without block-scalar ceremony), and in that case we use
// JSON.stringify which emits valid YAML double-quoted strings with proper
// newline escaping. Reviewer #R2b-Q1.
function quote(s: string): string {
  if (s.includes("'") || s.includes("\n")) {
    // JSON.stringify handles both cases with correct escape sequences;
    // YAML flow-style double-quoted scalars are a strict superset of JSON.
    return JSON.stringify(s);
  }
  return `'${s}'`;
}

function asString(v: unknown, fallback: string): string {
  return typeof v === "string" ? v : fallback;
}
function asNumber(v: unknown, fallback: number): number {
  return typeof v === "number" && Number.isFinite(v) ? v : fallback;
}
function asKind(v: unknown): "global" | "workspace-scoped" {
  return v === "workspace-scoped" ? "workspace-scoped" : "global";
}
function asTransport(v: unknown): "stdio-bridge" | "native-http" {
  return v === "native-http" ? "native-http" : "stdio-bridge";
}
function asStringArray(v: unknown): string[] {
  return Array.isArray(v) ? v.filter((x) => typeof x === "string") : [];
}

// parseYAMLToForm takes a YAML string (from backend extract-manifest, from
// a user's "Paste YAML" action, or from edit-mode in A2b) and normalizes
// it into a ManifestFormState. Missing optional fields are coerced to
// BLANK_FORM's defaults so the form always has a complete object to render.
// Throws on unparseable YAML.
//
// A2b extensions:
// - assigns a fresh _id (UUID) to each daemon and language entry.
// - re-keys client_bindings.daemon (name string) → daemonId (_id ref).
// - extracts unrecognized top-level keys into _preservedRaw so they
//   survive a round-trip through toYAML without data loss.
// - returns loadedHash: "" — the caller (Load path) sets this from the
//   /api/manifest/get response hash field.
export function parseYAMLToForm(yaml: string): ManifestFormState {
  const raw = yamlParse(yaml) as Record<string, unknown> | null;
  if (raw == null || typeof raw !== "object" || Array.isArray(raw)) {
    return { ...BLANK_FORM, _preservedRaw: {} };
  }

  // Extract _preservedRaw: any TOP-LEVEL key we don't recognize.
  const preserved: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(raw)) {
    if (!TOP_LEVEL_KNOWN.has(k)) preserved[k] = v;
  }

  // env map → array-of-{key,value}.
  const envRaw = raw.env;
  const env: Array<{ key: string; value: string }> =
    envRaw && typeof envRaw === "object" && !Array.isArray(envRaw)
      ? Object.entries(envRaw as Record<string, unknown>).map(([key, value]) => ({
          key,
          value: asString(value, ""),
        }))
      : [];

  // Daemons: assign fresh _id per row.
  const daemonsRaw = raw.daemons;
  const daemons: DaemonFormEntry[] = Array.isArray(daemonsRaw)
    ? daemonsRaw
        .filter((d): d is Record<string, unknown> => typeof d === "object" && d !== null)
        .map((d) => {
          const entry: DaemonFormEntry = {
            _id: generateUUID(),
            name: asString(d.name, ""),
            port: asNumber(d.port, 0),
          };
          if (typeof d.context === "string") entry.context = d.context;
          const extra = asStringArray(d.extra_args);
          if (extra.length > 0) entry.extra_args = extra;
          return entry;
        })
    : [];

  // Build a map: daemon.name → _id for re-keying client_bindings.
  const nameToId = new Map<string, string>();
  for (const d of daemons) nameToId.set(d.name, d._id);

  // client_bindings: daemon name → daemonId.
  const bindingsRaw = raw.client_bindings;
  const bindings: BindingFormEntry[] = Array.isArray(bindingsRaw)
    ? bindingsRaw
        .filter((b): b is Record<string, unknown> => typeof b === "object" && b !== null)
        .map((b) => ({
          client: asString(b.client, ""),
          daemonId: nameToId.get(asString(b.daemon, "")) ?? "",
          url_path: asString(b.url_path, ""),
        }))
    : [];

  // Advanced: idle_timeout_min, base_args_template, port_pool, languages.
  const langRaw = raw.languages;
  const languages: LanguageFormEntry[] | undefined = Array.isArray(langRaw)
    ? langRaw
        .filter((l): l is Record<string, unknown> => typeof l === "object" && l !== null)
        .map((l) => {
          const entry: LanguageFormEntry = {
            _id: generateUUID(),
            name: asString(l.name, ""),
            backend: asString(l.backend, ""),
          };
          const t = l.transport;
          if (t === "stdio" || t === "http_listen" || t === "native_http") entry.transport = t;
          if (typeof l.lsp_command === "string") entry.lsp_command = l.lsp_command;
          const flags = asStringArray(l.extra_flags);
          if (flags.length > 0) entry.extra_flags = flags;
          return entry;
        })
    : undefined;

  const pp = raw.port_pool;
  const port_pool =
    pp && typeof pp === "object" && !Array.isArray(pp)
      ? (() => {
          const r = pp as Record<string, unknown>;
          const start = asNumber(r.start, 0);
          const end = asNumber(r.end, 0);
          return start > 0 || end > 0 ? { start, end } : undefined;
        })()
      : undefined;

  const bat = asStringArray(raw.base_args_template);

  return {
    name: asString(raw.name, ""),
    kind: asKind(raw.kind),
    transport: asTransport(raw.transport),
    command: asString(raw.command, ""),
    base_args: asStringArray(raw.base_args),
    env,
    daemons,
    client_bindings: bindings,
    weekly_refresh: raw.weekly_refresh === true,
    idle_timeout_min: typeof raw.idle_timeout_min === "number" ? raw.idle_timeout_min : undefined,
    base_args_template: bat.length > 0 ? bat : undefined,
    languages,
    port_pool,
    loadedHash: "",
    _preservedRaw: preserved,
  };
}

// hasNestedUnknown parses the YAML and checks whether any entry inside
// daemons[], languages[], or client_bindings[] contains a field that is
// not in the per-level allowlist. Returns true if so — the caller uses
// this to switch AddServer into a read-only passthrough mode so that
// future-schema fields inside arrays are not silently dropped.
//
// Top-level unknown keys are NOT flagged here — they go to _preservedRaw
// and are round-tripped safely by toYAML.
export function hasNestedUnknown(yaml: string): boolean {
  let raw: unknown;
  try {
    raw = yamlParse(yaml);
  } catch {
    // Safe default: if the YAML is syntactically corrupt, treat the file as
    // opaque and force read-only mode. The alternative (return false) would
    // open edit mode on unparseable input, letting the user overwrite a
    // file they cannot structurally see. Reviewer #R2b-Q2.
    return true;
  }
  if (raw == null || typeof raw !== "object" || Array.isArray(raw)) return false;
  const r = raw as Record<string, unknown>;

  const daemons = r.daemons;
  if (Array.isArray(daemons)) {
    for (const d of daemons) {
      if (d && typeof d === "object" && !Array.isArray(d)) {
        for (const k of Object.keys(d as Record<string, unknown>)) {
          if (!DAEMON_KNOWN.has(k)) return true;
        }
      }
    }
  }

  const languages = r.languages;
  if (Array.isArray(languages)) {
    for (const l of languages) {
      if (l && typeof l === "object" && !Array.isArray(l)) {
        for (const k of Object.keys(l as Record<string, unknown>)) {
          if (!LANGUAGE_KNOWN.has(k)) return true;
        }
      }
    }
  }

  const bindings = r.client_bindings;
  if (Array.isArray(bindings)) {
    for (const b of bindings) {
      if (b && typeof b === "object" && !Array.isArray(b)) {
        for (const k of Object.keys(b as Record<string, unknown>)) {
          if (!BINDING_KNOWN.has(k)) return true;
        }
      }
    }
  }

  return false;
}

// toYAML serializes a ManifestFormState into a YAML string that
// api.ManifestValidate / api.ManifestCreate / api.ManifestEdit accept
// verbatim.
//
// A2b extensions:
// - resolves daemonId → daemon.name at serialize time.
// - drops client_bindings whose daemonId no longer exists in state.daemons
//   (orphan safety net for delete operations).
// - merges _preservedRaw top-level keys into the output so round-trips
//   through the GUI do not lose unknown fields.
// - kind-gates workspace-only sections: languages, port_pool, and
//   daemon.context are only emitted when kind === "workspace-scoped".
export function toYAML(state: ManifestFormState): string {
  const lines: string[] = [];
  // All user-supplied scalars go through quote() so names like "yes",
  // "true", "null", "1234", or values with leading whitespace round-trip
  // safely instead of being re-parsed as booleans / numbers / structure
  // markers. The two enum-style fields (kind, transport) + numeric fields
  // (port, idle_timeout_min, port_pool.{start,end}) are bare because their
  // domains are controlled (asKind/asTransport/asNumber on the inverse).
  lines.push(`name: ${quote(state.name)}`);
  lines.push(`kind: ${state.kind}`);
  lines.push(`transport: ${state.transport}`);
  lines.push(`command: ${quote(state.command)}`);
  if (state.base_args.length > 0) {
    lines.push(`base_args: [${state.base_args.map(quote).join(", ")}]`);
  }
  if (state.base_args_template && state.base_args_template.length > 0) {
    lines.push(`base_args_template: [${state.base_args_template.map(quote).join(", ")}]`);
  }
  if (state.env.length > 0) {
    lines.push("env:");
    for (const { key, value } of state.env) {
      lines.push(`  ${key}: ${quote(value)}`);
    }
  }
  if (state.daemons.length > 0) {
    lines.push("daemons:");
    for (const d of state.daemons) {
      lines.push(`  - name: ${quote(d.name)}`);
      lines.push(`    port: ${d.port}`);
      if (state.kind === "workspace-scoped") {
        if (typeof d.context === "string" && d.context.length > 0) {
          lines.push(`    context: ${quote(d.context)}`);
        }
      }
      if (d.extra_args && d.extra_args.length > 0) {
        lines.push(`    extra_args: [${d.extra_args.map(quote).join(", ")}]`);
      }
    }
  }
  // Resolve daemonId → daemon.name; drop bindings whose daemon was deleted.
  const idToName = new Map<string, string>();
  for (const d of state.daemons) idToName.set(d._id, d.name);
  const liveBindings = state.client_bindings.filter((b) => idToName.has(b.daemonId));
  if (liveBindings.length > 0) {
    lines.push("client_bindings:");
    for (const b of liveBindings) {
      lines.push(`  - client: ${quote(b.client)}`);
      lines.push(`    daemon: ${quote(idToName.get(b.daemonId)!)}`);
      lines.push(`    url_path: ${quote(b.url_path)}`);
    }
  }
  if (state.weekly_refresh) {
    lines.push("weekly_refresh: true");
  }
  // Workspace-gated Advanced.
  if (state.kind === "workspace-scoped") {
    if (state.languages && state.languages.length > 0) {
      lines.push("languages:");
      for (const l of state.languages) {
        lines.push(`  - name: ${quote(l.name)}`);
        lines.push(`    backend: ${quote(l.backend)}`);
        if (l.transport) lines.push(`    transport: ${l.transport}`);
        if (l.lsp_command) lines.push(`    lsp_command: ${quote(l.lsp_command)}`);
        if (l.extra_flags && l.extra_flags.length > 0) {
          lines.push(`    extra_flags: [${l.extra_flags.map(quote).join(", ")}]`);
        }
      }
    }
    if (state.port_pool) {
      lines.push("port_pool:");
      lines.push(`  start: ${state.port_pool.start}`);
      lines.push(`  end: ${state.port_pool.end}`);
    }
  }
  if (typeof state.idle_timeout_min === "number") {
    lines.push(`idle_timeout_min: ${state.idle_timeout_min}`);
  }
  // Merge top-level _preservedRaw using yaml library's stringify for
  // complex values. Simple scalars inlined.
  for (const [k, v] of Object.entries(state._preservedRaw)) {
    if (typeof v === "string") {
      lines.push(`${k}: ${quote(v)}`);
    } else if (typeof v === "number" || typeof v === "boolean") {
      lines.push(`${k}: ${v}`);
    } else {
      // Fallback: delegate to JSON (YAML accepts JSON-like inline). Not
      // pretty, but we don't know the shape. Users editing via GUI
      // won't touch these anyway.
      lines.push(`${k}: ${JSON.stringify(v)}`);
    }
  }
  return lines.join("\n") + "\n";
}
