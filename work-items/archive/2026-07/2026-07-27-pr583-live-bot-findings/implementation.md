# Implementation: managed router proof before direct-entry cleanup

## Receiving echo

Implemented the accepted bounded behavior in the four authorized files only:

- `internal/api/register.go`
- `internal/api/register_test.go`
- `internal/gui/projects_toggle.go`
- `internal/gui/projects_toggle_test.go`

The production GUI supplies its own bound port, process identifier, and version.
Command-line and legacy callers retain the zero-value identity and therefore
cannot authorize destructive cleanup from a pre-existing router entry alone.
The public `/api/ping` producer and its wire shape were not edited.

## Root cause and owner

The defect was in the cleanup authorization boundary:
`clientHasActiveLSPRouterReplacement` accepted configured URL shape, ownership,
language, and port equality as sufficient evidence (`internal/api/register.go:192-209`).
`cleanupDirectLanguageServerEntriesAfterRegister` could then reach backup and
removal (`internal/api/register.go:623-719`) without proving that the configured
port belonged to the live managed GUI/router process.

The correction stays in that owner:

- `ManagedGUIIdentity` is the typed caller evidence (`internal/api/register.go:212-218`).
- `probeManagedRouter` makes one bounded identity decision
  (`internal/api/register.go:542-614`).
- `cleanupDirectLanguageServerEntriesAfterRegister` snapshots the result once
  and passes only the immutable verdict and resolved port downstream
  (`internal/api/register.go:623-668`).
- `lspCleanupAliasesForClient` preserves the same-registration bound bypass and
  requires managed proof before consulting a pre-existing router entry for an
  unbound client (`internal/api/register.go:754-787`).
- The GUI project-toggle caller supplies the identity from the same `Server`
  instance that owns the request (`internal/gui/projects_toggle.go:170-187`).

## Defect-class sweep

| Axis | Covered disposition |
|---|---|
| Replacement origin | Same-registration bound replacement bypasses the network proof; pre-existing router replacement requires both managed identity and the existing configured-entry gate. |
| Listener state | Stopped, timeout, foreign, malformed, and identity mismatch fail closed; exact managed identity may authorize cleanup. |
| Router-entry validity | Missing, disabled, malformed, non-loopback, wrong-language, stale-port, and valid-owned rows remain owned by the existing per-language gate. |
| Direct-entry kind | `mcp-language-server` and direct `gopls` candidates consume the same alias authorization. |
| Grain | One proof snapshot covers the cleanup; configured-entry authorization remains per client and language. |
| Caller provenance | GUI supplies identity; command-line and legacy callers supply none. |
| Warning surface | One classified warning is appended to `RegisterReport.Warnings` and written once to progress on proof failure. |
| Resource behavior | One 500 ms request/context ceiling, zero retries, redirects refused, 4 KiB body cap, and response-body closure. |

## Probe and failure contract

The probe sends one loopback `GET /api/ping` only after validating the resolved
port and expected tuple. It requires HTTP 200, `application/json`, at most 4 KiB,
one JSON value with no trailing value, `ok:true`, a positive PID, a non-empty
version, and exact PID/version equality. Failure classes are:

- `identity-not-supplied`
- `port-unresolved`
- `port-mismatch`
- `stopped-or-timeout`
- `http-status`
- `content-type`
- `malformed-response`
- `identity-mismatch`

No retry or fallback can turn a proof failure into cleanup permission.

## Test-first evidence

