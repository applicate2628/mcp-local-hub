import { useEffect, useState } from "preact/hooks";

// useRouter is a minimal hash router. It returns the active screen name
// (the part after "#/" up to the first "?") and updates on hashchange.
// Screen modules in the legacy vanilla-JS code registered into
// window.mcphub.screens; here we lift that into an app-level switch in
// App.tsx that consumes the hook.
//
// Query params after the screen key (e.g. "#/add-server?server=foo") are
// stripped from the returned screen key — they are read directly from
// window.location.hash by screens that need them (see parseAddServerQuery
// in screens/AddServer.tsx). The screen key must match a SCREENS entry
// for the route to render; without stripping, "add-server?..." becomes
// an unknown screen and A1→A2a handoff fails silently. (PR #5 Codex R2.)
//
// Why not preact-router / wouter: this app has a handful of static routes,
// no route params, no redirects, no nested layouts. A 20-line hook beats
// a 2KB dependency.
export function useRouter(defaultScreen: string): string {
  const parse = () => {
    const hash = window.location.hash || `#/${defaultScreen}`;
    // Strip the leading "#/" and any "?query..." suffix before matching
    // against SCREENS.
    const afterPrefix = hash.replace(/^#\//, "");
    const screen = afterPrefix.split("?")[0];
    return screen || defaultScreen;
  };
  const [screen, setScreen] = useState<string>(parse());
  useEffect(() => {
    const onHash = () => setScreen(parse());
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  return screen;
}
