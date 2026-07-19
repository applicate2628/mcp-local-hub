# Item 3 Unit B — PR #563 round-1 backend corrections

Outcome: **PASS** for the bounded C2/C3 backend correction scope.

## Summary

The restart-v3 child now uses `Quiesce + Bind` as its same-port standby bind budget while port-change keeps `Bind` alone (`internal/cli/gui.go:232`). The parent target resolver now validates the already-bound actual GUI port against TCP `[1,65535]`, while persisted GUI settings remain governed by `[1024,65535]` (`internal/cli/gui.go:155`, `internal/cli/gui_port.go:45`). No default deadline changed.

## Changed files

- `internal/cli/gui.go` — route-specific standby bind budget and actual-bound TCP-port validation.
- `internal/cli/gui_self_restart_handoff_test.go` — deterministic 3.2-second same-port bind regression plus privileged actual-port/spawn-seam regression and invalid-port guards.
- `internal/gui/gui_restart_record_test.go` — default deadline ordering guard for the composed same-port budget.

No frontend source, generated asset, endpoint, handler, marker schema, or default duration was changed.

## TDD evidence

RED command:

```powershell
go test -tags=test_state_path_env -count=1 -timeout 20s ./internal/cli -run 'TestRestartV3_(SamePortStandbyBindWaitsForParentClose|ParentCompositionPrivilegedActualPortReachesSpawn)$' -v
```

Preserved output: `.scratch/pr563-p2-backend-red.txt`.

- Same-port guard failed after 2.09 seconds with address-in-use plus `context deadline exceeded` while the listener remained held.
- Privileged-port guard failed because `TargetPort(80)` returned the pre-spawn range error.

Final focused CLI command exited 0 and is preserved at `.scratch/pr563-p2-backend-final-cli.txt`. It covered the two named regressions, dedicated bind deadline behavior, port-change child startup, persisted-port classification, parser-aware argv, and retained-lease composition. The same-port regression passed in 3.34 seconds and the privileged-port regression observed exactly one spawn call.

Final focused GUI command exited 0 and is preserved at `.scratch/pr563-p2-backend-final-gui.txt`:

```powershell
go test -tags=test_state_path_env -count=1 -timeout 20s ./internal/gui -run '^TestRestartDeadlines_DefaultPolicy$' -v
```

`gofmt -d` was empty and scoped `git diff --check` exited 0 for all three changed Go files.

## Root cause and correction

### C2 — same-port standby bind

The parent same-port path may spend `Quiesce` in `EnterGrace` before spending up to `Bind` closing the listener (`internal/gui/gui_restart_protocol.go:351`, `internal/gui/gui_restart_protocol.go:356`). The child previously bounded its whole standby bind retry by `Bind` alone. The decoded handoff already owns the route fact, so `runRestartV3ChildStartup` now selects:

- same port: `Quiesce + Bind`;
- port change: `Bind`.

With defaults this is bounded at 7 seconds. The policy guard verifies the composed budget is greater than quiesce and lower than both proof and reservation (`internal/gui/gui_restart_record_test.go:51`).

### C3 — privileged explicit actual port

`buildRestartV3ParentDependencies.TargetPort` applied `validPersistedGUIPort` to the already-bound actual port. That helper owns the settings contract and intentionally rejects ports below 1024. The callback now performs its own TCP-port check `[1,65535]`; the only remaining `validPersistedGUIPort` production caller is persisted-intent classification (`internal/cli/gui_port.go:58`). Actual 0 and 65536 remain rejected before spawn.

## Defect-class inventory

### `Deadlines.Bind` in child/parent bind, confirm, and close paths

- `internal/cli/gui.go:232` — **fixed**: initial child budget remains `Bind`; same-port adds `Quiesce` at `internal/cli/gui.go:234`.
- `internal/gui/gui_restart_protocol.go:330` — **not affected**: parent standby confirmation remains bounded by `Bind`.
- `internal/gui/gui_restart_protocol.go:356` — **not affected**: parent same-port listener close remains bounded by `Bind`; the child budget now spans the preceding quiesce plus this close window.
- `internal/gui/gui_restart_protocol.go:564` — **not affected**: dependency validation still requires positive bind/quiesce durations.
- `internal/gui/gui_restart_record.go:55` — **not affected**: default `Bind=2s`; default `Quiesce=5s` remains unchanged at the next line.

