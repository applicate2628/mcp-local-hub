# Managed LSP-router proof for destructive registration cleanup

## Decision

Registration may treat a pre-existing shared Language Server Protocol (LSP)
router entry as a destructive-cleanup replacement only after one bounded probe
of the existing Graphical User Interface (GUI) `/api/ping` endpoint proves the
listener is live and returns the expected managed identity. The probe result is
an immutable, cleanup-wide snapshot. Per-client and per-language checks continue
to validate the configured router entry through the existing alias
authorization owner.

The existing ordinary ping producer returns the exact JSON object
`{"ok":true,"pid":<int>,"version":<string>}` and sets
`Content-Type: application/json` at `internal/gui/ping.go:17-29`. Its producer
test validates all three fields at `internal/gui/ping_test.go:13-31`, and the
byte-compatibility test pins their serialized names and order at
`internal/gui/ping_test.go:34-42`. The incumbent handshake already treats the
ping process identifier as identity evidence by comparing it with a separately
known process identifier at `internal/gui/handshake.go:83-125`.

The cleanup therefore consumes the existing wire contract. It does not add an
endpoint or invent a second identity format.

## Change-Surface Contract

| Field | Contract |
|---|---|
| Intended change surface | `internal/api/register.go`: expected GUI identity input, one cleanup-wide HTTP probe, immutable proof snapshot, single warning, and propagation into `lspCleanupAliasesForClient`; `internal/gui/projects_toggle.go`: supply the running server's port, process identifier, and version; `internal/api/register_test.go` and the directly corresponding GUI ping/probe tests. |
| Approved extension seams | Extend the internal Go-only `RegisterOpts` input with a typed expected managed-GUI identity; consume the already-stable `/api/ping` wire; extend the existing `lspCleanupAliasesForClient` replacement gate with one immutable `managedRouterLive` snapshot. |
| Protected / must-not-touch surfaces | Local commit `c826a48d` closures for preflight isolation, one binding snapshot, relay warnings, and CLI help; router entry writers and persisted client schemas; GUI route behavior and ordinary ping bytes; scheduler, supervisor, tray, GUI launch, production state, CLI help, legacy migration semantics outside the cleanup result; workspace-free language-server cleanup and unregister. |
| Declared blast radius | One internal registration option, one GUI caller, the registration cleanup authorization path, and focused tests. No public HTTP, command-line, configuration, or persisted-state contract changes. |
| Single writer-owner | `cleanupDirectLanguageServerEntriesAfterRegister` owns the one proof snapshot for the cleanup invocation; no client/language loop may recompute it. |
| Downstream-observable settled signal | The returned `RegisterReport.Warnings` contains exactly one router-proof warning on an unavailable or rejected proof; the GUI returns these warnings at `internal/gui/projects_toggle.go:182-185`, and the CLI prints report warnings at `internal/cli/register.go:184-194`. |

## Chosen components and dependency direction

### 1. Expected identity input

Add a typed, internal Go-only expected identity to `RegisterOpts`:

```text
ManagedGUIIdentity {
    Port
    PID
    Version
}
```

The GUI workspace-toggle caller supplies this tuple from the same `Server`
instance that owns the request. It already supplies `s.Port()` at
`internal/gui/projects_toggle.go:169-177`; `Server` retains its normalized
configuration at `internal/gui/server.go:611-618`, and `NewServer` replaces a
zero process identifier before the server is exposed at
`internal/gui/server.go:883-897`. Production GUI composition supplies a
non-empty version at `internal/cli/gui.go:879-884`; `versionString` falls back to
`"dev"` rather than an empty string at `internal/cli/gui.go:1963-1969`.

The tuple is caller evidence, not ambient configuration. A caller that does not
own a running GUI, including the CLI register path at
`internal/cli/register.go:113-120` and legacy migration at
`internal/api/legacy_migrate.go:162-172`, leaves it absent. Absence cannot
authorize pre-existing-router cleanup.

`GUIPort` remains the port-provenance input used by existing route validation.
The expected identity's port must equal the once-resolved router port; a
mismatch is a rejected proof, not a fallback to either value.

### 2. One bounded managed-listener probe

`cleanupDirectLanguageServerEntriesAfterRegister` remains the owner. It already
resolves the router port once before iterating clients at
`internal/api/register.go:552-603`. Immediately after that resolution, it
creates one immutable snapshot:

```text
managedRouterSnapshot {
    port
    liveManaged
    failureClass
}
```

The production probe has this exact contract:

1. Fail closed without a request when the resolved port is invalid, the
   expected identity is absent or invalid, or its port differs from the
   resolved port.
