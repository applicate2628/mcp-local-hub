import { defineConfig } from "vitest/config";
import preact from "@preact/preset-vite";

export default defineConfig({
  plugins: [preact()],
  test: {
    environment: "happy-dom",
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    // Global localStorage polyfill — happy-dom 20.9.0 ships a bare
    // localStorage with no Storage methods, so wire the Map-backed shim
    // globally (it also runs as a before-each reset). See src/test-setup.ts.
    setupFiles: ["./src/test-setup.ts"],
  },
});
