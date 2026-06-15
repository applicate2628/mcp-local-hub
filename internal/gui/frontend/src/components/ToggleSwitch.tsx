import type { JSX } from "preact";

// Props the ToggleSwitch owns explicitly. Everything else (data-*, aria-*,
// title, name, …) is spread straight onto the underlying <input> via the
// index signature below, so callers keep their existing selectors/attributes
// without ToggleSwitch having to enumerate them.
export type ToggleSwitchProps = {
  /** Controlled checked state. */
  checked: boolean;
  /** Disabled cell — renders dimmed and refuses interaction. */
  disabled?: boolean;
  /** Change handler — receives the native <input> change event. */
  onChange?: JSX.GenericEventHandler<HTMLInputElement>;
  /**
   * Accent / "pending" variant. When true the switch shows the unsaved-edit
   * cue (an accent ring + accent-tinted track) so a dirty toggle reads
   * visibly different from a clean one. The Servers matrix uses this to keep
   * the pending-direction cue the operator relied on with raw checkboxes.
   */
  pending?: boolean;
  /** Extra classes appended to the wrapping <span>. */
  class?: string;
  /**
   * Pass-through bag for data-* / aria-* / title / name / etc. These land on
   * the REAL <input type="checkbox"> under the hood, so existing test
   * selectors (`input[type="checkbox"]`, data-testids) and form semantics
   * keep working unchanged.
   */
  [key: `data-${string}`]: string | undefined;
  [key: `aria-${string}`]: string | undefined;
  title?: string;
  name?: string;
};

// ToggleSwitch — a polished, accessible sliding switch that is a real
// <input type="checkbox"> under the hood, visually hidden (NOT display:none —
// it stays in the a11y tree, focusable, and clickable) beneath a CSS track +
// knob styled from the app's design tokens (light/dark aware, focus-visible
// ring, smooth knob transition via .toggle-switch* in style.css).
//
// Why a hidden real checkbox rather than a div with role="switch": it
// preserves native form semantics, label association (drop it inside a
// <label> and a label-click toggles it once with no JS), keyboard handling
// (Space toggles), and every existing `input[type="checkbox"]` selector +
// `.checked`/`.click()` test assertion. role="switch" is added on the input so
// assistive tech announces it as a toggle, not a plain checkbox — the visual
// is a switch, so the semantics should match.
//
// Shared by design: the Servers matrix cells are the primary consumer, but any
// on/off checkbox can adopt this for a consistent look (DRY) without each call
// site re-implementing track/knob/focus styling.
export function ToggleSwitch({
  checked,
  disabled = false,
  onChange,
  pending = false,
  class: extraClass,
  ...rest
}: ToggleSwitchProps): JSX.Element {
  const cls = [
    "toggle-switch",
    pending ? "toggle-switch--pending" : "",
    disabled ? "toggle-switch--disabled" : "",
    extraClass ?? "",
  ]
    .filter(Boolean)
    .join(" ");
  return (
    <span class={cls} data-pending={pending ? "true" : undefined}>
      <input
        type="checkbox"
        role="switch"
        class="toggle-switch-input"
        checked={checked}
        disabled={disabled}
        aria-checked={checked}
        onChange={onChange}
        {...rest}
      />
      {/* Purely decorative — the track + sliding knob. aria-hidden so screen
          readers announce only the real input above, never this duplicate. */}
      <span class="toggle-switch-track" aria-hidden="true">
        <span class="toggle-switch-knob" />
      </span>
    </span>
  );
}
