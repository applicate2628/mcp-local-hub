// SectionDaemons — editable cron + retry policy + register-time default knob
// + per-workspace WeeklyRefresh membership table. ONE Save button drives
// THREE sequential transactional operations per memo D4:
//
//   op 1: settings — putSetting per dirty key (knob, retry policy)
//   op 2: weekly schedule — putWeeklySchedule(value), parses + swaps task
//   op 3: membership — putMembership(deltas), idempotent partial update
//
// Partial-failure semantics (memo D4):
//   - op 1 fails → ops 2+3 skipped; banner names failed key. Settings dirty.
//   - op 2 fails → op 3 skipped. Schedule field stays dirty; settings ops 1
//     committed. Banner carries restore_status + manual_recovery.
//   - op 3 fails → ops 1+2 already committed; membership stays dirty.
//
// The banner copy MUST explicitly tell the user which prior ops committed
// and which remain dirty — that is the D4 "UI messaging contract".
//
// Reset reverts the three field values to baseline. The WeeklyMembershipTable
// resets via section-level remount (parent's `key={discardKey}` pattern in
// app.tsx); this component does NOT call into the table to clear it.

import { useEffect, useMemo, useState } from "preact/hooks";
import { putSetting } from "../../lib/settings-api";
import {
  putMembership,
  putWeeklySchedule,
  type MembershipDelta,
  type WeeklyScheduleParseError,
  type WeeklyScheduleSwapFailure,
} from "../../lib/api-daemons";
import { WeeklyMembershipTable } from "./WeeklyMembershipTable";
import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";

export type SectionDaemonsProps = {
  snapshot: SettingsSnapshot;
  /** Bubbles dirty-state for the cross-section unsaved-changes guard. Optional. */
  onDirtyChange?: (b: boolean) => void;
};

type Banner =
  | { kind: "ok"; text: string }
  | { kind: "partial"; text: string }
  | { kind: "error"; text: string };

const SCHEDULE_KEY = "daemons.weekly_schedule";
const RETRY_KEY = "daemons.retry_policy";
const KNOB_KEY = "daemons.weekly_refresh_default";

