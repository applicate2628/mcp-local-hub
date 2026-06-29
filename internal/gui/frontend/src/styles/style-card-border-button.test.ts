/// <reference types="node" />
import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

// Read the SOURCE stylesheet from disk (mirrors style-dark-token-parity.test.ts:
// the `?raw` import is intercepted by the CSS pipeline and import.meta.url is not
// a file: URL under happy-dom, so resolve against the vitest cwd = package root).
const cssRaw = readFileSync(resolve(process.cwd(), "src/styles/style.css"), "utf8");
// Strip comments so a selector/token MENTIONED in an explanatory comment cannot
// be mistaken for the real rule by the substring scanner below.
const css = cssRaw.replace(/\/\*[\s\S]*?\*\//g, "");

// Brace-matched extraction of the first rule body whose header contains the
// needle (same helper shape as the parity test).
function ruleBody(source: string, headerNeedle: string): string {
  const headerIdx = source.indexOf(headerNeedle);
  expect(headerIdx, `rule header not found: ${headerNeedle}`).toBeGreaterThan(-1);
  const open = source.indexOf("{", headerIdx);
  let depth = 0;
  for (let i = open; i < source.length; i++) {
    if (source[i] === "{") depth++;
    else if (source[i] === "}") {
      depth--;
      if (depth === 0) return source.slice(open + 1, i);
    }
  }
  throw new Error(`unterminated rule for ${headerNeedle}`);
}

describe("pixel-perfect-2: card border is paint-stable", () => {
  // P2 regression guard. The `.cards` grid uses `repeat(auto-fit, minmax(240px,
  // 1fr))`, whose 1fr tracks resolve to fractional widths (e.g. 277.25px) at
  // dpr=1; Chromium then drops the 1px hairline `border` on certain cards (the
  // Catalog MIDI/Ableton/Suno cards rendered borderless). The fix backs the
  // border with a composited box-shadow RING that the rasterizer cannot drop.
  // This asserts the `.card` rule still carries that ring so a future edit can't
  // silently revert to the border-only (droppable) form.
  it(".card has a box-shadow border ring keyed on --border", () => {
    const body = ruleBody(css, ".card {");
    expect(body).toContain("box-shadow:");
    // The first shadow layer is the 1px ring drawn from the border token.
    expect(body).toMatch(/box-shadow:\s*0 0 0 1px var\(--border\)/);
    // The CSS border is still present (layout box + rounded-corner clip).
    expect(body).toContain("border: 1px solid var(--border)");
  });
});

describe("pixel-perfect-2: dark primary button keeps AA contrast", () => {
  // P3 regression guard. The accent-filled primary buttons (New group, Add to
  // hub, Set up LSP, Save) must NOT use the link TEXT color (--link / dark
  // #58a6ff) as their white-text background — that was only 2.53:1 in dark. The
  // dedicated --btn-primary-bg token carries a saturated AA blue in dark.

  it("primary-button background reads --btn-primary-bg (not --link directly)", () => {
    for (const header of [
      ".btn.btn-primary:not(:disabled) {",
      ".catalog-marketplace-install button.btn-primary:not(:disabled) {",
      "section[data-section] button.btn-primary:not(:disabled) {",
    ]) {
      const body = ruleBody(css, header);
      expect(body, `${header} should use the primary-button token`).toContain(
        "background: var(--btn-primary-bg)",
      );
      expect(body, `${header} must not paint white text on the light --link bg`).not.toMatch(
        /background:\s*var\(--link\)\s*;/,
      );
    }
  });

  it("both dark contexts assign --btn-primary-bg to the saturated AA blue", () => {
    // Explicit-dark + system-dark both reassign the token off the light --link
    // default to the AA-passing #1f6feb (white text = 4.63:1).
    const explicitDark = ruleBody(css, '[data-theme="dark"] {');
    expect(explicitDark).toContain("--btn-primary-bg: var(--dark-btn-primary-bg)");
    expect(explicitDark).toContain("--btn-primary-bg-hover: var(--dark-btn-primary-bg-hover)");

    const systemDark = ruleBody(css, ':root[data-theme="system"] {');
    expect(systemDark).toContain("--btn-primary-bg: var(--dark-btn-primary-bg)");
    expect(systemDark).toContain("--btn-primary-bg-hover: var(--dark-btn-primary-bg-hover)");

    // The single-source dark value is the AA-passing Primer accent blue.
    const root = ruleBody(css, ":root {");
    expect(root).toContain("--dark-btn-primary-bg: #1f6feb");
  });
});
