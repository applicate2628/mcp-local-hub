// Placeholder so Settings.tsx compiles before Task 10 lands. Task 10
// replaces these with real per-section components and deletes this file
// entirely. Tasks 9 already moved Appearance/GuiServer/Daemons to their
// own per-section files.
import type { SettingsSnapshot } from "../../lib/settings-types";

type Props = { snapshot: SettingsSnapshot; onDirtyChange?: (b: boolean) => void };

export function SectionBackups(_: Props): preact.JSX.Element {
  return <section data-section="backups"><h2>Backups</h2></section>;
}
export function SectionAdvanced(_: Props): preact.JSX.Element {
  return <section data-section="advanced"><h2>Advanced</h2></section>;
}
