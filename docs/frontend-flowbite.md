# Flowbite in the GUI frontend (Preact + Tailwind v4)

This note explains how to use [Flowbite](https://flowbite.com) components in
the mcp-local-hub web UI (`internal/gui/frontend`). It is the reference for
the design lanes that build new screens on top of the Flowbite component
vocabulary.

## What was integrated and why this way

The frontend is **Preact** (not React), built with **Vite**, and already runs
on **Tailwind CSS v4** through the `@tailwindcss/vite` plugin. Flowbite was
added as plain `flowbite` (the framework-agnostic package) — **not**
`flowbite-react`, which is React-only and brittle under Preact.

Because the project is on Tailwind **v4**, Flowbite is wired the
**v4-native way** — through the `@plugin` CSS directive — rather than the
legacy v3 path (`tailwind.config.js` + `postcss.config.js` +
`plugins: [require('flowbite/plugin')]`). There is no `tailwind.config.js` or
`postcss.config.js` in this project, and there does not need to be: Tailwind v4
is CSS-first and config-less by default. The wiring lives entirely in
`src/styles/style.css`:

```css
@layer theme, base, components, utilities;
@import "tailwindcss/theme.css" layer(theme);
@import "tailwindcss/utilities.css" layer(utilities);

@plugin "flowbite/plugin";
@source "../../node_modules/flowbite";
```

- `@plugin "flowbite/plugin"` registers Flowbite's component utilities and
  variants.
- `@source "../../node_modules/flowbite"` tells Tailwind to scan Flowbite's
  compiled JS for any class names it toggles at runtime (e.g. when a dropdown
  opens), so those classes survive Tailwind's content-based purge.

## Coexistence: the existing custom design is untouched

The big risk with adding Tailwind/Flowbite to an app that already ships a
hand-written stylesheet is **Preflight** — Tailwind's global base reset, which
would clobber the existing `.card`, `.btn-primary`, `InfoTip`, etc. styles.

**Preflight is intentionally NOT imported.** The CSS entry imports only the
`theme` and `utilities` layers (`tailwindcss/theme.css` +
`tailwindcss/utilities.css`), never `tailwindcss/preflight.css`. The Flowbite
plugin does not re-introduce Preflight. This was already the project's
coexistence strategy for Tailwind v4 utilities, and Flowbite slots into it with
zero changes to any existing screen. The full vitest suite
(`npm run test`) stays green and `npm run typecheck` stays clean after the
integration.

## Two kinds of Flowbite components

### 1. Static (class-only) components

Badge, Button, Card, Alert, Avatar, Spinner, etc. are pure Tailwind utility
classes baked into the markup. They need **no JavaScript** — copy the classes
from the Flowbite docs into your JSX and they render:

```tsx
<span class="bg-blue-100 text-blue-800 text-xs font-medium px-2.5 py-0.5 rounded-sm dark:bg-blue-900 dark:text-blue-300">
  Badge text
</span>
```

### 2. Interactive (data-attribute) components

Tooltip, Dropdown, Modal, Drawer, Toast, Accordion, Tabs, etc. are driven by
Flowbite's **vanilla JS**, which it wires up by scanning the DOM for `data-*`
attributes (`data-tooltip-target`, `data-modal-toggle`, `data-dropdown-toggle`,
…). That scan runs when you call `initFlowbite()`.

Because Preact renders the DOM itself, you must call `initFlowbite()` **after**
the markup mounts. This is the direct Preact equivalent of Flowbite's
documented Vue `onMounted` hook:

```tsx
import { useEffect } from "preact/hooks";
import { initFlowbite } from "flowbite";

export function MyScreen() {
  // Run once after the first mount — wires every data-attribute trigger
  // currently in the DOM.
  useEffect(() => {
    initFlowbite();
  }, []);

  return (
    <>
      <button data-tooltip-target="tip-1" type="button">Hover me</button>
      <div id="tip-1" role="tooltip" class="... tooltip ...">
        Tooltip text
        <div class="tooltip-arrow" data-popper-arrow></div>
      </div>
    </>
  );
}
```

If a screen **re-renders new data-attribute markup later** (e.g. a list of
modals built from fetched data), call `initFlowbite()` again in an effect keyed
on that data so the freshly-rendered triggers get wired:

```tsx
useEffect(() => {
  initFlowbite();
}, [items]); // re-wire whenever the rendered triggers change
```

`initFlowbite()` is idempotent and a no-op when no Flowbite data attributes are
present, so calling it is always safe.

## Working reference component

A copy-paste-ready example lives at
`src/components/FlowbiteExample.tsx` (with a test at
`src/components/FlowbiteExample.test.tsx`). It renders a Flowbite Badge, a
Flowbite Button, and an interactive Flowbite tooltip, and demonstrates the
`initFlowbite()` mount effect. It is **not** wired into any production screen —
it is a pattern source for the next lanes to copy.

## Building

Frontend-only changes do not require `go build`. Use the standard frontend
workflow:

```bash
cd internal/gui/frontend
npm install        # picks up the flowbite dependency
npm run typecheck  # clean
npm run test       # all tests pass
npm run build      # succeeds; writes ../assets/{index.html,app.js,style.css}
```

Regenerating the committed embedded bundle (`go generate ./internal/gui/...`)
is a **separate** commit step — see the root `CLAUDE.md` "GUI frontend"
section.

## Terms and Abbreviations

- **Flowbite** — an open-source UI component library built on Tailwind CSS
  utility classes, with vanilla-JS interactive components driven by HTML
  `data-*` attributes.
- **`initFlowbite()`** — Flowbite's JS entry point that scans the DOM for its
  `data-*` triggers and attaches the interactive behavior.
- **Preflight** — Tailwind CSS's global base/reset stylesheet. Intentionally
  omitted here so the existing custom design is not reset.
- **Preact** — a small React-compatible UI library; this project uses it, not
  React. `useEffect` is its mount/update hook (the Vue `onMounted` analogue).
- **Tailwind v4 `@plugin` / `@source`** — CSS directives that register a plugin
  and add extra files to Tailwind's class-scanning set, replacing the v3
  `tailwind.config.js` `plugins`/`content` keys.
