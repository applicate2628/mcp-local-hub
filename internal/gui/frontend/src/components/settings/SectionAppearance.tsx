import { useSectionSaveFlow } from "./useSectionSaveFlow";
import { InfoTip } from "../InfoTip";
import { SettingsCard } from "./SettingsCard";
import type { SettingsSnapshot, ConfigSettingDTO } from "../../lib/settings-types";

export type SectionAppearanceProps = {
  snapshot: SettingsSnapshot;
  onDirtyChange: (b: boolean) => void;
};

const SECTION_KEYS = [
  "appearance.theme",
  "appearance.density",
  "appearance.shell",
  "appearance.layout",
  "appearance.default_screen",
  "appearance.default_home",
];

export function SectionAppearance({ snapshot, onDirtyChange }: SectionAppearanceProps): preact.JSX.Element {
  const flow = useSectionSaveFlow(snapshot, SECTION_KEYS, onDirtyChange);
  if (snapshot.status !== "ok") return <section data-section="appearance"><h2>Appearance</h2></section>;
  const defs = SECTION_KEYS
    .map((k) => snapshot.data.settings.find((s) => s.key === k))
    .filter((s): s is ConfigSettingDTO => !!s && s.type !== "action");

  return (
    <SettingsCard
      section="appearance"
      title="Appearance"
      infoTip="Visual appearance of the GUI — theme, spacing density, default shell, navigation layout, and which screen opens first."
      subtitle="Theme, density, and navigation defaults."
    >
      <div class="divide-y divide-app-border/60">
        {defs.map((d) => (
          <AppearanceField
            key={d.key}
            def={d}
            value={flow.effective(d.key)}
            onChange={(v) => flow.setLocal(d.key, v)}
            error={flow.errors[d.key]}
          />
        ))}
      </div>
      <SectionFooter flow={flow} />
    </SettingsCard>
  );
}

type AppearanceFieldProps = {
  def: ConfigSettingDTO;
  value: string;
  onChange: (next: string) => void;
  error?: string;
};

// AppearanceField renders one registry def as a clean card row: label (with an
// on-demand InfoTip carrying the registry Help) on the left, the native control
// on the right. The control markup mirrors FieldRenderer exactly (same id,
// handlers, and aria-* bindings) — only the wrapping markup + className strings
// change. The `.settings-field` class is retained on the row wrapper because
// unit tests count fields by it.
function AppearanceField({ def, value, onChange, error }: AppearanceFieldProps): preact.JSX.Element {
  // Honor the registry Deferred flag exactly as the shared FieldRenderer does:
  // disable the control and render the "(coming in A4-b)" badge next to the
  // label. Without this, deferred-but-unwired settings (appearance.shell,
  // appearance.default_home) rendered as editable controls that silently did
  // nothing on save.
  const disabled = def.deferred === true;
  const ariaProps = error
    ? { "aria-invalid": true as const, "aria-describedby": `${def.key}-error` }
    : {};
  let control: preact.JSX.Element;
  let isCheckbox = false;
  switch (def.type) {
    case "enum":
      control = (
        <select
          id={def.key}
          class="field-ctl w-56"
          value={value}
          disabled={disabled}
          onChange={(e) => onChange((e.target as HTMLSelectElement).value)}
          {...ariaProps}
        >
          {(def.enum ?? []).map((opt) => (
            <option key={opt} value={opt}>{opt}</option>
          ))}
        </select>
      );
      break;
    case "bool":
      isCheckbox = true;
      control = (
        <input
          id={def.key}
          type="checkbox"
          class="h-4 w-4 accent-app-accent"
          checked={value === "true"}
          disabled={disabled}
          onChange={(e) => onChange((e.target as HTMLInputElement).checked ? "true" : "false")}
          {...ariaProps}
        />
      );
      break;
    case "int":
      control = (
        <input
          id={def.key}
          type="number"
          class="field-ctl w-20"
          value={value}
          disabled={disabled}
          min={def.min}
          max={def.max}
          onInput={(e) => onChange((e.target as HTMLInputElement).value)}
          {...ariaProps}
        />
      );
      break;
    case "string":
    case "path":
      control = (
        <input
          id={def.key}
          type="text"
          class="field-ctl w-64"
          value={value}
          disabled={disabled}
          onInput={(e) => onChange((e.target as HTMLInputElement).value)}
          {...ariaProps}
        />
      );
      break;
  }
  return (
    <div class={`settings-field flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 py-3${error ? " has-error" : ""}${disabled ? " disabled" : ""}`}>
      <label class="flex items-center gap-1.5 text-sm font-medium text-app-text" for={def.key}>
        {labelFromKey(def.key)}
        {disabled && def.deferred ? <span class="deferred-badge"> (coming in A4-b)</span> : null}
        {def.help ? <InfoTip text={def.help} /> : null}
      </label>
      {isCheckbox ? (
        control
      ) : (
        <div class="flex flex-col items-start gap-1 sm:items-end">
          {control}
          {error ? (
            <small id={`${def.key}-error`} class="settings-field-error text-xs text-app-danger" role="alert">{error}</small>
          ) : null}
        </div>
      )}
    </div>
  );
}

function labelFromKey(key: string): string {
  // "appearance.theme" → "theme"; "gui_server.browser_on_launch" → "browser on launch"
  const last = key.split(".").pop() || key;
  return last.replace(/_/g, " ");
}

export function SectionFooter({ flow }: { flow: ReturnType<typeof useSectionSaveFlow> }): preact.JSX.Element {
  return (
    <div class="settings-section-footer">
      {flow.banner ? <span class={`save-banner ${flow.banner.kind}`}>{flow.banner.text}</span> : null}
      <button type="button" class="btn-primary" disabled={!flow.dirty || flow.busy} onClick={() => void flow.save()}>
        {flow.busy ? "Saving…" : "Save"}
      </button>
      <button type="button" disabled={!flow.dirty || flow.busy} onClick={flow.reset}>Reset</button>
    </div>
  );
}
