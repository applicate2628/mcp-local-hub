// SettingsCard — the shared card shell every Settings section renders.
//
// Before this component, each section repeated the identical card markup:
//
//   <section data-section="X" class="mb-6 rounded-xl border border-app-border
//                                     bg-app-card p-5 shadow-sm sm:p-6">
//     <header class="mb-2 flex items-center gap-1.5">
//       <h2 class="m-0 text-lg font-semibold text-app-text">Title</h2>
//       <InfoTip text="…" />
//     </header>
//     <p class="m-0 mb-4 text-sm text-app-muted">subtitle</p>
//     …section body…
//   </section>
//
// SettingsCard owns that shell verbatim so the class strings live in one
// place. It is a PURE presentational wrapper — NO behavior change: it emits
// the exact same DOM (same `data-section`, same class strings, same `<h2>`
// text, the same on-demand InfoTip in the header, the same muted subtitle).
// Sections keep ownership of their own dirty-guard / save-isolation / state;
// only the repeated chrome moved here.
//
// The `not-ok` fallback variants (rendered while a section's snapshot is
// loading/errored) are deliberately NOT routed through here — those differ
// per section (some drop the card classes entirely, none render the InfoTip
// or subtitle), so folding them in would risk a behavior change. They stay
// inline in each section.

import { InfoTip } from "../InfoTip";

export type SettingsCardProps = {
  /** The `data-section` anchor (scroll-spy + e2e selectors key on this). */
  section: string;
  /** The section heading text rendered in the `<h2>`. */
  title: string;
  /** The on-demand InfoTip help text shown in the header. */
  infoTip: string;
  /** Optional accessible label forwarded to the header InfoTip trigger. */
  infoTipLabel?: string;
  /** The muted one-line subtitle under the header. */
  subtitle: preact.ComponentChildren;
  /** The section body. */
  children: preact.ComponentChildren;
};

export function SettingsCard({
  section,
  title,
  infoTip,
  infoTipLabel,
  subtitle,
  children,
}: SettingsCardProps): preact.JSX.Element {
  return (
    <section
      data-section={section}
      class="mb-6 rounded-xl border border-app-border bg-app-card p-5 shadow-sm sm:p-6"
    >
      <header class="mb-2 flex items-center gap-1.5">
        <h2 class="m-0 text-lg font-semibold text-app-text">{title}</h2>
        <InfoTip text={infoTip} {...(infoTipLabel ? { label: infoTipLabel } : {})} />
      </header>
      <p class="m-0 mb-4 text-sm text-app-muted">{subtitle}</p>
      {children}
    </section>
  );
}
