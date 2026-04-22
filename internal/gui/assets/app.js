// Minimal hash-router scaffold. Screen modules register into window.mcphub.screens.
//
// Lifecycle model: #screen-root is a SINGLE element that gets its innerHTML
// cleared between screens — it is never removed from the DOM. That means a
// MutationObserver watching for the element's removal never fires and any
// long-lived resource (EventSource, interval, listener on document/window)
// the previous screen opened leaks for the lifetime of the page.
//
// To fix that, the router exposes a per-render cleanup registry. Each screen
// calls window.mcphub.registerCleanup(fn) for every resource that must be
// released on screen swap; the router drains the registry immediately before
// rendering the next screen (and before re-rendering the same screen on
// refresh/reload). Cleanups run in LIFO order and are isolated via try/catch
// so one failing cleanup cannot block the rest.
window.mcphub = window.mcphub || { screens: {}, _cleanups: [] };

window.mcphub.registerCleanup = function(fn) {
  window.mcphub._cleanups.push(fn);
};

// Load per-screen modules. Each module sets window.mcphub.screens[name].
const screenModules = ["/assets/servers.js", "/assets/dashboard.js", "/assets/logs.js"];
screenModules.forEach(src => {
  const sc = document.createElement("script");
  sc.src = src;
  document.head.appendChild(sc);
});

function render() {
  // Drain cleanups from the previous screen before we render the next one.
  // LIFO pop order mirrors a stack of resources: inner resources (opened
  // later) close before outer ones. Isolated so a throwing cleanup does
  // not leak the remaining resources.
  while (window.mcphub._cleanups.length > 0) {
    const fn = window.mcphub._cleanups.pop();
    try { fn(); } catch (e) { console.error(e); }
  }
  const hash = location.hash || "#/servers";
  const name = hash.replace(/^#\//, "");
  document.querySelectorAll("nav a").forEach(a => {
    a.classList.toggle("active", a.dataset.screen === name);
  });
  const root = document.getElementById("screen-root");
  root.textContent = "";
  const fn = window.mcphub.screens[name];
  if (fn) fn(root);
  else root.textContent = "Unknown screen: " + name;
}

window.addEventListener("hashchange", render);
window.addEventListener("DOMContentLoaded", render);
