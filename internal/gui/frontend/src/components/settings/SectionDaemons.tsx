import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";

export type SectionDaemonsProps = {
  snapshot: SettingsSnapshot;
};

// Memo §9.3 (Codex r1 P1.7): the labels MUST distinguish *configured*
// from *currently active*. A user-written-via-CLI deferred value must
// not be mis-read as runtime state.
export function SectionDaemons({ snapshot }: SectionDaemonsProps): preact.JSX.Element {
  if (snapshot.status === "loading") {
    return <section data-section="daemons" class="settings-section"><h2>Daemons</h2><p>Loading…</p></section>;
  }
  if (snapshot.status === "error") {
    return (
      <section data-section="daemons" class="settings-section">
        <h2>Daemons</h2>
        <p class="error-banner">Schedule unavailable.</p>
      </section>
    );
  }
  const sched = snapshot.data.settings.find((s) => s.key === "daemons.weekly_schedule") as ConfigSettingDTO | undefined;
  const retry = snapshot.data.settings.find((s) => s.key === "daemons.retry_policy") as ConfigSettingDTO | undefined;
  return (
    <section data-section="daemons" class="settings-section settings-section-readonly">
      <h2>Daemons</h2>
      <p class="settings-section-help">Background daemon settings.</p>
      <div class="readonly-row">
        <span class="readonly-label">Configured schedule:</span>
        <span class="readonly-value">{sched?.value ?? sched?.default ?? ""}</span>
        <span class="readonly-suffix">(effective in A4-b)</span>
      </div>
      <div class="readonly-row">
        <span class="readonly-label">Configured retry policy:</span>
        <span class="readonly-value">{retry?.value ?? retry?.default ?? ""}</span>
        <span class="readonly-suffix">(effective in A4-b)</span>
      </div>
      <p class="deferred-affordance">edit coming in A4-b</p>
    </section>
  );
}
