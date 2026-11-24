# P04: registered front-session stdio isolation

## Contents

- [Scope and current evidence](#scope-and-current-evidence)
- [Ownership and admission](#ownership-and-admission)
- [Retirement and receipts](#retirement-and-receipts)
- [Explicit application policy](#explicit-application-policy)
- [Executed focused checks and remaining gates](#executed-focused-checks-and-remaining-gates)
- [Remaining ARA-01 gates](#remaining-ara-01-gates)

## Scope and current evidence

This change extends existing PR #671, starting from `09dc07e01d1a3236ab5f45f384e11fe2f9dfd30d`. The protected headless `routeAggregateOwner`, `HubListener`, `HubSessionStore`, `StdioLaunchContext`, ordinary endpoint admission, and `IsolatedStdioSessions` already existed at that head. The corresponding prepared Wave aggregate and isolated-owner sources matched that head; no replacement service or second lifecycle controller is introduced.

Windows run 3 compiled and passed the registered-route fixture and the isolated-owner regression group on the combined source. Its per-step hashes match the 23 reviewed Windows source files recorded in [the combined evidence JSON](pr671-remediation-evidence-20261125.json). This is focused native fixture evidence; it does not qualify the installed Product or all repository gates. The run as a whole remained **FAIL** because a separate rollback fixture lacked its compiled registry input; that fixture passed in run 4 with the normal registry input. [The combined scope and history](pr671-remediation-evidence-20261125.md) retain both outcomes. No installed-state change, release, CI dispatch or merge is part of this change.

## Ownership and admission

`routeAggregateOwner.Apply` supplies the existing attached lifetime and original bind authority to `StartHubListener`. It additionally supplies `prepareRouteStdio`, which captures the parent environment, reads the registered manifest, checks the captured descriptor, and reuses the existing CLI secret and environment-overlay materializer. Process creation remains inside the existing daemon owner.

Each original protected listener holds one `IsolatedStdioSessions` per configured session-local provider. The ordinary `HubSessionStore` creates a distinct cancellable lifetime for each admitted front session. The existing aggregator request descriptor carries that owner through initialize, list, call, cancellation and retirement. Private requests use a synchronous in-process transport; there is no separately exposed private `/mcp` socket.

Private role authorization carries the original front-session context, the exact provider descriptor and a current admission predicate. Equal roles alone do not authorize another session's private identifier. Current admission checks the registered binding, launch descriptor and digest of the credential admitted at initialization. A changed credential cannot adopt an existing private conversation; observing the change cancels its original lifetime. Token checks do not store or log the raw credential in the session.

The configuration fingerprint covers command, base arguments, environment declarations, forwarded environment names and the selected daemon descriptor. Binding/visibility edits continue through the existing resolver. Launch-descriptor changes deny private dispatch; they cannot redirect that conversation to an ordinary TCP endpoint. Previously private bindings remain classified as private across port changes. Private provider loss does not silently recreate conversation state through the ordinary self-heal path.

## Retirement and receipts

Front DELETE, eviction and expiry cancel the same original front lifetime. DELETE cancels it **before** the best-effort downstream DELETE. The existing isolated-session cancellation watcher therefore owns actual process retirement; an HTTP 204 is not evidence that a process, Job or stream has settled.

Listener retirement revokes its attached lifetime and retains the original private owners. It cannot report settled while their `Close` still reports outstanding custody. `Retirements()` only snapshots the already-existing opaque receipt holders, without exposing private identifiers or reopening process handles. Tests capture those holders before revocation and await their original native and stream outcomes.

## Explicit application policy

These values are application policy, not limits derived from the Model Context Protocol (MCP), Windows, or an asserted provider compatibility measurement.

**Table 1. Application policy.**

| Policy | Current value | Meaning |
|---|---:|---|
| Session allocation per provider/listener | 32 units / 8 units per original tree | At most four concurrent private front sessions; original settlement returns the reservation. This is not an OS process-count limit. |
| Capacity retry hint | 2 seconds | Existing isolated owner returns HTTP 429 when its allocation is exhausted. |
| Remembered request identifiers | 4096 per private session | Existing cancellation fences remain bounded; exhausting this budget retires the session. |
| Request identifier size | 512 bytes | Existing typed identifier parser enforces the bound. |
| Private startup budget | 60 seconds | The actual operation also obeys the caller deadline; existing aggregate initialize is capped at 5 seconds. |
| Private operation budget | 60 seconds | Existing aggregate list is capped at 10 seconds and tool calls at 60 seconds; the tighter applicable deadline wins. |
| Private idle budget | 30 minutes | Existing private owner and front-store expiry retain bounded lifetime. |
| Original teardown / EOF grace | 5 seconds / 1 second | Existing original-child owner controls the stop and settlement receipts. |
| In-process response body | 4 MiB | Matches the existing aggregate response budget; an oversized response returns an error. |

Native isolated stdio currently supports Windows only. A missing private preparation, unsupported platform, ambiguous descriptor or denied launch cannot fall through to ordinary shared HTTP. Ordinary shared providers keep their existing route and transport behavior.

## Executed focused checks and remaining gates

**Table 2. Executed focused checks, Windows run 3.** Go package time excludes some command startup and compilation overhead; the command elapsed time comes from the original runner receipt.

| Check | Result | Go package time | Command elapsed time |
|---|---|---:|---:|
| `TestRegisteredHubStdioSessionsIsolateAndRetireOriginalProviders` | PASS; test case 12.82 s | 14.323 s | 29.429 s |
| `TestOwnedIsolatedStdio*` in `internal/daemon` | PASS | 5.407 s | 16.578 s |

Both commands returned exit 0. Their runner receipts record the original root reaped, Job active count zero, closed native handles, joined original stdout/stderr readers, EOF and closed capture files. Those runner facts are separate from the provider settlement assertions inside the tests.

Two earlier registered-route attempts supplied the `StatusContext` seam even though `Scan()` calls `Status()`. The corrected fixture sets `SetStatusInternalDialForTest` before listener startup and restores it after deferred original retirement. This replaces the existing observation only; actual route admission and private-owner execution remain in the test. Earlier failed fixture attempts are history, not successful qualification or installed-Product failures.

The following broader commands remain acceptance targets; this document does **not** claim their complete filters ran in run 3:

```text
go test -count=1 -timeout 5m ./internal/mcproute -run "TestRegisteredHubStdioSessionsIsolateAndRetireOriginalProviders|TestFixedHub|TestHubListener"
go test -count=1 -timeout 5m ./internal/daemon -run "TestOwnedIsolatedStdio"
go test -count=1 -timeout 5m ./internal/api -run "TestRegisteredSessionLocalBindingRefusesOrdinaryListener|TestHub.*Ordinary|TestHandler|TestAggregate|TestSession"
```

The registered-route test uses real on-disk client token rows and manifest bindings, the original protected bind, the actual `StartHubListener` composition, public `/clients/claude-code/mcp` and `/clients/cursor/mcp` routes, and two real native fixture children. It checks distinct state and process identities, continuity, cross-client identifier refusal, A's complete original process/stream receipt before B continues, descriptor-change refusal, token-rotation refusal and B's original receipt. An ordinary TCP honeypot on the configured provider port must receive no request.

The fixture substitutes the provider launch input and supplies test-owned qualification data at the existing bind boundary. The recorded run establishes the tested registered listener composition; it does **not** qualify the installed Product, the complete canonical-entry admission path, npm package resolution or Sequential Thinking 2.2.0 itself. The recorded direct daemon group additionally checks same-role cross-front denial and a late initialize after original front cancellation. The separate API regression checks refusal without private assembly, but its execution is not established by these two run-3 commands.

## Remaining ARA-01 gates

The shipped Sequential Thinking manifest stays unchanged. Generic install currently creates ordinary wrapper intent and per-daemon URLs; aggregate transition remains owned by the existing reconcile operation. Enabling the shipped session-local declaration requires a separately reviewed, consistent install/reconcile/restore transition and rollback evidence. This change does not advertise that transition as implemented.

Dormant project-enrollment/rebind helpers remain unconnected and retain their strict refusal. Two registered front sessions are not proof of project-specific enrollment or rebind behavior. Full ARA-01 closure additionally requires the pinned real provider through the qualified Product, original process/stream settlement, registration/restore and rollback acceptance on the exact final source and package inputs.

Focused Windows results and exact source hashes are now recorded. Required repository gates, independent review, the remaining integration transitions and exact final Product/package acceptance still remain. Continue in the existing PR; no new PR or merge is required by this lane.
