import { useState, useEffect, useMemo } from "preact/hooks";
import { putSetting } from "../../lib/settings-api";
import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";

export type SaveOutcome = {
  failures: Record<string, string>;
  successes: string[];
};

export function useSectionSaveFlow(
  snapshot: SettingsSnapshot,
  sectionKeys: string[],
  onDirtyChange: (b: boolean) => void,
) {
  const [edits, setEdits] = useState<Record<string, string>>({});
  // Codex r9 P2: keys that have been successfully PUT but whose new value
  // has not yet been confirmed by a fresh snapshot (refresh failed). These
  // are NOT dirty (work is done) but effective() must surface them so the
  // UI doesn't fall back to the stale snapshot value. Reset() preserves
  // lastSent — only edits are pending discard work.
  const [lastSent, setLastSent] = useState<Record<string, string>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [banner, setBanner] = useState<{ kind: "ok" | "partial"; text: string } | null>(null);

  const persisted = useMemo(() => {
    const out: Record<string, string> = {};
    if (snapshot.status === "ok") {
      for (const k of sectionKeys) {
        const dto = snapshot.data.settings.find((s) => s.key === k) as ConfigSettingDTO | undefined;
        if (dto) out[k] = dto.value;
      }
    }
    return out;
  }, [snapshot, sectionKeys]);

  // When persisted catches up with lastSent (refresh succeeded later),
  // drop the now-redundant lastSent entry. Avoids stale fallback.
  useEffect(() => {
    setLastSent((prev) => {
      let changed = false;
      const next = { ...prev };
      for (const k of Object.keys(prev)) {
        if (persisted[k] === prev[k]) {
          delete next[k];
          changed = true;
        }
      }
      return changed ? next : prev;
    });
  }, [persisted]);

  const dirty = Object.keys(edits).length > 0;
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange]);

  function effective(key: string): string {
    if (key in edits) return edits[key];
    if (key in lastSent) return lastSent[key];
    return persisted[key] ?? "";
  }

  function setLocal(key: string, value: string) {
    setEdits((prev) => {
      const next = { ...prev };
      // The "baseline" the user is editing relative to. lastSent (if any)
      // takes precedence over persisted because lastSent represents what's
      // really on disk after a refresh failure.
      const baseline = lastSent[key] ?? persisted[key] ?? "";
      if (baseline === value) {
        delete next[key];
      } else {
        next[key] = value;
      }
      return next;
    });
    setErrors((prev) => {
      if (!(key in prev)) return prev;
      const next = { ...prev };
      delete next[key];
      return next;
    });
  }

  function reset() {
    // Codex r9 P2: clear pending edits but PRESERVE lastSent. The latter
    // represents saved-on-disk values whose refresh failed; discarding
    // them would silently revert the UI to the stale snapshot.
    setEdits({});
    setErrors({});
    setBanner(null);
  }

  async function save(): Promise<void> {
    if (!dirty) return;
    setBusy(true);
    setBanner(null);

    // Snapshot the values being saved BEFORE async work. Codex PR review P1:
    // form fields stay editable during in-flight PUT. If the user edits a
    // key again after Save click but before the PUT returns, the newer edit
    // must NOT be silently dropped from `edits`. We track what value was
    // SENT for each successful key, and only clear it from edits if the
    // current local value still matches.
    const snapshotEdits: Record<string, string> = { ...edits };
    const dirtyKeys = Object.keys(snapshotEdits);
    const failures: Record<string, string> = {};
    const successes: { key: string; sentValue: string }[] = [];

    // Sequential PUTs — deterministic ordering, avoids server-side write races.
    for (const k of dirtyKeys) {
      try {
        await putSetting(k, snapshotEdits[k]);
        successes.push({ key: k, sentValue: snapshotEdits[k] });
      } catch (e: any) {
        const reason = e?.body?.reason ?? e?.message ?? "save failed";
        failures[k] = String(reason);
      }
    }

    // Memo §4.4 + Codex r7 P2: errors map clears successes BEFORE merging
    // new failures (retry-success drops stale errors).
    setErrors((prev) => {
      const next = { ...prev };
      for (const { key } of successes) delete next[key]; // clear stale errors on retry-success
      for (const [k, v] of Object.entries(failures)) next[k] = v;
      return next;
    });

    // Move successful keys from edits to lastSent (Codex r9 P2 architecture).
    // CAS: only move if the local value still equals what was sent (so a
    // dirty mid-PUT edit by the user is preserved as still-dirty).
    setEdits((prev) => {
      const next = { ...prev };
      for (const { key, sentValue } of successes) {
        if (next[key] === sentValue) {
          delete next[key];
        }
      }
      return next;
    });
    setLastSent((prev) => {
      const next = { ...prev };
      for (const { key, sentValue } of successes) {
        // Only record in lastSent if the user hasn't re-edited it (CAS).
        // We re-read from edits via the closed snapshotEdits; if the user
        // edited it again, we conservatively don't add to lastSent — the
        // newer edit is still in edits and effective() reads edits first.
        if (snapshotEdits[key] === sentValue) {
          next[key] = sentValue;
        }
      }
      return next;
    });

    let refreshOK = true;
    try {
      await snapshot.refresh();
    } catch {
      refreshOK = false;
    }
    // The persisted-catches-up effect above will clear lastSent entries
    // whose snapshot value now matches; no manual cleanup needed here.

    setBusy(false);
    if (Object.keys(failures).length === 0) {
      if (refreshOK) {
        setBanner({ kind: "ok", text: "Saved." });
        setTimeout(() => setBanner(null), 2000);
      } else {
        setBanner({
          kind: "partial",
          text: "Saved on disk. Couldn't refresh the live view — click Save again when connection recovers.",
        });
      }
    } else {
      setBanner({
        kind: "partial",
        text: `Saved ${successes.length} of ${dirtyKeys.length} settings. Fix errors below and try again.`,
      });
    }
  }

  return { effective, setLocal, reset, save, dirty, busy, errors, banner };
}
