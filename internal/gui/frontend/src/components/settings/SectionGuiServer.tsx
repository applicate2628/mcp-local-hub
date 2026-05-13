import { FieldRenderer } from "./FieldRenderer";
import { SectionFooter } from "./SectionAppearance";
import { useSectionSaveFlow } from "./useSectionSaveFlow";
import type { SettingsSnapshot, ConfigSettingDTO, SettingDTO } from "../../lib/settings-types";

export type SectionGuiServerProps = {
  snapshot: SettingsSnapshot;
  onDirtyChange: (b: boolean) => void;
};

const SECTION_KEYS = [
  "gui_server.browser_on_launch",
  "gui_server.port",
  "gui_server.hub_endpoint_enabled",
  "gui_server.tray",
];
const EDITABLE_KEYS = [
  "gui_server.browser_on_launch",
  "gui_server.port",
  "gui_server.hub_endpoint_enabled",
];

export function SectionGuiServer({ snapshot, onDirtyChange }: SectionGuiServerProps): preact.JSX.Element {
  const flow = useSectionSaveFlow(snapshot, EDITABLE_KEYS, onDirtyChange);
  if (snapshot.status !== "ok") return <section data-section="gui_server"><h2>GUI server</h2></section>;

  const portDef = snapshot.data.settings.find((s) => s.key === "gui_server.port") as ConfigSettingDTO;
  const persistedPort = Number(portDef.value);
  const actualPort = snapshot.data.actual_port;
  // Codex r3 P2.1 + r4 P2.1: badge anchored to PERSISTED port, NOT local draft.
  const showPortBadge = !Number.isNaN(persistedPort) && actualPort !== persistedPort;

  // Phase 5 Task 5.4: pending-restart indicator for the hub-endpoint
  // toggle. Per codex bot phase5 r2 P3 closure on PR #160, the
  // restart badge MUST reflect persisted-vs-runtime divergence
  // (matching the port badge convention), NOT draft-vs-persisted.
  // The snapshot DTO does not currently expose the live hub gate
  // state (no actual_hub_endpoint_enabled field), so we cannot
  // emit a true persisted-vs-runtime badge in this Phase 5 commit.
  //
  // Decision: drop the badge for this PR. The Deferred:true
  // registry flag already surfaces "Restart required" in the field
  // help text via FieldRenderer, which is the minimum operator
  // signal. The runtime-state surface will be added in a follow-up
  // (see follow-up issue #159 / Phase 5 deferrals).
  //
  // hubDef lookup retained for the EDITABLE_KEYS render path below;
  // the badge JSX is intentionally absent.
  const hubDef = snapshot.data.settings.find((s) => s.key === "gui_server.hub_endpoint_enabled") as
    | ConfigSettingDTO
    | undefined;
  void hubDef;

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
            {/* Hub-endpoint restart badge intentionally absent — see
                hubDef lookup above for the rationale (codex bot
                phase5 r2 P3 closure on PR #160). Deferred:true in
                the registry already surfaces "Restart required" via
                FieldRenderer; runtime-state badge needs a snapshot
                DTO extension tracked as follow-up. */}
          </div>
        );
      })}
      <SectionFooter flow={flow} />
    </section>
  );
}