2. Issue exactly one `GET http://127.0.0.1:<port>/api/ping`.
3. Use one 500 ms total timeout covering connect, headers, and body. Bind the
   request to a 500 ms context and give the HTTP client the same ceiling.
4. Accept only HTTP 200, `application/json`, and a bounded body of at most
   4 KiB containing one JSON object with no non-whitespace trailing payload.
5. Require `ok == true`, `pid > 0`, non-empty `version`, and exact equality of
   `pid` and `version` with the caller's expected managed identity.
6. Close the response body on every response path. The snapshot owns no handle
   after the probe returns.
7. Do not retry.

The 500 ms ceiling follows the existing GUI identity-probe policy: the shared
ping helper documents a 500 ms-or-shorter timeout at
`internal/gui/probe.go:74-105`, and the incumbent handshake constructs a
500 ms HTTP client at `internal/gui/handshake.go:58-60`. No retry is correct
here because this is not a startup-wait path: a failed proof merely suppresses
destructive cleanup while registration's successfully written replacements
remain valid. A retry would add latency without strengthening the identity
contract.

### 3. Immutable propagation into the existing alias owner

The one `liveManaged` Boolean is passed into
`lspCleanupAliasesForClient`. That function remains the single owner of the
per-client/per-language cleanup authorization invariant at
`internal/api/register.go:672-727`.

For each registered language:

- `bound == true`: add aliases without consulting `liveManaged`. The same
  registration wrote that client's replacement from the one binding snapshot
  resolved at `internal/api/register.go:486-512`.
- `bound == false && liveManaged == false`: do not inspect a configured router
  entry for authorization and do not add aliases.
- `bound == false && liveManaged == true`: call the existing
  `clientHasActiveLSPRouterReplacement` gate. It continues to enforce entry
  presence, enabled state, loopback route grammar, language, port equality, and
  hub ownership at `internal/api/register.go:241-258`.

This retains per-language isolation: a managed live router plus a valid `go`
entry cannot authorize `python` cleanup unless that client also has a valid
`python` router entry. Both direct `mcp-language-server` candidates and direct
`gopls` Model Context Protocol (MCP) candidates continue to converge on the
same aliases before matching and removal at `internal/api/register.go:621-660`.

### 4. Warning and report flow

Any stopped listener, timeout, request failure, non-200 status, wrong media
type, oversized or malformed JSON, `ok:false`, invalid identity fields,
identity mismatch, missing expected identity, or port mismatch produces:

- `liveManaged = false`;
- no backup and no removal attributable only to a pre-existing router entry;
- one cleanup-wide warning appended once to `RegisterReport.Warnings`;
- the same warning written once to the registration progress writer.

The warning shape is:

```text
LSP router managed-identity proof unavailable (<failure class>); keeping direct
LSP entries whose only replacement is a pre-existing shared-router entry
```

The parenthetical uses one bounded class, not raw response bodies or
machine-specific paths: `identity-not-supplied`, `port-unresolved`,
`port-mismatch`, `stopped-or-timeout`, `http-status`, `content-type`,
`malformed-response`, or `identity-mismatch`. Underlying network errors may be
wrapped for local diagnostics but must not duplicate the report warning per
client or language.

Same-registration bound cleanup does not emit this warning merely because no
pre-existing-router proof is needed. The implementation may defer the probe
until it sees at least one installed unbound client participating in cleanup,
but once needed it evaluates at most once for the whole invocation.

## Alternatives

| Alternative | Decision | Tradeoff and decisive evidence |
|---|---|---|
| Bare TCP connect or existing port equality | Rejected | Port equality currently checks only configured URL shape at `internal/api/register.go:241-258`; it does not observe a listener. A TCP connect distinguishes stopped from listening but cannot distinguish the managed GUI from a foreign listener. The existing ping producer provides process and version identity fields at `internal/gui/ping.go:17-29`. |
| Per-client/per-language MCP `initialize` against `/lsp/<language>/mcp` | Rejected | It would move a network request into the nested authorization grain, violating the required one-snapshot cleanup decision and multiplying timeouts by clients and languages. The current shared-router route is language-specific while listener identity is process-wide; entry language and ownership are already checked by `clientHasActiveLSPRouterReplacement` at `internal/api/register.go:241-258`. The existing MCP readiness implementation also carries a 2 second client and retry loop at `internal/api/register.go:1603-1658`, which is unsuitable for best-effort destructive-cleanup authorization. |

## Contracts and compatibility

- **HTTP contract:** unchanged. `/api/ping` remains GET-only and emits the same
  bytes pinned at `internal/gui/ping_test.go:34-42`.
- **Internal Go contract:** additive `RegisterOpts` identity input. Zero/absent
  is the safe path and cannot authorize destructive router-only cleanup.
