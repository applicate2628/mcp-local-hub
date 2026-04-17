# Post-Phase 1 — Antigravity relay (HTTP daemon → stdio client)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Restore Antigravity IDE's ability to use the shared Serena daemon that Claude Code / Codex CLI / Gemini CLI already share, without requiring Antigravity to adopt loopback-HTTP MCP support. Add a new `mcp relay` subcommand that exposes any HTTP MCP endpoint as a stdio MCP server by forwarding JSON-RPC between the client's stdin/stdout and the daemon's HTTP endpoint.

**Why now (not deferred to Phase 2):** Phase 1 closed with Antigravity excluded from managed bindings because its Cascade agent silently drops loopback-HTTP entries regardless of schema. That exclusion undoes mcp-local-hub's core value proposition for multi-window Antigravity users — each IDE window spawns its own `uvx serena` (~200-500 MB RAM, cold gopls indexing per workspace). This post-Phase 1 work restores the shared-daemon invariant for all four supported clients before moving on to Phase 2's memory-server work.

**Tech stack:** Go 1.22+ (stdlib `net/http` + `bufio` + `encoding/json`), no new dependencies. Test harness uses `net/http/httptest`.

**Reference implementations:**
- [`mcp-proxy`](https://github.com/sparfenyuk/mcp-proxy) — Python implementation of the same HTTP→stdio direction; consult for SSE framing edge cases
- MCP Streamable HTTP spec: https://modelcontextprotocol.io/docs/concepts/transports (streamable-http section)
- Our own `internal/daemon/bridge.go` (Task 15 of Phase 0-1) — reverse direction (stdio→HTTP via supergateway), useful as a shape reference

**Spec reference:** `docs/superpowers/specs/2026-04-16-mcp-local-hub-design.md` (keep updated per §3.6 client adapter table and §3.4.3 stdio-bridge lane)

**Prerequisites:**
- Phase 1 install artifacts present and functional — shared Serena daemon on 9121 reachable, client configs have Serena entries for Claude/Codex/Gemini
- `internal/daemon/bridge.go` and `internal/daemon/launcher.go` exist (Tasks 14-15 of Phase 0-1 plan)
- User on Windows 11 (Linux/macOS not in scope until scheduler stubs are replaced)

---

## Naming and flag conventions

**Subcommand:** `mcp relay`

**Flags:**

| Flag | Meaning |
|---|---|
| `--server <name>` | Manifest-aware mode: look up `servers/<name>/manifest.yaml`, find `--daemon`'s port, construct URL |
| `--daemon <name>` | Required with `--server`; picks which daemon from the manifest |
| `--url <url>` | Direct mode: use this HTTP endpoint verbatim; mutually exclusive with `--server`/`--daemon` |
| `--project <path>` | Optional: pass `X-Mcp-Project-Path` header (future Phase 3 use; pass-through for now, server may ignore) |

**Primary invocation** (used by Antigravity adapter after Task 11):
```
mcp.exe relay --server serena --daemon claude
```

**Escape hatch for ad-hoc setups**:
```
mcp.exe relay --url http://localhost:9121/mcp
```

Both modes produce the same behavior past argument resolution.

---

## File structure additions

```
internal/
├── cli/
│   ├── relay.go                 # NEW: newRelayCmdReal() cobra command
│   └── relay_test.go            # NEW: integration test with httptest.Server
├── daemon/
│   └── relay.go                 # NEW: HTTPToStdioRelay core logic (reusable for Phase 2 Part A memory too)
└── clients/
    ├── antigravity.go           # MODIFY: adapter writes stdio entry invoking "mcp.exe relay ..."
    └── antigravity_test.go      # MODIFY: assertions updated for new stdio shape

servers/serena/manifest.yaml     # MODIFY: re-add client: antigravity binding
internal/config/serena_test.go   # MODIFY: expect 4 bindings again
docs/phase-1-verification.md     # MODIFY: append "Post-Phase 1: Antigravity relay" section
README.md                        # MODIFY: update Antigravity row in clients table
INSTALL.md                       # MODIFY: update Antigravity section to describe relay
docs/superpowers/specs/2026-04-16-mcp-local-hub-design.md  # MODIFY: §3.6 antigravity transport
```

Approximately 400-600 net LOC added (relay.go + tests + doc updates).

---

## Tasks

### Task 1: Research phase — confirm Streamable HTTP protocol details

**Outputs:** brief notes appended to this plan's "Protocol notes" appendix below. Cover:
- `Mcp-Session-Id` header lifecycle (client-generated? server-generated on initialize?)
- How server-initiated notifications are delivered (SSE over GET /mcp? POST response chunks?)
- Session termination signal (DELETE /mcp? client-side timeout?)
- Whether bidirectional parallelism is required (simultaneous POST + long GET) or request-response is enough

**Verify via:** reading MCP spec streamable-http section + inspecting mcp-proxy Python source + packet capture of an existing MCP session (e.g. Claude Code talking to our running Serena daemon — logs already show `POST /mcp`, `GET /mcp`, `DELETE /mcp`).

**Acceptance:** appendix has a ≤200-word summary answering all four questions. Decisions drive Tasks 2-4 implementation.

- [ ] **Step 1:** Fetch MCP Streamable HTTP docs via context7 (`/modelcontextprotocol/modelcontextprotocol` or similar)
- [ ] **Step 2:** Read `~/AppData/Roaming/Antigravity/logs/.../Claude VSCode.log` for "MCP server serena" entries showing Claude Code's HTTP transport behavior — that IS the pattern we're mimicking
- [ ] **Step 3:** Read `internal/daemon/launcher.go` and `internal/cli/daemon.go` — existing HTTP handling reference
- [ ] **Step 4:** Append "Protocol notes" section to this plan

### Task 2: Implement `internal/daemon/relay.go` (core relay logic)

Stateless relay-session struct:

```go
type HTTPToStdioRelay struct {
    URL       string
    ProjectPath string  // optional, for future per-session routing
    Stdin     io.Reader // typically os.Stdin
    Stdout    io.Writer // typically os.Stdout
    Stderr    io.Writer // typically os.Stderr, for diagnostics
}

func (r *HTTPToStdioRelay) Run(ctx context.Context) error {
    // 1. Start parallel goroutines:
    //    a. stdin reader → POST /mcp → write POST response to stdout
    //    b. GET /mcp (SSE) → stream incoming notifications to stdout
    // 2. Session-id minted on first POST (Mcp-Session-Id header on request, server echoes on response)
    // 3. On stdin EOF: DELETE /mcp (terminate session), cancel SSE goroutine, return
    // 4. On HTTP error: write synthetic JSON-RPC error to stdout so client gets a response
}
```

- [ ] **Step 1:** Define `HTTPToStdioRelay` struct with fields
- [ ] **Step 2:** Implement `readJSONRPCLine(io.Reader)` — read one line, parse as JSON-RPC 2.0 message, return raw bytes + decoded metadata (jsonrpc, method, id)
- [ ] **Step 3:** Implement `writeJSONRPCLine(io.Writer, []byte)` — write bytes + newline, flush
- [ ] **Step 4:** Implement POST path: for each stdin line, construct HTTP request with `Content-Type: application/json` + `Accept: application/json, text/event-stream` + `Mcp-Session-Id` (if set), send, handle response (chunked SSE vs single JSON)
- [ ] **Step 5:** Implement GET SSE path: long-lived GET with `Accept: text/event-stream`, parse `data: <json>` lines, write each JSON payload as a line to stdout
- [ ] **Step 6:** Implement graceful shutdown — context cancellation, DELETE /mcp request, close HTTP client
- [ ] **Step 7:** Unit tests with `httptest.NewServer` — one test per: simple request/response, notification via SSE, error propagation, shutdown

**Acceptance:** `go test ./internal/daemon/... -run TestRelay` passes 4+ scenarios. Relay against a dummy HTTP MCP server produces correct stdio output for a canned request script.

### Task 3: Implement `internal/cli/relay.go` (cobra subcommand)

Thin wrapper around Task 2's `HTTPToStdioRelay`:

```go
func newRelayCmdReal() *cobra.Command {
    var server, daemon, url, project string
    c := &cobra.Command{
        Use:   "relay",
        Short: "Forward stdio MCP to an HTTP MCP endpoint (for clients that lack HTTP support)",
        RunE: func(cmd *cobra.Command, args []string) error {
            resolvedURL, err := resolveRelayURL(server, daemon, url)
            if err != nil {
                return err
            }
            r := &daemon.HTTPToStdioRelay{
                URL: resolvedURL, ProjectPath: project,
                Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
            }
            return r.Run(cmd.Context())
        },
    }
    c.Flags().StringVar(&server, "server", "", "server name (looks up manifest)")
    c.Flags().StringVar(&daemon, "daemon", "", "daemon name within the server manifest")
    c.Flags().StringVar(&url, "url", "", "direct HTTP URL (alternative to --server/--daemon)")
    c.Flags().StringVar(&project, "project", "", "optional project path for per-session routing")
    return c
}

func resolveRelayURL(server, daemon, explicitURL string) (string, error) {
    if explicitURL != "" {
        if server != "" || daemon != "" {
            return "", errors.New("--url is mutually exclusive with --server/--daemon")
        }
        return explicitURL, nil
    }
    if server == "" || daemon == "" {
        return "", errors.New("either --url or both --server and --daemon are required")
    }
    // Parse servers/<server>/manifest.yaml, find daemon by name, construct URL
    // Reuse existing config.ParseManifest
    ...
}
```

- [ ] **Step 1:** Write `newRelayCmdReal()` in `internal/cli/relay.go`
- [ ] **Step 2:** Implement `resolveRelayURL()` with manifest lookup
- [ ] **Step 3:** Wire into `internal/cli/root.go`: add `root.AddCommand(newRelayCmd())` + `func newRelayCmd() { return newRelayCmdReal() }`
- [ ] **Step 4:** Test via `go build -o mcp.exe ./cmd/mcp && ./mcp.exe relay --help` — verify all 4 flags documented
- [ ] **Step 5:** Unit test `resolveRelayURL` with 4 cases: (url only, server+daemon, both set, neither)

**Acceptance:** `./mcp.exe relay --help` shows usage. `./mcp.exe relay --server serena --daemon claude` runs and forwards stdin to the daemon. `./mcp.exe relay` with missing flags exits non-zero with a clear error.

### Task 4: Integration test — end-to-end relay against running daemon

Not automated — smoke test:

- [ ] **Step 1:** Verify Serena daemon running on 9121 (from Phase 1 install)
- [ ] **Step 2:** Run `echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"relay-smoke-test","version":"0"}}}' | ./mcp.exe relay --server serena --daemon claude`
- [ ] **Step 3:** Expect stdout line with `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":..., "serverInfo":{"name":"Serena",...},"capabilities":{...}}}`
- [ ] **Step 4:** Verify serena-claude.log shows a new session (POST /mcp 200 OK, ListToolsRequest)

**Acceptance:** initialize + list_tools round-trip works through relay.

### Task 5: Update `internal/clients/antigravity.go` to write stdio-via-relay entry

Reverse of commit `96c9aa3` (which removed Antigravity from bindings). New entry shape written to `~/.gemini/antigravity/mcp_config.json`:

```json
"serena": {
  "command": "C:\\path\\to\\mcp.exe",
  "args": ["relay", "--server", "serena", "--daemon", "claude"],
  "disabled": false
}
```

Path to `mcp.exe` = `os.Executable()` at install time (same as what's already passed to Task Scheduler `.Command`).

- [ ] **Step 1:** Modify `antigravityClient.AddEntry` to detect "we're in an HTTP-relay binding context" — need a new signal. Options:
  - (A) Add `RelayVia` field to `MCPEntry` struct — explicit, clean
  - (B) Detect by URL starting with `http://` and make adapter choice — implicit
  - (C) Antigravity adapter ALWAYS writes stdio-relay entry (never direct HTTP), since we empirically know Cascade rejects HTTP

  **Decision: option C.** The adapter knows it's for Antigravity, and we've proven HTTP doesn't work there. Simplest. If future versions add HTTP support we flip the implementation.
- [ ] **Step 2:** Adapter takes `exePath string` in constructor or via a setter so it can embed into args. `install.go` calls `client.SetRelayExecutable(os.Args[0])` before `AddEntry`.
- [ ] **Step 3:** Rewrite `AddEntry` to produce the stdio-relay entry shape

**Acceptance:** given MCPEntry{Name:"serena", URL:"http://localhost:9121/mcp"}, Antigravity adapter writes `{command, args:[relay, --server, serena, --daemon, claude], disabled:false}` — specifically `args` must include `relay` subcommand + both `--server` and `--daemon` flags derived from entry context. URL is NOT written (Antigravity doesn't speak HTTP).

### Task 6: Update install flow to pass server/daemon context to adapter

Current `install.go:executeInstall` calls `client.AddEntry(clients.MCPEntry{Name: m.Name, URL: u.URL})` — Antigravity adapter needs MORE than just URL to generate relay args. Either:

- (a) Extend `MCPEntry` with `RelayServer string, RelayDaemon string` fields; install populates them from `Plan.ClientUpdatePlan`
- (b) Antigravity adapter parses URL to extract port, cross-references manifest in its own helper

**Decision: option (a).** Explicit, no reverse-engineering. Phase 3 workspace-scoped daemons will need similar context-passing anyway.

- [ ] **Step 1:** Add `RelayServer, RelayDaemon string` fields to `MCPEntry`
- [ ] **Step 2:** In `install.go`'s `executeInstall`, pass `m.Name` and `daemonName` when constructing the entry
- [ ] **Step 3:** Other adapters (Claude/Codex/Gemini/stdio) ignore these fields — backward-compatible
- [ ] **Step 4:** Test updates: Claude/Codex/Gemini tests should not regress; Antigravity tests should assert the new args shape

**Acceptance:** `go test ./internal/clients/...` passes all existing + new Antigravity assertions.

### Task 7: Re-add `client: antigravity` binding to manifest

Reverse of commit `96c9aa3`:

```yaml
client_bindings:
  - client: claude-code
    daemon: claude
    url_path: /mcp
  - client: codex-cli
    daemon: codex
    url_path: /mcp
  - client: antigravity     # re-added, now routes via relay subprocess
    daemon: claude
    url_path: /mcp
  - client: gemini-cli
    daemon: claude
    url_path: /mcp
```

- [ ] **Step 1:** Edit `servers/serena/manifest.yaml` — add antigravity binding
- [ ] **Step 2:** Update `internal/config/serena_test.go` — expect 4 bindings, remove the "antigravity must NOT appear" regression guard (or invert it)
- [ ] **Step 3:** Update comment block in manifest explaining the relay-based approach

**Acceptance:** `go test ./internal/config/... -run TestSerena` passes.

### Task 8: Live E2E — reinstall + verify Antigravity sees Serena

- [ ] **Step 1:** `./mcp.exe uninstall --server serena` (clear prior state)
- [ ] **Step 2:** Rebuild: `go build -o mcp.exe ./cmd/mcp`
- [ ] **Step 3:** `./mcp.exe install --server serena` — expect 4 client updates now (antigravity included)
- [ ] **Step 4:** Verify `~/.gemini/antigravity/mcp_config.json` has stdio-relay entry for serena
- [ ] **Step 5:** Fully kill Antigravity (all 20+ processes): `Get-Process -Name Antigravity | Stop-Process -Force`
- [ ] **Step 6:** Restart Antigravity, open any project
- [ ] **Step 7:** Ask Cascade: "list your MCP tools" — serena tools should appear
- [ ] **Step 8:** Verify `%LOCALAPPDATA%\mcp-local-hub\logs\serena-claude.log` shows new Antigravity-origin requests (different session IDs than Claude's)
- [ ] **Step 9:** Ask Cascade: "use find_symbol to locate main in this repo" — tool call should succeed through the relay

**Acceptance:** Antigravity's Cascade agent shows serena tools in its MCP list AND successfully round-trips a tool call through the shared daemon.

### Task 9: Update documentation

- [ ] **Step 1:** `README.md` — update Antigravity row in clients table: change "not managed (see below)" to "stdio via `mcp relay` subprocess (shared with 9121)". Update caveat section.
- [ ] **Step 2:** `INSTALL.md` — rewrite Antigravity section. Describe stdio-relay entry shape, note that `mcp.exe` must be on an absolute path.
- [ ] **Step 3:** `docs/phase-1-verification.md` — append "Post-Phase 1: Antigravity relay" section with E2E results from Task 8
- [ ] **Step 4:** `docs/superpowers/specs/2026-04-16-mcp-local-hub-design.md` — §3.6 client adapter table: Antigravity transport column from "stdio (unmanaged)" to "stdio via mcp.exe relay"
- [ ] **Step 5:** Add new section §3.4.3 in design spec: "Client-side relay pattern" — architectural rationale

**Acceptance:** documentation accurately describes the new state; no references to "Antigravity excluded" remain except in historical/verification context.

### Task 10: Commit discipline

Six commits, each compilable and test-green:

1. `feat(daemon): HTTP→stdio relay primitives (protocol + core)` — Task 2
2. `feat(cli): relay subcommand (mcp relay --server/--daemon/--url)` — Task 3
3. `feat(clients): Antigravity writes stdio-relay entry (MCPEntry.RelayServer/RelayDaemon)` — Tasks 5-6
4. `feat(serena): re-add antigravity binding (now via relay subprocess)` — Task 7
5. `docs: post-Phase 1 relay — README/INSTALL/spec/verification updates` — Task 9
6. `docs: Phase 1.5 verification — Antigravity live-tested through relay` — concludes Task 8 E2E in phase-1-verification.md

Commit after each file-group task set, NOT one mega-commit.

---

## Acceptance criteria (phase-level)

1. **Unit tests green:** all packages, new ≥6 relay tests added, existing tests not regressed
2. **Build green:** `go build ./...` on Windows; cross-compile `GOOS=linux`, `GOOS=darwin` still passes
3. **Install idempotent:** `mcp install --server serena` twice produces the same state, no surviving backup-explosion
4. **Antigravity sees serena:** Cascade's MCP list enumerates all 20 Serena tools under its namespace
5. **Round-trip works:** `find_symbol` call from Cascade returns correct results from shared daemon's gopls
6. **Shared daemon invariant:** exactly 2 Serena processes running (9121 + 9122), regardless of how many Antigravity windows or Claude/Codex/Gemini sessions are active
7. **Process count claim:** 3 Antigravity windows + 1 Claude Code + 1 Codex session = 2 Serena processes + 3 `mcp.exe relay` subprocess (per Antigravity window); verify via `Get-Process`
8. **Gitignored build backups cleaned:** no `mcp.exe~` or `.serena/` leaks (gitignore already covers, just verify)

---

## Out of scope

- Phase 2 Part A (memory server via stdio-bridge) — separate plan after this
- Linux/macOS scheduler — untouched
- `mcp relay` HTTPS support — only http:// URLs for now (loopback only); HTTPS+auth is a future concern
- OAuth bearer-token passthrough via relay — if a future HTTP daemon requires auth, relay needs to forward/inject `Authorization` header. Not needed for Serena 1.x.
- Relay for bidirectional MCP initiation (server-side requests asking client for things) — only request/response + server-to-client notifications. Bidirectional initiation is rare in current MCP servers and adds significant complexity.
- Pooling / reuse of relay processes across client sessions — each Antigravity window spawns its own relay, that's fine

---

## Protocol notes (from Task 1 research, 2026-04-17)

Sources: MCP spec 2025-11-25 (`basic/transports`) via context7, plus empirical observation of Claude Code extension ↔ Serena live handshake (`serena-claude.log` + `~/AppData/Roaming/Antigravity/logs/.../Claude VSCode.log`).

### Stdio side (Antigravity ↔ relay, via stdin/stdout)

- **Framing:** line-delimited JSON-RPC 2.0 messages. Each message is one line, terminated by `\n`. Messages **MUST NOT** contain embedded newlines. `{"jsonrpc":"2.0","id":1,"method":"...","params":{...}}` followed by `\n`.
- **Stderr:** relay writes any diagnostic/logging here; stdio client (Antigravity) may ignore or capture.
- **Hard rule:** relay's stdout must contain **only** valid MCP messages. No banner text, no "Starting relay..." lines.
- **Message types:** requests (has `id`), notifications (no `id`), responses (has `id`, has `result` or `error`). All flow through the same line protocol; client→server direction is primarily requests+notifications, server→client is responses+notifications (and rarely server-initiated requests).

### HTTP side (relay ↔ Serena daemon, Streamable HTTP transport)

**Endpoint:** single URL (e.g. `http://localhost:9121/mcp`) supports POST and GET.

**Client → Server (POST /mcp):**
- Headers: `Content-Type: application/json`, `Accept: application/json, text/event-stream` (both required, server may choose either response type), `MCP-Session-Id: <id>` (after session established).
- Body: single JSON-RPC message.
- **Response depends on body type:**
  - If body was a **notification or a client→server response** → server returns `202 Accepted` with **no body**. No further work.
  - If body was a **request** (has `id`) → server returns:
    - (a) `200 OK` with `Content-Type: application/json` and a single JSON-RPC response in body, OR
    - (b) `200 OK` with `Content-Type: text/event-stream` and an SSE stream containing potentially multiple messages (server-initiated notifications/requests before the final response, then the response matching the request's `id`, then optionally more messages or stream close).
  - Server chooses (a) or (b) per-request. Relay must handle both. Real Serena empirically uses **both** — simple tool calls get (a), long-running operations (e.g. `find_symbol` during gopls cold indexing) may get (b).
- **Error responses:** `400` with optional JSON-RPC error body (missing/invalid session, malformed message, etc.). `404` specifically means "session unknown" — client must re-initialize.

**Server → Client (GET /mcp, SSE):**
- Client opens long-lived `GET /mcp` with `Accept: text/event-stream` and session header.
- Server streams events as they arrive; each event is `data: <json>\n\n` (optionally prefixed `id: <event-id>\n` and `event: <type>\n`).
- Event payload = one JSON-RPC message (request/notification from server; NOT a response unless this stream is a resumption).
- Server **MAY** close connection at any time; relay reconnects using `Last-Event-ID` header for resumption (nice-to-have, not required for v1).

### Session lifecycle

- **Header name:** `MCP-Session-Id` (exact casing per spec; HTTP headers are case-insensitive but we preserve spec casing in our code for clarity).
- **Mint:** server includes `MCP-Session-Id: <uuid-or-similar>` in the **response headers** of the very first POST (which carries `InitializeRequest` and has no session header on the request side). Session ID is opaque to client — relay captures and echoes.
- **Reuse:** relay injects `MCP-Session-Id: <captured>` into every subsequent POST and into the parallel GET. If server ever returns 404 on a request, session expired — relay must issue fresh initialize and re-establish.
- **Termination:** when stdin EOF arrives (Antigravity killed relay), relay issues `DELETE /mcp` with the session header, then closes HTTP client. Server may also independently drop session; relay handles 404 gracefully.

### Edge cases for implementation

1. **Simultaneous writes to stdout** — POST response goroutine and GET SSE goroutine both produce stdio lines. Guard with `sync.Mutex` around stdout write path, or route everything through a single serializing goroutine via channel.
2. **Response-before-request** on SSE stream — event IDs may arrive out of order relative to POST responses. Relay must pass each message to stdout as-is; client (Antigravity) correlates by `id` field.
3. **HTTP 5xx / network error on POST** — relay synthesizes a JSON-RPC error response with the original request's `id` and writes to stdout. Otherwise Antigravity hangs awaiting response.
4. **Malformed stdin JSON** — log to stderr, skip. Antigravity is the trusted source, this would be a bug in it, not a runtime security concern.
5. **SSE reconnect** — on connection drop during streaming, relay retries GET `/mcp` with `Last-Event-ID` header. For v1, simple "retry once after 1s" is sufficient — don't add exponential backoff complexity.
6. **Concurrent POSTs** — MCP allows multiple outstanding requests from client. Relay must not serialize them; each POST gets its own goroutine and returns its response to stdout independently.

### Implementation sketch

```go
// HTTPToStdioRelay structure (internal/daemon/relay.go)
type HTTPToStdioRelay struct {
    URL           string
    Stdin         io.Reader
    Stdout        io.Writer
    Stderr        io.Writer
    HTTPClient    *http.Client  // default: 60s total timeout, loopback-only
    sessionID     atomic.Value  // string, mint on first response header
    stdoutMu      sync.Mutex    // serialize writes
}

// Goroutine 1: stdin → POST → stdout
func (r *HTTPToStdioRelay) stdinPump(ctx context.Context) error { ... }

// Goroutine 2: GET /mcp (SSE) → stdout
func (r *HTTPToStdioRelay) sseListener(ctx context.Context) error { ... }

// Entry point: parallel start both, wait for either stdin EOF or ctx cancel
func (r *HTTPToStdioRelay) Run(ctx context.Context) error {
    errCh := make(chan error, 2)
    go func() { errCh <- r.stdinPump(ctx) }()
    go func() { errCh <- r.sseListener(ctx) }()
    // First error wins, other goroutine canceled via ctx
    ...
    // On shutdown: DELETE /mcp with session id
    r.terminateSession()
    return firstErr
}
```

Estimated Task 2 LOC: ~250 Go + ~150 test. Total relay.go ≈ 300 LOC, well within "single-file-fits-in-head" territory.

---

## Post-plan rollover

On phase completion:
- `docs/phase-1-verification.md` appends the final Task 8 verification results
- The exclusion rationale for Antigravity documented in the original Phase 1 verification becomes historical (doc not edited, just superseded by Post-Phase 1 section)
- Design spec §3.6 now reflects uniform architecture: all 4 clients go through shared daemons via appropriate transport adapters
- Phase 2 proper (memory server + stdio→HTTP supergateway bridge) can proceed with `internal/daemon/relay.go` primitives reusable for its needs