| Gate | Command shape | Result and preserved output |
|---|---|---|
| Initial stopped/foreign RED | Tagged isolated API run with a fresh override and regex for the two cleanup guards | Exit 1. Both failures state that cleanup removed the direct entry: `.scratch/pr583-a-red-stopped-foreign-20260727-055305137-ec3c6f06fba14c3492aeea95341ffa8a/{go-test.txt,exit-code.txt}`. |
| Fail-closed gate GREEN | Same two tagged isolated API guards | Exit 0: `.scratch/pr583-a-green-stopped-foreign-gate-20260727-055426987-5440eac3f67843c78ed17a9bb8c59a4e/{go-test.txt,exit-code.txt}`. |
| Managed-positive RED | Tagged isolated `TestRegister_CleanupRemovesDirectEntryWithProvenManagedRouter` | Exit 1 because the direct Go entry remained: `.scratch/pr583-a-red-managed-positive-20260727-055509941-a1c7e4481d694bb5884f256a922d09c6/{go-test.txt,exit-code.txt}`. |
| Managed-positive GREEN | Same tagged isolated positive guard | Exit 0: `.scratch/pr583-a-green-managed-positive-20260727-055856761-df00cb34c67e4017a4e211f46c65e898/{go-test.txt,exit-code.txt}`. |
| Focused API suite | Tagged isolated regex covering probe, stopped, foreign, managed, bound bypass, invalid router entries, both direct kinds, binding snapshot, and earlier closure guards | Exit 0, `ok mcp-local-hub/internal/api 0.628s`: `.scratch/pr583-a-api-20260727-061214956-6073e13271a74d2cb7bf5a5ea16e0307/{go-test.txt,exit-code.txt}`. |
| GUI seam | Narrow `internal/gui` run for identity propagation and ping wire | Exit 0, `ok mcp-local-hub/internal/gui 0.024s`: `.scratch/pr583-a-gui-final-20260727-061140891-a32518efa6fe450b936bac92394f929f/go-test.log`. |

## Acceptance reconciliation

| Criterion | Evidence |
|---|---|
| A-AC1 | GUI identity seam and guard at `internal/gui/projects_toggle.go:170-187` and `internal/gui/projects_toggle_test.go:61-108`. |
| A-AC2 | Invalid input table at `internal/api/register_test.go:3934-3968`; cleanup failure guards assert zero backup/removal and one warning. |
| A-AC3 | Timeout/cancellation/request-count guard at `internal/api/register_test.go:4034-4075`. |
| A-AC4 | Response contract table at `internal/api/register_test.go:3970-4032`. |
| A-AC5 | Stopped and foreign cleanup guards at `internal/api/register_test.go:4311-4451`. |
| A-AC6 | Foreign multi-client/multi-language case asserts one request and one warning at `internal/api/register_test.go:4371-4451`. |
| A-AC7 | Bound bypass guard at `internal/api/register_test.go:4133-4205`. |
| A-AC8 | Managed Go versus sibling Python guard at `internal/api/register_test.go:4453-4529`. |
| A-AC9 | Invalid configured-entry table at `internal/api/register_test.go:4531-4626`. |
| A-AC10 | Direct-entry-kind table at `internal/api/register_test.go:4628-4742`. |
| A-AC11 | Existing one-binding-snapshot guard remains in the focused green run. |
| A-AC12 | No diff in `internal/gui/ping.go`; the unchanged ping-wire test is in the focused GUI green run. |
| A-AC13 | All API runs used `-tags=test_state_path_env`, a fresh state override, `-count=1`, `-timeout 10m`, and narrow regexes; no GUI, tray, supervisor, scheduler, or child process was launched. |

## Residual risk

The liveness proof is point-in-time: the managed GUI can stop after a successful
probe and before a later client write. The change closes the reviewed stale or
foreign-listener authorization defect but does not create a transaction across
the network probe and client configuration writes.

## Gate

PASS — implementation and test-first evidence are ready for independent
mutation testing.

## Revision 1

The architecture-review REVISE is addressed at the existing cleanup boundary.
`managedRouterProofNeeded` now requires an existing unbound client to have both
an enabled configured-router entry for a registered language and a matching
workspace-scoped direct-cleanup candidate before the cleanup-wide managed
listener proof runs. The bound-client bypass is unchanged. Candidate discovery
and survivor matching are composed once in
`directLanguageServerCleanupMatches`; both the proof-need decision and the
removal path consume that owner.

| Gate | Result and preserved output |
|---|---|
| Behavioral RED: two no-candidate guards | Exit 1 for the intended behaviors: the no-router case emitted `identity-not-supplied`; the wrong-workspace direct-entry case made one request. `.scratch/pr583-router-proof-needed-red-20260727-065908319-2f0c683f4e804d3f9a4650259aad28ca/go-test.txt` |
| Exact two-test GREEN | Exit 0, `ok mcp-local-hub/internal/api 0.039s`. `.scratch/pr583-router-proof-needed-green-20260727-070300058-46582f9ec03f425183382457fe7e90ee/go-test.txt` |
| Focused API regression set | Exit 0, `ok mcp-local-hub/internal/api 0.929s`; covers probe, stopped/foreign listeners, managed cleanup, bound bypass, invalid entries, both direct kinds, binding snapshot, and both new guards. `.scratch/pr583-router-proof-needed-focused-rerun-20260727-070427156-3330768a4c41429a981e8b88abf9f5d2/go-test.txt` |
| Go language-server diagnostics | No errors in the two touched Go files; only two pre-existing `maps.Copy` style hints in the package. |

