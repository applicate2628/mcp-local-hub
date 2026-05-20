import { useEffect, useState } from "preact/hooks";
import { postAction } from "../../lib/settings-api";
import type { SettingsSnapshot } from "../../lib/settings-types";
import { SectionAdvancedDiagnostics } from "./SectionAdvancedDiagnostics";

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

  return (
    <section data-section="advanced" class="settings-section">
      <h2>Advanced</h2>
      <p class="settings-section-help">Power-user actions.</p>
      <div class="advanced-actions">
        <button type="button" onClick={() => void openFolder()} disabled={busy} data-test-id="open-folder">
          Open app-data folder
        </button>
        <button type="button" onClick={() => void exportBundle()} disabled={busy} data-testid="export-bundle">
          Export bundle
        </button>
      </div>
      {err ? <p class="error-banner" role="alert">Could not open folder: {err}</p> : null}

      <h3 style="margin-top: 16px">Strict-DACL relax</h3>
      <p class="settings-section-help">
        Allow mcphub's supervisor to read its state files on
        corp-managed Windows hosts whose <code>%LOCALAPPDATA%</code>
        inherits a Domain Users / Authenticated Users ACE that you
        cannot remove. When enabled, mcphub sets the user-scope
        <code>MCPHUB_ALLOW_UNHARDENED_STATE_READ=1</code> env var in
        the Windows registry; future-spawned mcphub processes
        (including Task-Scheduler logon-trigger) inherit it
        automatically. Leave off unless you actually hit a "supervisor
        insecure parent directory" error at startup.
      </p>
      <label class="settings-toggle" style="display: inline-flex; align-items: center; gap: 6px">
        <input
          type="checkbox"
          data-testid="state-relax-toggle"
          disabled={!relaxSupported || relaxBusy || relaxEnabled === null}
          checked={relaxEnabled === true}
          onChange={(ev) => void toggleRelax((ev.currentTarget as HTMLInputElement).checked)}
        />
        <span>Allow strict-DACL relax for state-file reads</span>
      </label>
      {!relaxSupported && (
        <p class="settings-section-help" style="margin-top: 4px; color: #666">
          (Not supported on this OS — set <code>MCPHUB_ALLOW_UNHARDENED_STATE_READ=1</code> in
          your shell profile if needed.)
        </p>
      )}
      {relaxMsg && (
        <p
          class={relaxMsg.includes("failed") || relaxMsg.includes("error") ? "error-banner" : "settings-section-help"}
          data-testid="state-relax-msg"
          role={relaxMsg.includes("failed") || relaxMsg.includes("error") ? "alert" : undefined}
          style="margin-top: 4px"
        >
          {relaxMsg}
        </p>
      )}

      <SectionAdvancedDiagnostics />
    </section>
  );
}