- **Configuration and persisted state:** no contract/persisted-state change.
  No migration, compatibility window, or rollback of stored data is required.
- **CLI contract:** unchanged. CLI registration can still clean replacements it
  wrote in the same invocation; it cannot claim a pre-existing live GUI identity
  it does not own.
- **GUI behavior:** no new route and no launch. The existing project toggle
  supplies identity to the API and returns the existing warnings field.
- **Security boundary:** the probe is loopback-only, accepts no redirects away
  from `127.0.0.1`, sends no credential, and never incorporates response data
  into a request or file path.

## Failure modes and observable discriminators

| Failure mode | Cleanup behavior | Observable discriminator |
|---|---|---|
| No listener / connection refused | Preserve router-only direct entries; bound replacements remain eligible | One warning with `stopped-or-timeout`; stopped-listener regression guard observes zero backup/removal |
| Deadline expires | Same as no listener | One warning with `stopped-or-timeout`; test uses a handler that blocks beyond 500 ms and asserts bounded return |
| Foreign listener returns non-200 | Preserve | One warning with `http-status`; controlled handler records one request |
| Foreign listener returns HTML, oversized, malformed, or trailing payload | Preserve | One warning with `content-type` or `malformed-response`; controlled handler records one request |
| Foreign listener returns ping-shaped but mismatched process identifier/version | Preserve | One warning with `identity-mismatch`; zero backup/removal |
| Producer returns `ok:false`, zero process identifier, or empty version | Preserve | One warning with `identity-mismatch`; zero backup/removal |
| Caller identity absent or its port differs | Preserve router-only entries without a network request | One warning with `identity-not-supplied` or `port-mismatch`; request-count control remains zero |
| Router port cannot be resolved | Preserve | Existing resolution warning is consolidated into the single proof warning class `port-unresolved` |
| Managed identity succeeds but entry is disabled, malformed, non-loopback, wrong language, or wrong port | Preserve that language | No proof warning; existing entry gate returns false at `internal/api/register.go:241-258` |
| Managed identity succeeds and matching entry is enabled and owned | Authorize only that client/language's aliases | Positive controlled test observes one request and removal of only the matching language |
| Per-client `GetEntry` fails after a managed proof | Preserve that client/language | Existing per-entry warning at `internal/api/register.go:713-721`; the listener is not re-probed |
| Backup fails | Preserve every match for that client | Existing backup warning and early continue at `internal/api/register.go:648-652` |
| Individual removal fails | Preserve failed entry and continue other matches | Existing removal warning at `internal/api/register.go:654-659` |

## Defect-class participant disposition

| Participant | Axes swept | Design disposition |
|---|---|---|
| Replacement origin | Same-registration binding vs pre-existing router entry | Bound origin bypasses network proof; pre-existing origin requires the one managed snapshot plus per-entry validation. |
| Listener state | Stopped, timeout, managed, foreign | Stopped/timeout/foreign fail closed with one warning; only managed identity sets `liveManaged`. |
| Router-entry validity | Missing, disabled, malformed, non-loopback, wrong language, stale port, matching owned | Existing per-language gate remains authoritative after the process-wide proof. |
| Direct-entry kind | `mcp-language-server`, direct `gopls` MCP | Both continue through one alias map and one removal path at `internal/api/register.go:621-660`. |
| Client grain | Bound default, opt-in with router evidence, absent client | Bound uses same-registration proof; opt-in requires live managed router plus its own entry; nil/nonexistent client is skipped at `internal/api/register.go:603-606`. |
| Language grain | One or multiple registered languages | Proof is process-wide once; route authorization remains per language. |
| Caller provenance | GUI, CLI, legacy migration | GUI supplies expected live identity; CLI and migration do not invent one and therefore preserve router-only direct entries. |
| Warning surfaces | Progress writer, report, GUI response, CLI stderr | One cleanup-wide warning value; existing report propagation remains the owner. |
| Return/skip paths | Port resolution, identity probe, client existence, alias empty, scans, backup, removal | Every proof failure exits only the router-origin authorization branch; no path converts failure into cleanup permission. |

## Object-axis record

| Object | Axes | Required result |
|---|---|---|
| Managed-router snapshot | Port provenance × expected identity × listener state × wire validity × timeout | Exactly one immutable verdict per cleanup; invalid or unavailable evidence is false. |
| Alias authorization | Binding origin × snapshot × client × language × router-entry shape | Same-registration binding or (`liveManaged` and matching entry), never either at whole-client grain. |
| Destructive cleanup | Alias set × direct-entry kind × workspace survivor × backup result × remove result | No backup/remove when aliases are empty; existing workspace and backup safety gates remain. |
| Diagnostics | Failure class × client count × language count × caller surface | One router-proof warning per registration, not one per client/language. |

