import type { JSX } from "preact";
import { useRouter } from "./hooks/useRouter";
import { AddServerScreen } from "./screens/AddServer";
import { DashboardScreen } from "./screens/Dashboard";
import { LogsScreen } from "./screens/Logs";
import { MigrationScreen } from "./screens/Migration";
import { ServersScreen } from "./screens/Servers";

const SCREENS: Record<string, () => JSX.Element> = {
  servers: () => <ServersScreen />,
  migration: () => <MigrationScreen />,
  "add-server": () => <AddServerScreen />,
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
          <a href="#/migration" class={screen === "migration" ? "active" : ""}>
            Migration
          </a>
          <a href="#/add-server" class={screen === "add-server" ? "active" : ""}>
            Add server
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