export function SectionDaemons({
  snapshot,
  onDirtyChange = () => {},
}: SectionDaemonsProps): preact.JSX.Element {
  if (snapshot.status === "loading") {
    return (
      <section data-section="daemons" class="settings-section">
        <h2>Daemons</h2>
        <p>Loading…</p>
      </section>
    );
  }
  if (snapshot.status === "error") {
    return (
      <section data-section="daemons" class="settings-section">
        <h2>Daemons</h2>
        <p class="error-banner">Schedule unavailable.</p>
      </section>
    );
  }

  const sched = snapshot.data.settings.find((s) => s.key === SCHEDULE_KEY) as ConfigSettingDTO | undefined;
  const retry = snapshot.data.settings.find((s) => s.key === RETRY_KEY) as ConfigSettingDTO | undefined;
  const knob = snapshot.data.settings.find((s) => s.key === KNOB_KEY) as ConfigSettingDTO | undefined;

  const persistedSched = sched?.value ?? sched?.default ?? "";
  const persistedRetry = retry?.value ?? retry?.default ?? "";
  const persistedKnob = (knob?.value ?? knob?.default ?? "false") === "true";

  const [schedValue, setSchedValue] = useState<string>(persistedSched);
  const [retryValue, setRetryValue] = useState<string>(persistedRetry);
  const [knobValue, setKnobValue] = useState<boolean>(persistedKnob);
  const [tableDirty, setTableDirty] = useState(false);
  const [tableDeltas, setTableDeltas] = useState<MembershipDelta[]>([]);
  const [busy, setBusy] = useState(false);
  const [banner, setBanner] = useState<Banner | null>(null);
  const [schedError, setSchedError] = useState<string | null>(null);

  // Re-anchor field state when the underlying snapshot value changes
  // (e.g. a successful refresh after a save). Fields are disabled during
  // busy=true so the user cannot edit during save; once persistedX moves,
  // the baseline moves and dirty correctly drops back to false. Mirrors
  // SectionBackups's `useEffect(() => setDraft(baseline), [baseline])`.
  useEffect(() => { setSchedValue(persistedSched); }, [persistedSched]);
  useEffect(() => { setRetryValue(persistedRetry); }, [persistedRetry]);
  useEffect(() => { setKnobValue(persistedKnob); }, [persistedKnob]);

  const schedDirty = schedValue !== persistedSched;
  const retryDirty = retryValue !== persistedRetry;
  const knobDirty = knobValue !== persistedKnob;
  const dirty = schedDirty || retryDirty || knobDirty || tableDirty;

  useEffect(() => onDirtyChange(dirty), [dirty, onDirtyChange]);

  const retryOptions = useMemo<string[]>(() => retry?.enum ?? ["none", "linear", "exponential"], [retry]);

  function reset() {
    setSchedValue(persistedSched);
    setRetryValue(persistedRetry);
    setKnobValue(persistedKnob);
    setBanner(null);
    setSchedError(null);
    // tableDirty resets via parent's discardKey remount (app.tsx pattern).
  }

  async function save() {
    if (!dirty) return;
    setBusy(true);
    setBanner(null);
    setSchedError(null);

    // Track which ops committed so the partial-failure banner can name
    // them explicitly per D4's UI messaging contract.
    const committed: string[] = [];
    const stillDirty: string[] = [];

    // -------- op 1: settings (knob + retry policy) --------
    // One key fails → STOP; do not attempt ops 2+3.
    let op1Failure: string | null = null;
    if (knobDirty) {
      try {
        await putSetting(KNOB_KEY, knobValue ? "true" : "false");
        committed.push(humanKey(KNOB_KEY));
      } catch (e: any) {
        op1Failure = `${humanKey(KNOB_KEY)}: ${errReason(e)}`;
      }
    }
    if (op1Failure === null && retryDirty) {
      try {
        await putSetting(RETRY_KEY, retryValue);
        committed.push(humanKey(RETRY_KEY));
      } catch (e: any) {
        op1Failure = `${humanKey(RETRY_KEY)}: ${errReason(e)}`;
      }
    }
    if (op1Failure !== null) {
      // Whatever didn't commit stays dirty.
      if (knobDirty && !committed.includes(humanKey(KNOB_KEY))) stillDirty.push(humanKey(KNOB_KEY));
      if (retryDirty && !committed.includes(humanKey(RETRY_KEY))) stillDirty.push(humanKey(RETRY_KEY));
      if (schedDirty) stillDirty.push("schedule");
      if (tableDirty) stillDirty.push("membership");
      await refreshBestEffort();
      setBusy(false);
      setBanner({
        kind: "error",
        text: composeBanner(committed, stillDirty, `Settings save failed: ${op1Failure}.`),
      });
      return;
    }

    // -------- op 2: weekly schedule swap --------
    let op2Failure: string | null = null;
    if (schedDirty) {
      try {
        await putWeeklySchedule(schedValue);
        committed.push("schedule");
      } catch (raw: unknown) {
        // The thrown envelope is either WeeklyScheduleParseError (400) or
        // WeeklyScheduleSwapFailure (5xx). They share `error` + `detail`
        // but the discriminator value distinguishes them. Cast to an
        // open record to read both shapes safely without forcing a
        // type-narrow over a runtime-checked branch.
        const err = (raw ?? {}) as Record<string, unknown>;
        if (err.error === "parse_error") {
          const parseErr = err as unknown as WeeklyScheduleParseError;
          // Inline-only error per D8: parse failure is re-editable; the
          // banner does not need to repeat the detail.
          const example = parseErr.example ? ` (example: ${parseErr.example})` : "";
          setSchedError(`${parseErr.detail}${example}`);
          op2Failure = `Schedule input rejected.`;
        } else {
          // 5xx swap failure — surface restore_status + manual_recovery.
          const swapErr = err as unknown as Partial<WeeklyScheduleSwapFailure>;
          const restore = swapErr.restore_status ?? "n/a";
          const recovery = swapErr.manual_recovery ? ` ${swapErr.manual_recovery}` : "";
          op2Failure = `Schedule update failed (restore: ${restore}). ${swapErr.detail ?? ""}${recovery}`.trim();
        }
      }
    }
    if (op2Failure !== null) {
      stillDirty.push("schedule");
      if (tableDirty) stillDirty.push("membership");
      await refreshBestEffort();
      setBusy(false);
      setBanner({
        kind: "partial",
        text: composeBanner(committed, stillDirty, op2Failure),
      });
      return;
    }

    // -------- op 3: membership update --------
    let op3Failure: string | null = null;
    if (tableDirty && tableDeltas.length > 0) {
      try {
        await putMembership(tableDeltas);
        committed.push("membership");
      } catch (e: any) {
        op3Failure = `Membership update failed: ${errReason(e)}.`;
      }
    } else if (tableDirty) {
      // Edge case: dirty flag but no deltas (shouldn't happen, but guard).
      committed.push("membership");
    }
    if (op3Failure !== null) {
      stillDirty.push("membership");
      await refreshBestEffort();
      setBusy(false);
      setBanner({
        kind: "partial",
        text: composeBanner(committed, stillDirty, op3Failure),
      });
      return;
    }

    // -------- success --------
    await refreshBestEffort();
    setBusy(false);
    setBanner({ kind: "ok", text: "Saved." });
    setTimeout(() => setBanner(null), 2000);
  }

  async function refreshBestEffort() {
    try {
      await snapshot.refresh();
    } catch {
      /* ignore — banner already conveys status */
    }
  }

  return (
    <section data-section="daemons" class="settings-section">
      <h2>Daemons</h2>
      <p class="settings-section-help">
        Background daemon settings. One Save runs settings, schedule swap, and membership update in sequence;
        if one step fails the banner names which prior steps committed and which remain dirty.
      </p>

      <div class="settings-field-row">
        <label class="settings-field-label" for="daemons-weekly-schedule">Weekly schedule</label>
        <input
          id="daemons-weekly-schedule"
          type="text"
          value={schedValue}
          disabled={busy}
          onInput={(e) => {
            setSchedValue((e.target as HTMLInputElement).value);
            if (schedError) setSchedError(null);
          }}
          aria-invalid={schedError ? true : undefined}
          aria-describedby={schedError ? "daemons-weekly-schedule-error" : undefined}
          data-testid="daemons-weekly-schedule-input"
        />
        {sched?.help ? <small class="settings-field-help">{sched.help}</small> : null}
        {schedError ? (
          <small id="daemons-weekly-schedule-error" class="settings-field-error" role="alert">
            {schedError}
          </small>
        ) : null}
      </div>

      <div class="settings-field-row">
        <label class="settings-field-label" for="daemons-retry-policy">Retry policy</label>
        <select
          id="daemons-retry-policy"
          value={retryValue}
          disabled={busy}
          onChange={(e) => setRetryValue((e.target as HTMLSelectElement).value)}
          data-testid="daemons-retry-policy-select"
        >
          {retryOptions.map((opt) => (
            <option key={opt} value={opt}>{opt}</option>
          ))}
        </select>
        {retry?.help ? <small class="settings-field-help">{retry.help}</small> : null}
      </div>

      <div class="settings-field-row">
        <label class="settings-field-label" for="daemons-weekly-refresh-default">
          <input
            id="daemons-weekly-refresh-default"
            type="checkbox"
            checked={knobValue}
            disabled={busy}
            onChange={(e) => setKnobValue((e.target as HTMLInputElement).checked)}
            data-testid="daemons-weekly-refresh-default-checkbox"
          />
          {" "}Default for new workspaces: enroll in weekly refresh
        </label>
        {knob?.help ? <small class="settings-field-help">{knob.help}</small> : null}
      </div>

      <WeeklyMembershipTable
        onDirtyChange={setTableDirty}
        onDeltasChange={setTableDeltas}
      />

      <div class="settings-section-footer">
        {banner ? (
          <span class={`save-banner ${banner.kind}`} role="status" data-testid="daemons-save-banner">
            {banner.text}
          </span>
        ) : null}
        <button
          type="button"
          disabled={!dirty || busy}
          onClick={() => void save()}
          data-testid="daemons-save"
        >
          {busy ? "Saving…" : "Save"}
        </button>
        <button
          type="button"
          disabled={!dirty || busy}
          onClick={reset}
          data-testid="daemons-reset"
        >
          Reset
        </button>
      </div>
    </section>
  );
}

function humanKey(key: string): string {
  switch (key) {
    case KNOB_KEY:
      return "weekly refresh default";
    case RETRY_KEY:
      return "retry policy";
    case SCHEDULE_KEY:
      return "weekly schedule";
    default:
      return key;
  }
}

function errReason(e: any): string {
  return String(e?.body?.reason ?? e?.message ?? "unknown error");
}

// composeBanner builds the D4-mandated "what committed / what's still dirty"
// banner copy. The canonical sample copy from the memo D4 contract is:
//   "Settings saved. Schedule update failed (degraded restore — see
//    Recovery). Membership not attempted; still dirty."
//
// We follow the same shape: sentence 1 lists committed ops (or "Nothing
// committed" if empty); sentence 2 carries the failure detail; sentence 3
// names what stays dirty.
function composeBanner(committed: string[], stillDirty: string[], failure: string): string {
  const parts: string[] = [];
  if (committed.length > 0) {
    parts.push(`Saved: ${committed.join(", ")}.`);
  } else {
    parts.push("Nothing committed.");
  }
  parts.push(failure);
  if (stillDirty.length > 0) {
    parts.push(`Still dirty: ${stillDirty.join(", ")}.`);
  }
  return parts.join(" ");
}
