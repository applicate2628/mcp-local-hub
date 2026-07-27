# mcp-local-hub (npm distribution)

`mcp-local-hub` is the npm meta package for **mcp-local-hub** — a local
supervisor + router that compresses dozens of duplicate MCP (Model Context
Protocol) server processes spawned across parallel clients (editors, agents,
CLIs) into one managed hub.

- Project: https://github.com/applicate2628/mcp-local-hub
- License: MPL-2.0
- Author: Dmitry Denisenko (@applicate2628)

## Install

```bash
npm install -g mcp-local-hub
# the installed command is `mcphub`:
mcphub version
# or run without installing:
npx mcp-local-hub version
```

> The npm **package** is `mcp-local-hub`; the **command** it installs is
> `mcphub` (the package's `bin` entry). Install the package, run `mcphub`.

The meta package ships **no binary itself**. It declares one
`@applicate2628/mcp-local-hub-<platform>-<arch>` package per supported target
in its `optionalDependencies`, and npm installs **only** the sub-package whose
`os`/`cpu` match your host (the esbuild / turbo pattern). A small Node shim
(`bin/cli.js`) then locates that platform binary and execs it, passing your
arguments through and propagating its exit code.

There is **no `postinstall` _download_ script** by design: a postinstall that
FETCHES a binary over the network is the top npm supply-chain attack vector.
The binary arrives purely through the optional dependency npm itself installs,
and it still resolves under `npm install --ignore-scripts` (that flag skips
lifecycle scripts, not optional dependencies).

## Canonicalize into `~/.local/bin` (copy-only postinstall, GLOBAL install only)

The package **does** ship one lifecycle script, `scripts/postinstall.js`, but it
performs **no network I/O**, and it only does real work on a **GLOBAL** install
(`npm install -g` / `npm i -g`). On Windows the canonical
`~/.local/bin/mcphub.exe` sits *earlier* on `PATH` than the npm global bin by
design — the scheduler tasks, the supervisor's own-child spawn, and
`mcphub install --upgrade` all reference `~/.local/bin`; npm is only the
delivery vehicle. Without this hook, `npm install -g mcp-local-hub@<newer>`
would leave a **stale** `~/.local/bin` binary shadowing the fresh npm one.

A **local** install of this package into a project's own `node_modules`, or a
**transitive** install where `mcp-local-hub` is merely a dependency of some
other package, is not something the operator asked mcphub for — the hook
detects that case (via `npm_config_global`, which npm sets for every lifecycle
script) and prints a one-line skip notice instead of touching
`~/.local/bin`.

On a global install, the postinstall closes the staleness gap: it resolves the
platform binary npm just installed and asks it to **copy itself** into
`~/.local/bin` via the binary-only `mcphub canonicalize` entry point (no PATH
edits, no scheduled tasks, no prompts/elevation, no fleet restart). The copy is
lock-safe on Windows — if the running fleet holds the canonical binary, the
prior copy is renamed aside to `.old-<ts>` (the same crash-safe swap
`install --upgrade` uses) and the new binary takes effect on the next
fleet/supervisor restart.

It is **fully fail-safe**: a copy failure (permission, file lock, missing
`~/.local`) never breaks `npm install` — it prints a one-line notice pointing at
`mcphub setup` and exits 0.

**`--ignore-scripts`:** the postinstall does not run, so `~/.local/bin` is left
as-is. Reconcile it manually with:

```bash
mcphub setup
```

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

- `package.json` — the meta package (`mcp-local-hub`). Its `version` is the
  single version authority; `npm/sync-version.js` propagates it into the Go
  build scripts.
- `bin/cli.js` — the platform-resolver shim (Node built-ins only; imports the
  shared map from `lib/platform-binary.js`).
- `lib/platform-binary.js` — the single owner of the
  `${platform}-${arch}` → sub-package map + binary basename, shared by
  `bin/cli.js` and `scripts/postinstall.js`.
- `scripts/postinstall.js` — the copy-only, global-install-only canonicalize
  hook (see "Canonicalize into `~/.local/bin`" above); no network I/O, fully
  fail-safe, and a no-op notice on a local or transitive install.
- `generate-platform-packages.js` — regenerates the six
  `packages/<platform>-<arch>/` sub-packages from one GOOS/GOARCH→os/cpu map.
  The sub-packages are generated artifacts (`@applicate2628/mcp-local-hub-*`);
  do not hand-edit them.
- Platform binaries are injected into each sub-package's `bin/` by the release
  job at publish time; they are **not** committed to git.

## Why the platform packages are scoped

The meta package is unscoped (`mcp-local-hub`) so the install command stays
short. The six platform packages are scoped under `@applicate2628/` because a
brand-new account publishing a family of near-identical unscoped names trips
npm's spam-detection heuristic. Scoping them under the publisher's namespace
clears that while keeping the user-facing install (`npm install -g
mcp-local-hub`) and command (`mcphub`) unchanged. Users never type the scoped
names — npm resolves them through `optionalDependencies`.
