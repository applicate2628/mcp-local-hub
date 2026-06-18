// lib/groups-draft.ts — pure draft/dirty/validation logic for the Groups
// authoring screen (groups Phase 5b-2). Mirrors the draft/dirty discipline
// in SectionClients (lib-extracted so it is unit-testable without rendering).
//
// A "group" is a named subset of MCP servers exposed at /g/<group>/mcp with
// optional per-server hidden tools (decision
// work-items/decisions/2026-06-18-groups-namespaces-tool-visibility.md). The
// editor draft is the in-progress form state; this module owns the rules for:
//   - name validation (mirrors the Go ValidateGroupName: non-empty, no ':',
//     matching the route-segment allowlist ^[A-Za-z0-9._-]+$, and not "."/".."),
//     so the operator gets the same verdict before the POST round-trip;
//   - hidden-tool parsing (a per-server comma/whitespace-separated tag input
//     → a deduped string[] the wire carries as tools_hidden[server]);
//   - the SaveGroupBody projection (selected servers + non-empty hidden maps);
//   - dirty detection (draft vs the persisted group, order-independent), the
//     predicate the screen and the nav-away guard both consume;
//   - mapping a backend GroupsApiError code → the offending field, so the
//     screen renders a precise inline error.

import type { GroupDTO, SaveGroupBody } from "../api";

// GroupDraft is the editor's in-progress form state. `servers` is the SET of
// selected member servers (a record so a checkbox toggle is O(1)); `hidden`
// is the raw per-server tag-input text keyed by server name (parsed to a
// deduped list only at projection time, so the operator can type freely).
export interface GroupDraft {
  // The group name being authored. For an edit this starts as the existing
  // name; the editor locks it (rename = delete + create, per the decision)
  // but the draft still carries it so validation + projection are uniform.
  name: string;
  description: string;
  // selected[server] === true → the server is a member of the group.
  selected: Record<string, boolean>;
  // hiddenText[server] is the raw tag-input string for that server's hidden
  // tools (comma / whitespace separated). Only meaningful for selected
  // servers; entries for unselected servers are ignored at projection time.
  hiddenText: Record<string, string>;
}

// GROUP_NAME_SEPARATOR mirrors groupNameSeparator in internal/api/hub_mcp_groups.go.
// A group name MUST NOT contain it (it namespaces the scope key as
// "g:<name>"), so a name carrying it could forge a kind prefix.
export const GROUP_NAME_SEPARATOR = ":";

// GROUP_NAME_ALLOWED mirrors groupNameAllowed in internal/api/hub_mcp_groups.go:
// the ALLOWLIST a group name must fully match to be a safe single URL path
// segment in the `/g/<group>/mcp` route. It is an allowlist, NOT a denylist, on
// purpose: a denylist of "route-unsafe" characters is unclosable — it kept
// missing '#' (a URL fragment), '%' (percent-encoding), and other separators
// http.ServeMux normalizes. The allowlist closes the WHOLE class by admitting
// only ASCII letters, digits, '.', '_', and '-'. (The exact names "." and ".."
// match this charset but are path-traversal segments rejected separately.)
const GROUP_NAME_ALLOWED = /^[A-Za-z0-9._-]+$/;
// GROUP_NAME_FIRST_BAD finds the FIRST character outside the allowlist, for the
// error message.
const GROUP_NAME_FIRST_BAD = /[^A-Za-z0-9._-]/;
// MAX_GROUP_NAME_LEN mirrors maxGroupNameLen in internal/api/hub_mcp_groups.go:
// a group name is one URL path segment + one scope-key suffix; 64 chars is a
// generous sanity cap. The allowlist restricts to single-byte ASCII, so a JS
// string length matches the Go byte length.
const MAX_GROUP_NAME_LEN = 64;

// validateGroupName mirrors the Go validateGroupName: non-empty (after trim),
// free of the reserved ':' separator, matching the route-segment allowlist
// (ASCII letters, digits, '.', '_', '-'), and not the path-traversal segments
// "." or "..". Returns null when valid, else a human-readable message. The
// screen calls this on every keystroke so the operator sees the verdict before
// Save (the backend re-validates and is the source of truth —
// GROUPS_INVALID_NAME).
export function validateGroupName(name: string): string | null {
  const trimmed = name.trim();
  if (trimmed === "") return "Group name is required.";
  if (trimmed.includes(GROUP_NAME_SEPARATOR)) {
    return `Group name cannot contain "${GROUP_NAME_SEPARATOR}" (it is reserved for the scope-key namespace).`;
  }
  if (!GROUP_NAME_ALLOWED.test(trimmed)) {
    const bad = trimmed.match(GROUP_NAME_FIRST_BAD);
    return `Group name cannot contain "${bad?.[0] ?? trimmed}" (a group name may contain only ASCII letters, digits, ".", "_", and "-"; it must be reachable as the /g/<name>/mcp route segment).`;
  }
  if (trimmed === "." || trimmed === "..") {
    return `Group name "${trimmed}" is a path-traversal segment (a name of "." or ".." is rewritten by the route mux and could never reach the /g/<name>/mcp route).`;
  }
  if (trimmed.length > MAX_GROUP_NAME_LEN) {
    return `Group name is too long (${trimmed.length} characters; the maximum is ${MAX_GROUP_NAME_LEN}).`;
  }
  return null;
}

// parseHiddenTools turns a raw tag-input string into a deduped, order-
// preserving list of raw tool names. Splits on commas and any whitespace,
// trims, drops empties + duplicates. "definition, references  references"
// → ["definition", "references"]. Pure — no validation of whether the tool
// exists (the operator hides by raw name; an absent tool is a harmless no-op
// per the merge-step filter).
export function parseHiddenTools(raw: string): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const tok of raw.split(/[\s,]+/)) {
    const t = tok.trim();
    if (t === "" || seen.has(t)) continue;
    seen.add(t);
    out.push(t);
  }
  return out;
}