## Diff-invisible invariants and named regression guards

| Invariant | Named regression guard and expected result |
|---|---|
| A stopped listener never authorizes cleanup. | `TestRegister_CleanupKeepsDirectEntryWhenConfiguredRouterIsStopped`: matching configured entry, expected identity, no listener; one bounded probe; direct entry remains; backup/remove counts are zero; one `stopped-or-timeout` warning. |
| A foreign HTTP listener never authorizes cleanup. | `TestRegister_CleanupKeepsDirectEntryWhenRouterPortHasForeignListener`: controlled listener returns non-managed or mismatched ping identity; request count is one; direct entry remains; backup/remove counts are zero; one classified warning. |
| A managed listener authorizes only current per-language candidates. | `TestRegister_CleanupRemovesDirectEntryWithProvenManagedRouter`: controlled ping returns the exact expected tuple; valid `go` router entry removes direct `go`; sibling `python` without a replacement remains; request count is one. |
| Same-registration bound clients remain cleanup-eligible without GUI proof. | Retain and strengthen the bound-client half of the existing cleanup test around `internal/api/register_test.go:3970-3997`: absent identity/probe still removes the bound client's replaced entry while preserving an unbound router-only entry. |
| Stale-port, disabled, malformed, wrong-language, and non-loopback entries remain ineligible. | Convert the current stale-port guard at `internal/api/register_test.go:4000-4087` to provide a positive managed snapshot, then retain both directions; add/retain table cases for the other invalid shapes, all with no deletion. |
| One registration uses one binding snapshot. | Retain `TestRegister_ClientScopeResolvedOnceForTheWholeRegistration`; production has one resolution at `internal/api/register.go:486-512`. |
| Both direct-entry kinds share one alias owner. | One table-driven positive/negative test runs equivalent `mcp-language-server` and `gopls` candidates through the same live snapshot and observes identical authorization outcomes. |
| Probe failure is visible once, not multiplied by loop cardinality. | Multi-client, multi-language stopped/foreign test asserts one HTTP request and exactly one router-proof warning in `RegisterReport.Warnings`. |
| Probe resources and latency are bounded. | Blocking-handler test asserts return within the 500 ms ceiling plus scheduler tolerance and observes handler/request cancellation; code review verifies every response body closes. |
| Ordinary ping wire is unchanged. | Retain `TestPing_OrdinaryWireShapeRemainsByteCompatible` at `internal/gui/ping_test.go:34-42`. |

## Test strategy

1. Add pure probe tests for status, media type, body bound, JSON shape,
   `ok`, process identifier, version, timeout, and redirect refusal.
2. Add the three controlled registration cases named above.
3. Mutate the implementation so port equality alone sets `liveManaged=true`;
   the stopped and foreign tests must fail by observing removal/backup.
4. Mutate the implementation to re-probe inside the client/language loop; the
   request-count and one-warning guard must fail.
5. Mutate the bound branch to require `liveManaged`; the bound-client guard
   must fail.
6. Run the package-scoped API and GUI tests under the user's required state
   isolation. Any run including `./internal/api` uses
   `-tags=test_state_path_env` and a fresh `MCPHUB_STATE_DIR_OVERRIDE`.
7. The downstream verification stage runs `go build ./...` and `go vet ./...`
   without launching GUI, tray, or supervisor.

## Claims

1. `{ guarantee: A pre-existing router entry cannot authorize direct-entry deletion unless one bounded cleanup-wide probe validates the existing managed ping identity; single-owner: cleanupDirectLanguageServerEntriesAfterRegister managed-router snapshot; enforcement-probe: stopped, foreign, malformed, timeout, and managed controlled tests plus request-count == 1 }`.
2. `{ guarantee: Same-registration bound clients remain cleanup-eligible without a GUI proof; single-owner: lspCleanupAliasesForClient bound branch; enforcement-probe: bound-client regression removes the bound direct entry with identity absent }`.
3. `{ guarantee: Router-origin authorization remains per client and language; single-owner: lspCleanupAliasesForClient; enforcement-probe: managed go replacement removes go while sibling python without replacement remains }`.
4. `{ guarantee: Both direct mcp-language-server and direct gopls candidates consume the same alias authorization; single-owner: aliases returned by lspCleanupAliasesForClient; enforcement-probe: two-kind table test maps identical inputs to identical cleanup eligibility }`.
5. `{ guarantee: Probe failure and timeout are fail-closed and operator-visible once; single-owner: cleanupDirectLanguageServerEntriesAfterRegister warning aggregation; enforcement-probe: multi-client multi-language failure yields zero backup/removal and one RegisterReport warning }`.
6. `{ guarantee: One registration retains one effective client-binding snapshot; single-owner: effectiveClientBindings invocation in registerWithManifest; enforcement-probe: TestRegister_ClientScopeResolvedOnceForTheWholeRegistration and production call-count search }`.
7. `{ guarantee: No public HTTP, CLI, configuration, or persisted-state schema changes; single-owner: existing /api/ping wire and RegisterReport contract; enforcement-probe: ping byte-compatibility test, CLI help diff check, and persisted-schema diff check }`.
8. `{ guarantee: Network lifetime is bounded to one 500 ms no-retry request and every response body is closed; single-owner: managed GUI ping probe; enforcement-probe: blocking-handler cancellation test, request-count assertion, and resource-path code review }`.

