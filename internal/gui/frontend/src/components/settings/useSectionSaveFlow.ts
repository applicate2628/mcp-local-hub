import { useState, useEffect, useMemo, useRef } from "preact/hooks";
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
  // Ref mirror of edits — kept in sync by setLocal, reset, and save so that
  // save() can read the live edit value synchronously during async PUT loops
  // without requiring a nested functional-setState trick.
  // Codex r12 P2: used for CAS gating of failure errors.
  const editsRef = useRef<Record<string, string>>({});
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
      editsRef.current = next; // keep ref in sync
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
    editsRef.current = {};
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

    // Read live edits from the ref — set synchronously by setLocal/reset,
    // so it accurately reflects any mid-PUT re-edits the user made.
    // Codex r12 P2: used for CAS gating of failure errors and lastSent writes.
    const liveEdits = editsRef.current;

    // Memo §4.4 + Codex r7 P2: errors map clears successes BEFORE merging
    // new failures (retry-success drops stale errors).
    // Codex r12 P2: only attach a failure error if the live edit still
    // matches the sent value. If the user re-edited during the in-flight PUT,
    // the failure referenced an OLD value they have moved on from; drop it.
    setErrors((prev) => {
      const next = { ...prev };
      for (const { key } of successes) delete next[key]; // clear stale errors on retry-success
      for (const [k, v] of Object.entries(failures)) {
        // liveEdits[k] is the value visible in the field right now;
        // snapshotEdits[k] is what was actually sent to the server.
        // Match = user hasn't re-edited; mismatch = stale failure, drop it.
        if (liveEdits[k] === snapshotEdits[k]) {
          next[k] = v;
        }
      }
      return next;
    });

    // CAS-clear successful keys from edits (r6 P2).
    setEdits((prev) => {
      const next = { ...prev };
      for (const { key, sentValue } of successes) {
        if (next[key] === sentValue) {
          delete next[key];
        }
      }
      editsRef.current = next; // keep ref in sync with cleared successes
      return next;
    });

    // Move successful keys to lastSent (Codex r9 P2 architecture).
    // CAS: only record in lastSent if the user hasn't re-edited (live value
    // still equals what was sent). If they re-edited, their newer value is
    // in edits and effective() reads edits first — no lastSent needed.
    setLastSent((prev) => {
      const next = { ...prev };
      for (const { key, sentValue } of successes) {
        if (liveEdits[key] === sentValue) {
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
        // Codex r12 P3: after a refresh failure the section is clean (edits
        // cleared, lastSent holds the value). Save is disabled — telling the
        // user to "click Save again" is misleading. Suggest reload instead.
        setBanner({
          kind: "partial",
          text: "Saved on disk. The live view didn't refresh — reload or revisit Settings to confirm.",
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
