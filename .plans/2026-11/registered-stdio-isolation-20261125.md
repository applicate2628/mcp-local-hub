# P04: registered front-session stdio isolation

## Contents

- [Scope and current evidence](#scope-and-current-evidence)
- [Ownership and admission](#ownership-and-admission)
- [Retirement and receipts](#retirement-and-receipts)
- [Explicit application policy](#explicit-application-policy)
- [Executed focused checks and remaining gates](#executed-focused-checks-and-remaining-gates)
- [Install and project attachment follow-up](#install-and-project-attachment-follow-up)
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

## Install and project attachment follow-up

The follow-up starts from published #671 `891ef8bfd0764a969db2f800578fd45acd5623d6`. Its new code and tests require separate execution; the earlier run-3 results above do not qualify them. The shipped Sequential Thinking declaration now selects `mcp_stdio_state: session-local`, retaining the pinned `@modelcontextprotocol/server-sequential-thinking@2.2.0` package. Windows, an admitted protected front, its original prepared descriptor and supported HTTP client adapter are required. No ordinary shared fallback is added.

Install borrows the actual built-in route descriptor, current Supervisor identity and received aggregate generation. The private plan creates no provider Scheduler task or Supervisor descriptor and writes registered `/clients/<id>/mcp` bindings. Before publication, the existing control-token endpoint receives initialize, tools/list and the original process/stream receipt through the existing front-session owner. A descriptor-only read checks preparation during preview. Control operations use the current instance, a separate control credential, strict bodies limited to 2048 bytes and a 30-second caller budget. Original retirement receiving has a 6-second budget; timeout remains failure. These are application policies, not MCP requirements.

The reconcile owner retains its existing routing and Hub leases. Private bindings require admitted gate-ON routing; gate-OFF refuses before writing a per-provider URL. The installed signal for a private provider comes from the existing managed-registration projection because there is intentionally no permanent provider task. Ordinary manifest create/delete/identity-edit/restore cannot introduce, remove or demote a protected private descriptor. The typed pre-environment identity fence reuses the prepared Wave `sequential-guarded-manifest-fence` donor, with text decoding repaired; the restore fence is an additional caller correction. Descriptor-preserving metadata edits remain supported. Generic recovery of a missing custom private manifest remains refused; this is not advertised as a protected manifest publisher.

An explicit `--project-client-group` selects an already declared group containing the requested provider. Membership is hashed and never widened. The existing project config, paired-entry transaction, backup owner and backend receipt remain the lifecycle owners. The actual route is recorded as `reused`; rollback receives its identity but cannot stop or remove it. V3 public plans bind a credential-issuance marker at the exact auth-header slot, every other desired field, the group, original backend generation, manifest, project beforeimage and release. Apply validates the accepted digest before generating a credential and commits the actual paired config through its existing byte owner. No secret or project path is included in the public plan.

Project enrollment stages the existing authority record and rotates only that selected group's token. The original front receives old-session retirement before the successor config is published. Project authentication requires the current enrollment, its original committed private capsule, the physically verified paired config, unchanged group membership and the actual reused backend's received generation. Caller headers cannot supply those facts. A pending or revoked record remains guarded against ordinary group authentication. Exact rollback revokes only its own current revision; an old revision refuses to alter a successor. Rebinding to another project requires a new preview and apply; same-project reissuance requires the original pair to be rolled back first.

Retirement receiving enumerates the original isolated owners, including receipts whose public front SID has already been deleted. Each receipt retains its original front lifetime and scope. An incomplete native or stream receipt retains the original reservation and is observed again on retry. Missing public SID, EOF, DELETE acknowledgement and equal role are not closure proofs.

Existing shared definitions use the included ARA-01 migration caller, which depends on the separately composed WL-04 owner helpers from PR #659: capture and validate the original descriptor before the intent lock, remove the exact row under that lock, confirm release, reconcile and receive the same retained creator's completion before any private probe/publication. Those owner helpers remain in #659; standalone #671 build/qualification without that dependency is not claimed. Pending removal uses the existing stop/tombstone state; absence alone must not turn a lost completion into success.

Added causal tests cover plan shape, no private TCP-port admission, reconcile refusal, control credential separation, receiving before 204, lost-receipt retry after SID deletion, plan issuance and paired restore, replay refusal, group drift, successor protection, and protected manifest mutation/restore. The existing native registered-route fixture now also receives the protected control admission's original receipt. These test sources are not runtime evidence until Root executes the exact composed source.

## Remaining ARA-01 gates

The exact follow-up source must pass independent review and native execution, including its WL-04 migration caller and both loss/retry negatives. Required end-to-end acceptance uses the existing qualified Product namespace and pinned real provider: install, two registered sessions, project A enrollment, project B rebind, old-token denial, B continuation, original A/B process and stream receipts, owned project rollback, and reconcile. No installed task or service is needed for the isolated qualification.

Custom private manifest recovery through an ordinary attach/restore remains deliberately refused because that operation does not possess an original protected listener publication capability. Same-project reissuance and rollback of a superseded project pair also remain explicit refusals rather than acquiring authority over the successor. These limits must remain visible when reporting completion. Focused historical native fixture evidence, new source coverage, cross-compilation and exact final Product/package acceptance are separate claims.

The applied review lenses are Software Architect requirements/system design and architecture decisions, MCP Boundary control ordering and receiving, Go concurrency/lifecycle and testing, and Ponytail reuse/simplicity. The current phase reread architecture decisions, MCP Boundary and its control-order reference; prior relevant skill reads remain in the audit/implementation registry. No mandatory broad role sequence, installation or model launch was performed.
