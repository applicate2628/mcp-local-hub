// internal/gui/assets/servers.js
window.mcphub.screens.servers = async function(root) {
  root.innerHTML = `
    <h1>Servers</h1>
    <div id="servers-toolbar">
      <button id="apply-migrate" disabled>Apply changes</button>
      <span id="apply-status" style="margin-left:12px"></span>
    </div>
    <div id="servers-content">Loading…</div>`;
  const content = document.getElementById("servers-content");
  const applyBtn = document.getElementById("apply-migrate");
  const applyStatus = document.getElementById("apply-status");
  // Per-cell dirty tracking: server name -> Set<client name>. A (server, client)
  // pair is dirty iff the user's checked state differs from defaultChecked.
  // Tracking per-cell (instead of per-server) is load-bearing: /api/migrate's
  // ClientsInclude narrows the rewrite to the listed clients, so sending ONLY
  // the affected (server, client) pairs prevents one flipped checkbox from
  // silently rewriting every other client binding on that server.
  const dirty = new Map();

  function onCheckboxChange(e) {
    const server = e.target.dataset.server;
    const client = e.target.dataset.client;
    const initial = e.target.defaultChecked;
    if (e.target.checked !== initial) {
      let clients = dirty.get(server);
      if (!clients) { clients = new Set(); dirty.set(server, clients); }
      clients.add(client);
    } else {
      const clients = dirty.get(server);
      if (clients) {
        clients.delete(client);
        if (clients.size === 0) dirty.delete(server);
      }
    }
    // Apply stays enabled as long as ANY cell (across any server) is dirty.
    applyBtn.disabled = dirty.size === 0;
  }

  async function applyChanges() {
    // Migrate is called PER-SERVER-GROUP, not once with a unioned client list.
    // MigrateOpts.ClientsInclude is a global filter applied to every Server in
    // the request, so a single call with {servers:[A,B], clients:[claude,gemini]}
    // would rewrite all four cells (A×claude, A×gemini, B×claude, B×gemini)
    // even if the user only dirtied (A,claude) and (B,gemini). Looping keeps
    // each server's client list scoped to exactly its own dirty cells.
    const changes = [];
    for (const [server, clients] of dirty.entries()) {
      if (clients.size === 0) continue;
      changes.push({server, clients: [...clients]});
    }
    applyBtn.disabled = true;
    applyStatus.textContent = `Migrating ${changes.length} server(s)…`;
    const failed = [];
    for (const change of changes) {
      try {
        const resp = await fetch("/api/migrate", {
          method: "POST",
          headers: {"Content-Type": "application/json"},
          body: JSON.stringify({servers: [change.server], clients: change.clients}),
        });
        if (!resp.ok) {
          const body = await resp.json().catch(() => ({error: resp.status}));
          failed.push(`${change.server}: ${body.error}`);
        }
      } catch (e) {
        failed.push(`${change.server}: ${e.message}`);
      }
    }
    if (failed.length === 0) {
      applyStatus.textContent = "Migrated. Refreshing…";
      dirty.clear();
      render(); // reload the matrix
    } else {
      applyStatus.innerHTML = `<span class="error">Failed: ${failed.join("; ")}</span>`;
      applyBtn.disabled = false;
    }
  }
  applyBtn.addEventListener("click", applyChanges);

  async function render() {
    content.textContent = "Loading…";
    try {
      const [scan, status] = await Promise.all([
        fetch("/api/scan").then(r => r.json()),
        fetch("/api/status").then(r => r.json()),
      ]);
      content.innerHTML = "";
      const table = document.createElement("table");
      table.className = "servers-matrix";
      const clients = ["claude-code", "codex-cli", "gemini-cli", "antigravity"];
      table.innerHTML = `<thead><tr><th>Server</th>${clients.map(c => `<th>${c}</th>`).join("")}<th>Port</th><th>State</th></tr></thead>`;
      const tbody = document.createElement("tbody");
      const statusByServer = aggregateStatus(status);
      const servers = collectServers(scan);
      for (const server of servers) {
        const row = document.createElement("tr");
        const st = statusByServer[server.name] || {};
        // server.name originates from client-config map keys (see /api/scan),
        // which are already constrained — but defense in depth escapes any
        // HTML metacharacters before interpolating into innerHTML. The GUI
        // binds 127.0.0.1 only (spec §2.2), so cross-origin XSS is not the
        // live threat model; this prevents a malicious config key from
        // injecting markup into the matrix regardless.
        row.innerHTML = `
          <td>${escapeHtml(server.name)}</td>
          ${clients.map(c => renderCell(server, c)).join("")}
          <td>${st.port ?? "—"}</td>
          <td>${st.state ?? "—"}</td>`;
        tbody.appendChild(row);
      }
      table.appendChild(tbody);
      content.appendChild(table);
      content.querySelectorAll("input[type=checkbox]").forEach(cb => cb.addEventListener("change", onCheckboxChange));
    } catch (err) {
      content.innerHTML = `<p class="error">Failed to load: ${err.message}</p>`;
    }
  }
  render();
};

