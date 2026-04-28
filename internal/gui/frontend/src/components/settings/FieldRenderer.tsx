import type { ConfigSettingDTO } from "../../lib/settings-types";

export type FieldRendererProps = {
  def: ConfigSettingDTO;
  value: string;
  onChange: (next: string) => void;
  disabled?: boolean;
  error?: string;
};

// FieldRenderer maps registry def types to native HTML controls.
// Memo §8.3.
export function FieldRenderer({ def, value, onChange, disabled, error }: FieldRendererProps): preact.JSX.Element {
  const ariaProps = error
    ? { "aria-invalid": true as const, "aria-describedby": `${def.key}-error` }
    : {};
  let control: preact.JSX.Element;
  switch (def.type) {
    case "enum":
      control = (
        <select
          id={def.key}
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
      control = (
        <input
          id={def.key}
          type="checkbox"
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
          value={value}
          disabled={disabled}
          onInput={(e) => onChange((e.target as HTMLInputElement).value)}
          {...ariaProps}
        />
      );
      break;
  }
  return (
    <div class={`settings-field${error ? " has-error" : ""}${disabled ? " disabled" : ""}`}>
      <label for={def.key} class="settings-field-label">
        {labelFromKey(def.key)}
        {disabled && def.deferred ? <span class="deferred-badge"> (coming in A4-b)</span> : null}
      </label>
      {control}
      {def.help ? <small class="settings-field-help">{def.help}</small> : null}
      {error ? <small id={`${def.key}-error`} class="settings-field-error" role="alert">{error}</small> : null}
    </div>
  );
}

function labelFromKey(key: string): string {
  // "appearance.theme" → "theme"; "gui_server.browser_on_launch" → "browser on launch"
  const last = key.split(".").pop() || key;
  return last.replace(/_/g, " ");
}
