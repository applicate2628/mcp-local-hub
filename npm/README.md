# mcphub (npm distribution)

`mcphub` is the npm meta package for **mcp-local-hub** — a local supervisor +
router that compresses dozens of duplicate MCP (Model Context Protocol) server
processes spawned across parallel clients (editors, agents, CLIs) into one
managed hub.

- Project: https://github.com/applicate2628/mcp-local-hub
- License: MPL-2.0
- Author: Dmitry Denisenko (@applicate2628)

## Install

```bash
npm install -g mcphub
# or run without installing:
npx mcphub version
```

The meta package ships **no binary itself**. It declares one
`@mcphub/<platform>-<arch>` package per supported target in its
`optionalDependencies`, and npm installs **only** the sub-package whose
`os`/`cpu` match your host (the esbuild / turbo pattern). A small Node shim
(`bin/cli.js`) then locates that platform binary and execs it, passing your
arguments through and propagating its exit code.

There is **no `postinstall` download script** by design: `postinstall` is the
top npm supply-chain attack vector and breaks under
`npm install --ignore-scripts`. The binary arrives purely through the
optional dependency npm itself installs.

## Supported platforms and support tiers

| npm `os`/`cpu`   | Go `GOOS`/`GOARCH` | Support tier |
| ---------------- | ------------------ | ------------ |
| `win32` / `x64`  | `windows/amd64`    | **GA** |
| `win32` / `arm64`| `windows/arm64`    | best-effort |
| `darwin` / `x64` | `darwin/amd64`     | best-effort |
| `darwin` / `arm64`| `darwin/arm64`    | best-effort |
| `linux` / `x64`  | `linux/amd64`      | best-effort |
| `linux` / `arm64`| `linux/arm64`      | best-effort |

**GA** = generally available. **best-effort** = the CLI runs, but the
long-lived supervisor lifecycle is **Windows-GA / Linux-beta / macOS-preview**
per the project's release scope. On Apple Silicon with no native arm64 build
present, the `darwin-x64` binary runs under Rosetta 2 — an accepted fallback.

## If your platform is unsupported, or the binary did not install

The shim prints a clear error naming your platform and points you to the
fallback channel:

1. Download the matching binary from
   https://github.com/applicate2628/mcp-local-hub/releases
2. Place it on your `PATH`.
3. Keep it current with `mcphub install --upgrade`.

## How this directory is maintained

- `package.json` — the meta package. Its `version` is the single version
  authority; `npm/sync-version.js` propagates it into the Go build scripts.
- `bin/cli.js` — the platform-resolver shim (no runtime dependencies).
- `generate-platform-packages.js` — regenerates the six
  `packages/<platform>-<arch>/` sub-packages from one GOOS/GOARCH→os/cpu map.
  The sub-packages are generated artifacts; do not hand-edit them.
- Platform binaries are injected into each sub-package's `bin/` by the release
  job at publish time (Phase 3+); they are **not** committed to git.

## Status

This is **Phases 1–2** of the npm delivery work: the static package scaffold
plus a CI version-sync gate. Actual publishing (`npm publish`, token/OIDC
wiring, the release binary-injection job) is **Phase 3+** and not enabled here.