These are local, single-work-item decisions. No cross-cutting
`work-items/decisions/` record is required.

## Receiving-side echo

Before implementation, the backend owner must echo these five points in its
plan:

1. expected identity source and why CLI/legacy absence is safe;
2. exact accepted ping wire and 500 ms/no-retry/body-close contract;
3. the single cleanup snapshot and single warning owner;
4. the bound bypass and per-language unbound formula;
5. the stopped/foreign/managed mutation tests and both direct-entry kinds.

Any need to change the ping bytes, introduce a persisted nonce/schema, move the
probe into the client/language loop, or require GUI proof for same-registration
bound clients is `REVISE` to architecture.

PASS

## Revision 2 design

### Decision

Replace the current Boolean candidate pass with one typed, fail-closed cleanup
preflight that completes every router-entry read and direct candidate/survivor
scan before router-port resolution, network proof, backup, or removal. The
preflight caches the resulting removal plans. Port resolution and the single
managed-listener proof then only select which already-planned router-origin
matches may execute.

This is a local orchestration correction. It does not create a second liveness
owner, route-shape owner, alias owner, or direct-entry matcher.

### Change-Surface Contract

`{ intended change surface: internal/api/register.go cleanup preflight and its
internal typed results, plus focused guards in internal/api/register_test.go;
approved extension seams: cleanupDirectLanguageServerEntriesAfterRegister as
the sole orchestration/removal owner, the configured-entry inspection currently
owned by clientHasActiveLSPRouterReplacement, lspCleanupAliasesForClient as the
sole per-client/per-language alias composer, and
directLanguageServerCleanupMatches as the shared direct-entry matcher;
protected / must-not-touch surfaces: public HTTP and ping bytes, CLI and
configuration contracts, persisted state/schema, GUI identity producer,
supervisor/scheduler/child processes, other registration cleanup owners, and
the bound-client proof bypass; declared blast radius: two existing internal
files, with no public contract or persisted-state change }`

Exactly one writer-owner remains:
`cleanupDirectLanguageServerEntriesAfterRegister`. The downstream-observable
settled result remains the returned `RegisterReport.Warnings` contribution plus
the existing progress writer output; no new event or state channel is added.

### Typed internal results

1. `routerReplacementCandidate` has `kind` (`not-candidate`,
   `structural-candidate`, or `indeterminate`), `entryPort`, and
   `diagnostic`. `structural-candidate` means the configured router entry is
   present, enabled, owned, loopback, parseable, for the requested language and
   `/lsp/<language>/mcp` route, and has a positive observed port. It deliberately
   does not mean that the observed port equals the resolved GUI port.
2. `lspCleanupAliasPlan` has `boundAliases`, `routerAliasesByPort`,
   `diagnostics`, and `complete`. `routerAliasesByPort` groups aliases by the
   structurally valid entry port. An entry-read error sets `complete=false`;
   missing, disabled, malformed, non-loopback, wrong-language, or otherwise
   non-owned entries are ordinary absence and add neither an alias nor a
   diagnostic.
3. `directCleanupMatchResult` has `matches`, `diagnostics`, and `complete`.
   Candidate, survivor, stdio, or gopls scan errors set `complete=false` and
   make `matches` unusable even if another sub-scan found a partial match.
4. `directCleanupClientPlan` has `clientName`, `client`, cached
   `boundMatches`, and cached `routerMatchesByPort`.
5. `directCleanupPreflight` has ordered `clients`, `diagnostics`, and
   `complete`. It exposes two pure queries: whether any non-empty
   router-origin match group exists, and the cached router-origin matches for
   one resolved port.

Completeness is an explicit field; orchestration must not infer safety from an
empty warning slice or a non-empty partial match slice.

### Existing-helper split and reuse