The invalid-router table now expects zero proof requests for missing, disabled,
malformed, non-loopback, wrong-language, and stale-port entries, and one request
only for the matching owned entry. Removal, backup, probe response, warning
text, public HTTP, command-line interface, configuration, and persisted-state
contracts are unchanged.

Gate: **PASS** for backend implementation; architecture re-review and quality
assurance re-verification remain the next gates.

## Terms and Abbreviations

- API — application programming interface.
- GUI — graphical user interface.
- LSP — Language Server Protocol.
- RED/GREEN — failing-then-passing test-driven development checkpoints.

## Revision 2

The architecture-review Revision 1 finding is corrected at the existing
post-register cleanup boundary. Cleanup now builds one typed, port-independent
preflight plan before resolving a router port, proving listener identity, or
mutating client state. The plan caches bound matches and router-origin matches
grouped by their observed structural port. Any router-entry or direct-inventory
diagnostic makes the plan incomplete and aborts before every resolver, network,
backup, or removal side effect.

The private cleanup worker receives cleanup-local resolver, probe, and matcher
dependencies; the production wrapper supplies the existing owners. It resolves
the router port only when the cached plan contains a router-origin direct match,
probes only when the resolved port selects an exact cached port group, and
executes the cached point-in-time plan without a post-proof rescan. Bound-only
cleanup remains independent of router resolution and proof. A single
cleanup-local warning accumulator deduplicates returned diagnostics and emits a
proof warning to progress output exactly once.

| Gate | Result and preserved output |
|---|---|
| Behavioral RED: resolution and structural/read failures | Exit 1 for the intended missing behaviors: no `port-unresolved` proof warning, no `GetEntry` diagnostic, and no candidate/survivor scan diagnostics. `.scratch/pr583-router-proof-r2-red-behavioral-20260727-073526073-94d342d8abe643bbb2538cc4d363c0f0/go-test.txt` |
| Cache RED | Exit 1 because the matching candidate path scanned twice instead of consuming one cached plan; the stale-port characterization already passed. `.scratch/pr583-router-proof-r2-cache-red-20260727-073745449-f99c7170af7c494b9e2abb8138d4b5b0/go-test.txt` |
| Final focused API gate | Exit 0 for the complete probe matrix, all eight Revision 2 preflight guards, and the adjacent cleanup regressions. `.scratch/pr583-router-proof-r2-final-tagged-20260727-074812458-15f6727d526b44c68aea8db9ef836457/go-test.txt` |
| Go language-server diagnostics | No errors or warnings in the two touched Go files; two non-blocking `maps.Copy` hints remain. |
| Formatting and diff hygiene | `gofmt` completed for both touched Go files; scoped `git diff --check` passed. |

The named guards prove:

- no router entry, no matching direct candidate, and bound-only plans make zero
  resolver and probe calls;
- a stale observed port resolves once, makes zero probe calls, emits no proof
  warning, and preserves the direct entry;
- a relevant resolver error emits one stable `port-unresolved` warning in both
  returned warnings and progress output and performs no mutation, including
  bound cleanup;
- configured-entry and direct candidate/survivor read failures return their
  diagnostic exactly once and precede every side effect;
- multiple eligible clients share one managed-listener proof, while each client
  is scanned once and the removal phase performs no rescan;
- exact managed proof still removes both supported direct-entry kinds, while
  invalid structural entries, sibling languages, stopped listeners, and foreign
  listeners remain fail-closed.

No public HTTP, command-line, configuration, persisted-state, or ping-wire
contract changed. The implementation slice modifies only
`internal/api/register.go`, `internal/api/register_test.go`, this canonical
implementation note, and the required backend session report. No commit or
publication action was performed.

Gate: **PASS** for Backend Revision 2; architecture re-review and quality
assurance re-verification remain the next gates.

## Terms and Abbreviations

- API — application programming interface.
- GUI — graphical user interface.
- LSP — Language Server Protocol.
- RED/GREEN — failing-then-passing test-driven development checkpoints.
