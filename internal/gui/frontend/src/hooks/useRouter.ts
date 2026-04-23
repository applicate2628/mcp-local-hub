import { useEffect, useState } from "preact/hooks";

// useRouter is a minimal hash router. It returns the active screen name
// (the part after "#/") and updates on hashchange. Screen modules in the
// legacy vanilla-JS code registered into window.mcphub.screens; here we
// lift that into an app-level switch in App.tsx that consumes the hook.
//
// Why not preact-router / wouter: this app has three static routes, no
// params, no redirects, no nested layouts. A 20-line hook beats a 2KB
// dependency.
export function useRouter(defaultScreen: string): string {
  const parse = () => {
    const hash = window.location.hash || `#/${defaultScreen}`;
    return hash.replace(/^#\//, "") || defaultScreen;
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