- Split the internals of `clientHasActiveLSPRouterReplacement` into one
  `inspectClientLSPRouterReplacement` operation and one pure exact-port
  predicate. The inspection performs the existing `GetEntry`, enabled/shape,
  loopback, language/path, ownership, and observed-port checks once.
  `clientHasActiveLSPRouterReplacement`, if retained for other internal callers,
  becomes a thin composition of that inspection and
  `candidate.entryPort == resolvedPort`; it contains no route-shape logic.
- Refactor `lspCleanupAliasesForClient` to produce
  `lspCleanupAliasPlan` from the structural inspection. Its existing
  language-spec and alias-composition loop remains the only alias owner.
  Selecting `boundAliases` or `routerAliasesByPort[resolvedPort]` is a pure
  consumer operation, not a second alias composer.
- Reuse `directLanguageServerCleanupMatches` as the only direct-entry matching
  owner. Give it the typed `directCleanupMatchResult` return so scan errors
  cannot be mistaken for “no match.” During preflight it is invoked once for
  the bound alias group and once for each distinct structural router-port alias
  group for that client. Those final match lists are cached. A client with one
  router port is scanned once for its router-origin plan; a client with several
  distinct structural ports may be scanned once per distinct alias group, but
  there is no scan after port resolution or after proof. No matching predicate
  is copied into the preflight.

The last point is deliberate: grouping through the existing matcher is a
smaller and safer change than teaching a second layer how a direct entry maps
to aliases. If repeated reads for multiple distinct structural ports later
become material, the matcher may gain a separate read-only inventory input, but
that optimization is outside Revision 2.

### Exact data flow and ordering

1. Create one warning accumulator and one preflight. Iterate the existing
   clients. Nil or non-existent clients are ignored exactly as today.
2. For each remaining client, obtain one `lspCleanupAliasPlan`.
   - A bound client contributes its unchanged `boundAliases`; it never creates
     proof need.
   - An unbound client contributes only structurally valid aliases, grouped by
     the observed configured-entry port.
3. Run the shared direct matcher for every non-empty alias group and cache its
   typed result in the client plan. Do not resolve the GUI port yet.
4. Aggregate all preflight diagnostics through the one warning accumulator.
   If any alias or direct-match result is incomplete, return immediately:
   zero port resolution, zero network request, zero backup, and zero removal.
5. If no cached unbound router-origin group contains a direct match, execute
   only any cached bound-client plans. Do not resolve the GUI port, call the
   probe, or emit a proof warning.
6. Otherwise resolve the router GUI port exactly once through the injected
   resolver.
   - On resolution error, emit
     `managedRouterProofWarning("port-unresolved")` exactly once to both the
     returned warning list and progress writer, then return without executing
     any bound or unbound removal plan. Do not call `probeManagedRouter`; this
     path has no resolved network destination.
   - On successful resolution, select only cached
     `routerMatchesByPort[resolvedPort]`.
7. If that selected set is empty, all structural router-origin candidates are
   stale-port candidates. Execute only cached bound plans. Do not call
   `probeManagedRouter` and do not emit a proof warning.
8. If the selected set is non-empty, call `probeManagedRouter` exactly once for
   the resolved port and immutable expected GUI identity.
   - On a failed proof, emit the existing proof warning once, preserve every
     unbound router-origin match, and retain the existing bound-client bypass
     behavior.
   - On a successful proof, authorize only the selected resolved-port groups.
9. Execute cached plans through the existing backup/removal owner. A client is
   backed up only when its selected cached match list is non-empty. No
   `GetEntry`, candidate scan, survivor scan, alias recomposition, port
   resolution, or liveness proof occurs in this execution stage.

This resolves the critical distinction: structural validity is established
before port resolution only to decide whether resolution is relevant; cleanup
authorization is established after successful resolution by selecting the
already-cached group whose observed entry port equals the resolved port. A
stale observed port is therefore never probed.

### Warning aggregation and deduplication

Use one cleanup-local accumulator keyed by the full warning string.

- `AddDiagnostic` appends an unseen existing GetEntry/direct-scan diagnostic
  once to the returned warning list and does not change its existing progress
  output contract.
- `AddProofWarning` appends an unseen proof warning once and writes exactly one
  `warning: ...` line to the progress writer.
- Backup/remove warnings continue through the same report accumulator with
  their current writer behavior.

Diagnostics gathered from repeated distinct-port match groups are deduplicated
by exact text. No helper writes warnings directly; the cleanup owner performs
the sole aggregation and progress emission.

### Return and skip paths

