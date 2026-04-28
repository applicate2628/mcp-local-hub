import { useState, useEffect } from "preact/hooks";
import type { RouterState } from "../hooks/useRouter";
import type { Section, SettingsSnapshot } from "../lib/settings-types";
import { SectionNav } from "../components/settings/SectionNav";
import { SectionAppearance } from "../components/settings/SectionAppearance";
import { SectionGuiServer } from "../components/settings/SectionGuiServer";
import { SectionDaemons } from "../components/settings/SectionDaemons";
import { SectionBackups } from "../components/settings/SectionBackups";
import { SectionAdvanced } from "../components/settings/SectionAdvanced";

export type SettingsScreenProps = {
  route: RouterState;
  onDirtyChange: (b: boolean) => void;
  // Codex PR #20 r11 P2: snapshot lifted to App level to avoid a
  // duplicate-instance race where App's stale fetch overwrites Settings's
  // just-saved theme/density attributes.
  snapshot: SettingsSnapshot;
};

const SECTION_IDS: Section[] = ["appearance", "gui_server", "daemons", "backups", "advanced"];

// Codex r1 P3.1 — destructure all props.
export function SettingsScreen({ route, onDirtyChange, snapshot }: SettingsScreenProps): preact.JSX.Element {
  const [appearanceDirty, setAppearanceDirty] = useState(false);
  const [guiServerDirty, setGuiServerDirty] = useState(false);
  const [backupsDirty, setBackupsDirty] = useState(false);
  const anyDirty = appearanceDirty || guiServerDirty || backupsDirty;

  useEffect(() => {
    onDirtyChange(anyDirty);
  }, [anyDirty, onDirtyChange]);

  const [activeSection, setActiveSection] = useState<Section | null>(null);

  // Scroll-spy via IntersectionObserver — flag the deepest in-viewport
  // section. This is registry-driven (no hardcoded selectors per section).
  useEffect(() => {
    const sections = SECTION_IDS
      .map((id) => document.querySelector<HTMLElement>(`section[data-section="${id}"]`))
      .filter((el): el is HTMLElement => el !== null);
    if (sections.length === 0) return;
    const observer = new IntersectionObserver(
      (entries) => {
        const visible = entries
          .filter((e) => e.isIntersecting)
          .sort((a, b) => b.intersectionRatio - a.intersectionRatio);
        if (visible.length > 0) {
          const id = visible[0].target.getAttribute("data-section") as Section | null;
          if (id) setActiveSection(id);
        }
      },
      { rootMargin: "-10% 0px -70% 0px", threshold: [0.1, 0.5, 0.9] },
    );
    for (const s of sections) observer.observe(s);
    return () => observer.disconnect();
  }, [snapshot.status]);

  // Deep-link on mount and on hash change. Memo §8.5 (Codex r1 P1.1).
  useEffect(() => {
    const params = new URLSearchParams(route.query ?? "");
    const target = params.get("section");
    if (target && SECTION_IDS.includes(target as Section)) {
      // Wait one tick so sections have mounted + measured.
      const id = setTimeout(() => {
        const el = document.querySelector<HTMLElement>(`section[data-section="${target}"]`);
        el?.scrollIntoView({ behavior: "smooth", block: "start" });
      }, 0);
      return () => clearTimeout(id);
    }
  }, [route.query, snapshot.status]);

  // Codex PR #20 r11 P2: theme/density apply removed from here.
  // App.tsx is the sole owner of the lifted snapshot and applies data-theme /
  // data-density in its own useEffect. When SectionAppearance saves and calls
  // snapshot.refresh(), App's effect re-runs on the refreshed data — there is
  // now only one apply pipeline, so no overwrite race is possible.

  if (snapshot.status === "loading") {
    return (
      <div class="settings-screen loading">
        <h1>Settings</h1>
        <p>Loading…</p>
      </div>
    );
  }
  if (snapshot.status === "error") {
    return (
      <div class="settings-screen error">
        <h1>Settings</h1>
        <p class="error-banner">Could not load settings: {(snapshot.error as Error).message}</p>
        <button type="button" onClick={() => void snapshot.refresh()}>Retry</button>
      </div>
    );
  }

  return (
    <div class="settings-screen settings-layout">
      <SectionNav active={activeSection} />
      <div class="settings-body">
        <h1>Settings</h1>
        <SectionAppearance snapshot={snapshot} onDirtyChange={setAppearanceDirty} />
        <SectionGuiServer  snapshot={snapshot} onDirtyChange={setGuiServerDirty}  />
        <SectionDaemons    snapshot={snapshot} />
        <SectionBackups    snapshot={snapshot} onDirtyChange={setBackupsDirty}    />
        <SectionAdvanced   snapshot={snapshot} />
      </div>
    </div>
  );
}
