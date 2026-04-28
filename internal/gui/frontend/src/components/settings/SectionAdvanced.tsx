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

  return (
    <section data-section="advanced" class="settings-section">
      <h2>Advanced</h2>
      <p class="settings-section-help">Power-user actions.</p>
      <div class="advanced-actions">
        <button type="button" onClick={() => void openFolder()} disabled={busy} data-test-id="open-folder">
          Open app-data folder
        </button>
        <button type="button" disabled data-test-id="export-bundle-disabled">
          Export bundle
          <span class="deferred-badge"> (coming in A4-b)</span>
        </button>
      </div>
      {err ? <p class="error-banner" role="alert">Could not open folder: {err}</p> : null}
    </section>
  );
}
