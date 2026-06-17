import { useEffect, useState } from "preact/hooks";
import { postAction } from "../../lib/settings-api";
import { restartSupervisor } from "../../api";
import type { SettingsSnapshot } from "../../lib/settings-types";
import { SectionAdvancedDiagnostics } from "./SectionAdvancedDiagnostics";
import { InfoTip } from "../InfoTip";

export type SectionAdvancedProps = {
  snapshot: SettingsSnapshot;
};

export function SectionAdvanced({ snapshot: _ }: SectionAdvancedProps): preact.JSX.Element {
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

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
    if (supRestartBusy) return;
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
      setSupRestartBusy(false);
    }
  }

  const relaxIsError = relaxMsg !== null && (relaxMsg.includes("failed") || relaxMsg.includes("error"));

  return (
    <section data-section="advanced" class="mb-6 rounded-xl border border-app-border bg-app-card p-5 shadow-sm sm:p-6">
      <header class="mb-2 flex items-center gap-1.5">
        <h2 class="m-0 text-lg font-semibold text-app-text">Advanced</h2>
        <InfoTip
          label="About this section"
          text="Power-user actions: open the app-data folder on disk, export a configuration bundle, toggle autorun on corp-managed Windows hosts (the MCPHUB_ALLOW_UNHARDENED_STATE_READ env var), restart the supervisor, and diagnose the single-instance lock."
        />
      </header>
      <p class="m-0 mb-4 text-sm text-app-muted">Power-user actions.</p>

      <div class="divide-y divide-app-border/60">
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
            disabled={supRestartBusy}
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
    </section>
  );
}
