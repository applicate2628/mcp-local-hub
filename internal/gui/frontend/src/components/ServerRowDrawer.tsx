// ServerRowDrawer — a Flowbite right-placement Drawer opened per server row
// in the Servers matrix. Three panes on one daemon-group:
//
//   1. Manifest preview — GET /api/manifest/get (reuses api.getManifest) and
//      renders the raw YAML read-only so the operator can eyeball the spec
//      without leaving the matrix.
//   2. Lifetime stats — projected from the /api/status DaemonStatus rows the
//      Servers screen already loaded (state / port / PID / uptime / RAM,
//      per-daemon). No extra fetch; the parent threads the rows in.
//   3. Stop / Restart — POST /api/servers/<name>/{stop,restart}, the SAME
//      endpoints the Dashboard cards use.
//
// Flowbite vocabulary: the shell uses Flowbite's drawer classes
// (`fixed top-0 right-0 z-40 h-screen ... bg-white w-80` + the
// `transition-transform translate-x-full` off-screen start), the documented
// `data-drawer-*` attributes, and Flowbite Button / Badge / Card Tailwind
// classes for the controls. Show/hide is driven by Flowbite's `Drawer` JS
// instance (constructed in an effect) so the slide-in + backdrop are the real
// Flowbite behavior, not a hand-rolled CSS toggle.

import { useEffect, useRef, useState } from "preact/hooks";
import { Drawer } from "flowbite";
import { getManifest } from "../api";
import { formatBytes, formatUptime } from "../lib/format";
import { stateShape } from "../lib/status";
import type { DaemonStatus } from "../types";

export interface ServerRowDrawerProps {
  // Server name (manifest + endpoint identity). The drawer is keyed on this
  // by the parent so re-opening for a different row remounts cleanly.
  serverName: string;
  // The /api/status rows for THIS server (already loaded by the Servers
  // screen). Empty array ⇒ no live daemon (renders an empty-state line).
  daemons: DaemonStatus[];
  // onClose hides the drawer (parent owns the open/closed flag). Called from
  // the × button, the backdrop click (via Flowbite onHide), and Esc.
  onClose: () => void;
}

// Per-variant Flowbite Badge class for the daemon State chip. Running →
// green; everything else → red, matching the matrix's state coloring.
function stateBadgeClass(state: string): string {
  return state === "Running"
    ? "bg-green-100 text-green-800 dark:bg-green-900 dark:text-green-300"
    : "bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-300";
}

