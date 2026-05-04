import { defineConfig } from "vite";
import preact from "@preact/preset-vite";

// Output layout is pinned so the existing Go embed + routes keep working:
//   internal/gui/assets/index.html  ← served by Go at "/"
//   internal/gui/assets/app.js      ← served by Go at "/assets/app.js"
//   internal/gui/assets/style.css   ← served by Go at "/assets/style.css"
//
// entryFileNames / assetFileNames disable Vite's default content-hash
// suffixes because this app is local-only (no CDN), so cache-busting is
// unnecessary and stable filenames keep every rebuild a no-op in git.
export default defineConfig({
  plugins: [preact()],
  base: "/assets/",
  build: {
    outDir: "../assets",
    emptyOutDir: true,
    assetsDir: ".",
    rollupOptions: {
      output: {
        entryFileNames: "app.js",
        chunkFileNames: "[name].js",
        // CSS extracted from the entry chunk is named after the chunk ("index")
        // by default. Force it back to "style" so the Go route /assets/style.css
        // and the embed path stay stable across rebuilds.
        assetFileNames: (info) =>
          info.name?.endsWith(".css") ? "style.css" : "[name].[ext]",
      },
    },
  },
  // Dev-server proxy to the running Go backend. Start the backend with a
  // fixed port (e.g. `go run ./cmd/mcphub gui --no-browser --no-tray --port 9125`)
  // and Vite's dev server on 5173 forwards /api/* to it so same-origin CSRF
  // guards keep working.
  //
  // BOTH `changeOrigin: true` AND the Origin-rewrite hook are required:
  //   - `changeOrigin: true` rewrites the Host header so `requireAllowedHost`
  //     (the DNS-rebinding gate) sees `127.0.0.1:9125` instead of the
  //     dev-server host the browser sent (`localhost:5173`).
  //   - The `configure` hook rewrites the Origin header from the browser's
  //     `http://localhost:5173` to the backend's loopback origin, so
  //     `requireSameOrigin` accepts the proxied request. Without this
  //     hook, the strict CSRF check rejects every dev POST as cross-origin.
  // Tightening either guard without loosening the other would break
  // `npm run dev` end-to-end; a backend regression test in
  // `internal/gui/csrf_test.go` asserts the bare `changeOrigin: true`
  // case (Host rewrite without Origin rewrite) still gets rejected, so
  // a future proxy edit that drops the hook fails closed instead of
  // silently bypassing CSRF.
  //
  // SECURITY (LAN deputy threat model):
  //   The proxy rewrites Origin/Host to the backend's trusted-loopback
  //   origin (`127.0.0.1:9125`). If the dev server itself is reachable
  //   from the LAN (e.g. `npm run dev -- --host`), Vite would launder
  //   LAN-attacker requests into the backend's loopback-only API —
  //   classic confused-deputy. We defend in two layers:
  //     1) `server.host = '127.0.0.1'` + `strictPort: true` keep the dev
  //        server bound to loopback by default. NOTE: Vite's CLI `--host`
  //        flag still overrides config, so layer 2 below is the actual
  //        backstop, not just the bind.
  //     2) The `configure(proxy)` hook below rejects any request whose
  //        ORIGINAL `Host` header is non-loopback BEFORE `changeOrigin`
  //        and the Origin rewrite run. That means a LAN attacker reaching
  //        Vite via its LAN-bound listener cannot deputy-forward.
  server: {
    host: "127.0.0.1",
    strictPort: true,
    proxy: {
      "/api": {
        target: "http://127.0.0.1:9125",
        changeOrigin: true,
        ws: false,
        configure(proxy) {
          proxy.on("proxyReq", (proxyReq, req, res) => {
            // Layer 2 of the LAN-deputy guard (see comment block above):
            // refuse to forward unless the browser-side Host header is
            // loopback. We check the ORIGINAL `req.headers.host` (the
            // value the client sent), not the post-rewrite Host that
            // changeOrigin synthesises — that's always loopback by
            // definition and would let LAN clients through.
            const origHost = (req.headers.host || "").toLowerCase();
            const ok =
              origHost === "" ||
              origHost.startsWith("localhost") ||
              origHost.startsWith("127.0.0.1") ||
              origHost.startsWith("[::1]");
            if (!ok) {
              if (res && !res.headersSent) {
                res.statusCode = 403;
                res.end("forbidden: Vite dev proxy is loopback-only");
              }
              proxyReq.destroy();
              return;
            }
            if (req.headers.origin) {
              proxyReq.setHeader("Origin", "http://127.0.0.1:9125");
            }
          });
        },
      },
    },
  },
});
