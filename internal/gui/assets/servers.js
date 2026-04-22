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
    // Derive request payload from per-cell dirty state:
    //   servers = unique server names with at least one dirty cell
    //   clients = unique client names across all dirty cells
    // The backend will process the cartesian intersection internally via
    // MigrateOpts.ClientsInclude. This is safe because the GUI only sends a
    // client name when at least one of its cells is dirty, so every listed
    // client has a justified rewrite in the current apply.
    const servers = [...dirty.keys()];
    const clientSet = new Set();
    for (const cs of dirty.values()) for (const c of cs) clientSet.add(c);
    const clients = [...clientSet];
    applyBtn.disabled = true;
    applyStatus.textContent = `Migrating ${servers.join(", ")}…`;
    try {
      const resp = await fetch("/api/migrate", {
        method: "POST",
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify({servers, clients}),
      });
      if (!resp.ok) {
        const body = await resp.json().catch(() => ({error: "unknown"}));
        throw new Error(body.error);
      }
      applyStatus.textContent = "Migrated. Refreshing…";
      dirty.clear();
      render(); // reload the matrix
    } catch (err) {
      applyStatus.innerHTML = `<span class="error">Failed: ${err.message}</span>`;
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
      const statusByServer = Object.fromEntries((status || []).map(s => [s.server, s]));
      const servers = collectServers(scan);
      for (const server of servers) {
        const row = document.createElement("tr");
        const st = statusByServer[server.name] || {};
        row.innerHTML = `
          <td>${server.name}</td>
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
  return `<td><input type="checkbox" data-server="${server.name}" data-client="${client}" ${checked} ${disabled}></td>`;
}