// selectedServers returns the sorted set of currently-selected member server
// names. Sorting makes the dirty/equality checks order-independent.
export function selectedServers(draft: GroupDraft): string[] {
  return Object.keys(draft.selected)
    .filter((s) => draft.selected[s])
    .sort();
}

// projectToolsHidden builds the wire tools_hidden map from the draft: for
// each SELECTED server with at least one parsed hidden tool, emit
// {server: [tools…]}. Servers with no hidden tools (or unselected servers)
// are omitted, so the wire shape stays compact and the backend's
// "tools_hidden key must be a member server" gate is satisfied by
// construction.
export function projectToolsHidden(draft: GroupDraft): Record<string, string[]> {
  const out: Record<string, string[]> = {};
  for (const server of selectedServers(draft)) {
    const tools = parseHiddenTools(draft.hiddenText[server] ?? "");
    if (tools.length > 0) out[server] = tools;
  }
  return out;
}

// draftToBody projects the draft into the SaveGroupBody wire shape. Name is
// trimmed (matching the backend's strings.TrimSpace); description is trimmed;
// servers is the sorted selected set; tools_hidden is omitted when empty so
// a group with only `servers` carries a compact body.
export function draftToBody(draft: GroupDraft): SaveGroupBody {
  const hidden = projectToolsHidden(draft);
  const body: SaveGroupBody = {
    name: draft.name.trim(),
    description: draft.description.trim(),
    servers: selectedServers(draft),
  };
  if (Object.keys(hidden).length > 0) body.tools_hidden = hidden;
  return body;
}

// emptyDraft builds a fresh draft for a NEW group: no name, no servers, an
// empty hidden-text entry per available server (so the per-server tag input
// is addressable the moment the server is checked).
export function emptyDraft(availableServers: string[]): GroupDraft {
  const hiddenText: Record<string, string> = {};
  for (const s of availableServers) hiddenText[s] = "";
  return { name: "", description: "", selected: {}, hiddenText };
}

// draftFromGroup hydrates an editor draft from an existing persisted group.
// Members are pre-checked; each member's hidden tools are joined back into the
// tag-input string. availableServers seeds a hiddenText slot for every server
// (so newly-checking a server that the group didn't previously include is
// addressable) without losing the persisted members' text.
export function draftFromGroup(group: GroupDTO, availableServers: string[]): GroupDraft {
  const selected: Record<string, boolean> = {};
  const hiddenText: Record<string, string> = {};
  for (const s of availableServers) hiddenText[s] = "";
  for (const s of group.servers) {
    selected[s] = true;
    if (!(s in hiddenText)) hiddenText[s] = "";
  }
  const hidden = group.tools_hidden ?? {};
  for (const [server, tools] of Object.entries(hidden)) {
    hiddenText[server] = (tools ?? []).join(", ");
  }
  return {
    name: group.name,
    description: group.description ?? "",
    selected,
    hiddenText,
  };
}

// sameStringSet reports whether two lists hold the same members
// (order-independent). Used by isDirty for the servers comparison.
function sameStringSet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false;
  const sa = [...a].sort();
  const sb = [...b].sort();
  return sa.every((v, i) => v === sb[i]);
}

// sameHidden reports whether two tools_hidden maps are equal — same key set,
// and each key's tool list equal as a SET (order-independent, matching how
// the operator may reorder tags without it being a real change).
function sameHidden(a: Record<string, string[]>, b: Record<string, string[]>): boolean {
  const ka = Object.keys(a).sort();
  const kb = Object.keys(b).sort();
  if (!sameStringSet(ka, kb)) return false;
  for (const k of ka) {
    if (!sameStringSet(a[k] ?? [], b[k] ?? [])) return false;
  }
  return true;
}

// isDirty reports whether the draft differs from the persisted group. When
// `persisted` is null (a brand-new group), ANY non-empty content is dirty:
// a name, a selected server, or hidden tools. For an existing group it
// compares name, description, the selected-server set, and the projected
// tools_hidden map — all order-independent. This is the single predicate the
// screen's Save-enabled gate and the nav-away dirty guard both consume.
export function isDirty(draft: GroupDraft, persisted: GroupDTO | null): boolean {
  const body = draftToBody(draft);
  const bodyHidden = body.tools_hidden ?? {};
  if (persisted === null) {
    return (
      body.name !== "" ||
      (body.description ?? "") !== "" ||
      body.servers.length > 0 ||
      Object.keys(bodyHidden).length > 0
    );
  }
  if (body.name !== persisted.name) return true;
  if ((body.description ?? "") !== (persisted.description ?? "")) return true;
  if (!sameStringSet(body.servers, persisted.servers)) return true;
  return !sameHidden(bodyHidden, persisted.tools_hidden ?? {});
}

// fieldForCode maps a backend GroupsApiError code to the offending editor
// field so the screen can render the error inline next to the right control.
// Unknown codes map to "form" (a top-level banner).
export type GroupErrorField = "name" | "servers" | "tools" | "form";

export function fieldForCode(code: string): GroupErrorField {
  switch (code) {
    case "GROUPS_INVALID_NAME":
    case "GROUPS_NAME_REQUIRED":
      return "name";
    case "GROUPS_UNKNOWN_SERVER":
      return "servers";
    case "GROUPS_HIDDEN_NONMEMBER":
      return "tools";
    default:
      return "form";
  }
}
