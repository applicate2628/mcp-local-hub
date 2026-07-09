# npx-stdio MCP servers orphan-accumulate because they bypass mcphub (hub-purpose failure)

- **status:** fixed
- **fixed-by:** PR #513 (`d518a511`), PR #516 (`487482cf`), and PR #520 (`c53d874a`) - CLI/GUI adopt plus auto-reaper H5 gate.
- **HEAD reconciliation (2026-07-09):** Verified against master `63b6a008`; the old residual-open note below is superseded by PR #520/#521/#522 hardening.
- **severity:** CRITICAL (raised from high 2026-07-07 by operator) — defeats mcphub's raison d'être (process-tail compression); ~360 node.exe accumulated on the dev host, 200+ were a single MCP server
- **filed:** 2026-07-04
- **context:** live-fleet / mcphub purpose / client-config routing
- **owner:** main conversation

## 2026-07-07 — operator P0 reframe + live re-confirmation (CRITICAL)

A fable-commanded 5-lane forensic (workflow `wf_e08d5606-406`) re-confirmed this LIVE: on
the dev host **65 `@mui/mcp` stdio servers + 60 orphaned `npx-cli` launchers = 125 processes
(52% of a 240-process sample)** accumulated **monotonically over 8 days** (2026-06-29 18:54 →
2026-07-07 18:29), one orphan per external-client reconnect, dead parents directly confirmed.
It bypasses the hub via a direct `npx -y @mui/mcp` entry in an external client config and is
NEVER registered with the hub, so neither the supervisor nor the 5-min auto-reaper touches it.