// aggregateStatus collapses /api/status's per-(server, daemon) rows into one
// row per server for the matrix display. Multi-daemon servers (serena ships
// claude + codex) otherwise had the second iterated daemon overwrite the
// first in an Object.fromEntries([server, s]) derivation, masking the case
// where one daemon was down while the other was Running.
//
// The aggregate state is:
//   - "Running"  iff every daemon for this server reports Running
//   - "All <X>"  (reuses first state) when every daemon is non-Running
//   - "Partial"  when states are mixed
// The representative port is the lowest non-zero port for stability and so
// one running daemon's port stays visible even when another daemon is down.
function aggregateStatus(rows) {
  const grouped = {};
  for (const r of rows || []) {
    if (!grouped[r.server]) grouped[r.server] = [];
    grouped[r.server].push(r);
  }
  const out = {};
  for (const [server, daemons] of Object.entries(grouped)) {
    const states = daemons.map(d => d.state);
    const allRunning = states.every(s => s === "Running");
    const allStopped = states.every(s => s !== "Running");
    let aggregate;
    if (allRunning) aggregate = "Running";
    else if (allStopped) aggregate = states[0] ?? "Stopped";
    else aggregate = "Partial";
    const ports = daemons.map(d => d.port).filter(p => p > 0).sort((a, b) => a - b);
    out[server] = {
      server,
      state: aggregate,
      port: ports[0] ?? null,
      daemonCount: daemons.length,
    };
  }
  return out;
}

// escapeHtml replaces HTML-significant characters with their entity forms
// so user-controlled strings can be safely interpolated into innerHTML or
// attribute values. Used on server.name and the data-server attribute on
// matrix checkboxes — both flow from /api/scan's client-config map keys.
function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c]));
}

// collectServers adapts api.ScanResult into a flat list of
// {name, routing:{client: "via-hub"|"direct"|"not-installed"|...}}.
// The actual ScanResult shape is {at, entries:[{name, status, client_presence:{client:{transport, endpoint, raw}}, ...}]}.
// Per-server `status` is an aggregate across clients, so per-client routing
// is derived from client_presence[client].transport + endpoint.
function collectServers(scan) {
  const entries = (scan && scan.entries) || [];
  const out = entries.map(e => ({
    name: e.name,
    routing: perClientRouting(e.client_presence || {}),
  }));
  return out.sort((a, b) => a.name.localeCompare(b.name));
}

// perClientRouting maps ClientPresence into the per-cell routing tag that
// renderCell expects. Keeps the plan's contract: "via-hub" → checked,
// "not-installed" → disabled, everything else → unchecked+enabled.
function perClientRouting(clientPresence) {
  const routing = {};
  for (const [client, entry] of Object.entries(clientPresence)) {
    const transport = entry && entry.transport;
    const endpoint = (entry && entry.endpoint) || "";
    if (!transport || transport === "absent") {
      routing[client] = "not-installed";
    } else if (transport === "http" && (endpoint.includes("localhost") || endpoint.includes("127.0.0.1"))) {
      routing[client] = "via-hub";
    } else if (transport === "relay") {
      routing[client] = "via-hub";
    } else {
      // "stdio", remote "http", or anything else we don't route through the hub.
      routing[client] = "direct";
    }
  }
  return routing;
}

function renderCell(server, client) {
  const routing = server.routing[client];
  const checked = routing === "via-hub" ? "checked" : "";
  const disabled = routing === "unsupported" || routing === "not-installed" ? "disabled" : "";
  // data-server carries server.name into an attribute value — escape it
  // for the same reason the name cell is escaped. `client` is bounded by
  // the hardcoded clients array above and does not need escaping.
  return `<td><input type="checkbox" data-server="${escapeHtml(server.name)}" data-client="${client}" ${checked} ${disabled}></td>`;
}