export function ServerRowDrawer(props: ServerRowDrawerProps) {
  const { serverName, daemons, onClose } = props;
  const drawerElRef = useRef<HTMLDivElement>(null);
  const drawerInstRef = useRef<Drawer | null>(null);
  const [yaml, setYaml] = useState<string | null>(null);
  const [yamlErr, setYamlErr] = useState<string | null>(null);
  const [actionMsg, setActionMsg] = useState<{ text: string; kind: "ok" | "error" } | null>(null);
  const [busy, setBusy] = useState<"" | "stop" | "restart">("");
  const mountedRef = useRef(true);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
    };
  }, []);

  // Construct the Flowbite Drawer instance once and slide it in. Flowbite
  // handles the backdrop, body-scroll lock, and the translate transition;
  // onHide fires for backdrop / Esc dismiss so the parent's open flag stays
  // in sync with the actual visible state.
  useEffect(() => {
    const el = drawerElRef.current;
    if (!el) return;
    const inst = new Drawer(el, {
      placement: "right",
      backdrop: true,
      bodyScrolling: false,
      onHide: () => {
        // Only propagate when the component is still mounted; the cleanup
        // below also calls hide() which would re-enter onHide otherwise.
        if (mountedRef.current) onClose();
      },
    });
    drawerInstRef.current = inst;
    inst.show();
    return () => {
      // Hide on unmount so the backdrop + body-scroll lock are released even
      // if the parent unmounts us directly (rather than via onClose).
      inst.hide();
      drawerInstRef.current = null;
    };
    // serverName in deps so opening a different row rebuilds the instance.
  }, [serverName, onClose]);

  // Fetch the manifest YAML on open (and whenever the row changes).
  useEffect(() => {
    let cancelled = false;
    setYaml(null);
    setYamlErr(null);
    getManifest(serverName)
      .then((res) => {
        if (!cancelled) setYaml(res.yaml);
      })
      .catch((err: Error) => {
        if (!cancelled) setYamlErr(err.message);
      });
    return () => {
      cancelled = true;
    };
  }, [serverName]);

  async function postAction(action: "stop" | "restart", daemon?: string) {
    if (busy) return;
    setBusy(action);
    setActionMsg(null);
    let url = `/api/servers/${encodeURIComponent(serverName)}/${action}`;
    if (daemon && daemon !== "default") url += `?daemon=${encodeURIComponent(daemon)}`;
    try {
      const resp = await fetch(url, { method: "POST" });
      const body = (await resp.json().catch(() => ({}))) as {
        error?: string;
        [k: string]: unknown;
      };
      if (resp.status === 207) {
        const rows = (body[`${action}_results`] as Array<{ task_name: string; error: string }>) ?? [];
        const failed = rows.filter((r) => r.error !== "");
        throw new Error(`partial ${action} failure: ${failed.map((r) => `${r.task_name}: ${r.error}`).join("; ")}`);
      }
      if (!resp.ok) throw new Error(body.error ?? String(resp.status));
      if (!mountedRef.current) return;
      setActionMsg({ text: `${action === "stop" ? "Stopped" : "Restarted"} ${serverName}.`, kind: "ok" });
    } catch (err) {
      if (!mountedRef.current) return;
      setActionMsg({ text: (err as Error).message, kind: "error" });
    } finally {
      if (mountedRef.current) setBusy("");
    }
  }

  const anyRunning = daemons.some((d) => d.state === "Running");

  return (
    <div
      ref={drawerElRef}
      id={`server-row-drawer-${serverName}`}
      class="fixed top-0 right-0 z-40 h-screen p-4 overflow-y-auto transition-transform translate-x-full bg-white w-96 dark:bg-gray-800"
      tabIndex={-1}
      aria-labelledby={`server-row-drawer-label-${serverName}`}
      data-testid="server-row-drawer"
      data-server={serverName}
    >
      <h5
        id={`server-row-drawer-label-${serverName}`}
        class="inline-flex items-center mb-4 text-base font-semibold text-gray-500 dark:text-gray-400"
      >
        <svg class="w-4 h-4 me-2.5" aria-hidden="true" xmlns="http://www.w3.org/2000/svg" fill="currentColor" viewBox="0 0 20 20">
          <path d="M10 .5a9.5 9.5 0 1 0 9.5 9.5A9.51 9.51 0 0 0 10 .5ZM9.5 4a1.5 1.5 0 1 1 0 3 1.5 1.5 0 0 1 0-3ZM12 15H8a1 1 0 0 1 0-2h1v-3H8a1 1 0 0 1 0-2h2a1 1 0 0 1 1 1v4h1a1 1 0 0 1 0 2Z" />
        </svg>
        {serverName}
      </h5>
      <button
        type="button"
        data-drawer-hide={`server-row-drawer-${serverName}`}
        aria-controls={`server-row-drawer-${serverName}`}
        class="text-gray-400 bg-transparent hover:bg-gray-200 hover:text-gray-900 rounded-lg text-sm w-8 h-8 absolute top-2.5 end-2.5 inline-flex items-center justify-center dark:hover:bg-gray-600 dark:hover:text-white"
        data-testid="server-row-drawer-close"
        onClick={onClose}
      >
        <svg class="w-3 h-3" aria-hidden="true" xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 14 14">
          <path stroke="currentColor" stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="m1 1 6 6m0 0 6 6M7 7l6-6M7 7l-6 6" />
        </svg>
        <span class="sr-only">Close menu</span>
      </button>

      {/* ── Lifetime stats (Flowbite Card per daemon) ──────────────── */}
      <section data-testid="server-row-drawer-stats" class="mb-6">
        <h6 class="text-sm font-semibold text-gray-700 dark:text-gray-300 mb-2">Lifetime stats</h6>
        {daemons.length === 0 ? (
          <p class="text-sm text-gray-500 dark:text-gray-400" data-testid="server-row-drawer-no-daemons">
            No live daemon for this server.
          </p>
        ) : (
          daemons.map((d) => {
            const uptime = formatUptime(d.uptime_sec);
            const ram = formatBytes(d.ram_bytes);
            return (
              <div
                key={`${d.server}/${d.daemon}`}
                class="p-4 mb-3 bg-white border border-gray-200 rounded-lg shadow-sm dark:bg-gray-700 dark:border-gray-600"
                data-testid={`server-row-drawer-daemon-${d.daemon}`}
              >
                <div class="flex items-center justify-between mb-2">
                  <span class="text-sm font-medium text-gray-900 dark:text-white">
                    {d.display_name || (d.daemon && d.daemon !== "default" ? `${d.server} (${d.daemon})` : d.server)}
                  </span>
                  <span
                    class={`text-xs font-medium px-2.5 py-0.5 rounded-sm ${stateBadgeClass(d.state)}`}
                    data-testid={`server-row-drawer-state-${d.daemon}`}
                  >
                    <span aria-hidden="true">{stateShape(d.state)}</span> {d.state}
                  </span>
                </div>
                <dl class="grid grid-cols-2 gap-x-4 gap-y-1 text-sm text-gray-600 dark:text-gray-300">
                  <dt class="font-normal text-gray-500 dark:text-gray-400">Port</dt>
                  <dd class="text-right">{d.port || "—"}</dd>
                  <dt class="font-normal text-gray-500 dark:text-gray-400">PID</dt>
                  <dd class="text-right">{d.pid || "—"}</dd>
                  {uptime ? (
                    <>
                      <dt class="font-normal text-gray-500 dark:text-gray-400">Uptime</dt>
                      <dd class="text-right" data-testid={`server-row-drawer-uptime-${d.daemon}`}>{uptime}</dd>
                    </>
                  ) : null}
                  {ram ? (
                    <>
                      <dt class="font-normal text-gray-500 dark:text-gray-400">RAM</dt>
                      <dd class="text-right" data-testid={`server-row-drawer-ram-${d.daemon}`}>{ram}</dd>
                    </>
                  ) : null}
                </dl>
              </div>
            );
          })
        )}
      </section>

      {/* ── Stop / Restart (Flowbite Buttons) ──────────────────────── */}
      <section class="mb-6">
        <div class="flex gap-2">
          <button
            type="button"
            class="text-white bg-blue-700 hover:bg-blue-800 focus:ring-4 focus:ring-blue-300 font-medium rounded-lg text-sm px-4 py-2 focus:outline-none disabled:opacity-50 disabled:cursor-not-allowed dark:bg-blue-600 dark:hover:bg-blue-700"
            onClick={() => postAction("restart")}
            disabled={busy !== ""}
            data-testid="server-row-drawer-restart"
          >
            {busy === "restart" ? "Restarting…" : "Restart"}
          </button>
          <button
            type="button"
            class="text-red-700 bg-white border border-red-300 hover:bg-red-50 focus:ring-4 focus:ring-red-100 font-medium rounded-lg text-sm px-4 py-2 focus:outline-none disabled:opacity-50 disabled:cursor-not-allowed dark:bg-gray-800 dark:text-red-400 dark:border-red-700 dark:hover:bg-gray-700"
            onClick={() => postAction("stop")}
            disabled={busy !== "" || !anyRunning}
            data-testid="server-row-drawer-stop"
          >
            {busy === "stop" ? "Stopping…" : "Stop"}
          </button>
        </div>
        {actionMsg && (
          <div
            class={`mt-3 p-3 text-sm rounded-lg ${
              actionMsg.kind === "error"
                ? "text-red-800 bg-red-50 dark:bg-gray-800 dark:text-red-400"
                : "text-green-800 bg-green-50 dark:bg-gray-800 dark:text-green-400"
            }`}
            role="alert"
            data-testid="server-row-drawer-action-msg"
          >
            {actionMsg.text}
          </div>
        )}
      </section>

      {/* ── Manifest preview (read-only YAML) ──────────────────────── */}
      <section data-testid="server-row-drawer-manifest">
        <h6 class="text-sm font-semibold text-gray-700 dark:text-gray-300 mb-2">Manifest preview</h6>
        {yamlErr ? (
          <div
            class="p-3 text-sm text-red-800 bg-red-50 rounded-lg dark:bg-gray-800 dark:text-red-400"
            role="alert"
            data-testid="server-row-drawer-manifest-err"
          >
            {yamlErr}
          </div>
        ) : yaml === null ? (
          <p class="text-sm text-gray-500 dark:text-gray-400">Loading manifest…</p>
        ) : (
          <pre
            class="p-3 overflow-x-auto text-xs font-mono text-gray-800 bg-gray-50 border border-gray-200 rounded-lg dark:bg-gray-900 dark:text-gray-200 dark:border-gray-700"
            data-testid="server-row-drawer-manifest-yaml"
          >
            {yaml}
          </pre>
        )}
      </section>
    </div>
  );
}
