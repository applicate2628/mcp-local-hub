import type { JSX } from "preact";
import { useState, useEffect, useRef } from "preact/hooks";
import { useRouter, type RouterState } from "./hooks/useRouter";
import { useUnsavedChangesGuard } from "./hooks/useUnsavedChangesGuard";
import { useSettingsSnapshot } from "./lib/use-settings-snapshot";

// LAYOUT_CACHE_KEY caches the last-known appearance.layout in
// localStorage so the second-and-later open of the GUI renders the
// correct layout SYNCHRONOUSLY on first paint, rather than flashing
// the default sidebar shell while /api/settings is in flight.
// Codex bot review on PR #43 r2 P2.
const LAYOUT_CACHE_KEY = "mcphub.appearance.layout";

function readCachedLayout(): "sidebar" | "tabs" {
  // localStorage may be unavailable (private mode, SSR, sandbox); fall
  // back to "sidebar" so the contract is the same as a fresh first run.
  try {
    const v = localStorage.getItem(LAYOUT_CACHE_KEY);
    return v === "tabs" ? "tabs" : "sidebar";
  } catch {
    return "sidebar";
  }
}

function writeCachedLayout(v: string): void {
  try {
    localStorage.setItem(LAYOUT_CACHE_KEY, v);
  } catch {
    /* ignore — quota / disabled storage */
  }
}

// DEFAULT_SCREEN_CACHE_KEY mirrors the layout cache for
// appearance.default_screen. useRouter caches the FIRST defaultScreen
// it receives — and useSettingsSnapshot always starts in `loading`,
// so without a synchronous source the router locks to "dashboard"
// before the snapshot resolves and the saved preference is never
// applied. Codex bot review on PR #40 P1.
const DEFAULT_SCREEN_CACHE_KEY = "mcphub.appearance.default_screen";

// VALID_DEFAULT_SCREENS mirrors the registry enum. Cached values are
// validated against this set so a corrupted or stale localStorage
// entry can't drop the router into an unknown screen and crash on
// the switch fall-through.
const VALID_DEFAULT_SCREENS = new Set([
  "dashboard", "servers", "catalog", "migration", "add-server",
  "secrets", "logs", "capabilities", "settings", "about",
]);

function readCachedDefaultScreen(): string {
  try {
    const v = localStorage.getItem(DEFAULT_SCREEN_CACHE_KEY);
    return v && VALID_DEFAULT_SCREENS.has(v) ? v : "dashboard";
  } catch {
    return "dashboard";
  }
}

function writeCachedDefaultScreen(v: string): void {
  try {
    localStorage.setItem(DEFAULT_SCREEN_CACHE_KEY, v);
  } catch {
    /* ignore */
  }
}

