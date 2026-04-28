import type { Section } from "../../lib/settings-types";

const SECTION_ORDER: { id: Section; label: string }[] = [
  { id: "appearance", label: "Appearance" },
  { id: "gui_server", label: "GUI server" },
  { id: "daemons", label: "Daemons" },
  { id: "backups", label: "Backups" },
  { id: "advanced", label: "Advanced" },
];

export type SectionNavProps = {
  active: Section | null;
};

// SectionNav is the sticky, secondary in-screen nav. Each link uses
// `#/settings?section=<id>` which the existing useRouter parses as
// `route.query = "section=<id>"` (memo §8.5, Codex r1 P1.1).
export function SectionNav({ active }: SectionNavProps): preact.JSX.Element {
  return (
    <nav class="settings-section-nav" aria-label="Settings sections">
      {SECTION_ORDER.map((s) => (
        <a
          key={s.id}
          href={`#/settings?section=${s.id}`}
          class={active === s.id ? "active" : ""}
          aria-current={active === s.id ? "true" : undefined}
        >
          {s.label}
        </a>
      ))}
    </nav>
  );
}