**Operator directive (verbatim intent):** this is NOT "external / not our bug" — **"хаб
ОБЯЗАН ВБИРАТЬ ТАКИЕ СЛУЧАИ В СЕБЯ"**: absorbing exactly these bypass npx-stdio servers is
the hub's whole purpose. The missing `mcphub adopt` / auto-onboard capability is therefore a
**CRITICAL mcphub недоработка (shortfall), not an external factor** — the fable lane's
`is_mcphub_fault:false` label is REJECTED at the product level. Ship the adopt verb (PR4) +
an auto-absorb / anti-drift surface so the hub self-onboards unmanaged npx-stdio servers, plus
the reaper `ScanClientConfigs` gap (PR5) so the leak cannot re-accumulate. Interim relief:
identity-gated sweep of the 125 leaked procs + manual `mcphub manifest create` of @mui +
repoint the spawner config (find via `grep '@mui/mcp'` across all client configs — every live
spawner parent is dead so the process table can't name the client).

## Symptom

~360 `node.exe` processes accumulated on the host; **200+ were `@mui/mcp`**
(`npx -y @mui/mcp@latest`, the MUI/Material-UI docs MCP server), the rest other
npx-stdio MCP servers. 249 dead-parent orphans were killed by hand as a **forced
fallback** (~18 in-use `@mui` + secondary npm-cli/other remain). The live mcphub
fleet (20 daemons) was untouched.

## Root cause — direct-stdio MCP servers bypass mcphub entirely

The leaking servers are configured as **DIRECT `npx` stdio MCP entries in BOTH
client configs** — codex (`~/.codex/config.toml` →
`C:\Users\dima_\OneDrive\Documents\env\Agents\.codex\config.toml`) AND claude
(`~/.claude.json`) — NOT as mcphub daemons. They do NOT appear in
`%LOCALAPPDATA%\mcp-local-hub\supervisor-intent.json`.

**Two client-config populations (from the codex config):**

- **BYPASS mcphub — direct `command='npx' args=['-y','...@latest']` stdio:**
  `mui-mcp`, `next-devtools`, `chrome-devtools`, `flowbite`, `playwright`,
  `raindrop` (mcp-remote), `repomix` (+ local-exe: codegraph, graphify, node_repl).
- **THROUGH mcphub (properly managed, no leak):** `mcphub-hub`
  (`url=http://127.0.0.1:10275/clients/codex-cli/mcp`) and every
  `mcp-language-server-*` (`url=http://127.0.0.1:9125/lsp/<lang>/mcp`). Remote-only
  (context7, exa, qt-docs, excalidraw) spawn no local process.

**The mechanism:** every client session (codex, claude, and each subagent / CLI
invocation) that starts re-spawns its OWN `npx @mui/mcp` → node stdio child. On
session exit or crash the child ORPHANS — it has no Job Object, no supervisor, no
lifecycle owner. On Windows the parent PID is not re-parented, so it lingers
forever. Hundreds of sessions → hundreds of orphans.

## Why the hub didn't handle it (the hub-purpose failure)

mcphub exists precisely to PREVENT this accumulation (the "process tails" motivation
— one shared supervised instance behind a hub URL instead of N per-session spawns;
this is already how the LSP servers work behind `9125/lsp/*` and the aggregate
behind `10275`). But mcphub can only manage what is REGISTERED as a mcphub daemon /
routed through the hub. These direct-stdio entries point the client straight at
`npx @mui/mcp` — the hub never sees them, never supervises them, never reaps them.
So the exact failure mode mcphub is built to eliminate happens for every MCP the
operator added directly to the client config instead of through mcphub. Cleaning
orphans by hand is a forced fallback the hub is supposed to make unnecessary.

## This is the SAME class mcphub already solved for serena + language-server (the mission, half-done)

The orphan-accumulation the user is fixing is mcphub's CORE problem, not a side bug.
serena and mcp-language-server were the FIRST wave of exactly this class: before
mcphub they spawned per-session npx/uvx stdio children that orphaned and piled up
(100s of duplicate node/python "process tails" — mcphub's founding motivation).
mcphub SOLVED it for them by routing them THROUGH the hub — one supervised instance
each behind a hub URL (`serena · <ws>` @ 9150-9152, `mcp-language-server-*` @
`9125/lsp/*`), Job-Object-owned + supervisor-reaped, shared across all sessions.

The 7 direct-stdio entries in this bug (mui-mcp, next-devtools, chrome-devtools,
flowbite, playwright, raindrop, repomix) are the REMAINING wave of the identical
class — they simply never got the serena/LSP treatment, so they still accumulate.
The fix is to give them the same hub-routing serena + LSP already have. The mission
is only half-done until every local-process client MCP runs once under mcphub, not
per-session.

## Fix design (fable inspector, 2026-07-04 — root-caused with file:line evidence)

### Root-cause verdicts

**Q1 (why not hub-routed) — CONFIRMED, and the surprise is that the machinery
already exists.** The "(b) stdio-MCP hub-proxy daemon kind" from the candidate list
SHIPPED long ago: `transport: stdio-bridge` (`internal/config/manifest.go:36-50`)
runs ANY stdio command under a supervised `mcphub daemon` and bridges it to HTTP via
the generic in-process `StdioHost` (`internal/cli/daemon.go:273-305`,
`internal/daemon/host.go:26-45`, `:215-217`, `:888-913`; it multiplexes concurrent
HTTP clients onto the one stdio child with request-id rewriting, `host.go:915-1118`,
and its Job-Object comment names the exact `npx → node → mcp server` tree,
`host.go:141-144`). The shipped `memory`/`fetch`/`time` daemons ARE npx/uvx stdio
servers wrapped this way (`servers/memory/manifest.yaml:1-42`,
`servers/fetch/manifest.yaml:9-28`). What is MISSING is purely the adoption seam:

1. **No one-shot adopt verb** — `internal/cli/root.go:27-67` has no
   `adopt`/`route`/`takeover`. Today adoption = 3 manual steps: `manifest extract`
   (draft-only, `internal/cli/manifest.go:225-265` →
   `api.ExtractManifestFromClient`, `internal/api/scan.go:2208-2495`) → `manifest
   create` → `install --server`. Nobody does 3 steps when `claude mcp add` /
   editing `config.toml` is 1 — so operators keep adding direct entries.
2. **Marketplace absence** — 6 of the 7 leak sources are NOT in
   `marketplace/v2/catalog.json` (only `playwright` present, `catalog.json:84-93`;
   mui/next-devtools/chrome-devtools/flowbite/raindrop/repomix absent).
3. **`--reconcile-hub-mode` is structurally blind to foreign entries** — it
   iterates ONLY manifest `ClientBindings` + the `mcphub-hub` aggregate
   (`internal/api/install_hub_reconcile.go:126-138`; every `EntryName` it writes is
   the aggregate or a manifest server name, `:177`, `:237`, `:260`, `:314`).

**Q2 (why no prevention) — CONFIRMED: detection exists, adoption doesn't.** The
scan reads EVERY client-config entry including foreign npx-stdio
(`internal/api/scan.go:983-990` claude, `:1015-1023` codex) and classifies a
manifest-less stdio entry `"unknown"` (`scan.go:2087-2089`) — so `mui-mcp` IS
visible on the Discovery screen. But the `"unknown"` bucket is a dead end: the only
actions are "Create manifest" (a NAVIGATION to Add-Server with prefill via
`GET /api/extract-manifest`, `Migration.tsx:510-516`,
`internal/gui/extract_manifest.go:71-92`) and "Dismiss" (a pure view-filter to
`gui-dismissed.json`, `internal/api/dismiss.go:96-137` — never touches the client
config). CLI `mcphub scan` even drops `"external"` rows from human output
(`internal/cli/scan.go:21-25`). Detection-without-adoption is a shelf, not
prevention. Additionally the per-server install path never emits a Remove for a
differently-named duplicate direct entry (`internal/api/install.go:1659-1663`).

**Q3 (safety net) — CONFIRMED absent.** No code path scans processes mcphub never
spawned. The only foreign-process logic is the per-task port-squatter classifier,
which is observe-only for foreign/unverified owners
(`internal/cli/supervise_squatter.go:31-37`) and reaps only verified-own squatters
(`mcphub daemon recover`). All primitives for a reaper EXIST: full process census
with CommandLine+CreationDate+ParentProcessId (`internal/api/processes.go:132-161`),
by-PID identity via CIM (`internal/process/lookup_process_identity_windows.go:52`),
and proof-gated kill `TerminatePIDWithIdentity` (exe-path + basename + start-time
verified on a held handle, fail-closed —
`internal/process/pid_identity_windows.go:51-108`). For hub-owned daemons the Job
Object already covers the whole npx→node descendant tree
(`internal/process/jobobject_windows.go:74-80`; assign-at-create closes the
Start→Assign race, `start_with_job_windows.go:89-95`) — the documented
`job_protection:false` fallback is the residual leak class a reaper would also
cover.

### Ranked fixes

**P0 — `mcphub adopt` one-shot + GUI one-click (prevention; the hub's job).**
New verb composing the EXISTING pipeline `ExtractManifestFromClient` →
`ManifestCreate` → `install --server <name>`:
- CLI: `mcphub adopt <entry> [--client X | --all-clients] [--port N] [--yes]`
  (plan-print dry-run default, like install), plus `mcphub adopt --all-unknown`
  batch sweep. GUI: "Adopt into hub" button on Discovery `"unknown"` rows →
  `POST /api/adopt {name, clients[]}` (same-origin guarded like existing routes).
- Same-name entries are replaced wholesale by the existing AddReplace (codex
  adapter explicitly drops stdio-era `command`/`args`,
  `internal/clients/codex_cli.go:94-114`; claude `claude_code.go:103-119`).
  Cross-client dedupe for DIFFERENTLY-named duplicates: emit `ClientUpdateRemove`
  for entries whose normalized command signature (command + args) matches the
  adopted one — always shown in the plan, confirm-gated.
- **Security must-fix before ship:** extract copies env LITERALLY into the draft
  (`scan.go:2479-2486`) — adopt must route sensitive env through the vault (reuse
  the marketplace sensitive-env classifier) or refuse with guidance, never persist
  cleartext tokens into `servers/<name>/manifest.yaml`.
- Port pool: `pickNextFreePort` is 9121-9139 (`scan.go:2497-2524`) — widen or give
  adopt its own range; 7+ adoptions on this host would exhaust the current pool.
- Gate-ON hosts: after adopt, the server rides the `mcphub-hub` aggregate
  automatically on the next reconcile (it is now a manifest server).

**P1 — anti-drift signal + catalog rows (prevention completeness).**
- Standing signal: `unknown`-stdio count > 0 → tray/Dashboard badge + a warn line
  in `mcphub status`. This is what makes prevention STRUCTURAL: the next direct
  entry an operator (or a tool installer) adds gets nagged into adoption instead of
  silently leaking for months. Optional `adoptPolicy: auto` later; nag-first is the
  safe default.
- Add S1 catalog rows for mui-mcp, next-devtools, chrome-devtools-mcp, flowbite,
  raindrop, repomix (playwright exists) so fresh setups start hub-routed.

**P2 — orphan reaper (fallback net; `$security-reviewer` MANDATORY — kill
authority).** v1 = manual `mcphub cleanup orphans` (report-only default, `--yes` to
kill); v2 = opt-in supervisor maintenance timer (existing `maintenance_timers`
seam). Candidate set = census rows matching signature tiers: T1 = command
signatures of mcphub-managed stdio-bridge manifests; T2 = signatures harvested from
the client-config scan (all stdio entries across clients); T3 = generic heuristic
(`@scope/mcp`, `mcp-server`, …) — **T3 never kills, report-only**. Kill iff ALL:
1. exact T1/T2 signature match;
2. parent DEAD: PPID not alive OR parent CreationDate > child CreationDate
   (PID-recycle guard). Windows never re-parents, so dead-PPID is necessary — but
   NOT sufficient (see 4);
3. age > grace (~10 min);
4. **pipe-peer liveness** — the trap the design must solve: a live client can hold
   the stdio pipes of a grandchild whose npx intermediary died (dead PPID, still
   in-use). Discriminator: enumerate the candidate's stdin/stdout pipe objects and
   check whether any OTHER live process holds a handle to the same pipe object
   (NtQuerySystemInformation SystemHandleInformation, same-user accessible). Holder
   found → live-owned, skip. Enumeration unavailable/failed → UNVERIFIED →
   observe-only (same fail-closed posture as `squatterUnverified`,
   `supervise_squatter.go:31-37`);
5. exclusions: PIDs in supervisor-state (own daemons + `transient_pids`), members
   of any mcphub Job (`Job.HasMember`), `ExecutablePath == mcphub`, operator
   allowlist.
Kill mechanics: capture identity at scan → `TerminatePIDWithIdentity` proof, so a
PID recycled between scan and kill is structurally unkillable. Audit every verdict
to `supervisor-events.log` (`orphan-reaped` / `orphan-skipped-live` /
`orphan-unverified`; precedent: `daemon-port-squatter-*`).

### Why this combination and not the reaper alone

P0 removes the SOURCE for adopted servers: one supervised, Job-tree-protected
instance shared by every session — the proven serena/LSP/memory shape. P1 stops the
next wave (drift back to direct entries). P2 covers the residuals prevention cannot
reach: not-yet-adopted entries, third-party clients mcphub doesn't manage, and
`job_protection:false` hosts. A reaper alone is cleanup-as-a-service — it makes the
hand-cleanup periodic instead of unnecessary, which fails the raison d'être. With
P0+P1 the accumulation is structurally impossible for the managed set; P2 makes the
unmanaged remainder self-healing.

## Immediate remediation applied

- 249 dead-parent `@mui/mcp` node orphans killed (identity-checked: only node.exe
  with `@mui/mcp` in the command line AND a dead parent PID; live-parent instances
  and the 20 mcphub daemons left untouched). @mui/mcp 210 → 18.
- Operator note: until the hub-routing fix lands, each new codex/claude session
  keeps spawning + orphaning these; the hand-cleanup will need repeating. That is
  exactly why the structural fix (route through the hub) is required.
