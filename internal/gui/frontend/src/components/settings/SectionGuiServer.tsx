// SectionGuiServer — card-based Settings panel for how the GUI server runs.
//
// Each field is rendered inline (label-left / control-right) rather than
// delegated to the shared FieldRenderer, because the card row layout
// (InfoTip-in-label, field-ctl controls, checkbox-on-the-right) is
// incompatible with FieldRenderer's self-contained .settings-field column
// markup, and FieldRenderer is shared by other sections so it must not
// change. The control markup below preserves FieldRenderer's exact contract:
// id={def.key}, the same onChange/onInput value semantics, the same
// disabled/aria wiring, the deferred "(coming in A4-b)" badge text, and the
// `${def.key}-error` inline error. Only wrapping markup + className strings
// differ. Save/Reset still go through the shared SectionFooter + save flow,
// and both restart-required badges keep their data-test-id anchors.

import { useState } from "preact/hooks";
import { SectionFooter } from "./SectionAppearance";
import { useSectionSaveFlow } from "./useSectionSaveFlow";
import { InfoTip } from "../InfoTip";
import { restartSupervisor } from "../../api";
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

// labelFromKey mirrors FieldRenderer.labelFromKey so the human-readable
// label is identical to the not-yet-redesigned sections.
// "gui_server.browser_on_launch" → "browser on launch".
function labelFromKey(key: string): string {
  const last = key.split(".").pop() || key;
  return last.replace(/_/g, " ");
}

