import { useState, useEffect } from "preact/hooks";
import { putSetting, cleanBackups } from "../../lib/settings-api";
import { ConfirmModal } from "../ConfirmModal";
import { BackupsList } from "./BackupsList";
import { BACKUPS_COPY } from "./backups-copy";
import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";

export type SectionBackupsProps = {
  snapshot: SettingsSnapshot;
  /** Called whenever the section's dirty state changes. Optional; defaults to no-op. */
  onDirtyChange?: (b: boolean) => void;
};

export function SectionBackups({ snapshot, onDirtyChange = () => {} }: SectionBackupsProps): preact.JSX.Element {
  if (snapshot.status !== "ok") return <section data-section="backups"><h2>Backups</h2></section>;
  const def = snapshot.data.settings.find((s) => s.key === "backups.keep_n") as ConfigSettingDTO;
  const persisted = Number(def.value);

  const [draft, setDraft] = useState<number>(persisted);
  // Codex r9 P2: value successfully PUT to disk but not yet confirmed by a
  // fresh snapshot (refresh failed). Reset() reverts draft to lastSent (not
  // the stale snapshot), so the user keeps the saved value visible.
  const [lastSent, setLastSent] = useState<number | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [banner, setBanner] = useState<string | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);

  // When persisted catches up with lastSent (refresh succeeded later),
  // drop lastSent. Avoids a stale fallback once the snapshot is fresh.
  useEffect(() => {
    if (lastSent !== null && persisted === lastSent) {
      setLastSent(null);
    }
  }, [persisted, lastSent]);

  // The effective baseline the user edits relative to: prefer lastSent
  // (saved-on-disk-unconfirmed) over the stale persisted snapshot.
  const baseline = lastSent ?? persisted;
  const dirty = draft !== baseline;
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange]);

  // Re-anchor draft when baseline changes (refresh success or initial mount).
  useEffect(() => { setDraft(baseline); }, [baseline]);

  async function save() {
    setBusy(true);
    setErr(null);
    // Codex PR #20 r11 P3: clear any stale success/error banner immediately
    // so the user doesn't see a leftover "Saved." from a prior call while
    // the new save is in-flight.
    setBanner(null);
    try {
      await putSetting("backups.keep_n", String(draft));
    } catch (e: any) {
      setErr(String(e?.body?.reason ?? e?.message ?? "save failed"));
      setBusy(false);
      return;
    }
    // PUT succeeded — record in lastSent so Reset preserves it (Codex r9 P2).
    const sentValue = draft;
    setLastSent(sentValue);
    // Refresh is best-effort. Codex r8 P2: split refresh failure from save
    // failure so transient GET errors don't surface as if the save itself failed.
    let refreshOK = true;
    try {
      await snapshot.refresh();
    } catch {
      refreshOK = false;
    }
    setBusy(false);
    if (refreshOK) {
      setBanner("Saved.");
      setTimeout(() => setBanner(null), 2000);
    } else {
      // Codex r12 P3: after refresh failure the section is clean and Save is
      // disabled, so "click Save again" is unreachable. Suggest reload instead.
      setBanner("Saved on disk. The live view didn't refresh — reload or revisit Settings to confirm.");
    }
  }

  // Codex r9 P2: Reset reverts draft to baseline (lastSent ?? persisted),
  // NOT unconditionally to persisted. After a refresh-fail Save+Reset cycle
  // the slider stays at the saved value, not the stale snapshot value.
  function onReset() {
    setDraft(baseline);
    setErr(null);
    setBanner(null);
  }

  async function doClean() {
    await cleanBackups();
    setConfirmOpen(false);
    await snapshot.refresh();
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
          onClick={() => setConfirmOpen(true)}
          data-testid="clean-now-button"
        >
          Clean now eligible backups
        </button>
      </div>

      <ConfirmModal
        open={confirmOpen}
        title="Delete eligible backups?"
        body={<>Originals are never cleaned.</>}
        confirmLabel="Delete"
        danger
        onConfirm={doClean}
        onCancel={() => setConfirmOpen(false)}
      />

      <div class="settings-section-footer">
        {banner ? <span class="save-banner ok">{banner}</span> : null}
        <button type="button" disabled={!dirty || busy} onClick={() => void save()}>
          {busy ? "Saving…" : "Save"}
        </button>
        <button type="button" disabled={!dirty || busy} onClick={onReset}>Reset</button>
      </div>
    </section>
  );
}
