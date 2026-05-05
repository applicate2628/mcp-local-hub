# Security policy

## Preview-status caveat

`mcp-local-hub` is preview software. The maintainers tighten security incrementally; expect ongoing changes to admission gates, manifest validation, subprocess hardening, and CSRF defenses. The threat model centers on a **single-user local Windows desktop** install — see "Threat model" below.

## Reporting a vulnerability

Please **do not** open a public GitHub issue for a suspected vulnerability. Use one of:

1. **GitHub private vulnerability report** — `Security` tab → `Report a vulnerability` on this repository. Preferred.
2. **Email** — open the [repo's About page](https://github.com/applicate2628/mcp-local-hub) and use the maintainer's contact email (publicly listed under the GitHub profile).

When reporting, please include:

- A concise description of the issue.
- Affected component(s) — typically a path under `internal/` or `cmd/`.
- A reproduction recipe (steps, sample manifest, sample HTTP request, etc.). Redact any real secrets.
- The mcphub version (`mcphub --version`) and OS.
- An assessment of impact (RCE / privilege escalation / data exposure / DoS / config tampering / etc.) and the trust boundary crossed.

We acknowledge reports within **3 business days**. We aim to triage within **7 business days** and ship a fix-or-mitigation within **30 days** for medium-or-higher findings, faster for critical ones.

## Coordinated disclosure

We prefer coordinated disclosure. If you have a public-disclosure deadline you intend to honor, please tell us up front so we can plan the patch and downstream notification windows around it. We will credit the reporter in the relevant CHANGELOG entry unless you ask to remain anonymous.

## What we do NOT consider security issues

These are out of scope for security reports — file them as regular GitHub issues if relevant:

- **Local-attacker-with-existing-shell scenarios** that require an attacker who already has the user's OS-level shell access. The threat model assumes a trusted local user; a malicious local actor with shell access can already read `%LOCALAPPDATA%\mcp-local-hub\` directly.
- **Tooling that explicitly executes commands** when the user has manually enabled it (e.g. `perftools.clang_tidy` runs clang-tidy locally; `godbolt` sends source to the public godbolt.org service). These document their footprint.
- **Self-induced vulnerabilities** caused by a user manually editing client config to point at a non-loopback host or by disabling the loopback admission guard.
- **Network attacks against the GUI when bound to a non-loopback address.** The GUI is hardcoded to `127.0.0.1` and the manifest schema does not expose a bind-address knob; if you find a way to bypass that, please report it.

## Threat model

`mcp-local-hub` defends against:

| Threat | Surface | Defense |
|---|---|---|
| DNS-rebinding browser attack on GUI | `127.0.0.1` HTTP server | `requireAllowedHost` Host-allowlist + Origin/Sec-Fetch-Site CSRF gate |
| LAN-attacker via Vite dev proxy | `npm run dev` proxy | Loopback-only Host preflight + same-origin Origin rewrite |
| DNS-rebinding browser attack on stdio daemons | each `127.0.0.1:<port>/mcp` | `rejectUnsafeLoopbackRequest` (`net/netip.IsLoopback`) + Content-Type gate |
| Path-traversal in subprocess invocation | `perftools.llvm_objdump`, `perftools.iwyu` | `validateBinaryInsideRoot` (double `filepath.EvalSymlinks` of root + target, then `filepath.Rel` inside-root assertion); required `project_root` parameter; filesystem root rejected as project boundary; `extra_args` denylist rejects `@FILE` response-files, positional inputs, and path-valued flags such as `--build-id=`, `--debug-file-directory=`, `--dsym=`, `--prefix=`, `--prefix-strip=`, plus IWYU-specific `-Xiwyu`, `--mapping_file=`, `--export_mappings=`, `--check_also=`, `--keep=` (see `internal/perftools/path_guard.go` for the full live list) |
| Hostile compiler-flag injection in clang-tidy | `perftools.clang_tidy` | `extra_args` denylist for mutating flags (`-fix`, `--fix`, `--fix-errors`, `-fix-notes`, `--fix-notes`), plugin loading (`-load`, `--load`), config-file injection (`-config`/`--config`/`-config-file`/`--config-file` — those re-enable arbitrary `ExtraArgs`/`ExtraArgsBefore` through YAML), and fixture export (`-export-fixes`, `--export-fixes`). Note: clang-tidy does NOT use `validateBinaryInsideRoot`; its files come from a project's `compile_commands.json`, so the gate is on flag shape, not on disk path. |
| Manifest-name confused-deputy | `install`, `migrate`, `register`, `scan`, `status` paths | `parseManifestForName(name, data)` validates name regex + Windows reserved-name + asserts YAML `m.Name` matches requested name |
| Stuck-instance recovery PID-spoof | `mcphub gui --force --kill` | Three-part identity gate: image basename + `argv[1]=="gui"` (or no-args) + start-time precedes pidport mtime |
| Argv-leaked secrets | `mcphub secrets set` | `--value` flag removed; interactive hidden prompt or `--from-stdin` only |
| Unbounded subprocess output | godbolt / perftools | Per-tool body cap (`maxGodboltResponseBytes = 480 KiB` < scanner cap, `clang_tidy` denylist for `--fix*`) |
| SSE subscriber DoS | GUI `/api/events` | Bounded `TrySubscribe` (cap 64) with atomic check+insert |

The threat model **explicitly excludes**:

- Multi-user shared workstations (each Windows user gets their own `%LOCALAPPDATA%`).
- Remote/server deployment with public ingress (the GUI binds loopback only and has no auth/TLS layer).
- Malicious manifests pulled from a marketplace (marketplace flow requires inspect → validate → dry-run → backup → apply; auto-install is rejected by design).
- Compromised npm/uvx packages referenced by manifests (we pin versions on a per-manifest basis but cannot audit upstream supply chains).

## Out-of-scope features (currently rejected for security reasons)

These remain rejected unless re-threatmodeled:

- **Mandatory single `/mcp` endpoint** — would weaken daemon isolation and migration model. Optional/opt-in form (G4) is acceptable.
- **Remote access as default** — current GUI binds loopback only; no auth/TLS layer exists.
- **Unrestricted `${cmd:...}` placeholders in manifests** — RCE surface. Adoption requires explicit unsafe gate, dry-run display, and audit output.
- **Automatic marketplace install** — violates inspect → validate → dry-run → backup → apply contract.

## Hall of fame

Reporters who have helped harden the project:

- **OpenAI Codex security cloud** — automated audit findings, batched into PR `#51` (DNS-rebind, path-traversal, manifest hardening) and PR `#128` (REVISE bundle for partial-fix improvements).

(Add yourself here when you report a valid issue, or ask us to credit you.)

## Terms and Abbreviations

- `argv` — the program-arguments array a process sees on launch; visible to other local users via `ps`/`wmic` and persisted in shell history.
- `CSRF` — Cross-Site Request Forgery; a browser-driven attack that uses a victim's authenticated session to trigger unwanted actions.
- `DNS-rebinding` — an attack that binds an attacker-controlled hostname to `127.0.0.1` so a victim's browser sends loopback requests on the attacker's behalf.
- `DoS` — Denial of Service; making a service unavailable, usually via resource exhaustion.
- `EvalSymlinks` — Go's `filepath.EvalSymlinks`, which resolves symlinks at every path component to the real on-disk path.
- `IPv4-mapped IPv6 loopback` — IPv6 addresses of the form `::ffff:127.0.0.1` that represent the IPv4 loopback inside an IPv6 socket.
- `loopback` — network traffic that does not leave the local machine (`127.0.0.0/8`, `::1`).
- `MCP` — Model Context Protocol; the protocol this hub multiplexes between clients and servers.
- `pidport` — the per-instance lock+metadata file that records a running `mcphub gui` process's PID and bound port.
- `RCE` — Remote Code Execution; a vulnerability that lets an attacker run arbitrary code on a target.
- `S1`–`S4` — section labels in the 2026-05-04 security audit (PR `#51`): `S1` DNS-rebind/Host gate, `S2` daemon HTTP loopback guard, `S3` subprocess path traversal, `S4` manifest name confused-deputy guard.
- `Sec-Fetch-Site` — a browser-emitted request header indicating whether a request is same-origin or cross-site.
- `SSE` — Server-Sent Events; a long-lived HTTP event stream the GUI uses for live updates.
