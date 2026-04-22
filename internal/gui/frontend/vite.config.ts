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
        assetFileNames: "[name].[ext]",
      },
    },
  },
  // Dev-server proxy to the running Go backend. Start the backend with a
  // fixed port (e.g. `go run ./cmd/mcphub gui --no-browser --no-tray --port 9125`)
  // and Vite's dev server on 5173 forwards /api/* to it so same-origin CSRF
  // guards keep working.
  server: {
    proxy: {
      "/api": {
        target: "http://127.0.0.1:9125",
        changeOrigin: false,
        ws: false,
      },
    },
  },
});