export function SectionGuiServer({ snapshot, onDirtyChange }: SectionGuiServerProps): preact.JSX.Element {
  const flow = useSectionSaveFlow(snapshot, EDITABLE_KEYS, onDirtyChange);
  // Restart-supervisor affordance for the port / hub-endpoint restart-required
  // badges. CAVEAT: restarting the supervisor (POST /api/supervisor/restart)
  // restarts the daemons, NOT the GUI process — the GUI OWNS the supervisor
  // (it spawns it), so a port change (a GUI-listener concern) still needs the
  // mcphub GUI itself relaunched. There is no GUI-self-restart endpoint yet
  // (ROADMAP: "Settings GUI self-restart endpoint"); until then the badges tell
  // the user to relaunch the GUI, and this button only restarts the daemons.
  const [restartBusy, setRestartBusy] = useState(false);
  const [restartMsg, setRestartMsg] = useState<{ kind: "ok" | "error"; text: string } | null>(null);
  async function doRestart() {
    if (restartBusy) return;
    setRestartBusy(true);
    setRestartMsg(null);
    try {
      const res = await restartSupervisor();
      if (res.spawned) {
        setRestartMsg({ kind: "ok", text: "Supervisor + daemons restarted. A GUI port or hub-endpoint change also needs the mcphub GUI itself relaunched (close and reopen) — the GUI can't restart its own listener yet." });
      } else {
        const detail = res.per_step_error
          ? Object.entries(res.per_step_error).map(([k, v]) => `${k}: ${v}`).join("; ")
          : "supervisor did not respawn";
        setRestartMsg({ kind: "error", text: `Restart incomplete: ${detail}` });
      }
    } catch (e: any) {
      setRestartMsg({ kind: "error", text: e?.message ?? "Restart failed" });
    } finally {
      setRestartBusy(false);
    }
  }
  if (snapshot.status !== "ok") {
    return (
      <section data-section="gui_server" class="mb-6 rounded-xl border border-app-border bg-app-card p-5 shadow-sm sm:p-6">
        <h2 class="m-0 text-lg font-semibold text-app-text">GUI server</h2>
      </section>
    );
  }

  const portDef = snapshot.data.settings.find((s) => s.key === "gui_server.port") as ConfigSettingDTO;
  const persistedPort = Number(portDef.value);
  const actualPort = snapshot.data.actual_port;
  // Codex r3 P2.1 + r4 P2.1: badge anchored to PERSISTED port, NOT local draft.
  const showPortBadge = !Number.isNaN(persistedPort) && actualPort !== persistedPort;

  // Issue #161 P2 closure: persisted-vs-runtime hub gate badge.
  // The snapshot DTO now emits `actual_hub_endpoint_enabled` (added
  // in internal/gui/settings.go), so we can render the same
  // restart-required convention as the port badge — when the
  // persisted value differs from the runtime state.
  const hubDef = snapshot.data.settings.find((s) => s.key === "gui_server.hub_endpoint_enabled") as
    | ConfigSettingDTO
    | undefined;
  const persistedHubEnabled = hubDef?.value === "true";
  const actualHubEnabled = snapshot.data.actual_hub_endpoint_enabled === true;
  const showHubBadge = hubDef !== undefined && persistedHubEnabled !== actualHubEnabled;

  return (
    <section data-section="gui_server" class="mb-6 rounded-xl border border-app-border bg-app-card p-5 shadow-sm sm:p-6">
      <header class="mb-2 flex items-center gap-1.5">
        <h2 class="m-0 text-lg font-semibold text-app-text">GUI server</h2>
        <InfoTip text="Controls how the local GUI server runs: whether a browser opens on launch, which port it listens on, whether the hub endpoint is exposed, and the tray icon. Port and hub-endpoint changes take effect after a restart." />
      </header>
      <p class="m-0 mb-4 text-sm text-app-muted">How the GUI server runs.</p>

      <div class="divide-y divide-app-border/60">
        {SECTION_KEYS.map((k) => {
          const def = snapshot.data.settings.find((s: SettingDTO) => s.key === k) as ConfigSettingDTO | undefined;
          if (!def) return null;
          const editable = EDITABLE_KEYS.includes(k);
          const value = editable ? flow.effective(k) : def.value;
          const disabled = !editable || def.deferred;
          const error = flow.errors[k];
          const onChange = (v: string) => editable && flow.setLocal(k, v);
          const ariaProps = error
            ? { "aria-invalid": true as const, "aria-describedby": `${def.key}-error` }
            : {};

          let control: preact.JSX.Element;
          if (def.type === "int") {
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
          } else {
            // bool — every gui_server field other than port is a checkbox.
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
          }

          return (
            <div key={k} class="flex flex-wrap items-center justify-between gap-x-4 gap-y-1.5 py-3">
              <label class="flex items-center gap-1.5 text-sm font-medium text-app-text" for={def.key}>
                {labelFromKey(def.key)}
                {disabled && def.deferred ? <span class="deferred-badge"> (coming in A4-b)</span> : null}
                {def.help ? <InfoTip text={def.help} /> : null}
              </label>
              <div class="flex flex-col items-start gap-1 sm:items-end">
                {control}
                {error ? (
                  <small id={`${def.key}-error`} class="settings-field-error text-xs text-app-danger" role="alert">
                    {error}
                  </small>
                ) : null}
                {k === "gui_server.port" && showPortBadge ? (
                  <span class="settings-restart-badge" data-test-id="port-restart-badge" role="status">
                    ⚠ Restart required — port {persistedPort} takes effect after the mcphub GUI is relaunched (close and reopen the app).
                  </span>
                ) : null}
                {k === "gui_server.hub_endpoint_enabled" && showHubBadge ? (
                  <span
                    class="settings-restart-badge"
                    data-test-id="hub-endpoint-restart-badge"
                    role="status"
                  >
                    ⚠ Restart required — hub endpoint {persistedHubEnabled ? "ON" : "OFF"} takes effect after the mcphub GUI restart (close and reopen the app).
                  </span>
                ) : null}
              </div>
            </div>
          );
        })}
      </div>

      {showPortBadge || showHubBadge ? (
        <div class="mt-3 flex flex-wrap items-center gap-3">
          <button
            type="button"
            onClick={() => void doRestart()}
            disabled={restartBusy}
            data-testid="gui-server-restart-now"
          >
            {restartBusy ? "Restarting…" : "Restart supervisor + daemons"}
          </button>
          {restartMsg ? (
            <span
              class={`save-banner ${restartMsg.kind}`}
              role={restartMsg.kind === "error" ? "alert" : "status"}
              data-testid="gui-server-restart-msg"
            >
              {restartMsg.text}
            </span>
          ) : null}
        </div>
      ) : null}

      <SectionFooter flow={flow} />
    </section>
  );
}
