import type { JSX } from "preact";
import { useState } from "preact/hooks";
import { useRouter } from "./hooks/useRouter";
import { AddServerScreen } from "./screens/AddServer";
import { DashboardScreen } from "./screens/Dashboard";
import { LogsScreen } from "./screens/Logs";
import { MigrationScreen } from "./screens/Migration";
import { ServersScreen } from "./screens/Servers";

export function App() {
  const screen = useRouter("servers");
  const [addServerDirty, setAddServerDirty] = useState(false);

  // guardClick is wired onto every sidebar <a>. If the Add server screen
  // is dirty AND the click leaves it for another screen, we prompt.
  // Cancelling restores the original hash via preventDefault. This covers
  // ~90% of exit paths; browser-back/refresh/tab-close coverage is
  // deferred to A2b (per design memo Q7).
  function guardClick(targetScreen: string): (e: MouseEvent) => void {
    return (e) => {
      if (
        screen === "add-server" &&
        addServerDirty &&
        targetScreen !== "add-server"
      ) {
        // eslint-disable-next-line no-alert
        const ok = window.confirm("Discard unsaved changes?");
        if (!ok) {
          e.preventDefault();
        } else {
          // User confirmed — reset the dirty flag so a second immediate
          // hashchange doesn't re-fire the prompt.
          setAddServerDirty(false);
        }
      }
    };
  }

  // renderScreen maps the current hash to the matching screen. We render
  // each <Screen /> inline (not via a lookup table of thunks) so that
  // Preact sees a stable component type across re-renders. Early attempts
  // used Record<string, () => JSX.Element> + <Render />, which gave Render
  // a fresh function identity on every parent re-render and forced Preact
  // to unmount/remount the screen — destroying AddServer's form state on
  // every keystroke because its dirty-effect round-trips through parent
  // state.
  let body: JSX.Element;
  switch (screen) {
    case "servers":
      body = <ServersScreen />;
      break;
    case "migration":
      body = <MigrationScreen />;
      break;
    case "add-server":
      body = <AddServerScreen onDirtyChange={setAddServerDirty} />;
      break;
    case "dashboard":
      body = <DashboardScreen />;
      break;
    case "logs":
      body = <LogsScreen />;
      break;
    default:
      body = <p>Unknown screen: {screen}</p>;
  }

  return (
    <>
      <aside class="sidebar">
        <div class="brand">mcp-local-hub</div>
        <nav>
          <a href="#/servers" class={screen === "servers" ? "active" : ""} onClick={guardClick("servers")}>Servers</a>
          <a href="#/migration" class={screen === "migration" ? "active" : ""} onClick={guardClick("migration")}>Migration</a>
          <a href="#/add-server" class={screen === "add-server" ? "active" : ""} onClick={guardClick("add-server")}>Add server</a>
          <a href="#/dashboard" class={screen === "dashboard" ? "active" : ""} onClick={guardClick("dashboard")}>Dashboard</a>
          <a href="#/logs" class={screen === "logs" ? "active" : ""} onClick={guardClick("logs")}>Logs</a>
        </nav>
      </aside>
      <main id="screen-root">
        {body}
      </main>
    </>
  );
}