The bind retry owner is otherwise unchanged: invalid non-address-in-use errors return immediately, cancellation terminates through the context, and retry remains restricted to address-in-use at `internal/cli/gui.go:316`.

### Persisted and actual-port validation

- `validPersistedGUIPort` definition (`internal/cli/gui_port.go:45`) — **not affected**, still `[1024,65535]`.
- `classifyPersistedGUIPort` caller (`internal/cli/gui_port.go:58`) — **not affected**, so settings keep the unprivileged range.
- Parent actual-bound validator (`internal/cli/gui.go:155`) — **fixed**, now `[1,65535]`.
- Child startup `cfg.Port <= 0` precondition (`internal/cli/gui.go:210`) — **not affected**; it is a structured-child configuration guard, not the parent actual-bound-port policy owner.

## Receiving-side echo

| Guard / invariant | Expected | Observed |
| --- | --- | --- |
| 3.2-second same-port address-in-use hold | Child returns a live listener and commits, without bind timeout | PASS in 3.34 seconds; listener used the held port and marker reached `committed` |
| Privileged actual port 80 with unset persisted intent | Resolve target 80 and invoke `deps.Spawn` once | PASS; target 80, one spawn, explicit `--port 80` retained |
| Invalid actual ports | 0 and 65536 rejected before spawn | PASS; spawn count remained one after both invalid calls |
| Default deadline/order | Bind 2s, Quiesce 5s, composed 7s below Proof/Reservation 10s | PASS |
| Port-change budget | Uses `Bind` only | VERIFIED by branch source at `internal/cli/gui.go:232-234` and tagged port-change startup regression |
| Invalid bind errors / cancellation | Immediate; no extra retry classes | VERIFIED by unchanged retry owner at `internal/cli/gui.go:307-323` and dedicated tagged bind test |
| Persisted setting range | `[1024,65535]` | PASS in `TestClassifyPersistedGUIPort` |
| Endpoint, wire, marker, manager-stop, argv contracts | Unchanged except intended internal validation/timing | VERIFIED by scoped diff plus parser-aware argv and retained-lease tests |

## Wire-level before / after

- Endpoint, HTTP status, response body fields, field order, and error/success envelope: **no change**.
- Marker schema and handoff field names/order: **no change**.
- Named consumers unaffected: restart endpoint/frontend response consumer, `RestartCoordinator`, manager-stop bypass, and spawn argv reconstruction.
- Internal timing: same-port child standby retry `2s` → `7s` under defaults; port-change remains `2s`.
- Internal validation: actual-bound parent port `[1024,65535]` → `[1,65535]`; persisted-setting validation remains `[1024,65535]`.

## Backend gate statements

- Outbound calls: the modified local listener bind has an explicit bounded timeout; retry remains 25ms only for address-in-use, and failure returns to the existing coordinator rollback/error path. No outbound HTTP, database, queue, cache, or remote procedure call was added or modified.
- Authorization: no endpoint or handler changed; authorization behavior is unchanged.
- Data queries/cardinality: none.
- Publication: no commit, push, deployment, real GUI spawn, process sweep/kill, or `MCPHUB_GUI_SPAWN_TESTS` use occurred.

## Risks / unknowns

- The full build, vet, and package suites were intentionally not run; the integration owner explicitly reserved those broader gates.
- ASSUMPTION (UNVERIFIED): real-process privileged-port restart behavior was not smoke-tested because real GUI spawn was forbidden. Resolver: an authorized integration smoke on an administrator-bound port after the broader tagged gate.

## Recommended next gate

`$qa-engineer`, followed by the main integration owner’s exact tagged CLI/GUI gate and broader build/vet checks.

## Terms and Abbreviations

- CLI — command-line interface.
- TCP — Transmission Control Protocol.
- TDD — test-driven development.

