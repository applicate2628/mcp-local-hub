// internal/gui/assets/dashboard.js
//
// The dashboard keys its in-memory state map by the composite
// "<server>/<daemon>" tuple, matching the poller convention. A
// multi-daemon server like serena (claude + codex) would otherwise
// collide on server alone — state[r.server] would only retain the
// last-seen daemon row, rendering exactly one card instead of two.
function keyFor(r) {
  return r.server + "/" + (r.daemon || "default");
}

window.mcphub.screens.dashboard = function(root) {
  root.innerHTML = `<h1>Dashboard</h1><div id="cards" class="cards"></div>`;
  const cardsEl = document.getElementById("cards");
  const state = {}; // composite "<server>/<daemon>" key -> DaemonStatus

  function render() {
    cardsEl.innerHTML = "";
    Object.values(state).sort((a, b) => keyFor(a).localeCompare(keyFor(b))).forEach(d => {
      const card = document.createElement("div");
      card.className = "card " + (d.state === "Running" ? "ok" : "down");
      const title = d.daemon && d.daemon !== "default" ? `${d.server} (${d.daemon})` : d.server;
      card.innerHTML = `
        <div class="card-title">${escapeHtml(title)}</div>
        <div class="card-kv"><span>Port</span><span>${d.port ?? "—"}</span></div>
        <div class="card-kv"><span>PID</span><span>${d.pid ?? "—"}</span></div>
        <div class="card-kv"><span>State</span><span class="state">${escapeHtml(d.state ?? "")}</span></div>
        <div class="card-actions">
          <button data-server="${escapeHtml(d.server)}" class="restart-btn">Restart</button>
        </div>`;
      cardsEl.appendChild(card);
    });
    cardsEl.querySelectorAll(".restart-btn").forEach(btn => btn.addEventListener("click", async () => {
      const name = btn.dataset.server;
      btn.disabled = true;
      btn.textContent = "Restarting…";
      try {
        const resp = await fetch(`/api/servers/${encodeURIComponent(name)}/restart`, {method: "POST"});
        if (!resp.ok) {
          const body = await resp.json().catch(() => ({error: resp.status}));
          throw new Error(body.error);
        }
        btn.textContent = "Restarted";
      } catch (e) {
        btn.textContent = "Failed";
      } finally {
        setTimeout(() => { btn.textContent = "Restart"; btn.disabled = false; }, 1500);
      }
    }));
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c]));
  }

  // Initial load via /api/status, then live updates via SSE.
  //
  // Backend returns the {error, code} JSON envelope (via writeAPIError)
  // on failures — not an array. The previous `(rows || []).forEach(...)`
  // guard treated the truthy error object as iterable and threw at
  // .forEach, leaving the dashboard blank with the live SSE stream
  // never getting wired up. Explicitly require resp.ok AND an actual
  // array, and surface the error envelope's message through the
  // existing .error CSS class.
  fetch("/api/status").then(async r => {
    const data = await r.json().catch(() => null);
    if (!r.ok || !Array.isArray(data)) {
      cardsEl.innerHTML = `<p class="error">Failed to load status: ${escapeHtml(data?.error ?? r.statusText ?? "unknown")}</p>`;
      return;
    }
    data.forEach(row => state[keyFor(row)] = row);
    render();
  }).catch(err => {
    cardsEl.innerHTML = `<p class="error">Failed to load status: ${escapeHtml(err.message)}</p>`;
  });

  const es = new EventSource("/api/events");
  es.addEventListener("daemon-state", e => {
    const body = JSON.parse(e.data);
    const k = keyFor(body);
    if (body.state === "Gone") delete state[k];
    else state[k] = Object.assign(state[k] ?? {server: body.server, daemon: body.daemon}, body);
    render();
  });

  // Close the EventSource when the router swaps screens. #screen-root is
  // reused across swaps (innerHTML is cleared but the element stays in the
  // DOM), so a MutationObserver on body removal would never fire — every
  // visit to Dashboard would leak another live EventSource. The router's
  // cleanup registry in app.js runs pending callbacks before rendering the
  // next screen, which is the load-bearing lifecycle hook here.
  window.mcphub.registerCleanup(() => es.close());
};
