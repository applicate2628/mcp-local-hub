import type { JSX } from "preact";
import { useRouter } from "./hooks/useRouter";
import { ServersScreen } from "./screens/Servers";

// Screen components get wired up in later tasks. For now render a "Coming
// soon" placeholder per route so the shell, sidebar, and hashchange
// handling are verifiable before any real screen lands.
function ComingSoon({ name }: { name: string }) {
  return (
    <div>
      <h1>{name}</h1>
      <p>Port in progress — see docs/superpowers/plans/2026-04-23-phase-3b-ii-d0-gui-vite-migration.md</p>
    </div>
  );
}

// JSX.Element is imported from preact (see import above). jsx:"preserve"
// + jsxImportSource:"preact" makes the preact/jsx-runtime inject the
// factory calls, but does NOT populate a global JSX namespace — so the
// type reference below requires the explicit import.
// A map of route -> thunk gives each entry a component-of-no-props
// signature, which is what App renders below.
const SCREENS: Record<string, () => JSX.Element> = {
  servers: () => <ServersScreen />,
  dashboard: () => <ComingSoon name="Dashboard" />,
  logs: () => <ComingSoon name="Logs" />,
};

export function App() {
  const screen = useRouter("servers");
  const Render = SCREENS[screen];
  return (
    <>
      <aside class="sidebar">
        <div class="brand">mcp-local-hub</div>
        <nav>
          <a href="#/servers" class={screen === "servers" ? "active" : ""}>
            Servers
          </a>
          <a href="#/dashboard" class={screen === "dashboard" ? "active" : ""}>
            Dashboard
          </a>
          <a href="#/logs" class={screen === "logs" ? "active" : ""}>
            Logs
          </a>
        </nav>
      </aside>
      <main id="screen-root">
        {Render ? <Render /> : <p>Unknown screen: {screen}</p>}
      </main>
    </>
  );
}
