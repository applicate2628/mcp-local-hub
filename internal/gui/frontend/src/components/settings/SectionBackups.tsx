import { useState, useEffect } from "preact/hooks";
import { putSetting } from "../../lib/settings-api";
import { BackupsList } from "./BackupsList";
import { BACKUPS_COPY } from "./backups-copy";
import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";

export type SectionBackupsProps = {
  snapshot: SettingsSnapshot;
  onDirtyChange: (b: boolean) => void;
};

export function SectionBackups({ snapshot, onDirtyChange }: SectionBackupsProps): preact.JSX.Element {
  if (snapshot.status !== "ok") return <section data-section="backups"><h2>Backups</h2></section>;
  const def = snapshot.data.settings.find((s) => s.key === "backups.keep_n") as ConfigSettingDTO;
  const persisted = Number(def.value);

  const [draft, setDraft] = useState<number>(persisted);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [banner, setBanner] = useState<string | null>(null);

  // Re-anchor when snapshot persisted value changes (e.g. after refresh).
  useEffect(() => { setDraft(persisted); }, [persisted]);

  const dirty = draft !== persisted;
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange]);

  async function save() {
    setBusy(true);
    setErr(null);
    try {
      await putSetting("backups.keep_n", String(draft));
      setBanner("Saved.");
      setTimeout(() => setBanner(null), 2000);
      await snapshot.refresh();
    } catch (e: any) {
      setErr(String(e?.body?.reason ?? e?.message ?? "save failed"));
    } finally {
      setBusy(false);
    }
  }

  return (
    <section data-section="backups" class="settings-section">
      <h2>Backups</h2>
      <p class="settings-section-help">Manage backup retention for managed client configs.</p>

      <div class="backups-slider-row">
        <label for="backups-keep-n-slider" class="backups-slider-label">
          {BACKUPS_COPY.sliderLabel}: <strong>{draft}</strong>
        </label>
        <input
          id="backups-keep-n-slider"
          type="range"
          min={def.min ?? 0}
          max={def.max ?? 50}
          value={draft}
          disabled={busy}
          onInput={(e) => setDraft(Number((e.target as HTMLInputElement).value))}
        />
        <small class="backups-helper-text">{BACKUPS_COPY.helperText}</small>
        {err ? <small class="settings-field-error" role="alert">{err}</small> : null}
      </div>

      <BackupsList keepN={draft} />

      <div class="backups-clean-row">
        <button
          type="button"
          disabled
          title={BACKUPS_COPY.cleanTooltip}
          aria-label={BACKUPS_COPY.cleanTooltip}
          data-test-id="clean-now-disabled"
        >
          Clean now
        </button>
        <span class="deferred-badge">(coming in A4-b)</span>
      </div>

      <div class="settings-section-footer">
        {banner ? <span class="save-banner ok">{banner}</span> : null}
        <button type="button" disabled={!dirty || busy} onClick={() => void save()}>
          {busy ? "Saving…" : "Save"}
        </button>
        <button type="button" disabled={!dirty || busy} onClick={() => setDraft(persisted)}>Reset</button>
      </div>
    </section>
  );
}