| Condition | Port resolution | Network proof | Backup/removal | Observable result |
|---|---:|---:|---:|---|
| nil/non-existent client | no, unless another client needs it | no, unless another client needs it | skip that client | unchanged |
| bound-only matches | no | no | cached bound plan may execute | bound bypass preserved |
| no configured router entry | no | no | no unbound removal | no proof warning |
| missing/disabled/malformed/non-loopback/wrong-language/non-owned entry | no | no | no unbound removal | no proof warning |
| valid structural router entry but no matching direct candidate | no | no | no unbound removal | no proof warning |
| GetEntry error | no | no | none | existing per-entry diagnostic once |
| candidate/survivor/stdio/gopls scan error | no | no | none | existing scan diagnostic once |
| structural match plus port-resolution error | once, failing | no | none, including bound plans | one `port-unresolved` warning in report and writer |
| structural match, resolved port differs from every matching group | once | no | stale unbound entries preserved; cached bound plan may execute | no proof warning |
| exact resolved-port group, proof fails | once | exactly one | unbound preserved; bound bypass unchanged | one existing proof warning |
| exact resolved-port group, proof succeeds | once | exactly one | cached exact group and bound plans only | normal backup/removal output |
| backup/remove error | already decided | at most one | existing per-client/per-entry fail-closed behavior | existing warning once |

### Internal injection seams and named tests

Add a package-private `directCleanupDeps` accepted by a private worker, while
the existing cleanup method remains the production wrapper. It contains
`resolveRouterPort`, `probeRouter`, and `matchDirect`; production values are
`a.lspRouterGUIPort`, `probeManagedRouter`, and
`directLanguageServerCleanupMatches`. This avoids mutable package-global test
hooks and changes no public API. `registerClient.GetEntry` remains the existing
entry-read injection seam.

Required guards:

1. `TestCleanupDirectLSP_NoRouterEntrySkipsResolverProbeAndWarning`.
2. `TestCleanupDirectLSP_NoDirectCandidateSkipsResolverProbeAndWarning`.
3. `TestCleanupDirectLSP_InvalidStructuralRouterEntriesSkipProof`, table rows
   `missing`, `disabled`, `malformed`, `non-loopback`, and `wrong-language`.
4. `TestCleanupDirectLSP_PortResolutionErrorWarnsOnceAndDoesNotMutate`: inject a
   resolver sentinel error; assert one resolver call, zero probe/request,
   backup, and removal calls, and one `port-unresolved` warning in both returned
   warnings and writer output.
5. `TestCleanupDirectLSP_GetEntryErrorIsReturnedOnceBeforeAnySideEffect`: the
   fake `registerClient.GetEntry` returns a sentinel error; resolver, probe, and
   matcher are fail-if-called; assert the existing per-entry diagnostic once
   and zero mutation.
6. `TestCleanupDirectLSP_DirectScanErrorIsReturnedOnceBeforeAnySideEffect`,
   table rows `candidate-scan` and `survivor-scan`: inject an incomplete
   `directCleanupMatchResult` carrying the existing formatted diagnostic;
   resolver/probe are fail-if-called; assert one report diagnostic and zero
   backup/removal.
7. `TestCleanupDirectLSP_StaleRouterPortIsNotProbed`: return a successful
   resolved port different from the cached structural match group; assert one
   resolver call, zero probe/request, no proof warning, and preservation of the
   unbound direct entry.
8. `TestCleanupDirectLSP_MatchingOwnedCandidateUsesOneCachedProof`: return the
   matching resolved port and a managed proof; assert one matcher call for the
   one-port client, one probe/request, one backup, expected removal, and no
   post-proof matcher call.
9. `TestCleanupDirectLSP_FailedProofWarnsOnceAndPreservesUnboundMatches` keeps
   the existing proof failure classes and bound bypass.
10. `TestCleanupDirectLSP_BoundOnlyPlanNeverResolvesOrProbes` protects the
    bypass after preflight caching.

The resolver and matcher counters are the falsifying probes for ordering. The
fake client counters are the falsifying probes for GetEntry, backup, and
removal. The managed test listener request counter is the network-proof probe.

### Failure modes and observable discriminators

| Failure mode | Fail-closed action | Observable discriminator |
|---|---|---|
| router entry cannot be read | abort before resolver/proof/mutation | existing client/language GetEntry warning text in returned warnings |
| direct candidate or survivor inventory cannot be read | abort before resolver/proof/mutation | existing `direct LSP ... scan failed` warning text in returned warnings |
| router port cannot be resolved | abort before probe/mutation | single proof warning with stable class `port-unresolved`, in report and writer |
| structural candidate is stale-port | preserve it without probing | successful resolver count plus zero probe count and no proof warning |
| listener/identity proof fails | preserve unbound matches | existing single managed-proof warning class |
| backup or removal fails | preserve or continue according to existing per-entry behavior | existing backup/remove warning text |

