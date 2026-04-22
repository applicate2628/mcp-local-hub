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

  fetch("/api/status").then(r => r.json()).then(rows => {
    (rows || []).forEach(r => {
      const opt = document.createElement("option");
      opt.value = r.server; opt.textContent = r.server;
      sel.appendChild(opt);
    });
    if ((rows || []).length) load();
  });

  async function load() {
    if (es) { es.close(); es = null; }
    body.textContent = "Loading…";
    const server = sel.value;
    const tail = tailEl.value;
    const resp = await fetch(`/api/logs/${encodeURIComponent(server)}?tail=${encodeURIComponent(tail)}`);
    body.textContent = await resp.text();
    if (followEl.checked) startFollow(server);
  }
  function startFollow(server) {
    es = new EventSource(`/api/logs/${encodeURIComponent(server)}/stream`);
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
