import { render, h } from "preact";

function Placeholder() {
  return h("div", null, "mcp-local-hub — booting Vite+Preact shell");
}

const root = document.getElementById("app");
if (!root) {
  throw new Error("index.html is missing #app mount point");
}
render(h(Placeholder, null), root);
