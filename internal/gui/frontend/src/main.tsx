import { render, h } from "preact";
import { App } from "./app";
import "./styles/style.css";

const root = document.getElementById("app");
if (!root) {
  throw new Error("index.html is missing #app mount point");
}
render(h(App, null), root);
