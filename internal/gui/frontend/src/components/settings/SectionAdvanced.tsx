import { useEffect, useRef, useState } from "preact/hooks";
import { postAction } from "../../lib/settings-api";
import { restartSupervisor } from "../../api";
import type { ConfigSettingDTO, SettingsSnapshot } from "../../lib/settings-types";
import { SectionAdvancedDiagnostics } from "./SectionAdvancedDiagnostics";
import { SectionFooter } from "./SectionAppearance";
import { useSectionSaveFlow } from "./useSectionSaveFlow";
import { InfoTip } from "../InfoTip";
import { SettingsCard } from "./SettingsCard";

export type SectionAdvancedProps = {
  snapshot: SettingsSnapshot;
  onDirtyChange?: (b: boolean) => void;
};

const MCP_FRONT_PORT_KEY = "mcp_front.port";
const SECTION_KEYS = [MCP_FRONT_PORT_KEY];
const ignoreDirtyChange = () => {};
type PortActionPhase = "clean" | "dirty" | "saving" | "save-settling" | "restarting";

export function SectionAdvanced({
  snapshot,
  onDirtyChange = ignoreDirtyChange,
}: SectionAdvancedProps): preact.JSX.Element {
  const sectionFlow = useSectionSaveFlow(snapshot, SECTION_KEYS, onDirtyChange);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // This one transition owner is authoritative for port Input/Save/Reset and
  // supervisor Restart. The ref rejects stale handlers before rendering catches
  // up; the state is only its disabled-control projection.
  const portActionPhaseRef = useRef<PortActionPhase>("clean");
  const [portActionPhase, setPortActionPhase] = useState<PortActionPhase>("clean");

  const portDef = snapshot.status === "ok"
    ? snapshot.data.settings.find(
      (setting): setting is ConfigSettingDTO =>
        setting.key === MCP_FRONT_PORT_KEY && setting.type === "int",
    )
    : undefined;
  const portError = sectionFlow.errors[MCP_FRONT_PORT_KEY];

  function transitionPortAction(next: PortActionPhase) {
    portActionPhaseRef.current = next;
    setPortActionPhase(next);
  }

  useEffect(() => {
    const phase = portActionPhaseRef.current;
    if (phase === "save-settling" && !sectionFlow.busy) {
      transitionPortAction(sectionFlow.dirty ? "dirty" : "clean");
      return;
    }
    if (phase === "clean" && sectionFlow.dirty) {
      transitionPortAction("dirty");
    } else if (phase === "dirty" && !sectionFlow.dirty) {
      transitionPortAction("clean");
    }
  }, [sectionFlow.busy, sectionFlow.dirty]);

  function setPortLocal(value: string) {
    const phase = portActionPhaseRef.current;
    if (phase !== "clean" && phase !== "dirty") return;
    transitionPortAction("dirty");
    flow.setLocal(
      MCP_FRONT_PORT_KEY,
      value,
    );
  }

  async function savePort() {
    if (portActionPhaseRef.current !== "dirty" || !sectionFlow.dirty || sectionFlow.busy) return;
    transitionPortAction("saving");
    try {
      await sectionFlow.save();
    } finally {
      // The shared flow remains the authority for success, failure, errors,
      // refresh, and banners. Wait for its rendered projection before admitting
      // another action.
      transitionPortAction("save-settling");
    }
  }

  function resetPort() {
    if (portActionPhaseRef.current !== "dirty") return;
    transitionPortAction("clean");
    sectionFlow.reset();
  }

  const portSaveBusy = portActionPhase === "saving" || portActionPhase === "save-settling";
  // Input projection includes Restart; the footer deliberately does not so it
  // never presents Restart as a Save in progress.
  const flow = {
    ...sectionFlow,
    busy: sectionFlow.busy || portSaveBusy || portActionPhase === "restarting",
  };
  const footerFlow = {
    ...sectionFlow,
    busy: sectionFlow.busy || portSaveBusy,
    save: savePort,
    reset: resetPort,
  };

  // state-read-relax toggle — see internal/gui/state_relax_setting_windows.go
  // for the underlying HKCU\Environment write + WM_SETTINGCHANGE
  // broadcast. The toggle is platform-gated: on POSIX the backend
  // returns 501, so we surface the disabled state + hint instead of
  // pretending the switch is wired.
  const [relaxEnabled, setRelaxEnabled] = useState<boolean | null>(null);
  const [relaxSupported, setRelaxSupported] = useState<boolean>(true);
  const [relaxBusy, setRelaxBusy] = useState<boolean>(false);
  const [relaxMsg, setRelaxMsg] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const r = await fetch("/api/settings/state-read-relax");
        if (cancelled) return;
        if (r.status === 501) {
          setRelaxSupported(false);
          setRelaxEnabled(false);
          return;
        }
        if (!r.ok) {
          setRelaxMsg(`GET failed: HTTP ${r.status}`);
          return;
        }
        const body = (await r.json()) as { enabled?: boolean };
        setRelaxEnabled(body.enabled === true);
      } catch (e: any) {
        if (!cancelled) setRelaxMsg(`GET error: ${e?.message ?? String(e)}`);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  async function toggleRelax(next: boolean) {
    if (!relaxSupported || relaxBusy) return;
    setRelaxBusy(true);
    setRelaxMsg(null);
    try {
      const r = await fetch("/api/settings/state-read-relax", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ enabled: next }),
      });
      if (!r.ok) {
        setRelaxMsg(`Toggle failed: HTTP ${r.status}`);
        return;
      }
      const body = (await r.json()) as { enabled?: boolean; restart_required?: boolean };
      setRelaxEnabled(body.enabled === true);
      if (body.restart_required) {
        setRelaxMsg(
          `${next ? "Enabled" : "Disabled"}. Restart mcphub (Dashboard → Restart supervisor, or full mcphub restart) so the new env value reaches the running supervisor.`,
        );
      } else {
        setRelaxMsg(`Already ${next ? "enabled" : "disabled"}; no change.`);
      }
    } catch (e: any) {
      setRelaxMsg(`Toggle error: ${e?.message ?? String(e)}`);
    } finally {
      setRelaxBusy(false);
    }
  }

  async function openFolder() {
    setBusy(true);
    setErr(null);
    try {
      await postAction("advanced.open_app_data_folder");
    } catch (e: any) {
      setErr(String(e?.body?.reason ?? e?.message ?? "spawn failed"));
    } finally {
      setBusy(false);
    }
  }

  async function exportBundle() {
    setBusy(true);
    setErr(null);
    try {
      const r = await fetch("/api/export-config-bundle", { method: "POST" });
      if (!r.ok) {
        setErr(`Export failed: HTTP ${r.status}`);
        return;
      }
      const blob = await r.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `mcphub-bundle-${new Date().toISOString().replace(/[:.]/g, "-").slice(0, 19)}.zip`;
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(url);
    } catch (e: any) {
      setErr(`Export failed: ${e?.message ?? String(e)}`);
    } finally {
      setBusy(false);
    }
  }

  // "Restart supervisor now" — the corp-managed autorun toggle needs a
  // supervisor restart for the new env value to reach the running
  // supervisor (see relaxMsg above). This reuses the Dashboard's
  // restartSupervisor() helper (POST /api/supervisor/restart) so operators
  // don't have to leave the Settings screen.
  const [supRestartBusy, setSupRestartBusy] = useState(false);
  const [supRestartMsg, setSupRestartMsg] = useState<{ kind: "ok" | "error"; text: string } | null>(null);
  async function restartSupervisorNow() {
    if (
      portActionPhaseRef.current !== "clean"
      || sectionFlow.dirty
      || sectionFlow.busy
      || supRestartBusy
    ) return;
    transitionPortAction("restarting");
    setSupRestartBusy(true);
    setSupRestartMsg(null);
    try {
      const res = await restartSupervisor();
      if (res.spawned) {
        setSupRestartMsg({ kind: "ok", text: "Supervisor restart requested." });
      } else {
        const detail = res.per_step_error
          ? Object.entries(res.per_step_error).map(([k, v]) => `${k}: ${v}`).join("; ")
          : "supervisor did not respawn";
        setSupRestartMsg({ kind: "error", text: `Restart incomplete: ${detail}` });
      }
    } catch (e: any) {
      setSupRestartMsg({ kind: "error", text: e?.message ?? "Restart failed" });
    } finally {
      transitionPortAction("clean");
      setSupRestartBusy(false);
    }
  }

  const relaxIsError = relaxMsg !== null && (relaxMsg.includes("failed") || relaxMsg.includes("error"));

  return (
    <SettingsCard
      section="advanced"
      title="Advanced"
      infoTipLabel="About this section"
      infoTip="Configure the supervisor-managed MCP front port and use power-user actions: open the app-data folder on disk, export a configuration bundle, toggle autorun on corp-managed Windows hosts (the MCPHUB_ALLOW_UNHARDENED_STATE_READ env var), restart the supervisor, and diagnose the single-instance lock."
      subtitle="MCP front daemon and power-user actions."
    >
      {portDef ? (
        <>
          <div class="divide-y divide-app-border/60">
            <div class={`settings-field flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 py-3${portError ? " has-error" : ""}`}>
              <label class="flex items-center gap-1.5 text-sm font-medium text-app-text" for={portDef.key}>
                MCP front port
                {portDef.help ? <InfoTip text={portDef.help} /> : null}
              </label>
              <div class="flex flex-col items-start gap-1 sm:items-end">
                <input
                  id={portDef.key}
                  type="number"
                  class="field-ctl w-24"
                  value={flow.effective(MCP_FRONT_PORT_KEY)}
                  min={portDef.min}
                  max={portDef.max}
                  step={1}
                  disabled={portDef.deferred || flow.busy}
                  onInput={(event) => {
                    setPortLocal((event.currentTarget as HTMLInputElement).value);
                  }}
                  aria-invalid={portError ? true : undefined}
                  aria-describedby={portError
                    ? `${portDef.key}-constraints ${portDef.key}-error`
                    : `${portDef.key}-constraints`}
                  data-testid="mcp-front-port-input"
                />
                <small id={`${portDef.key}-constraints`} class="text-xs text-app-muted">
                  Default {portDef.default}
                  {portDef.min !== undefined && portDef.max !== undefined
                    ? `; allowed ${portDef.min}–${portDef.max}.`
                    : "."}
                </small>
                {portError ? (
                  <small id={`${portDef.key}-error`} class="settings-field-error text-xs text-app-danger" role="alert">
                    {portError}
                  </small>
                ) : null}
              </div>
            </div>
          </div>
          <SectionFooter flow={footerFlow} />
        </>
      ) : null}

      <div class={`${portDef ? "mt-5 border-t border-app-border/60 pt-4 " : ""}divide-y divide-app-border/60`}>
        <div class="flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 py-3">
          <span class="flex items-center gap-1.5 text-sm font-medium text-app-text">Open app-data folder</span>
          <button type="button" onClick={() => void openFolder()} disabled={busy} data-test-id="open-folder">
            Open app-data folder
          </button>
        </div>

        <div class="flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 py-3">
          <span class="flex items-center gap-1.5 text-sm font-medium text-app-text">Export configuration bundle</span>
          <button type="button" onClick={() => void exportBundle()} disabled={busy} data-testid="export-bundle">
            Export bundle
          </button>
        </div>
      </div>
      {err ? <p class="mt-2 text-sm text-app-danger" role="alert">Could not open folder: {err}</p> : null}

      <div class="mt-5">
        <h3 class="m-0 mb-1 text-xs font-semibold uppercase tracking-wide text-app-muted">Autorun on corp-managed Windows</h3>
        <div class="flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 py-3">
          <label class="flex items-center gap-1.5 text-sm font-medium text-app-text" for="advanced-state-relax-toggle">
            Autorun on corp-managed Windows (sets MCPHUB_ALLOW_UNHARDENED_STATE_READ)
            <InfoTip text="Required on corp-managed Windows hosts whose %LOCALAPPDATA% inherits a Domain Users / Authenticated Users ACE that you cannot remove. Without this, mcphub's supervisor crashes at logon-trigger startup with 'insecure parent directory' and the Dashboard shows 'Failed to load' with no daemons. When enabled, mcphub writes the user-scope MCPHUB_ALLOW_UNHARDENED_STATE_READ=1 env var to the Windows registry (HKCU\\Environment); Task-Scheduler-spawned mcphub processes inherit it at every logon, so the autostart pipeline actually works. Leave off on single-user dev machines where this isn't needed." />
          </label>
          <input
            id="advanced-state-relax-toggle"
            type="checkbox"
            class="h-4 w-4 accent-app-accent"
            data-testid="state-relax-toggle"
            disabled={!relaxSupported || relaxBusy || relaxEnabled === null}
            checked={relaxEnabled === true}
            onChange={(ev) => void toggleRelax((ev.currentTarget as HTMLInputElement).checked)}
          />
        </div>
        {!relaxSupported && (
          <p class="mt-1 text-xs text-app-muted">
            (Not supported on this OS — set <code>MCPHUB_ALLOW_UNHARDENED_STATE_READ=1</code> in
            your shell profile if needed.)
          </p>
        )}
        {relaxMsg && (
          <p
            class={relaxIsError ? "mt-1 text-sm text-app-danger" : "mt-1 text-xs text-app-muted"}
            data-testid="state-relax-msg"
            role={relaxIsError ? "alert" : undefined}
          >
            {relaxMsg}
          </p>
        )}
        <div class="mt-2 flex flex-wrap items-center gap-3">
          <button
            type="button"
            onClick={() => void restartSupervisorNow()}
            disabled={portActionPhase !== "clean" || flow.dirty || flow.busy || supRestartBusy}
            data-testid="advanced-restart-supervisor"
          >
            {supRestartBusy ? "Restarting…" : "Restart supervisor now"}
          </button>
          {supRestartMsg && (
            <span
              class={supRestartMsg.kind === "error" ? "text-sm text-app-danger" : "text-xs text-app-muted"}
              data-testid="advanced-restart-supervisor-msg"
              role={supRestartMsg.kind === "error" ? "alert" : "status"}
            >
              {supRestartMsg.text}
            </span>
          )}
        </div>
      </div>

      <div class="mt-5 border-t border-app-border/60 pt-4">
        <SectionAdvancedDiagnostics />
      </div>
    </SettingsCard>
  );
}
