import { useState } from "preact/hooks";
import { postAction } from "../../lib/settings-api";
import type { SettingsSnapshot } from "../../lib/settings-types";

export type SectionAdvancedProps = {
  snapshot: SettingsSnapshot;
};

export function SectionAdvanced({ snapshot: _ }: SectionAdvancedProps): preact.JSX.Element {
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

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
    </section>
  );
}
