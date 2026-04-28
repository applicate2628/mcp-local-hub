import { useState, useEffect, useMemo } from "preact/hooks";
import { putSetting } from "../../lib/settings-api";
import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";

export type SaveOutcome = {
  // Map of dirty-key → outcome: success or error message.
  failures: Record<string, string>;
  successes: string[];
};

export function useSectionSaveFlow(
  snapshot: SettingsSnapshot,
  sectionKeys: string[],
  onDirtyChange: (b: boolean) => void,
) {
  // local edited values, keyed by registry key. Empty = clean.
  const [edits, setEdits] = useState<Record<string, string>>({});
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

  const dirty = Object.keys(edits).length > 0;
  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange]);

  function effective(key: string): string {
    return edits[key] ?? persisted[key] ?? "";
  }

  function setLocal(key: string, value: string) {
    setEdits((prev) => {
      const next = { ...prev };
      // If matches persisted, drop from edits (clean).
      if ((persisted[key] ?? "") === value) {
        delete next[key];
      } else {
        next[key] = value;
      }
      return next;
    });
    // Clear that key's error on edit.
    setErrors((prev) => {
      if (!(key in prev)) return prev;
      const next = { ...prev };
      delete next[key];
      return next;
    });
  }

  function reset() {
    setEdits({});
    setErrors({});
    setBanner(null);
  }

  async function save(): Promise<void> {
    if (!dirty) return;
    setBusy(true);
    setBanner(null);
    const dirtyKeys = Object.keys(edits);
    const failures: Record<string, string> = {};
    const successes: string[] = [];
    // Sequential PUTs — deterministic ordering, avoids server-side write races.
    for (const k of dirtyKeys) {
      try {
        await putSetting(k, edits[k]);
        successes.push(k);
      } catch (e: any) {
        const reason = e?.body?.reason ?? e?.message ?? "save failed";
        failures[k] = String(reason);
      }
    }
    // Memo §4.4 merge rule: drop successes from edits + errors; keep failures dirty.
    // Codex r7 P2: errors map must clear successes BEFORE merging new failures —
    // otherwise a key that failed previously and now saved successfully would
    // keep its stale inline error message while becoming clean.
    setEdits((prev) => {
      const next = { ...prev };
      for (const k of successes) delete next[k];
      return next;
    });
    setErrors((prev) => {
      const next = { ...prev };
      for (const k of successes) delete next[k]; // clear stale errors on retry-success
      for (const [k, v] of Object.entries(failures)) next[k] = v;
      return next;
    });
    setBusy(false);
    if (Object.keys(failures).length === 0) {
      setBanner({ kind: "ok", text: "Saved." });
      setTimeout(() => setBanner(null), 2000);
    } else {
      setBanner({
        kind: "partial",
        text: `Saved ${successes.length} of ${dirtyKeys.length} settings. Fix errors below and try again.`,
      });
    }
    // Refresh the snapshot AFTER the merge so successful keys re-anchor cleanly.
    await snapshot.refresh();
  }

  return { effective, setLocal, reset, save, dirty, busy, errors, banner };
}