// Synchronously seed data-layout from the cached value at module load,
// BEFORE the first render. Without this the JSX that conditionally
// renders <header class="topbar"> fires fine, but the CSS rules gated
// on :root[data-layout="tabs"] never apply — so the topbar shows
// without tabs styling. Codex bot review on PR #43 r3 P1.
//
// guard typeof document for SSR / test bootstraps where document
// doesn't exist yet; the test setup mounts a JSDOM later.
if (typeof document !== "undefined") {
  document.documentElement.setAttribute("data-layout", readCachedLayout());
}
import { FirstRunBanner } from "./components/FirstRunBanner";
import { ToastContainer } from "./components/Toast";
import { AboutScreen } from "./screens/About";
import { AddServerScreen } from "./screens/AddServer";
import { CapabilitiesScreen } from "./screens/Capabilities";
import { CatalogScreen } from "./screens/Catalog";
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

  // Codex PR #20 r2 P1 / r11 P2: single source of truth for appearance
  // attribute application. App owns the snapshot and passes it down to
  // SettingsScreen as a prop. This ensures there is exactly one apply
  // pipeline for data-theme / data-density — after Save, SectionAppearance
  // calls snapshot.refresh() which triggers this App-level effect, updating
  // the attributes. Settings.tsx no longer holds its own hook instance, so
  // the race where App's stale fetch could overwrite post-Save values is
  // eliminated.
  const globalSettings = useSettingsSnapshot();
  // Capture the cache value at mount — before the settings effect overwrites it with
  // the snapshot value. useRouter latches whatever was in cache at first render; this
  // ref lets the cold-start reconciliation know what screen the router was given, even
  // after writeCachedDefaultScreen() has already updated localStorage. Codex r3 P2.
  const initialDefaultScreenRef = useRef(readCachedDefaultScreen());
  useEffect(() => {
    if (globalSettings.status !== "ok") return;
    const theme = globalSettings.data.settings.find((s) => s.key === "appearance.theme");
    const density = globalSettings.data.settings.find((s) => s.key === "appearance.density");
    const layout = globalSettings.data.settings.find((s) => s.key === "appearance.layout");
    if (theme && "value" in theme) document.documentElement.setAttribute("data-theme", theme.value);
    if (density && "value" in density) document.documentElement.setAttribute("data-density", density.value);
    if (layout && "value" in layout) {
      document.documentElement.setAttribute("data-layout", layout.value);
      // Update the localStorage cache so the next-load synchronous read
      // reflects the latest persisted value. Cache is best-effort; a
      // failed write doesn't affect this run's correctness.
      writeCachedLayout(layout.value);
    }
    // Mirror the cache write for default_screen — same pattern, same
    // rationale. Without this the next launch's useRouter would cache
    // the previous value forever (or "dashboard" on first install).
    const defaultScreen = globalSettings.data.settings.find((s) => s.key === "appearance.default_screen");
    if (defaultScreen && "value" in defaultScreen && defaultScreen.value) {
      writeCachedDefaultScreen(defaultScreen.value);
    }
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [globalSettings.status, globalSettings.status === "ok" ? globalSettings.data : null]);

  // Layout switcher (spec §5 line 241): sidebar (default) vs top tabs.
  //
  // The snapshot from /api/settings is async, so on every page open the
  // first paint runs before the snapshot resolves. Reading the layout
  // from localStorage gives us the SAME value the user saw last time
  // synchronously — eliminating the sidebar→tabs flash flagged by Codex
  // bot on r2 P2. The first-ever open (no cache) defaults to "sidebar"
  // exactly as before. After the snapshot resolves the useEffect above
  // refreshes the cache for next time.
  const cachedLayout = readCachedLayout();
  const layoutValue = (() => {
    if (globalSettings.status !== "ok") return cachedLayout;
    const entry = globalSettings.data.settings.find((s) => s.key === "appearance.layout");
    return entry && "value" in entry && entry.value ? entry.value : "sidebar";
  })();

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

  // Default landing screen comes from the persisted preference
  // appearance.default_screen (Settings → Appearance). useRouter caches
  // the FIRST defaultScreen it receives — re-renders don't re-route
  // mid-session, which is intentional: the user shouldn't see the page
  // swap out from under them after they save a new preference.
  //
  // Codex bot review on PR #40 P1: useSettingsSnapshot always starts
  // in `loading`, so reading from the snapshot on first render gives
  // "dashboard" and locks the router there forever — the saved
  // preference never wins, even after restart. Reading the localStorage
  // cache synchronously gives us the SAME value the user saw last time
  // before useRouter latches. After the snapshot resolves the useEffect
  // above refreshes the cache for next launch.
  const persistedDefaultScreen = (() => {
    if (globalSettings.status === "ok") {
      const entry = globalSettings.data.settings.find((s) => s.key === "appearance.default_screen");
      if (entry && "value" in entry && entry.value) return entry.value;
    }
    return readCachedDefaultScreen();
  })();
  const route = useRouter(persistedDefaultScreen, guard);
  useUnsavedChangesGuard(dirtyAny);

  // Cold-start reconciliation: when the snapshot resolves for the
  // first time AND the user is still on the synthetic default with
  // no URL hash, navigate to the persisted preference. Covers the
  // edge case Codex bot r2 P2 flagged — new browser profile, cleared
  // localStorage, or a value set via CLI (`mcphub settings set
  // appearance.default_screen X`) without ever opening the GUI: the
  // cache is empty so useRouter latches "dashboard", but the snapshot
  // then arrives with the real preference. The guard on "no hash" +
  // "on cached/default screen" keeps this from yanking a user who
  // already navigated manually.
  //
  // Codex bot r3 P2: the guard uses initialDefaultScreenRef (captured at
  // mount) instead of re-reading localStorage, because the settings effect
  // above runs first (same dependency) and overwrites the cache before this
  // effect reads it — causing the guard to see the NEW value and bail early.
  const [coldStartHandled, setColdStartHandled] = useState(false);
  useEffect(() => {
    if (coldStartHandled) return;
    if (globalSettings.status !== "ok") return;
    setColdStartHandled(true);
    const entry = globalSettings.data.settings.find((s) => s.key === "appearance.default_screen");
    const persisted = entry && "value" in entry && entry.value ? entry.value : "dashboard";
    if (persisted === route.screen) return;
    if (window.location.hash && window.location.hash !== "#/") return;
    if (route.screen !== initialDefaultScreenRef.current) return;
    window.location.hash = `#/${persisted}`;
  }, [globalSettings.status, coldStartHandled, route.screen]);

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
    case "catalog":
      body = <CatalogScreen />;
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
    case "capabilities":
      body = <CapabilitiesScreen />;
      break;
    case "settings":
      // `key={discardKey}` forces a full remount on confirmed discard so
      // every section's local draft state resets cleanly (Codex r2 P1).
      // Codex PR #20 r11 P2: snapshot lifted to App — pass as prop so
      // Settings never creates a competing instance.
      body = <SettingsScreen key={discardKey} route={route} onDirtyChange={setSettingsDirty} snapshot={globalSettings} />;
      break;
    case "about":
      body = <AboutScreen />;
      break;
    default:
      body = <p>Unknown screen: {route.screen}</p>;
  }

  // Nav links — same set in both layouts; CSS swaps direction.
  //
  // Keyboard a11y (WAI-ARIA Navigation pattern): the links form a roving
  // set inside one <nav> landmark. Native <a> already gives Tab order +
  // Enter activation + the `nav a:focus-visible` ring (style.css §3.1), so
  // here we add the three remaining pieces: an aria-label distinguishing
  // this landmark for AT, aria-current="page" on the active link, and
  // Arrow/Home/End roving focus between sibling links. The roving handler
  // only moves DOM focus — it never changes the route — so pressing Enter
  // (native anchor) is what actually navigates, matching the pointer flow.
  // Orientation is layout-aware: vertical sidebar listens to Up/Down,
  // horizontal tabs to Left/Right (both also accept the cross-axis keys so
  // muscle memory works either way). Mirrors the ARIA combobox keyboard
  // discipline already in SecretPicker (env-picker).
  const NAV_ITEMS: ReadonlyArray<{ screen: string; href: string; label: string }> = [
    { screen: "servers",      href: "#/servers",      label: "Servers" },
    { screen: "catalog",      href: "#/catalog",      label: "Catalog" },
    { screen: "migration",    href: "#/migration",    label: "Discovery" },
    { screen: "add-server",   href: "#/add-server",   label: "Add server" },
    { screen: "secrets",      href: "#/secrets",      label: "Secrets" },
    { screen: "dashboard",    href: "#/dashboard",    label: "Dashboard" },
    { screen: "logs",         href: "#/logs",         label: "Logs" },
    { screen: "capabilities", href: "#/capabilities", label: "Capabilities" },
    { screen: "settings",     href: "#/settings",     label: "Settings" },
    { screen: "about",        href: "#/about",        label: "About" },
  ];

  function onNavKeyDown(e: KeyboardEvent): void {
    const fwd = e.key === "ArrowDown" || e.key === "ArrowRight";
    const back = e.key === "ArrowUp" || e.key === "ArrowLeft";
    const home = e.key === "Home";
    const end = e.key === "End";
    if (!fwd && !back && !home && !end) return;
    // The event target must be one of the nav links — query siblings off
    // the <nav> the keydown bubbled to so both layouts (sidebar/topbar)
    // resolve their own link set.
    const navEl = e.currentTarget as HTMLElement;
    const links = Array.from(navEl.querySelectorAll<HTMLAnchorElement>("a"));
    if (links.length === 0) return;
    const current = links.indexOf(document.activeElement as HTMLAnchorElement);
    let next: number;
    if (home) {
      next = 0;
    } else if (end) {
      next = links.length - 1;
    } else if (current === -1) {
      // Focus isn't on a link yet (e.g. arrowed in from the brand) — land
      // on the first for forward, last for backward.
      next = fwd ? 0 : links.length - 1;
    } else {
      const delta = fwd ? 1 : -1;
      next = (current + delta + links.length) % links.length;
    }
    e.preventDefault();
    links[next]?.focus();
  }

  const navLinks = (
    <nav aria-label="Primary" onKeyDown={onNavKeyDown}>
      {NAV_ITEMS.map((item) => {
        const active = route.screen === item.screen;
        return (
          <a
            key={item.screen}
            href={item.href}
            class={active ? "active" : ""}
            aria-current={active ? "page" : undefined}
            onClick={guardClick(item.screen)}
          >
            {item.label}
          </a>
        );
      })}
    </nav>
  );

  // Shared brand header: a decorative inline hub-mark logo next to the
  // product name. One owner for both the sidebar and top-tabs layouts so
  // the brand markup never drifts between them. The SVG uses currentColor
  // so it inherits the brand text color and adapts to the light/dark
  // data-theme automatically; aria-hidden keeps it decorative (the text
  // name remains the accessible label).
  const brandHeader = (
    <div class="brand">
      <svg
        class="brand-logo"
        viewBox="0 0 24 24"
        width="20"
        height="20"
        fill="none"
        stroke="currentColor"
        stroke-width="1.75"
        stroke-linecap="round"
        stroke-linejoin="round"
        aria-hidden="true"
        focusable="false"
      >
        {/* Hub/node-cluster mark: a central node routing to three satellites. */}
        <line x1="12" y1="12" x2="12" y2="4" />
        <line x1="12" y1="12" x2="5" y2="17" />
        <line x1="12" y1="12" x2="19" y2="17" />
        <circle cx="12" cy="12" r="2.5" fill="currentColor" stroke="none" />
        <circle cx="12" cy="4" r="2" />
        <circle cx="5" cy="17" r="2" />
        <circle cx="19" cy="17" r="2" />
      </svg>
      <span class="brand-name">mcp-local-hub</span>
    </div>
  );

  if (layoutValue === "tabs") {
    return (
      <>
        <header class="topbar">
          {brandHeader}
          {navLinks}
        </header>
        <main id="screen-root">
          <FirstRunBanner />
          {body}
        </main>
        {/* One shared Flowbite Toast stack for every screen (toast store
            is module-level; ToastContainer just subscribes + renders). */}
        <ToastContainer />
      </>
    );
  }

  return (
    <>
      <aside class="sidebar">
        {brandHeader}
        {navLinks}
      </aside>
      <main id="screen-root">
        <FirstRunBanner />
        {body}
      </main>
      {/* One shared Flowbite Toast stack for every screen. */}
      <ToastContainer />
    </>
  );
}
