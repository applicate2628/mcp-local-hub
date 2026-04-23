import type { JSX } from "preact";
import { useRouter } from "./hooks/useRouter";
import { DashboardScreen } from "./screens/Dashboard";
import { LogsScreen } from "./screens/Logs";
import { ServersScreen } from "./screens/Servers";

// JSX.Element is imported from preact (see import above). jsx:"preserve"
// + jsxImportSource:"preact" makes the preact/jsx-runtime inject the
// factory calls, but does NOT populate a global JSX namespace — so the
// type reference below requires the explicit import.
// A map of route -> thunk gives each entry a component-of-no-props
// signature, which is what App renders below.
const SCREENS: Record<string, () => JSX.Element> = {
  servers: () => <ServersScreen />,
  dashboard: () => <DashboardScreen />,
  logs: () => <LogsScreen />,
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
