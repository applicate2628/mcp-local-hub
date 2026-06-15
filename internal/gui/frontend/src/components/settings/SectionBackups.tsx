import { useState, useEffect } from "preact/hooks";
import { putSetting, cleanBackups } from "../../lib/settings-api";
import { ConfirmModal } from "../ConfirmModal";
import { BackupsList } from "./BackupsList";
import { BACKUPS_COPY } from "./backups-copy";
import { InfoTip } from "../InfoTip";
import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";

export type SectionBackupsProps = {
  snapshot: SettingsSnapshot;
  /** Called whenever the section's dirty state changes. Optional; defaults to no-op. */
  onDirtyChange?: (b: boolean) => void;
};

export function SectionBackups({ snapshot, onDirtyChange = () => {} }: SectionBackupsProps): preact.JSX.Element {
  if (snapshot.status !== "ok") {
    return (
      <section data-section="backups" class="mb-6 rounded-xl border border-app-border bg-app-card p-5 shadow-sm sm:p-6">
        <h2 class="m-0 text-lg font-semibold text-app-text">Backups</h2>
      </section>
    );
  }
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
  const [cleanErr, setCleanErr] = useState<string | null>(null);

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
    setCleanErr(null);
    try {
      // Pass the live slider draft so the global "Clean now" deletes exactly
      // what the preview eligible-badges showed at this keep_n — WYSIWYG even
      // when keep_n hasn't been Saved (Bug #2).
      await cleanBackups(draft);
      setConfirmOpen(false);
      await snapshot.refresh();
    } catch (e: any) {
      setConfirmOpen(false);
      setCleanErr(e?.message ?? "Clean-now failed");
    }
  }

  return (
    <section data-section="backups" class="mb-6 rounded-xl border border-app-border bg-app-card p-5 shadow-sm sm:p-6">
      <header class="mb-2 flex items-center gap-1.5">
        <h2 class="m-0 text-lg font-semibold text-app-text">Backups</h2>
        <InfoTip
          label="About this section"
          text="Retention applies per client. Each client keeps its newest N timestamped backups; older timestamped copies become eligible for cleanup. Original (pre-migration) backups are never deleted."
        />
      </header>
      <p class="m-0 mb-4 text-sm text-app-muted">Manage backup retention for managed client configs.</p>

      <div class="divide-y divide-app-border/60">
        <div class="flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 py-3">
          <label for="backups-keep-n-slider" class="flex items-center gap-1.5 text-sm font-medium text-app-text">
            {BACKUPS_COPY.sliderLabel}: <strong>{draft}</strong>
          </label>
          <div class="flex flex-col items-start gap-1 sm:items-end">
            <input
              id="backups-keep-n-slider"
              type="range"
              min={def.min ?? 0}
              max={def.max ?? 50}
              value={draft}
              disabled={busy}
              onInput={(e) => setDraft(Number((e.target as HTMLInputElement).value))}
            />
            <small class="text-xs text-app-muted">{BACKUPS_COPY.helperText}</small>
            {err ? <small class="settings-field-error text-xs text-app-danger" role="alert">{err}</small> : null}
          </div>
        </div>
      </div>

      <div class="mt-5 border-t border-app-border/60 pt-4">
        <BackupsList
          keepN={draft}
          // Bug-bash B2 closure (#21): per-client clean fires its own
          // refresh internally; this callback lets the parent (Settings)
          // also re-fetch the global snapshot so the global eligible
          // count + bulk preview stay consistent.
          onClientCleaned={() => void snapshot.refresh()}
        />

        <div class="backups-clean-row mt-4">
          <button
            type="button"
            class="btn-danger"
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
        {cleanErr ? <p class="error-banner" role="alert">Clean-now failed: {cleanErr}</p> : null}
      </div>

      <div class="settings-section-footer">
        {banner ? <span class="save-banner ok">{banner}</span> : null}
        <button type="button" class="btn-primary" disabled={!dirty || busy} onClick={() => void save()}>
          {busy ? "Saving…" : "Save"}
        </button>
        <button type="button" disabled={!dirty || busy} onClick={onReset}>Reset</button>
      </div>
    </section>
  );
}
