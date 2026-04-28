import type { JSX } from "preact";
import { useState, useEffect } from "preact/hooks";
import { useRouter, type RouterState } from "./hooks/useRouter";
import { useUnsavedChangesGuard } from "./hooks/useUnsavedChangesGuard";
import { useSettingsSnapshot } from "./lib/use-settings-snapshot";
import { AddServerScreen } from "./screens/AddServer";
import { DashboardScreen } from "./screens/Dashboard";
import { LogsScreen } from "./screens/Logs";
import { MigrationScreen } from "./screens/Migration";
import { SecretsScreen } from "./screens/Secrets";
import { ServersScreen } from "./screens/Servers";
import { SettingsScreen } from "./screens/Settings";

export function App() {
  const [addServerDirty, setAddServerDirty] = useState(false);
  const [settingsDirty, setSettingsDirty] = useState(false);
  const dirtyAny = addServerDirty || settingsDirty;

  // Codex r2 P1: discard signal for in-screen navigation. Section-local
  // edit state in useSectionSaveFlow / SectionBackups stays mounted across
  // intra-Settings hash changes. Bumping this counter on confirmed discard
  // forces SettingsScreen to remount via React `key` prop, resetting every
  // section's local draft state in one go. Memo §10.4.
  const [discardKey, setDiscardKey] = useState(0);

  // Codex PR #20 r2 P1: apply appearance attributes at app bootstrap so
  // saved theme + density are active on every route, not just while
  // SettingsScreen is mounted. Without this the attributes are only set
  // while the user has #/settings open; any other route (Servers, Dashboard,
  // etc.) renders with default styling until Settings is visited once.
  //
  // SettingsScreen ALSO applies these on its own snapshot instance for
  // instantaneous post-Save feedback — the duplication is intentional and
  // idempotent (setAttribute with the same value is a no-op).
  const globalSettings = useSettingsSnapshot();
  useEffect(() => {
    if (globalSettings.status !== "ok") return;
    const theme = globalSettings.data.settings.find((s) => s.key === "appearance.theme");
    const density = globalSettings.data.settings.find((s) => s.key === "appearance.density");
    if (theme && "value" in theme) document.documentElement.setAttribute("data-theme", theme.value);
    if (density && "value" in density) document.documentElement.setAttribute("data-density", density.value);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [globalSettings.status, globalSettings.status === "ok" ? globalSettings.data : null]);

  const guard = (target: RouterState): boolean => {
    if (!dirtyAny) return true;
    // Same screen AND same query → no navigation, no prompt.
    if (target.screen === route.screen && target.query === route.query) return true;
    // eslint-disable-next-line no-alert
    const ok = window.confirm("Discard unsaved changes?");
    if (ok) {
      setAddServerDirty(false);
      setSettingsDirty(false);
      setDiscardKey((n) => n + 1);
    }
    return ok;
  };

  const route = useRouter("servers", guard);
  useUnsavedChangesGuard(dirtyAny);

  function guardClick(targetScreen: string): (e: MouseEvent) => void {
    return (e) => {
      if (!dirtyAny) return;
      // Only prompt if leaving a dirty-guarded screen for a different one.
      const onGuardedScreen =
        route.screen === "add-server" || route.screen === "edit-server" || route.screen === "settings";
      if (!onGuardedScreen) return;
      if (targetScreen === route.screen) return;
      // eslint-disable-next-line no-alert
      const ok = window.confirm("Discard unsaved changes?");
      if (!ok) {
        e.preventDefault();
      } else {
        setAddServerDirty(false);
        setSettingsDirty(false);
        setDiscardKey((n) => n + 1);
      }
    };
  }

  let body: JSX.Element;
  switch (route.screen) {
    case "servers":
      body = <ServersScreen />;
      break;
    case "migration":
      body = <MigrationScreen />;
      break;
    case "add-server":
      body = <AddServerScreen mode="create" route={route} onDirtyChange={setAddServerDirty} />;
      break;
    case "edit-server":
      body = <AddServerScreen mode="edit" route={route} onDirtyChange={setAddServerDirty} />;
      break;
    case "secrets":
      body = <SecretsScreen />;
      break;
    case "dashboard":
      body = <DashboardScreen />;
      break;
    case "logs":
      body = <LogsScreen />;
      break;
    case "settings":
      // `key={discardKey}` forces a full remount on confirmed discard so
      // every section's local draft state resets cleanly (Codex r2 P1).
      body = <SettingsScreen key={discardKey} route={route} onDirtyChange={setSettingsDirty} />;
      break;
    default:
      body = <p>Unknown screen: {route.screen}</p>;
  }

  return (
    <>
      <aside class="sidebar">
        <div class="brand">mcp-local-hub</div>
        <nav>
          <a href="#/servers"    class={route.screen === "servers"    ? "active" : ""} onClick={guardClick("servers")}>Servers</a>
          <a href="#/migration"  class={route.screen === "migration"  ? "active" : ""} onClick={guardClick("migration")}>Migration</a>
          <a href="#/add-server" class={route.screen === "add-server" ? "active" : ""} onClick={guardClick("add-server")}>Add server</a>
          <a href="#/secrets"    class={route.screen === "secrets"    ? "active" : ""} onClick={guardClick("secrets")}>Secrets</a>
          <a href="#/dashboard"  class={route.screen === "dashboard"  ? "active" : ""} onClick={guardClick("dashboard")}>Dashboard</a>
          <a href="#/logs"       class={route.screen === "logs"       ? "active" : ""} onClick={guardClick("logs")}>Logs</a>
          <a href="#/settings"   class={route.screen === "settings"   ? "active" : ""} onClick={guardClick("settings")}>Settings</a>
        </nav>
      </aside>
      <main id="screen-root">
        {body}
      </main>
    </>
  );
}
