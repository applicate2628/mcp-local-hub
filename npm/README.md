# mcp-local-hub (npm distribution)

`mcp-local-hub` is the npm meta package for **mcp-local-hub** — a local
supervisor + router that compresses dozens of duplicate MCP (Model Context
Protocol) server processes spawned across parallel clients (editors, agents,
CLIs) into one managed hub.

- Project: https://github.com/applicate2628/mcp-local-hub
- License: MPL-2.0
- Author: Dmitry Denisenko (@applicate2628)

## Install

The 0.4.41 npm distribution supports **Windows x64 only**. The other five
previously packaged targets are deferred; the 0.4.41 shim rejects them even
if an older platform package is already present. Do not use `@latest` as an
implicit cross-platform compatibility promise. Choose an explicitly pinned
older version only after checking its support and availability for your host.

```bash
npm install -g mcp-local-hub@0.4.41 --allow-scripts=mcp-local-hub --foreground-scripts
# the installed command is `mcphub`:
mcphub version
# or run without installing:
npx mcp-local-hub@0.4.41 version
```

> The npm **package** is `mcp-local-hub`; the **command** it installs is
> `mcphub` (the package's `bin` entry). Install the package, run `mcphub`.

Keep the positional `mcp-local-hub@0.4.41` before
`--allow-scripts=mcp-local-hub`. Omitting that positional install root is not a
valid remediation command, and the scoped flag does not grant blanket script
trust.

The meta package ships **no binary itself**. Version 0.4.41 declares only
`@applicate2628/mcp-local-hub-win32-x64` at the same exact version in its
`optionalDependencies`. A small Node shim
(`bin/cli.js`) then locates that platform binary and execs it, passing your
arguments through and propagating its exit code.

On Windows x64, the platform package contains one native file, `mcphub.exe`
(GUI subsystem 2), plus non-executable `windows-product-identity.json`. It
declares no npm `bin` entry. The meta package is therefore the sole producer of
the public `mcphub` Node shim, which waits for `mcphub.exe` and preserves argv,
standard streams, and exit status. No windowless or admission helper executable
is shipped.

There is **no `postinstall` _download_ script** by design: a postinstall that
FETCHES a binary over the network is the top npm supply-chain attack vector.
The binary arrives purely through the optional dependency npm itself installs,
and it still resolves under `npm install --ignore-scripts` (that flag skips
lifecycle scripts, not optional dependencies).

## Direct npm execution and explicit local mode

The `mcphub` npm command resolves and launches the native payload from the
installed platform package. npm owns delivery and replacement of that payload.
The shipped `scripts/postinstall.js` is a no-op: it does not launch the product
or create another copy under `~/.local/bin`. This applies to global, local and
transitive installs.

Skipping lifecycle scripts with `--ignore-scripts` does not add a local-install
prerequisite. The shim still resolves the installed optional dependency, and
native startup owns the required Hub state reconciliation.

`mcphub setup` and `mcphub canonicalize` remain separate, explicit local-install
actions. Use them only when choosing that local mode; they are not required for
npm execution. Fixed local manifest/data storage policy remains unchanged.

A verified running Hub keeps its selected executable until its existing
handover completes. Starting an npm helper does not switch that active owner.

## 0.4.41 npm target

| npm `os`/`cpu`   | Go `GOOS`/`GOARCH` | Support tier |
| ---------------- | ------------------ | ------------ |
| `win32` / `x64`  | `windows/amd64`    | **GA** |
**GA** = generally available. Windows ARM64, Linux x64/ARM64, and macOS
x64/ARM64 are not 0.4.41 npm targets. This release neither publishes matching
0.4.41 binaries for them nor silently falls back to older native packages.

## If your platform is unsupported, or the binary did not install

The shim prints a clear error naming your platform. Check which pinned older
version or release asset is actually available for that host; 0.4.41 does not
provide a fallback binary. For a supported Windows x64 host with a missing
optional package, reinstall the exact package with optional dependencies
enabled before using the shim.

## How this directory is maintained

- `package.json` — the meta package (`mcp-local-hub`). Its `version` is the
  single version authority; `npm/sync-version.js` propagates it into the Go
  build scripts.
- `bin/cli.js` — the platform-resolver shim (Node built-ins only; imports the
  shared map from `lib/platform-binary.js`).
- `lib/platform-binary.js` — derives the runtime
  `${platform}-${arch}` → sub-package map from the shipped meta manifest and
  owns the binary basename rule for `bin/cli.js`.
- `scripts/postinstall.js` — a no-op lifecycle entry. It does not download,
  launch, or copy the product into a separate local installation.
- `generate-platform-packages.js` — regenerates the admitted
  `packages/<platform>-<arch>/` sub-packages from one GOOS/GOARCH→os/cpu map.
  The sub-packages are generated artifacts (`@applicate2628/mcp-local-hub-*`);
  do not hand-edit them.
- Platform binaries are injected into each sub-package's `bin/` by the release
  job at publish time; they are **not** committed to git.
- `../windows-product-artifacts.json` — the single build/package contract for
  exact Windows filenames, roles, source packages, PE subsystems, metadata
  policy, payload membership, and the non-executable identity manifest. The
  meta package is the sole public npm `bin` owner on Windows.

## Why the platform packages are scoped

The meta package is unscoped (`mcp-local-hub`) so the install command stays
short. Platform packages are scoped under `@applicate2628/` because a
brand-new account publishing a family of near-identical unscoped names trips
npm's spam-detection heuristic. Scoping them under the publisher's namespace
clears that while keeping the user-facing install (`npm install -g
mcp-local-hub@0.4.41 --allow-scripts=mcp-local-hub --foreground-scripts`) and
command (`mcphub`) unchanged. Users never type the scoped names — npm resolves
them through `optionalDependencies`.
