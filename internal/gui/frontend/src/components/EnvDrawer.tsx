// internal/gui/frontend/src/components/EnvDrawer.tsx
//
// v0.5.x Servers-matrix revamp Task 4.3 — per-row env drawer surfaced
// when the operator clicks a matrix row. Two operations on one daemon:
//
//   1. Apply env: edit PATH (and other keys, future iterations), POST
//      /api/daemon/env which writes a source:operator row into
//      daemon-env-overrides.yaml. Does NOT restart — the drawer makes
//      that explicit so operators can review effective env first
//      (per spec §"Apply env edit from GUI" + plan Task 4.3 AC 3).
//   2. Respawn: POST /api/daemon/respawn which performs graceful 5 s
//      shutdown → spawn-with-overlay. Force checkbox toggles the
//      QUARANTINED gate (HTTP 409 → DaemonRespawnError.code).
//
// Warning chip semantic (spec §"${parent_path} token semantics"): if
// the operator-edited PATH does NOT include the literal ${parent_path}
// substring, the parent process's PATH is DROPPED for that daemon at
// spawn time. The chip is the only pre-Apply signal operators get
// before they discover the daemon can't find its dependencies.

import { useEffect, useRef, useState } from "preact/hooks";
import {
  applyDaemonEnv,
  DaemonRespawnError,
  respawnDaemon,
} from "../api";

export interface EnvDrawerProps {
  // taskName is the canonical leading-backslash form (matches the
  // SupervisorDaemon.TaskName invariant; the backend handler
  // re-normalizes via NormalizeOverlayKey defensively).
  taskName: string;
  // Optional initial PATH value rendered into the textarea. Empty
  // string is a valid starting state — operator can type a fresh PATH
  // from scratch.
  initialPath?: string;
  // Visible row label (e.g. `mcp-language-server-clangd` or the LSP
  // language name) — keeps the drawer header readable for operators
  // who opened multiple drawers in a tab session.
  rowLabel: string;
  // onClose closes the drawer (parent owns visibility). Called from
  // the explicit Close button AND after a successful Apply (so the
  // matrix can refresh and re-open if needed).
  onClose: () => void;
}

const PARENT_PATH_TOKEN = "${parent_path}";

export function EnvDrawer(props: EnvDrawerProps) {
  const { taskName, initialPath = "", rowLabel, onClose } = props;
  const [pathValue, setPathValue] = useState<string>(initialPath);
  const [force, setForce] = useState<boolean>(false);
  const [working, setWorking] = useState<boolean>(false);
  const [applyMsg, setApplyMsg] = useState<{ text: string; kind: "ok" | "error" } | null>(null);
  const [restartMsg, setRestartMsg] = useState<{ text: string; kind: "ok" | "error" } | null>(null);

  // mountedRef guards post-await setState calls. Same pattern as
  // ServersScreen's mountedRef — without it, a parent that unmounts
  // the drawer between click and POST settle would log a React
  // warning and risk picking up the stale banner on remount.
  const mountedRef = useRef<boolean>(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  const pathLacksParentToken =
    pathValue.trim() !== "" && !pathValue.includes(PARENT_PATH_TOKEN);

  async function handleApply() {
    if (working) return;
    setWorking(true);
    setApplyMsg(null);
    try {
      // Build the env map. Send Path explicitly when the textarea is
      // non-empty; never send an empty Path (that would clear the
      // overlay row's Path and immediately drop the parent PATH at
      // spawn — almost certainly not what the operator wanted).
      const env: Record<string, string> = {};
      if (pathValue.trim() !== "") {
        // Uppercase "PATH" — the supervisor's mergeDaemonEnv
        // (internal/cli/supervise.go:1664) folds key case only on
        // Windows, so on Linux/macOS a `Path` key would NOT collide
        // with the parent process's `PATH` entry and the operator-
        // typed value would be silently dropped at spawn time.
        env.PATH = pathValue;
      }
      const result = await applyDaemonEnv(taskName, env);
      if (!mountedRef.current) return;
      setApplyMsg({
        text: `Applied ${result.changed_keys.length} key(s) to ${result.task_name}. Click Restart to take effect.`,
        kind: "ok",
      });
    } catch (err) {
      if (!mountedRef.current) return;
      setApplyMsg({ text: (err as Error).message, kind: "error" });
    } finally {
      if (mountedRef.current) setWorking(false);
    }
  }

  async function handleRestart() {
    if (working) return;
    setWorking(true);
    setRestartMsg(null);
    try {
      const result = await respawnDaemon(taskName, force);
      if (!mountedRef.current) return;
      setRestartMsg({
        text: `Daemon ${result.task_name} respawned (force=${result.force}, state=${result.state}).`,
        kind: "ok",
      });
    } catch (err) {
      if (!mountedRef.current) return;
      const e = err as Error;
      let text = e.message;
      // Quarantined error → make the recovery affordance explicit so
      // the operator doesn't have to read the raw API error and
      // figure out that the force checkbox is the gate. Spec
      // §"Respawn from GUI" requires this UX.
      if (err instanceof DaemonRespawnError && err.code === "QUARANTINED") {
        text =
          "Daemon is currently quarantined. Tick the 'force' checkbox below and click Restart again to override.";
      }
      setRestartMsg({ text, kind: "error" });
    } finally {
      if (mountedRef.current) setWorking(false);
    }
  }

  return (
    <div class="env-drawer" data-testid="env-drawer">
      <div class="env-drawer-header">
        <strong>Edit env: {rowLabel}</strong>
        <button
          type="button"
          class="env-drawer-close"
          onClick={onClose}
          data-testid="env-drawer-close"
        >
          ×
        </button>
      </div>
      <div class="env-drawer-task" data-testid="env-drawer-task">
        <code>{taskName}</code>
      </div>

      <label class="env-drawer-field">
        <span>PATH</span>
        <textarea
          rows={4}
          value={pathValue}
          onInput={(ev) => setPathValue((ev.currentTarget as HTMLTextAreaElement).value)}
          data-testid="env-drawer-path"
          placeholder="C:\msys64\ucrt64\bin;${parent_path}"
        />
      </label>
      {pathLacksParentToken && (
        <div
          class="env-drawer-warning"
          role="alert"
          data-testid="env-drawer-parent-path-warning"
        >
          ⚠ PATH does not include <code>{PARENT_PATH_TOKEN}</code> — parent
          PATH will be DROPPED for this daemon at spawn time.
        </div>
      )}

      <div class="env-drawer-actions">
        <button
          type="button"
          onClick={handleApply}
          disabled={working}
          data-testid="env-drawer-apply"
        >
          Apply env
        </button>
        <label class="env-drawer-force">
          <input
            type="checkbox"
            checked={force}
            onChange={(ev) => setForce((ev.currentTarget as HTMLInputElement).checked)}
            data-testid="env-drawer-force"
          />
          <span>force (override quarantine)</span>
        </label>
        <button
          type="button"
          onClick={handleRestart}
          disabled={working}
          data-testid="env-drawer-restart"
        >
          Restart daemon
        </button>
      </div>

      {applyMsg && (
        <div
          class={applyMsg.kind === "error" ? "error" : "info"}
          data-testid="env-drawer-apply-msg"
        >
          {applyMsg.text}
        </div>
      )}
      {restartMsg && (
        <div
          class={restartMsg.kind === "error" ? "error" : "info"}
          data-testid="env-drawer-restart-msg"
        >
          {restartMsg.text}
        </div>
      )}
    </div>
  );
}