### Alternatives

1. Keep two uncached Boolean need passes around port resolution. Rejected:
   diagnostics can be duplicated or lost, and a third post-proof scan can fail
   after the request, violating the required zero-request failure path.
2. Probe every successfully resolved router port before candidate discovery.
   Rejected: no-router/no-direct-candidate cases would regain unnecessary
   network traffic and proof warnings.
3. Add port/matcher checks directly to each removal branch. Rejected: it creates
   duplicate route-shape, matcher, and liveness owners and breaks the single
   cleanup-wide proof.

### Diff-invisible invariants

- **Bound bypass. Named regression guard:**
  `TestCleanupDirectLSP_BoundOnlyPlanNeverResolvesOrProbes`; expected result is
  normal cached bound cleanup with zero resolver/probe calls.
- **One cleanup-wide proof. Named regression guard:**
  `TestCleanupDirectLSP_MatchingOwnedCandidateUsesOneCachedProof`; expected
  result is exactly one request across multiple eligible clients/languages.
- **Per-client/per-language authorization. Named regression guard:** retain the
  managed Go/sibling Python guard; a Go route never authorizes Python removal.
- **Both direct-entry kinds share one matcher. Named regression guard:** retain
  the two-kind cleanup guard; both kinds consume the same cached match result.
- **Point-in-time plan. Named regression guard:** the cache-reuse test makes a
  second matcher call fail; expected result is successful cleanup with no
  second call after proof.

### Claims

1. `{ guarantee: no router-origin direct match means no port resolution,
   network proof, or proof warning; single-owner:
   directCleanupPreflight.hasRouterMatches; enforcement-probe:
   TestCleanupDirectLSP_NoRouterEntrySkipsResolverProbeAndWarning and
   TestCleanupDirectLSP_NoDirectCandidateSkipsResolverProbeAndWarning }`.
2. `{ guarantee: structural route validity is decided without the configured
   GUI port and exact ownership is selected only by observed-port equality
   after resolution; single-owner: inspectClientLSPRouterReplacement plus the
   pure exact-port selector; enforcement-probe:
   TestCleanupDirectLSP_StaleRouterPortIsNotProbed }`.
3. `{ guarantee: a relevant port-resolution error produces zero request and
   removal plus exactly one port-unresolved warning in report and writer;
   single-owner: cleanupDirectLanguageServerEntriesAfterRegister warning
   accumulator; enforcement-probe:
   TestCleanupDirectLSP_PortResolutionErrorWarnsOnceAndDoesNotMutate }`.
4. `{ guarantee: GetEntry and direct scan errors are returned exactly once and
   precede every side effect; single-owner: directCleanupPreflight.complete;
   enforcement-probe:
   TestCleanupDirectLSP_GetEntryErrorIsReturnedOnceBeforeAnySideEffect and
   TestCleanupDirectLSP_DirectScanErrorIsReturnedOnceBeforeAnySideEffect }`.
5. `{ guarantee: one invocation performs at most one managed-listener network
   proof; single-owner: cleanupDirectLanguageServerEntriesAfterRegister;
   enforcement-probe:
   TestCleanupDirectLSP_MatchingOwnedCandidateUsesOneCachedProof }`.
6. `{ guarantee: no candidate, route-shape, alias, or liveness logic is copied
   into the removal loop; single-owner: the three named existing helpers;
   enforcement-probe: architecture re-review of internal/api/register.go }`.
7. `{ guarantee: bound clients remain independent of GUI proof; single-owner:
   lspCleanupAliasPlan.boundAliases; enforcement-probe:
   TestCleanupDirectLSP_BoundOnlyPlanNeverResolvesOrProbes }`.
8. `{ guarantee: public HTTP, command-line, configuration, and persisted-state
   contracts remain unchanged; single-owner: existing public contract owners;
   enforcement-probe: protected-file diff and existing ping-byte guard }`.

### Security, resource, and migration impact

The proof remains loopback-only, redirect-refusing, bounded by the existing
timeout and body limit, and body closure remains owned by `probeManagedRouter`.
The preflight performs read-only local discovery and holds no external resource.
There is no public contract or persisted-state change, so no migration,
compatibility window, or migrated-state rollback is required. Rollback is the
local removal of the Revision 2 orchestration/types/tests.

### Terms and Abbreviations

- **GUI:** graphical user interface.
- **LSP:** Language Server Protocol.
- **Preflight:** the read-only, fail-closed plan built before port resolution,
  proof, backup, or removal.
- **Structural candidate:** a valid owned router entry with an observed port,
  before comparison with the resolved configured port.

Gate: PASS
