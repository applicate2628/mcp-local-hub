import type { ManifestFormState } from "../types";
import { parse as yamlParse } from "yaml";

// toYAML serializes a ManifestFormState into a YAML string that
// api.ManifestValidate / api.ManifestCreate accept verbatim. The output
// follows the convention shown in servers/memory/manifest.yaml:
//
//   name: ...
//   kind: ...
//   transport: ...
//   command: ...
//   base_args: ["-y", ...]        # flow-style array
//   env:                          # only when non-empty
//     KEY: "value"                # always double-quoted
//   daemons:                      # only when non-empty
//     - name: ...
//       port: ...
//   client_bindings:              # only when non-empty
//     - client: ...
//       daemon: ...
//       url_path: ...
//   weekly_refresh: true          # only when true
//
// Values that contain a double-quote are rendered as single-quoted strings
// to avoid manual escape bookkeeping. Keys are known identifiers and are
// never quoted.
export function toYAML(state: ManifestFormState): string {
  const lines: string[] = [];
  lines.push(`name: ${state.name}`);
  lines.push(`kind: ${state.kind}`);
  lines.push(`transport: ${state.transport}`);
  lines.push(`command: ${state.command}`);
  if (state.base_args.length > 0) {
    const quoted = state.base_args.map((a) => quote(a)).join(", ");
    lines.push(`base_args: [${quoted}]`);
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
      lines.push(`  - name: ${d.name}`);
      lines.push(`    port: ${d.port}`);
    }
  }
  if (state.client_bindings.length > 0) {
    lines.push("client_bindings:");
    for (const b of state.client_bindings) {
      lines.push(`  - client: ${b.client}`);
      lines.push(`    daemon: ${b.daemon}`);
      lines.push(`    url_path: ${b.url_path}`);
    }
  }
  if (state.weekly_refresh) {
    lines.push("weekly_refresh: true");
  }
  return lines.join("\n") + "\n";
}

// quote picks the right YAML string wrapper:
//   - values containing a double-quote OR a backslash → single-quote
//     branch (YAML single-quoted strings treat backslashes literally,
//     which matches what a user typing "C:\\Users\\..." intends — no
//     \U hex-digit parse failure, no \n becoming a newline).
//   - otherwise → double-quote branch (handles most cases cleanly).
//   - empty strings render as `""`.
// Single-quote escape: `'` becomes `''` inside a single-quoted string.
function quote(s: string): string {
  if (s.includes(`"`) || s.includes("\\")) {
    return `'${s.replace(/'/g, `''`)}'`;
  }
  return `"${s}"`;
}

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
};

// parseYAMLToForm takes a YAML string (from backend extract-manifest, from
// a user's "Paste YAML" action, or — eventually — from edit-mode in A2b)
// and normalizes it into a ManifestFormState. Missing optional fields are
// coerced to BLANK_FORM's defaults so the form always has a complete
// object to render. Throws on unparseable YAML.
//
// Normalization is critical for the Q8 snapshot-dirty detection: a freshly
// parsed form must equal the baseline taken AFTER this same normalization,
// otherwise deepEqual reports false-dirty on first render. See design memo
// §3 gotcha 3.
export function parseYAMLToForm(yaml: string): ManifestFormState {
  const raw = yamlParse(yaml) as Record<string, unknown> | null;
  if (raw == null || typeof raw !== "object") {
    return { ...BLANK_FORM };
  }
  const asString = (v: unknown, fallback: string): string =>
    typeof v === "string" ? v : fallback;
  const asKind = (v: unknown): "global" | "workspace-scoped" =>
    v === "workspace-scoped" ? "workspace-scoped" : "global";
  const asTransport = (v: unknown): "stdio-bridge" | "native-http" =>
    v === "native-http" ? "native-http" : "stdio-bridge";
  const asStringArray = (v: unknown): string[] =>
    Array.isArray(v) ? v.filter((x) => typeof x === "string") : [];
  const envRaw = raw.env;
  const env: Array<{ key: string; value: string }> =
    envRaw && typeof envRaw === "object" && !Array.isArray(envRaw)
      ? Object.entries(envRaw as Record<string, unknown>).map(([key, value]) => ({
          key,
          value: asString(value, ""),
        }))
      : [];
  const daemonsRaw = raw.daemons;
  const daemons: Array<{ name: string; port: number }> = Array.isArray(daemonsRaw)
    ? daemonsRaw
        .filter((d): d is Record<string, unknown> => typeof d === "object" && d !== null)
        .map((d) => ({
          name: asString(d.name, ""),
          port: typeof d.port === "number" ? d.port : 0,
        }))
    : [];
  const bindingsRaw = raw.client_bindings;
  const bindings: Array<{ client: string; daemon: string; url_path: string }> = Array.isArray(
    bindingsRaw,
  )
    ? bindingsRaw
        .filter((b): b is Record<string, unknown> => typeof b === "object" && b !== null)
        .map((b) => ({
          client: asString(b.client, ""),
          daemon: asString(b.daemon, ""),
          url_path: asString(b.url_path, ""),
        }))
    : [];
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
  };
}
