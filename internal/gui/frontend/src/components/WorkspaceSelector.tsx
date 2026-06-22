// internal/gui/frontend/src/components/WorkspaceSelector.tsx
//
// v0.5.x Servers-matrix revamp Task 4.3 — active-workspace dropdown
// surfaced above the matrix table. Each registered workspace gets one
// option; the matrix scopes LSP rows to the selected key so an
// operator with N workspaces sees one workspace's daemons at a time
// instead of N×9 rows. The dropdown's "all" sentinel keeps the legacy
// "render every workspace's entries" behavior available for diagnosis.
//
// Data source: GET /api/workspaces returns deduplicated (key, path)
// pairs. Empty registry → render a "(none — register a workspace
// first)" placeholder span (per Task 4.3 acceptance criterion 2).
//
// Layout: a single <label> wrapping a <select>; the optional path
// preview is rendered as a sibling <span> so screen readers stay
// quiet on the visual-only annotation. Matches the existing
// settings-section pattern of in-line labelled selects without a
// modal.

import type { WorkspacePair } from "../api";

export interface WorkspaceSelectorProps {
  workspaces: WorkspacePair[];
  // ALL_WORKSPACES_KEY ("") is the sentinel value for "no filter
  // applied" — Servers.tsx initializes selectedKey to this so the
  // matrix starts with every workspace's rows visible. Once the
  // operator picks a workspace, the value becomes that key.
  selectedKey: string;
  onChange: (workspaceKey: string) => void;
}

export const ALL_WORKSPACES_KEY = "";

export function WorkspaceSelector(props: WorkspaceSelectorProps) {
  const { workspaces, selectedKey, onChange } = props;

  // Locate the selected pair so the path preview can render alongside
  // the dropdown. The ALL_WORKSPACES_KEY sentinel intentionally has no
  // matching pair — `selected === undefined` is the correct truthy
  // signal for the "no filter" state.
  const selected = workspaces.find((w) => w.workspace_key === selectedKey);

  if (workspaces.length === 0) {
    return (
      <div class="workspace-selector workspace-selector-empty" data-testid="workspace-selector">
        <span class="workspace-selector-empty-text">
          (none — register a workspace first with{" "}
          <code>mcphub register</code>)
        </span>
      </div>
    );
  }

  return (
    <div class="workspace-selector" data-testid="workspace-selector">
      <label class="workspace-selector-label">
        <span>Active workspace:</span>
        <select
          class="field-ctl"
          value={selectedKey}
          onChange={(ev) => onChange((ev.currentTarget as HTMLSelectElement).value)}
          data-testid="workspace-selector-select"
        >
          <option value={ALL_WORKSPACES_KEY}>(all workspaces)</option>
          {workspaces.map((w) => (
            <option key={w.workspace_key} value={w.workspace_key}>
              {w.workspace_key}
            </option>
          ))}
        </select>
      </label>
      {selected && (
        <span
          class="workspace-selector-path"
          data-testid="workspace-selector-path"
          title={selected.workspace_path}
        >
          {selected.workspace_path}
        </span>
      )}
    </div>
  );
}
