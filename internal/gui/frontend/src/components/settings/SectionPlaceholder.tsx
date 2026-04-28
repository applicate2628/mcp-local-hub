// Placeholder so Settings.tsx compiles before Tasks 9 + 10 land. Tasks 9
// + 10 replace these with real per-section components (memo §8.1, §9.x,
// §10.x). Task 9 trims this file to retain only SectionBackups +
// SectionAdvanced; Task 10 deletes the file entirely.
import type { SettingsSnapshot } from "../../lib/settings-types";

type Props = { snapshot: SettingsSnapshot; onDirtyChange?: (b: boolean) => void };

export function SectionAppearance(_: Props): preact.JSX.Element {
  return <section data-section="appearance"><h2>Appearance</h2></section>;
}
export function SectionGuiServer(_: Props): preact.JSX.Element {
  return <section data-section="gui_server"><h2>GUI server</h2></section>;
}
export function SectionDaemons(_: Props): preact.JSX.Element {
  return <section data-section="daemons"><h2>Daemons</h2></section>;
}
export function SectionBackups(_: Props): preact.JSX.Element {
  return <section data-section="backups"><h2>Backups</h2></section>;
}
export function SectionAdvanced(_: Props): preact.JSX.Element {
  return <section data-section="advanced"><h2>Advanced</h2></section>;
}
