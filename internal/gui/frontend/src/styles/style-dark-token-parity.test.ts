/// <reference types="node" />
import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

// Read the SOURCE stylesheet from disk. (`?raw` is intercepted by the CSS
// pipeline and returns empty under the test config, and import.meta.url is not
// a file: URL under happy-dom — so resolve against the vitest cwd, which is the
// frontend package root.) @types/node is present transitively; the triple-slash
// reference above makes tsc resolve the node: builtins for this one non-browser
// test file.
const cssRaw = readFileSync(
  resolve(process.cwd(), "src/styles/style.css"),
  "utf8",
);

// Strip /* … */ comments first so a header/selector MENTIONED inside a comment
// (e.g. the explanatory `@media (prefers-color-scheme: dark) :root[...]` note)
// cannot be mistaken for the real rule by the substring scanner below.
const css = cssRaw.replace(/\/\*[\s\S]*?\*\//g, "");

// Regression guard for the system-dark white-card bug.
//
// The DEFAULT appearance theme is `system`. On a dark-OS user, CSS resolves
// that via `@media (prefers-color-scheme: dark) :root[data-theme="system"]`.
// That block historically assigned only a SUBSET of the dark tokens that the
// explicit `[data-theme="dark"]` blocks assign — it silently omitted
// --card-surface / --sidebar-bg / --success / --danger / --link* — so the
// default theme rendered WHITE cards + washed-out chrome on a dark OS.
//
// The fix routes BOTH dark contexts through one single-source --dark-* palette
// and the SAME complete assignment list. This test re-derives the token set
// each context assigns straight from style.css and asserts they are identical,
// so the two can never drift apart again (adding a token to one block without
// the other fails this test loud).

/**
 * Extract the body (text between the first `{` and its matching `}`) of the
 * first rule whose header line contains `headerNeedle`. Brace-matched so a
 * nested media block is handled correctly.
 */
function ruleBody(source: string, headerNeedle: string): string {
  const headerIdx = source.indexOf(headerNeedle);
  expect(headerIdx, `rule header not found: ${headerNeedle}`).toBeGreaterThan(-1);
  const open = source.indexOf("{", headerIdx);
  expect(open, `opening brace not found after ${headerNeedle}`).toBeGreaterThan(-1);
  let depth = 0;
  for (let i = open; i < source.length; i++) {
    if (source[i] === "{") depth++;
    else if (source[i] === "}") {
      depth--;
      if (depth === 0) return source.slice(open + 1, i);
    }
  }
  throw new Error(`unbalanced braces after ${headerNeedle}`);
}

/** Names of every `--token:` LHS assigned directly in a rule body (top level). */
function assignedTokens(body: string): Set<string> {
  const tokens = new Set<string>();
  // Strip any nested block bodies so a media-wrapped child's own assignments
  // are not double counted from the parent extraction.
  const flat = body.replace(/\{[^{}]*\}/g, "");
  for (const m of flat.matchAll(/(--[a-z0-9-]+)\s*:/gi)) tokens.add(m[1]);
  return tokens;
}

describe("dark-token parity — explicit-dark vs system-dark", () => {
  // The system-dark block lives inside the media query; grab the media block
  // then the inner :root[data-theme="system"] body.
  const mediaBody = ruleBody(css, "@media (prefers-color-scheme: dark)");
  const systemDark = assignedTokens(ruleBody(mediaBody, ':root[data-theme="system"]'));

  // The explicit-dark assignments are spread across two blocks (Primer family
  // and A4-a family). Their UNION is what system-dark must match.
  const primerDark = assignedTokens(ruleBody(css, '[data-theme="dark"] {'));
  const a4aDark = assignedTokens(ruleBody(css, ':root[data-theme="dark"] {'));
  const explicitDark = new Set([...primerDark, ...a4aDark]);

  it("system-dark assigns every token explicit-dark assigns (no missing token)", () => {
    const missing = [...explicitDark].filter((t) => !systemDark.has(t)).sort();
    expect(missing, `system-dark is MISSING these dark tokens: ${missing.join(", ")}`).toEqual([]);
  });

  it("system-dark assigns no token explicit-dark lacks (no extra token)", () => {
    const extra = [...systemDark].filter((t) => !explicitDark.has(t)).sort();
    expect(extra, `system-dark has EXTRA tokens not in explicit-dark: ${extra.join(", ")}`).toEqual([]);
  });

  it("the high-impact surface/chrome tokens are present in system-dark", () => {
    // These are the exact tokens whose omission caused the white-card bug.
    for (const t of ["--card-surface", "--sidebar-bg", "--success", "--danger", "--link"]) {
      expect(systemDark.has(t), `system-dark must assign ${t}`).toBe(true);
    }
  });

  it("every dark token is sourced from a single --dark-* value (no raw literal in the dark blocks)", () => {
    // Each assignment in the dark blocks must read a var(--dark-*), not a raw
    // color literal — that is what keeps the literals single-source.
    for (const [label, body] of [
      ["[data-theme=dark]", ruleBody(css, '[data-theme="dark"] {')],
      [":root[data-theme=dark]", ruleBody(css, ':root[data-theme="dark"] {')],
      ["system-dark", ruleBody(mediaBody, ':root[data-theme="system"]')],
    ] as const) {
      // Look for a `--token: #hex` or `--token: rgb(...)` raw literal.
      const rawLiteral = body.match(/--[a-z0-9-]+\s*:\s*(#[0-9a-f]{3,8}|rgb)/i);
      expect(rawLiteral, `${label} has a raw color literal; route it through a --dark-* source: ${rawLiteral?.[0]}`).toBeNull();
    }
  });
});
