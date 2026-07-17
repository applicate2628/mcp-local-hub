# Item 3 — Unit A plan: fail-closed hub-port dependency guard

Delivery plan by `$planner`. Scope = **Unit A ONLY** from `item3-restart-design.md` §7
("Fail-closed hub-port dependency guard"). Unit B (the self-restart handoff / CAS / listener-owner
machinery in §2–§5, §8–§9) is **out of scope** and untouched here. Unit A is design-confirmed,
independent of the handoff machinery, and ships as one standalone PR.

Branch baseline: `master @ 9f39c2a2`. Every existing-behavior claim below cites `file:line` verified
against the live tree this session.

---

## Problem being fixed (verified fail-open holes)

Two destructive hub-port reset callers fail **open** on unreadable client/group state:

- `--reset-port` exit-8 guard (`internal/cli/gui.go:242`) calls `api.GatedOnClients()`, which is
  `ProbeHubGate().GatedOn` and **discards** the `Unreadable` set (`hub_gate_detect.go:110-114,121-123`).
  A corrupt / DACL-blocked client config that is actually gate-ON is not counted → the reset orphans its
  `…/clients/<client>/mcp` URL. The same guard **never inspects groups at all**
  (`gui.go:242` keys only on `GatedOnClients`), so a host using only `/g/<group>/mcp` URLs is unguarded
  (`item3-restart-recon.md:61-63`).
- Initial hub startup passes `startHubMcpListenerOptions` with **no** `preservePortOnReloadHandlerFailure`
  (`server.go:1059-1063` → default `false`), so a reload-handler failure hits the unconditional reset at
  `hub_listener.go:657-663` (`ResetHubPortContext`, `Port=0`) with **no gate-ON guard**
  (`item3-restart-recon.md:64-67`). The auto-restart driver already opts out (`hub_listener.go:178`,
  `preservePortOnReloadHandlerFailure: true`); the two callers currently have opposite polarity.

`LoadGroups()` already distinguishes *missing* (empty, no error) from every other read/parse/DACL/validation
failure (`hub_mcp_groups.go:328-357`); a reset predicate that ignores that error class fails open.

---

## 1. Probe owner + signature

### Owner package — `internal/api` (fixed by the Change-Surface Contract §0, verified import-safe)

The design's Change-Surface Contract already fixes the owner: `New internal/api/hub_port_dependencies.go —
the one typed dependency probe` and names `ProbeHubPortDependencies` "the only reset-safety predicate"
(`item3-restart-design.md:46-48,68`). Import-cycle safety confirmed:

- Both consumers already import `internal/api`: `internal/cli/gui.go` calls `api.GatedOnClients()`,
  `api.ResetHubPort()`, `api.NewAPI()` (`gui.go:242,251,281`); `internal/gui/server.go` calls
  `api.LogHubMcpEvent`, `api.NewHubSessionStore`, etc. (`server.go`, `hub_listener.go`). Dependency
  direction is CLI → GUI → api (design §3, `item3-restart-design.md:155`).
- `internal/api` is the lowest layer — it imports `internal/clients` (`hub_gate_detect.go:32`) and does not
  import `internal/gui` or `internal/cli`. Placing the probe in `internal/api` adds **zero new imports**
  (its three inputs all live in-package or in already-imported `clients`) and **cannot** create a cycle.
- Neither consumer can own the probe: a probe in `internal/gui` cannot be imported by nothing-below, and
  `internal/cli` is not importable by `internal/gui`. `internal/api` is the only shared owner both can
  import. **Confirmed correct.**

### Typed signature (signatures only — no bodies)

```text
// internal/api/hub_port_dependencies.go
type HubPortDependencyState int   // or a small string-enum; one const set, single owner
const (
    DependencyStateClear     HubPortDependencyState = iota // no deps, no errors
    DependencyStateDependent                                // ≥1 proved dep, no errors
    DependencyStateUnknown                                  // ≥1 unreadable/parse/DACL/validation source
)

type HubPortDependencySource struct { // one unreadable source, for the operator message
    Kind string // "client" | "groups"
    Name string // client id, or "groups.yaml"
    Err  string // short reason (parse/DACL/validation)
}

type HubPortDependencies struct {
    GatedClients []string                  // proved gate-ON client ids
    Groups       []string                  // group names from a successfully-loaded groups.yaml
    State        HubPortDependencyState
    Errors       []HubPortDependencySource // unreadable client + groups sources
}

