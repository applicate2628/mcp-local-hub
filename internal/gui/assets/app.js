// Minimal hash-router scaffold. Screen modules register into window.mcphub.screens.
window.mcphub = window.mcphub || { screens: {} };

function render() {
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
