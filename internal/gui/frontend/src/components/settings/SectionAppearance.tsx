import { FieldRenderer } from "./FieldRenderer";
import { useSectionSaveFlow } from "./useSectionSaveFlow";
import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";

export type SectionAppearanceProps = {
  snapshot: SettingsSnapshot;
  onDirtyChange: (b: boolean) => void;
};

const SECTION_KEYS = [
  "appearance.theme",
  "appearance.density",
  "appearance.shell",
  "appearance.default_home",
];

export function SectionAppearance({ snapshot, onDirtyChange }: SectionAppearanceProps): preact.JSX.Element {
  const flow = useSectionSaveFlow(snapshot, SECTION_KEYS, onDirtyChange);
  if (snapshot.status !== "ok") return <section data-section="appearance"><h2>Appearance</h2></section>;
  const defs = SECTION_KEYS
    .map((k) => snapshot.data.settings.find((s) => s.key === k))
    .filter((s): s is ConfigSettingDTO => !!s && s.type !== "action");

  return (
    <section data-section="appearance" class="settings-section">
      <h2>Appearance</h2>
      <p class="settings-section-help">Visual appearance of the GUI.</p>
      {defs.map((d) => (
        <FieldRenderer
          key={d.key}
          def={d}
          value={flow.effective(d.key)}
          onChange={(v) => flow.setLocal(d.key, v)}
          error={flow.errors[d.key]}
        />
      ))}
      <SectionFooter flow={flow} />
    </section>
  );
}

export function SectionFooter({ flow }: { flow: ReturnType<typeof useSectionSaveFlow> }): preact.JSX.Element {
  return (
    <div class="settings-section-footer">
      {flow.banner ? <span class={`save-banner ${flow.banner.kind}`}>{flow.banner.text}</span> : null}
      <button type="button" disabled={!flow.dirty || flow.busy} onClick={() => void flow.save()}>
        {flow.busy ? "Saving…" : "Save"}
      </button>
      <button type="button" disabled={!flow.dirty || flow.busy} onClick={flow.reset}>Reset</button>
    </div>
  );
}
