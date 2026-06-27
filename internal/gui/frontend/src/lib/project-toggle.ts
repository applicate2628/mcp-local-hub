// internal/gui/frontend/src/lib/project-toggle.ts
//
// Pure helpers for the per-project-GUI Phase 3b detail-lens toggles
// (work-items/decisions/2026-06-27-per-project-gui-p3b-uxdesign.md). Kept out of
// Projects.tsx so the SCOPE-selection rule and the §3.1 code→copy map are
// unit-testable without rendering Preact.
//
// SINGLE-OWNER SCOPE RULE (acceptance criterion 2 — "never branch on client name
// to pick a write owner"). The frontend's ONLY scope logic is `scopeForToggle`
// below: it maps a (mechanism, claude-substrate) pair to one of the four stable
// ProjectToggleScope values. The backend clients.ProjectToggleOwner is the
// classifier that turns (client, scope) into the actual write owner — the GUI
// never re-derives ownership.

import type { ProjectToggleScope } from "../api";

// ToggleMechanism is the per-project mechanism a row belongs to. It is NOT the
// client name — the claude-code DUAL substrate is disambiguated by the explicit
// "claude-project" / "claude-local" values, so the caller never has to (and must
// never) branch on the client string to choose a scope.
export type ToggleMechanism =
  | "workspace" // Model A — workspace LSP daemon
  | "object-member" // Model B — cursor/vscode/claude-code .mcp.json object member
  | "claude-local" // Model B-claude Local — array move in ~/.claude.json
  | "group"; // Model C — group membership

// scopeForToggle is the SINGLE place the frontend decides which substrate scope a
// toggle targets. It maps the mechanism (and, for claude, which substrate) to the
// stable ProjectToggleScope the backend dispatches on. There is deliberately NO
// branch on a client name here:
//   - workspace            → "workspace-lsp"
//   - object-member        → "project-object-member"  (cursor / vscode / claude .mcp.json Project)
//   - claude-local         → "claude-local-membership" (the array move; D1 read-only in P3b v1)
//   - group                → "group-servers"
//
// claude-code's .mcp.json Project rows use mechanism "object-member" → scope
// "project-object-member" (the array-move-free object member); the design's
// "claude Project rows → claude-local-membership" line refers to the LOCAL-array
// APPROVAL move, which is mechanism "claude-local". The two claude substrates are
// distinct mechanisms, never one client-name branch.
export function scopeForToggle(mechanism: ToggleMechanism): ProjectToggleScope {
  switch (mechanism) {
    case "workspace":
      return "workspace-lsp";
    case "object-member":
      return "project-object-member";
    case "claude-local":
      return "claude-local-membership";
    case "group":
      return "group-servers";
  }
}

// ToggleErrorCopy is the row-scoped plain-copy + behavior for a failed toggle.
//   message — the human-readable line shown ON the row (never the raw code).
//   retry   — whether to offer a Retry affordance.
//   reload  — whether the failure suggests reloading the project/section
//             (PROJECT_ROOT_INVALID → full reload; GROUP_NOT_FOUND → section reload).
//   hideToggle — PROJECT_TOGGLE_UNSUPPORTED: this client has no project-local
//             config here, so the toggle should be hidden (managed in the client).
export interface ToggleErrorCopy {
  message: string;
  retry: boolean;
  reload: boolean;
  hideToggle: boolean;
}

// toggleErrorCopy maps a stable backend code to its §3.1 plain-copy + behavior.
// The raw code is NOT in the message (the caller puts it in a tooltip). An
// unknown code falls back to the generic 500-style "couldn't be saved" + Retry,
// which is the safe, honest default for an unmodelled failure.
export function toggleErrorCopy(code: string): ToggleErrorCopy {
  switch (code) {
    case "PROJECT_TOGGLE_INVALID":
      return {
        message: "Couldn't change this — a required field was missing.",
        retry: true,
        reload: false,
        hideToggle: false,
      };
    case "PROJECT_TOGGLE_UNSUPPORTED":
      return {
        message: "This client has no project-local config here — manage it in the client.",
        retry: false,
        reload: false,
        hideToggle: true,
      };
    case "PROJECT_ROOT_INVALID":
      return {
        message: "This project's root could not be read — it may have moved or been deleted.",
        retry: true,
        reload: true,
        hideToggle: false,
      };
    case "PROJECT_TOGGLE_UNKNOWN_SERVER":
      return {
        message: "That server isn't a known routable server.",
        retry: false,
        reload: false,
        hideToggle: false,
      };
    case "PROJECT_TOGGLE_GROUP_NOT_FOUND":
      return {
        message: "That group no longer exists — refresh.",
        retry: false,
        reload: true,
        hideToggle: false,
      };
    case "PROJECT_TOGGLE_FAILED":
    default:
      return {
        message: "The change couldn't be saved. Retry, or check the app log.",
        retry: true,
        reload: false,
        hideToggle: false,
      };
  }
}
