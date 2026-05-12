# G6 — Remote MCP Manifests Design

**Status:** active design, 2026-05-12. v1 spec for the G6 backlog item ("Extend manifest handling for url + headers + secrets entries so direct HTTPS servers are first-class instead of special-case wiring"). Effort estimate: ~2-3 days. Implementation gated on codex r1 review approval.

## Goal

Let an operator define a server manifest pointing at a REMOTE HTTPS MCP endpoint (e.g., context7.com) — no local daemon spawned, no per-daemon proxy port. Install writes the URL + (expanded) headers directly into client configs. Secrets referenced by `${secret:KEY}` placeholders are looked up at install time from the existing encrypted vault — the cleartext secret travels with the install, never landed in the manifest.

This complements (does not replace):
- `transport: stdio-bridge` — local process spoken-to over stdio, proxied through hub.
- `transport: native-http` — local process exposing HTTP, proxied through hub.
- **NEW** `transport: remote-http` — remote HTTPS endpoint, NO hub proxy; client connects directly.

## Architecture

Remote servers don't participate in the per-daemon-port model. There's no daemon process to manage, no scheduler task, no log file (the upstream is responsible for its own logging). The hub's role for remote servers is **manifest validation + install-time placeholder expansion + secrets routing + client config write**.

### Manifest schema additions

```yaml
name: context7
kind: global
transport: remote-http       # NEW transport value
url: https://mcp.context7.com/mcp   # NEW required field for remote-http
headers:                     # NEW: HTTP headers sent on every request
  Authorization: "Bearer ${secret:CONTEXT7_TOKEN}"  # ${secret:KEY} expansion
client_bindings:
  - client: claude-code
  - client: codex-cli
  - client: cursor
weekly_refresh: false        # remote servers have no daemon to refresh; field ignored
```

`ServerManifest` struct gains:

```go
type ServerManifest struct {
    // ...existing fields...
    URL     string            `yaml:"url"`     // remote-http only
    Headers map[string]string `yaml:"headers"` // remote-http only
    // existing Command, BaseArgs, Env are NOT required when Transport=="remote-http"
}
```

`TransportRemoteHTTP = "remote-http"` constant added alongside existing `TransportNativeHTTP` and `TransportStdioBridge`.

### Validation rules

Updated `ServerManifest.Validate()`:

- For `transport: remote-http`:
  - `URL` is required.
  - `URL` must start with `https://`. **Plain `http://` is rejected** — remote servers without TLS are out of scope (operator can self-host with TLS via a reverse proxy; we don't allow plaintext credentials over the wire).
  - `Command`, `BaseArgs`, `Env`, `Daemons`, `PortPool`, `Languages`, `IdleTimeoutMin` are IGNORED (not validated for presence).
  - `WeeklyRefresh` is IGNORED.
  - `Headers` keys + values are normal strings; values may contain `${secret:KEY}` placeholders (expanded at install time).
- For `transport: stdio-bridge | native-http`:
  - Existing validation unchanged.
  - `URL` and `Headers` MUST be empty (rejected if set with non-remote-http transport).

### Secret-reference placeholders

The `${secret:KEY}` placeholder is a NEW expansion class, distinct from the existing `${env:VAR}` / `${HOME}` / `${USERPROFILE}` patterns (which expand against host env at parse time).

`${secret:KEY}` is resolved at INSTALL time, not at manifest parse time:
- The manifest stays cleartext-free — `headers.Authorization: "Bearer ${secret:CONTEXT7_TOKEN}"` is what lives on disk + git.
- `mcphub install` walks the manifest, looks each `${secret:KEY}` up in the vault via existing `internal/api/secrets.go::SecretsGet`, substitutes the cleartext, and writes the expanded headers into client config.
- If a referenced secret is missing → install fails with a clear "secret CONTEXT7_TOKEN not in vault" error before any client config is touched.
- The vault stays the single source of truth for secrets; manifests never leak them.

### Install path

`mcphub install <name>` for a remote-http manifest:

1. Parse manifest; validate (above).
2. Resolve every `${secret:KEY}` placeholder in `Headers`. Missing secrets → fail.
3. Compute per-client write set from `ClientBindings`.
4. For each binding: backup current client config (existing `BackupKeep` path); write `MCPEntry{Name, URL, Headers: expanded}` via the existing per-adapter `AddEntry` interface.
5. NO scheduler task created. NO log file allocated. NO port assigned.
6. Print "Installed remote-http server <name> for clients: <list>". Operator runs their client; the client connects directly to the upstream URL.

Uninstall: same as today — backups restore prior state; the entry is removed via adapter `RemoveEntry`.

### Adapter compatibility

Reusing G7's adapter capability matrix (which checks header support):

| Adapter | Custom headers | Remote-http support |
|---|---|---|
| claude-code | yes | yes |
| codex-cli | yes | yes |
| cursor | yes | yes |
| vscode | yes | yes |
| gemini-cli | TBD (plan-time verify) | TBD |
| qwen-cli | TBD (plan-time verify) | TBD |
| antigravity | no (relay-tuple only) | no — install refuses + WARN |

For antigravity, the operator can manually run `mcphub relay <name>` as a child stdio process that forwards to the remote URL — but that's the existing G2/G3 relay path, not a remote-http manifest. G6 doesn't add new relay logic; antigravity remote-http installs just fail with a clear message.

## CLI surface

`mcphub install <name>` and `mcphub manifest create <name>` work as today — they take a manifest YAML (from disk or stdin) and apply it. Remote-http detection happens inside Parse/Validate.

NEW: `mcphub manifest test-remote <name>` — quick smoke command that:
- Loads the manifest, expands secrets.
- Sends an `initialize` JSON-RPC POST to `manifest.URL` with the expanded headers.
- Prints the upstream's response (protocolVersion + serverInfo) or surfaces the error.

This lets operators verify a remote-http manifest is reachable + credentialed correctly BEFORE running install. Standalone — no client config side effects.

## Hub interaction (G4)

Remote-http servers do NOT participate in the G4 unified hub endpoint. The hub aggregates LOCAL daemons; remote servers go straight from client → URL with no local proxy. The hub's per-client URL `/clients/{adapter-id}/mcp` only exposes tools from `stdio-bridge` / `native-http` daemons.

This is intentional — proxying a remote HTTPS endpoint through the local hub:
- Doubles the network latency for every tool call.
- Forces credentials through the hub auth gate (which has different rotation semantics than the upstream's API tokens).
- Conflates two threat models (local trust boundary vs remote API trust).

If a future operator wants hub-aggregated remote servers, they can self-host a stdio-bridge or native-http wrapper that proxies to the remote URL — but that's an explicit choice, not the default.

## G5 marketplace + G7 vscode-workspace integration

When G5 marketplace fetches catalog entries with `transport: "http"`, OR when G7 imports VS Code workspaces with `type: "http"` server entries, the generated drafts can NOW use `transport: remote-http` (instead of being skipped with a G6-deferral warning). After G6 ships:

- G5 `marketplace generate <id>` for http entries: emit `transport: remote-http` + `url:` + `headers:` (operator sees a draft they can use immediately).
- G7 `import vscode-workspace`: emit `transport: remote-http` for `type: http` entries.

Both surfaces still skip `type: sse` entries — SSE is a different transport contract that v0.3.0 doesn't model. `sse` lands in a future G-phase.

## Out of scope (MVP)

| Feature | Reason |
|---|---|
| `sse` transport | Different protocol contract; defer. |
| HTTP (plaintext) URLs | Security — plaintext credentials rejected; force operators to TLS-terminate. |
| Token rotation per remote server | Out of scope; operators rotate via vault + reinstall. |
| Caching upstream responses | Latency optimization; out of MVP. |
| Remote-server health probe in `/api/health` | G4 doesn't aggregate remote; G2 health endpoint focuses on local daemons. |
| Auto-discovery of remote endpoints | Operator provides URL explicitly. |

## Threat model

| Vector | Mitigation |
|---|---|
| Plaintext credentials over the wire | `https://` enforced at validate time; `http://` rejected. |
| Secret leakage via manifest commits | Manifest never carries cleartext — `${secret:KEY}` lookup happens at install time. |
| Stale credentials after rotation | Vault is the single source of truth; rotation via `mcphub secrets edit` + `mcphub install` re-resolves placeholders. |
| Hostile upstream URL | Operator-supplied — trust is operator responsibility. Future v0.4.x can add SSL pinning. |
| Header injection via `${secret:KEY}` value containing newline | Validate expanded header values: reject `\r` and `\n` before write. |
| Remote-http manifest with missing secret | Install fails BEFORE backup runs (validate-first ordering). |
| `${secret:KEY}` referenced in URL field | Allowed (e.g., a path-segment token); same validation as headers — reject `\r\n` injection. |

## Test surface

**Unit:**

- `manifest_test.go` (extend): `transport: remote-http` validates with url-only; rejects http://; rejects empty url; rejects `command:` set with remote-http.
- `secrets_placeholder_test.go`: `${secret:KEY}` expansion happy path; missing secret → error; expanded value with `\r\n` → reject; URL placeholder expansion.
- `install_test.go` (extend): remote-http install writes URL + expanded headers + no scheduler task + no log file; uninstall restores prior state.

**Integration:**

- `manifest_test_remote_test.go`: `mcphub manifest test-remote` sends initialize → fixture server responds → success path; bad URL → error; missing secret → error.

**Manual smoke**: `docs/phase-3b-ii-verification.md` D2.9: create remote-http manifest pointing at a fake fixture server, install, verify client config got the URL + headers, run `mcphub manifest test-remote` against it, observe expected response.

## Acceptance criteria

- Manifest schema accepts `transport: remote-http` + `url:` + `headers:`.
- Validation rejects `http://`, empty url, `command:` set with remote-http, conflicting `daemons[]`.
- `${secret:KEY}` placeholders in `headers` + `url` expanded at install time via existing vault.
- Missing secrets fail install BEFORE backup.
- Install writes URL + expanded headers to client config; no scheduler/daemon/log allocated.
- Uninstall removes client entries cleanly.
- `mcphub manifest test-remote` smoke command works.
- G5 marketplace + G7 vscode-import can emit `transport: remote-http` drafts (cross-spec coupling).
- Adapter capability matrix honored; antigravity install refuses + WARN.
- Header values containing `\r` or `\n` after expansion → reject.

## Files to create / modify

| File | Kind | Purpose |
|---|---|---|
| `internal/config/manifest.go` | modify | URL, Headers fields; TransportRemoteHTTP constant; Validate branches |
| `internal/api/manifest.go` | modify | manifest-name validation unchanged; schema additions visible |
| `internal/api/secrets_placeholder.go` | new | `${secret:KEY}` expansion + injection-rejection (\r\n) |
| `internal/api/install.go` | modify | remote-http branch: skip scheduler/log/daemon; expand-then-write client config |
| `internal/api/manifest_remote_test.go` | new | `mcphub manifest test-remote` impl + tests |
| `internal/cli/manifest_test_remote.go` | new | CLI subcommand |
| `internal/cli/root.go` | modify | register subcommand |
| `internal/api/import_vscode.go` | modify | G7's http/sse skip path: emit `transport: remote-http` for http entries (G6 closure for G7's TODO) |
| `internal/api/marketplace_generate.go` | modify | G5's http skip path: emit `transport: remote-http` for http entries |

## Terms and Abbreviations

- `MCP`: Model Context Protocol; JSON-RPC over Streamable HTTP.
- `remote-http`: NEW transport value — server is a remote HTTPS endpoint, no local daemon.
- `stdio-bridge`: existing transport — local process spoken to over stdio.
- `native-http`: existing transport — local process exposing HTTP, proxied through hub.
- `${secret:KEY}` placeholder: looked up in the encrypted vault at install time; never appears in on-disk manifest.
- `vault`: existing encrypted secrets store at `<state-dir>/secrets.age`.
- `Adapter`: per-IDE installer logic (claude-code, codex-cli, cursor, etc.).
- `G7`: VS Code workspace import — emits `remote-http` for http entries once G6 ships.
- `G5`: Marketplace draft import — emits `remote-http` for http catalog entries once G6 ships.
