// internal/gui/assets/servers.js
window.mcphub.screens.servers = async function(root) {
  root.innerHTML = '<h1>Servers</h1><div id="servers-content">Loading…</div>';
  const content = document.getElementById("servers-content");

  try {
    const [scan, status] = await Promise.all([
      fetch("/api/scan").then(r => r.json()),
      fetch("/api/status").then(r => r.json()),
    ]);
    content.innerHTML = "";
    const table = document.createElement("table");
    table.className = "servers-matrix";
    const clients = ["claude-code", "codex-cli", "gemini-cli", "antigravity"];
    const header = document.createElement("thead");
    header.innerHTML = `
      <tr>
        <th>Server</th>
        ${clients.map(c => `<th>${c}</th>`).join("")}
        <th>Port</th>
        <th>State</th>
      </tr>`;
    table.appendChild(header);
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
  } catch (err) {
    content.innerHTML = `<p class="error">Failed to load: ${err.message}</p>`;
  }
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
