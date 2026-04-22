// internal/gui/assets/logs.js
window.mcphub.screens.logs = function(root) {
  root.innerHTML = `
    <h1>Logs</h1>
    <div id="logs-controls">
      <select id="logs-server"></select>
      <label><input type="number" id="logs-tail" value="500" min="1" max="10000"> lines</label>
      <label><input type="checkbox" id="logs-follow"> Follow</label>
      <button id="logs-refresh">Refresh</button>
    </div>
    <pre id="logs-body"></pre>`;

  const sel = document.getElementById("logs-server");
  const tailEl = document.getElementById("logs-tail");
  const followEl = document.getElementById("logs-follow");
  const body = document.getElementById("logs-body");
  let es = null;

  // /api/status returns one row per (server, daemon) pair. Multi-daemon
  // servers (serena: claude + codex) appear as multiple rows with the
  // same `server` and different `daemon` names and no "default" daemon,
  // so we key the picker by (server, daemon) and render "server (daemon)"
  // whenever the daemon is not the single-daemon "default".
  //
  // Workspace-scoped lazy-proxy daemons (registered via `mcphub register
  // <workspace> <lang>`) write logs to lsp-<workspaceKey>-<language>.log
  // per internal/cli/daemon_workspace.go, not to the <server>-<daemon>.log
  // path that api.LogsGet reads. Selecting such a row from this dropdown
  // would hit api.LogsGet and show "no log output yet" even when the
  // workspace log file exists. Until Phase 3B-II adds proper workspace
  // log surfacing, filter them out of the picker using a structural
  // task_name match (rather than field-based on workspace/lifecycle,
  // which are registry-derived and may be empty if enrichment failed,
  // and must not let a workspace-proxy row sneak back into the picker).
  //
  // Mirror api.IsLazyProxyTaskName (internal/api/status_enrich.go):
  // lazy-proxy task names are strictly
  // `mcp-local-hub-lsp-<workspaceKey>-<language>`, where workspaceKey
  // is exactly 8 lowercase hex chars (api.WorkspaceKey) and language is
  // a non-empty suffix. A loose startsWith("mcp-local-hub-lsp-") check
  // would false-positive on a hypothetical global server named `lsp-*`
  // (e.g., `lsp-tools` → task `mcp-local-hub-lsp-tools-default`),
  // silently hiding its row from the picker. Match the canonical
  // structure directly.
  const LAZY_PROXY_RE = /^mcp-local-hub-lsp-[0-9a-f]{8}-[^/]+$/;
  function isWorkspaceScoped(row) {
    const tn = row && row.task_name ? String(row.task_name) : "";
    // Windows scheduler occasionally emits a leading backslash on task names.
    const stripped = tn.startsWith("\\") ? tn.slice(1) : tn;
    return LAZY_PROXY_RE.test(stripped);
  }

  // Backend returns the {error, code} JSON envelope (via writeAPIError) on
  // failures — not an array. The previous `(rows || []).filter(...)` would
  // treat the truthy error object as iterable and throw at .filter, leaving
  // Logs blank with the controls still enabled; a later Refresh click would
  // then try to JSON.parse("") in load() and throw again. Require resp.ok
  // AND an actual array before iterating; on failure surface the envelope
  // message and disable every downstream control so no later click or
  // change event can re-enter load() with an empty sel.value.
  fetch("/api/status").then(async r => {
    const data = await r.json().catch(() => null);
    if (!r.ok || !Array.isArray(data)) {
      body.textContent = "Failed to load status: " + (data?.error ?? r.statusText ?? "unknown");
      sel.disabled = true;
      document.getElementById("logs-refresh").disabled = true;
      followEl.disabled = true;
      return;
    }
    const eligible = data.filter(r => !isWorkspaceScoped(r));
    eligible.forEach(r => {
      const opt = document.createElement("option");
      const label = r.daemon && r.daemon !== "default" ? `${r.server} (${r.daemon})` : r.server;
      opt.value = JSON.stringify({server: r.server, daemon: r.daemon || ""});
      opt.textContent = label;
      sel.appendChild(opt);
    });
    if (eligible.length) {
      load();
    } else {
      const skipped = data.length - eligible.length;
      body.textContent = skipped > 0
        ? `No global-server logs available (${skipped} workspace-proxy entries hidden — Phase 3B-II will surface their lsp-<key>-<lang>.log files).`
        : "No daemons running.";
      // Disable controls when there are no eligible rows. Otherwise the
      // refresh/tail/follow event listeners can still invoke load(),
      // where JSON.parse(sel.value) throws on an empty "" value and
      // leaves the screen stuck in "Loading…". (The early return in
      // load() also guards this path; disabling the controls makes the
      // "nothing to pick" state visible to the user.)
      document.getElementById("logs-refresh").disabled = true;
      followEl.disabled = true;
      sel.disabled = true;
    }
  }).catch(err => {
    body.textContent = "Failed to load status: " + err.message;
    sel.disabled = true;
    document.getElementById("logs-refresh").disabled = true;
    followEl.disabled = true;
  });

  async function load() {
    if (es) { es.close(); es = null; }
    if (!sel.value) {
      // No eligible daemons (fresh install, or only workspace-proxy rows
      // hidden by the dropdown filter). The hint message is already in
      // body.textContent from the populate-dropdown branch above, so do
      // not overwrite it with "Loading…" and do not JSON.parse("").
      return;
    }
    body.textContent = "Loading…";
    const {server, daemon} = JSON.parse(sel.value);
    const tail = tailEl.value;
    const qs = `tail=${encodeURIComponent(tail)}` + (daemon ? `&daemon=${encodeURIComponent(daemon)}` : "");
    const resp = await fetch(`/api/logs/${encodeURIComponent(server)}?${qs}`);
    body.textContent = await resp.text();
    if (followEl.checked) startFollow(server, daemon);
  }
  function startFollow(server, daemon) {
    const qs = daemon ? `?daemon=${encodeURIComponent(daemon)}` : "";
    es = new EventSource(`/api/logs/${encodeURIComponent(server)}/stream${qs}`);
    es.addEventListener("log-line", e => {
      body.textContent += e.data + "\n";
      body.scrollTop = body.scrollHeight;
    });
  }
  sel.addEventListener("change", load);
  tailEl.addEventListener("change", load);
  followEl.addEventListener("change", load);
  document.getElementById("logs-refresh").addEventListener("click", load);

  // Close the active EventSource (if any) on screen swap. See dashboard.js
  // for the full rationale: #screen-root is reused across swaps so a
  // MutationObserver on removal never fires. The cleanup closure captures
  // `es` by reference, so it always sees the latest connection — whether
  // the user toggled Follow multiple times or never enabled it at all.
  window.mcphub.registerCleanup(() => { if (es) es.close(); });
};
