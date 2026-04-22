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
  // log surfacing, filter them out of the picker using the same structural
  // predicate as internal/cli/status.go `filterWorkspaceScoped`: task_name
  // prefix `mcp-local-hub-lsp-`. Structural (task_name) rather than
  // field-based (workspace/lifecycle) because those fields are registry-
  // derived and may be empty if enrichment failed, which must not let a
  // workspace-proxy row sneak back into the picker.
  const LAZY_PROXY_PREFIX = "mcp-local-hub-lsp-";
  function isWorkspaceScoped(row) {
    const tn = row && row.task_name ? String(row.task_name) : "";
    // Windows scheduler occasionally emits a leading backslash on task names.
    const stripped = tn.startsWith("\\") ? tn.slice(1) : tn;
    return stripped.startsWith(LAZY_PROXY_PREFIX);
  }

  fetch("/api/status").then(r => r.json()).then(rows => {
    const all = rows || [];
    const eligible = all.filter(r => !isWorkspaceScoped(r));
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
      const skipped = all.length - eligible.length;
      body.textContent = skipped > 0
        ? `No global-server logs available (${skipped} workspace-proxy entries hidden — Phase 3B-II will surface their lsp-<key>-<lang>.log files).`
        : "No daemons running.";
    }
  });

  async function load() {
    if (es) { es.close(); es = null; }
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
