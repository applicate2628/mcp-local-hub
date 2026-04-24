import type { ManifestFormState } from "../types";

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

// quote picks the right YAML string wrapper. If the value contains a
// double-quote, use single quotes (YAML escapes `'` as `''` inside single
// quotes). Otherwise use double quotes — they handle backslash-n etc.
// consistently. Empty strings render as `""`.
function quote(s: string): string {
  if (s.includes(`"`)) {
    return `'${s.replace(/'/g, `''`)}'`;
  }
  return `"${s}"`;
}
