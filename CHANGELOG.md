# Changelog

All notable changes to `mcp-local-hub` will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project will adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html) once a 1.0 tag is cut. Until then the public surface (manifest schema, HTTP API, CLI flags) may change in incompatible ways between preview tags.

## [Unreleased]

### Added

- **G10 community docs** — `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CHANGELOG.md` ([phase-3b-ii-backlog#G10](docs/superpowers/plans/phase-3b-ii-backlog.md)).

### Planned for the next preview tag

- **G1** — README feature/readiness matrix (transports, auth, capabilities, GUI/CLI/API surfaces, tested platforms, known gaps).
- **G2** — unified `/api/health` endpoint that combines daemon status, client routing, ports, process info, workspace registry, and probe summaries.
- **Pre-tag fix** — `daemons.retry_policy` Settings UI labeled "saved only; runtime applier deferred".
- **Manual smoke** — `docs/phase-3b-ii-verification.md` D2 + D3 on a real Windows desktop session.

## [Phase 3B-II security audit] — 2026-05-04 → 2026-05-05

### Security

- **DNS-rebinding gate (S1)** — entire GUI mux wrapped in `requireAllowedHost` middleware; rejects `Host` header that is not loopback on the backend port. `requireSameOrigin` honors both `Origin` and `Sec-Fetch-Site` (no OR fallback). Vite dev proxy enforces loopback-only host preflight before forwarding (PR `#51`).
- **Daemon HTTP loopback guard (S2)** — `rejectUnsafeLoopbackRequest` applied to `StdioHost.HTTPHandler` and `LazyProxy.handleMCP`. Uses `net/netip.IsLoopback` for strict classification (PR `#51`).
- **Subprocess path-traversal guard (S3)** — `validateBinaryInsideRoot` shared helper for `llvm-objdump` and `iwyu`. Required `project_root` parameter; double `filepath.EvalSymlinks` + inside-root assertion via `filepath.Rel`; filesystem root rejected as project boundary. Tool-specific `extra_args` denylists reject `@FILE` response-file directives, positional inputs, and path-valued flags (`--build-id=`, `--debug-file-directory=`, `--prefix=`, `--prefix-strip=`, plus IWYU's `-Xiwyu`, `--mapping_file=`, `--export_mappings=`, `--check_also=`, `--keep=`) (PR `#51`).
- **Clang-tidy flag injection guard (S3a)** — separate denylist on `clang_tidy` extra_args (its files come from `compile_commands.json`, not a path-validated arg, so the gate is on flag shape). Rejects mutating flags (`-fix`, `--fix`, `--fix-errors`, `-fix-notes`, `--fix-notes`), plugin loading (`-load`, `--load`), config-file injection (`-config`/`--config`/`-config-file`/`--config-file` — these re-enable arbitrary `ExtraArgs`/`ExtraArgsBefore` through a YAML file), and fixture export (`-export-fixes`, `--export-fixes`) (REVISE bundle PR `#128`).
- **Manifest name confused-deputy (S4)** — `parseManifestForName(name, data)` validates name via `checkManifestName` regex + Windows reserved-name rejection (`CON`, `PRN`, `AUX`, `NUL`, `COM0-9`, `LPT0-9`) and asserts YAML `m.Name` matches the requested name. Migrated 9 `install.go` call sites + `migrate`/`register`/`scan`/`status_enrich`. `loadManifestYAMLEmbedFirst` validates at the loader boundary so direct callers cannot drive a pre-validation filesystem probe (PR `#51`).
- **Stdio scanner DoS hardening** — `maxGodboltResponseBytes` lowered from 10 MiB to 480 KiB, strictly below the 1 MiB stdout scanner cap; accounts for JSON-RPC envelope + JSON string escaping (PR `#51`).
- **Force-kill three-part identity gate** — `KillRecordedHolder` runs full image basename + `argv[1]=="gui"` + start-time-precedes-pidport-mtime gate on the production HTTP path (no longer gated on `opts.Expected`); fails closed on probe error (PR `#128`).
- **Encrypted vault export bundle** — `WriteConfigBundle` now includes the actual encrypted vault file `secrets.age` (was incorrectly looking for `secrets.json`, which never existed; the bundle silently shipped without secrets) (PR `#51`).
- **Secrets argv leak** — `mcphub secrets set --value` flag removed; replaced by interactive hidden prompt + `--from-stdin` for scripted use (PR `#128`).

### Fixed

- **30 Codex-bot APPROVE PRs merged** for various correctness + reliability fixes; full list in `git log --oneline`.
- **REVISE bundle (PR `#128`)** — 13 corrected versions of bot-flagged fixes (Content-Type ordering, `application/json` exact match via `mime.ParseMediaType`, SSE subscriber atomic admission, log rotation that survives file-leg failure, log line ordering on overflow, clang-tidy `--config`/`--fix-notes` bypass, INSTALL.md drift, manifest disk-fallback name guard, `keep_n=0` honest semantic, uninstall HTTP-native entry recognition + prefix narrowing, legacy workspace-key fallback `activeWSKey` threading, launcher-token cross-platform normalization).

### Closed (not adopted)

- Codex bot PR `#95` (`isHubDaemonPID`) — substring classifier rejected legitimate Windows hub daemons. Closed as BLOCK.
- Codex bot PR `#52` (gdb non-migratable) — workspace-scope intent correct but ports.yaml not updated; CI failed. Closed pending rework.

## Earlier history

For the pre-2026-05-04 history, see `git log` and the verification docs:

- [docs/phase-3b-verification.md](docs/phase-3b-verification.md) — Phase 3B-I MVP closeout.
- [docs/phase-3b-ii-verification.md](docs/phase-3b-ii-verification.md) — Phase 3B-II ongoing manual-smoke matrix.