func ProbeHubPortDependencies() HubPortDependencies
```

### Derivation — which existing surface supplies each field

| Field | Source function | Notes |
| --- | --- | --- |
| `GatedClients` | `ProbeHubGate().GatedOn` (`hub_gate_detect.go:51-88`) | The probe **must call `ProbeHubGate()` directly**, NOT the `GatedOnClients()` wrapper — the wrapper throws away the `Unreadable` set that fail-closed needs (`hub_gate_detect.go:110-114,121-123`). |
| client `Errors` | `ProbeHubGate().Unreadable` (`hub_gate_detect.go:43-45,71`) | Each name → `HubPortDependencySource{Kind:"client", Name:name, Err:"config unreadable (parse/DACL)"}`. `ProbeHubGate` retains only the name, not the raw error — a generic reason is sufficient for the message and keeps `hub_gate_detect.go` **unmodified** (see §5). |
| `Groups` | `LoadGroups()` (`hub_mcp_groups.go:328-357`) on success | `Groups = [name for g in cfg.Groups]`. Missing file ⇒ empty, no error (`loadGroupsLocked` NotExist branch, `hub_mcp_groups.go:351-353`). |
| groups `Errors` | `LoadGroups()` non-nil `err` | version/parse/decode/validation/DACL error (`hub_mcp_groups.go:304,313,317,323,354`) → one `HubPortDependencySource{Kind:"groups", Name:"groups.yaml", Err:err.Error()}`. |

State derivation (deterministic precedence **Unknown > Dependent > Clear**):

```text
if len(Errors)  > 0                         -> Unknown   // safety cannot be proven
else if len(GatedClients)+len(Groups) > 0   -> Dependent
else                                        -> Clear
```

`State` is the coarse proceed/refuse gate; the operator message is composed from the **data fields**
(`GatedClients`, `Groups`, `Errors`) independently, so a mixed "dependent client + unreadable groups" host
both refuses *and* names everything (see §4). The probe acquires **no lock** — it is a pure read, matching
`ProbeHubGate` and `LoadGroups`, which neither hold `hub-mcp.lock` (`hub_mcp_groups.go:334-337`); each caller
owns its own locking context.

---

## 2. Commit phases

Three phases. **A1 lands first** (probe + its own unit tests); **A2 and A3 adopt it** and are
mutually independent (disjoint files `internal/cli/gui.go` vs `internal/gui/server.go`, both depend only on
A1's fixed contract) — commit in either order or in parallel after A1. Each phase builds and its tests pass
alone.

Per-phase gate command (repo canonical, CLAUDE.md "Step 1"):
`go build ./... && go vet ./... && go test -count=1 -timeout 5m ./...`
plus, for phases touching `internal/api` / `internal/cli`:
`go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/`.

### Phase A1 — the typed probe (lands first)

- **File / seam:** NEW `internal/api/hub_port_dependencies.go` (`ProbeHubPortDependencies` + the three types
  above). Consumes `ProbeHubGate()` and `LoadGroups()`; adds no imports beyond what `hub_gate_detect.go`
  and `hub_mcp_groups.go` already pull in.
- **Invariant established:** a single typed predicate that returns `Clear` **iff** every applicable client is
  proved gate-OFF *and* `groups.yaml` is missing-or-valid-empty; `Dependent` on any proved gate-ON client or
  group; `Unknown` on any client/groups read/parse/DACL/validation error. No caller yet — dead-code-safe,
  build-clean.
- **Acceptance tests** (NEW `internal/api/hub_port_dependencies_test.go`, package `api`, reusing
  `hermeticHome(t)` from `hub_gate_detect_test.go:17` and `api.SetDaemonStateRootForTest` for the state dir):
  - **AC1** clear: hermetic home, no gated client, no `groups.yaml` ⇒ `State==Clear`, all slices empty.
  - **AC2** dependent-by-client: seed one client config with a live `mcphub-hub` entry
    (`hub_gate_detect_test.go:57-61` pattern) ⇒ `GatedClients` contains it, `State==Dependent`.
  - **AC3** dependent-by-group: write a valid `groups.yaml` with ≥1 group under the test state root ⇒
    `Groups` contains the name, `State==Dependent`.
  - **AC4** unknown-by-client-error: seed a **malformed** client config so `GetEntry` returns a non-NotExist
    error routed to `Unreadable` (`hub_gate_detect.go:69-72`) ⇒ `Errors` has a `Kind:"client"` entry,
    `State==Unknown`. (Implementer verifies the chosen adapter — claude-code `~/.claude.json` — surfaces a
    parse error rather than treating malformed content as empty; if it swallows, pick an adapter that
    strict-parses.)
  - **AC5** unknown-by-group-error: write a `groups.yaml` with `version: 2` (or an unknown field) so
    `LoadGroups` errors (`hub_mcp_groups.go:304,313`) ⇒ `Errors` has the `Kind:"groups"` entry,
    `State==Unknown`.
  - **AC6** precedence + message-completeness: dependent client **and** a groups error together ⇒
    `State==Unknown` **and** `GatedClients` still non-empty (pins Unknown>Dependent and that data survives
    for the message).
- **Gate:** builds; the six probe tests pass under both the plain sweep and `-tags=test_state_path_env`.

### Phase A2 — `--reset-port` fails closed (adopts the probe)

- **File / seam:** `internal/cli/gui.go:242-250` (the B2 exit-8 branch, inside the already-held
  single-instance flock at `gui.go:216-226`). Replace `if gated := api.GatedOnClients(); len(gated) > 0`
  with `res := api.ProbeHubPortDependencies(); if res.State != api.DependencyStateClear { … exit 8 }`. Write
  the predicate as `!= Clear` (fail-closed-by-construction: any future non-Clear state also refuses).
- **Invariant established:** `mcphub gui --reset-port` refuses (exit 8) on `Dependent` **and** `Unknown`,
  naming proved dependencies (clients + groups) and separately listing unreadable sources (§4). No lock
  change — the probe runs under the existing at-rest read (flock already proved no GUI running,
  `gui.go:216-241`).
- **Acceptance tests** (extend `internal/cli/gui_resetport_test.go`, run with `-tags=test_state_path_env`;
  seed state via `api.SetDaemonStateRootForTest` and hermetic home via `resetPortHermeticHome(t)`
  `gui_resetport_test.go:24`):
  - **AC7** clear happy path still proceeds: existing `TestGuiResetPortClearsPortKeepsInstanceID`
    (`gui_resetport_test.go:44`) still passes unchanged (no gated client, no groups ⇒ Clear ⇒ reset runs,
    exit 0). Regression anchor.
  - **AC8** dependent-by-group refuses: seed `groups.yaml` with ≥1 group, no gated client ⇒ exit 8, message
    names the group(s). (This is the previously-unguarded `/g/` hole.)
  - **AC9** unknown-by-client refuses: seed a malformed client config ⇒ exit 8, message lists the unreadable
    client. (Previously a fail-open reset.)
  - **AC10** unknown-by-group refuses: seed a corrupt/`version:2` `groups.yaml` ⇒ exit 8, message lists
    `groups.yaml` as unreadable.
  - **AC11** dependent-by-client still refuses with the new message (existing gate-ON refusal preserved;
    message now data-driven per §4).
- **Gate:** builds; `internal/cli` reset-port tests pass with `-tags=test_state_path_env`.

### Phase A3 — initial hub-startup preserves-unless-clear (adopts the probe)

- **File / seam:** `internal/gui/server.go:1043-1063` — the `Server.Start` hub-startup composition. After
  `hubEnabled` is resolved (`server.go:1043-1049`) and **only when `hubEnabled` is true** (when false the
  listener never binds and never resets — `hub_listener.go:540-542`), compute
  `preserve := api.ProbeHubPortDependencies().State != api.DependencyStateClear` and set
  `preservePortOnReloadHandlerFailure: preserve` in the `startHubMcpListenerOptions{…}` literal at
  `server.go:1059-1063`. The reset **mechanism** at `hub_listener.go:657-663` is unchanged; the composition
  supplies the safe policy (design §7, `item3-restart-design.md:670-673`). This tightens the initial caller
  from unconditional `false` → `clear ? false : true`, matching the auto-restart driver's always-preserve
  posture (`hub_listener.go:178`) on non-clear hosts while keeping the historical reset-to-0 on a genuinely
  clear host.
- **Invariant established:** on a host with any gated client, any group, or any unreadable client/groups
  source, an initial reload-handler failure **preserves** the persisted hub port (never orphans gated
  `/clients/` or `/g/` URLs); only a proved-clear host still resets to 0.
- **Acceptance tests** (extend `internal/gui/hub_health_test.go`, package `gui`, using the established
  seam `s.hubEndpointGateFn` + `s.startHubMcpListenerFn` at `hub_health_test.go:264,267` /
  `server.go:624-625`; hermetic env + `api.SetDaemonStateRootForTest` so the probe reads sandbox
  clients/groups):
  - **AC12** dependent host ⇒ preserve true: inject `hubEndpointGateFn` true; seed a gated client (or a
    `groups.yaml` group); inject `startHubMcpListenerFn` that **captures** the received
    `opts.preservePortOnReloadHandlerFailure`; assert it is `true`.
  - **AC13** clear host ⇒ preserve false: same seam, no gated client / no groups ⇒ captured flag is `false`
    (byte-identical to today's behavior on a clean host).
  - **AC14** unknown host ⇒ preserve true: seed an unreadable client or corrupt `groups.yaml` ⇒ captured
    flag `true` (fail-closed).
  - The injected `startHubMcpListenerFn` short-circuits the real bind, so A3's test asserts only that the
    **composition threads the correct policy value**; it does not re-exercise the reset mechanism (that is
    covered by `hub_listener_restart_test.go` and stays unchanged — see §5).
- **Gate:** builds; `internal/gui` tests pass (`go test ./internal/gui/`).

---

## 3. Test plan — fail-open cases that must become fail-closed

| # | Fixture | Pre-Unit-A (fail-open) | Unit-A required outcome | Where pinned |
| --- | --- | --- | --- | --- |
| T1 | Unreadable client config that is gate-ON | not counted → reset orphans its URL | `Unknown` ⇒ `--reset-port` **exit 8** | AC4, AC9 |
| T2 | `groups.yaml` with ≥1 group, no gated client | unguarded → reset orphans `/g/` URLs | `Dependent` ⇒ **exit 8** | AC3, AC8 |
| T3 | `LoadGroups` parse/DACL/version error | ignored → reset proceeds | `Unknown` ⇒ **exit 8** | AC5, AC10 |
| T4 | Clear host (no gated client, no groups file) | reset proceeds (correct) | `Clear` ⇒ reset **proceeds, exit 0** (unchanged) | AC1, AC7, AC13 |
| T5 | Initial startup, dependent host, reload-handler fails | resets `Port=0`, orphans URLs | `preserve=true`, port retained | AC12 |
| T6 | Initial startup, clear host, reload-handler fails | resets `Port=0` (nothing to orphan) | `preserve=false` (unchanged) | AC13 |

Exact test files (all NEW additions to existing files except A1's new file):

- `internal/api/hub_port_dependencies_test.go` (NEW, package `api`) — AC1–AC6. Standard sweep + `-tags=test_state_path_env`.
- `internal/cli/gui_resetport_test.go` (extend) — AC7–AC11. **Requires `-tags=test_state_path_env`** (repo canonical CLI-test tag, CLAUDE.md Step 1).
- `internal/gui/hub_health_test.go` (extend) — AC12–AC14. `go test ./internal/gui/`.

State/config seeding helpers already in-tree: `hermeticHome(t)` (`hub_gate_detect_test.go:17`),
`resetPortHermeticHome(t)` (`gui_resetport_test.go:24`), `api.SetDaemonStateRootForTest`
(`gui_resetport_test.go:46`), `MCPHUB_GUI_TEST_PIDPORT_DIR` (`gui_resetport_test.go:62`). Post-test process
sweep per CLAUDE.md Step 2 (`Get-Process mcphub | Stop-Process`) if any test binds a real port (A2/A3 use
the injected non-binding seam, so none should).

---

## 4. Exit-8 message contract (`--reset-port`)

Current message names gated clients only (`gui.go:243-248`). Unit A makes it **data-driven** from the probe:
refuse iff `State != Clear`; compose the body from whichever data fields are populated, so a mixed host names
everything. Load-bearing content the test pins as substrings (not verbatim full text):

- **Header:** `--reset-port refused:` and terminal exit code **8** (`&forceExitError{code: 8}`, preserving
  `gui.go:249`).
- **Proved dependencies block** (present iff `len(GatedClients) > 0 || len(Groups) > 0`):
  - names each gate-ON client (`GatedClients`), stating their `/clients/<client>/mcp` URLs are pinned to the
    current hub port (preserve the existing gated-client wording, `gui.go:244-246`);
  - names each group (`Groups`), stating their `/g/<group>/mcp` URLs are pinned to the current hub port and
    that **no reconcile path rewrites group URLs** — the operator must re-copy them from the Groups screen
    after any port change (design §7 non-goal, and CLAUDE.md "Groups `/g/` routes share the hub port (C7)").
- **Unreadable-sources block** (present iff `len(Errors) > 0`): lists each source as `Kind Name (Err)` — e.g.
  `client vscode (config unreadable)`, `groups.yaml (decode error)` — and states safety **cannot be proven**
  for those sources; instruct the operator to **fix the unreadable file** (repair its DACL/parse error), then
  retry.
- **Remediation footer** (always): gate OFF first (`mcphub settings set gui_server.hub_endpoint_enabled
  false` then `mcphub install --reconcile-hub-mode`, preserving `gui.go:246`), and note `mcphub gui --force
  --kill` is a separate recovery not blocked by this guard (preserve `gui.go:247`).

Test assertions (AC8–AC11): dependent-only body contains the dep names and no "unreadable" block; unknown-only
body contains the unreadable-source name(s) and the "cannot be proven" phrasing; mixed body contains both.
Exit code 8 in all refusal cases; exit 0 only on Clear (AC7).

---

## 5. Blast-radius / regression guards

- **The `Clear` (currently-working) reset path is byte-unchanged.** On a host with no gated client, no
  `groups.yaml`, and no unreadable source, the probe returns `Clear` and both callers behave exactly as
  today (`--reset-port` runs `api.ResetHubPort()`; initial startup keeps `preserve=false`). Protected by the
  unchanged happy-path test AC7 and the clear-host AC13, and by the derivation: Unit A only **adds** refusals
  for the previously fail-open group/unreadable cases; it removes no previously-legal clear reset.
- **No handoff / CAS / listener-owner machinery is touched.** Unit A adds one probe file and edits two call
  sites only. It imports and references **none** of §2–§5/§8–§9 (`HandoffRecordStore`, `GUIListenerOwner`,
  `SpawnedGUIChild`, `gui-restart.json`, reservation/CAS) — those are Unit B and do not exist yet. Enforced
  by file scope: `internal/api/hub_port_dependencies.go` (new), `internal/cli/gui.go` (reset-port branch
  only), `internal/gui/server.go` (options literal only).
- **`hub_listener.go:657-663` reset mechanism is unchanged.** Unit A sets only the composition caller's
  `preservePortOnReloadHandlerFailure` **input** at `server.go:1059-1063`; it does not edit the reset branch,
  the `ResetHubPortContext` call, or the struct field (`hub_listener.go:531`, which already exists). The
  auto-restart driver's `preservePortOnReloadHandlerFailure: true` (`hub_listener.go:178`) is untouched.
  `hub_listener.go` is **read-only** for Unit A; its behavior is covered by the unchanged
  `hub_listener_restart_test.go`.
- **`hub_gate_detect.go` and `hub_mcp_groups.go` are unchanged** (protected surfaces, `item3-restart-design.md:74-75`).
  The probe consumes `ProbeHubGate()` and `LoadGroups()` as-is; it does **not** expand `ProbeHubGate` to carry
  per-client error strings (the message uses names + a generic reason). This keeps the Change-Surface Contract
  intact — no protected-surface edit.
- **`GatedOnClients()` / `AnyClientGatedOn()` stay** (`hub_gate_detect.go:121-131`) — other callers may use
  them; Unit A only stops `--reset-port` from relying on the wrapper's `Unreadable`-discarding behavior. No
  removal, no signature change.

Rollback: each phase is a clean single-concern commit. A1 is additive (revert = delete one file + its test).
A2 reverts to the `GatedOnClients()` line. A3 reverts to the three-field options literal. Local-only until PR;
prefer `git reset --hard` over `revert` if a phase is pulled before push (CLAUDE.md Bootstrap step 5).

---

## 6. Non-goals (explicit)

- **No automatic group-URL reconcile path.** Design §7 (`item3-restart-design.md:675-677`) records that there
  is no authoritative inventory of where operators pasted `/g/<group>/mcp` URLs, so none is built. Unit A only
  *detects* group dependence and *blocks*; it does not rewrite group URLs. Already-noted follow-up — do **not**
  plan or implement it here. The `--reset-port` message tells the operator to re-copy group URLs manually
  after a port change (§4).
- **No Unit B work** (self-restart handoff, CAS record, listener owner, reservation, frontend restart
  progress). Separate PR.
- **No change to the reset mechanism, the `mcphub-hub` aggregate detection semantics, or the groups schema.**

---

## Gate decision: **PASS**

Every phase is small, independently buildable/reviewable/revertable, with explicit file:line seam, invariant,
AC-ids, and gate command. Parallel eligibility (A2 ∥ A3) is bounded by A1's fixed probe contract and disjoint
write boundaries. No implementation code is included. Ready to implement — recommended next role:
`$backend-engineer` for A1, then `$backend-engineer`/`$frontend-engineer`-N-A (all Go) for A2 + A3, then
`$qa-engineer` to verify AC1–AC14.
