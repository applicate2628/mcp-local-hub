import { FieldRenderer } from "./FieldRenderer";
import { SectionFooter } from "./SectionAppearance";
import { useSectionSaveFlow } from "./useSectionSaveFlow";
import type { SettingsSnapshot, ConfigSettingDTO, SettingDTO } from "../../lib/settings-types";

export type SectionGuiServerProps = {
  snapshot: SettingsSnapshot;
  onDirtyChange: (b: boolean) => void;
};

const SECTION_KEYS = ["gui_server.browser_on_launch", "gui_server.port", "gui_server.tray"];
const EDITABLE_KEYS = ["gui_server.browser_on_launch", "gui_server.port"];

export function SectionGuiServer({ snapshot, onDirtyChange }: SectionGuiServerProps): preact.JSX.Element {
  const flow = useSectionSaveFlow(snapshot, EDITABLE_KEYS, onDirtyChange);
  if (snapshot.status !== "ok") return <section data-section="gui_server"><h2>GUI server</h2></section>;

  const portDef = snapshot.data.settings.find((s) => s.key === "gui_server.port") as ConfigSettingDTO;
  const persistedPort = Number(portDef.value);
  const actualPort = snapshot.data.actual_port;
  // Codex r3 P2.1 + r4 P2.1: badge anchored to PERSISTED port, NOT local draft.
  const showPortBadge = !Number.isNaN(persistedPort) && actualPort !== persistedPort;

  return (
    <section data-section="gui_server" class="settings-section">
      <h2>GUI server</h2>
      <p class="settings-section-help">How the GUI server runs.</p>
      {SECTION_KEYS.map((k) => {
        const def = snapshot.data.settings.find((s: SettingDTO) => s.key === k) as ConfigSettingDTO | undefined;
        if (!def) return null;
        const editable = EDITABLE_KEYS.includes(k);
        return (
          <div key={k} class="settings-field-row">
            <FieldRenderer
              def={def}
              value={editable ? flow.effective(k) : def.value}
              onChange={(v) => editable && flow.setLocal(k, v)}
              disabled={!editable || def.deferred}
              error={flow.errors[k]}
            />
            {k === "gui_server.port" && showPortBadge ? (
              <span class="settings-restart-badge" data-test-id="port-restart-badge" role="status">
                ⚠ Restart required — port {persistedPort} will take effect after restart
              </span>
            ) : null}
          </div>
        );
      })}
      <SectionFooter flow={flow} />
    </section>
  );
}
