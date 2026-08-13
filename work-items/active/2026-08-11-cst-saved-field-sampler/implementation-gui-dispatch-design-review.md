# W8 F1 GUI dispatcher correction final architecture re-review

Date: 2026-08-13

Execution role: `$architecture-reviewer`

Reviewed design: `implementation-gui-dispatch-design.md`, SHA-256
`CC0AE477C1ACD4A62D4A39094A4D82A5687A89D4F4EF9C707E22F825C450A3BC`.
Prior review: SHA-256
`014398336E93268A873742B6C6034FBD3A97EB363EBEC2224076F8B4A016FBC4`.
Evidence remains bound to immutable commit
`7aa0a2168e09139bf8deeac0fe31b8d0260c017d`. CodeGraph MCP was fresh after
`status -> sync -> status` at 2,118 indexed files. This was a design-only gate;
no source, Git index/history, service, CST, or production state changed.

## Reviewed scope

The final pass checked only the prior present-empty selector gap and possible
regression to the already accepted classes: four Go selector spellings and
parse stops/duplicates/malformed inputs; Windows versus POSIX environment-key
identity; canonical R6 marker consumption; terminal-worker framing; and the
Linux PIDFD role, fd 3/4 borrowing, root bypass and parent cleanup ownership.

## Finding dispositions

| Finding | Disposition | Evidence |
|---|---|---|
| Original W8 F1: unmarked outer R6 rejected and marked receiver leaked into terminal worker | `fixed` | One role state machine admits the unmarked parent, validates the marked receiver and consumes `R/S` before both nested workers. |
| Prior F1: incomplete Go selector grammar | `fixed` | One/two-dash attached/split forms, `--` and first-positional stops, mixed duplicates, missing split values, malformed reserved-looking tokens and terminal conflicts are explicitly owned. |
| Prior F2: Windows case-insensitive environment identity | `fixed` | One eight-key registry uses Windows lower-case logical identity, rejects duplicate variants and scrubs all variants; POSIX remains byte-sensitive. |
| Prior F3: Linux PIDFD same-binary child omitted | `fixed` | A Linux-only role owns `P/T`, calls the common filter, bypasses package-root setup and preserves parent-owned cancellation, wait, pipe and fd cleanup. |
| Prior F4: present empty `test.run` changed from valid to invalid | `fixed` | Design lines 77-85 and truth-table line 138 distinguish present empty from genuinely missing split value. All four present-empty forms normalize to ordinary `normal-parent`; helper frames remain conflicts. Positive guards cover root creation/removal and no helper-body entry. |

## Architecture assessment

- Exactly one parser/classifier and one platform-aware registry/filter own the
  complete inventoried same-binary process framing.
- Normal parent alone creates/removes the package root; four child roles bypass
  setup and propagate their exact return semantics.
- Empty ordinary selectors preserve installed Go string-flag behavior without
  weakening any reserved helper selector.
- R6 role authority is consumed before production-shaped nested work; terminal
  worker acceptance remains strict and unchanged.
- Windows and POSIX key identity are not conflated, and classifier tests receive
  full environment slices rather than a misleading value-only getter.
- Linux PIDFD child lifetime remains subordinate to its existing parent resource
  owner. No new handle, goroutine, listener, production state or cleanup owner is
  introduced.
- Anti-layering verdict: `CLEAN-SINGLE-OWNER`. No fallback, parallel parser,
  duplicate registry, or production behavior change is designed.

## Claim reconciliation

Canonical verdicts are `verified`, `failed`, and `not-verifiable (with reason)`.

| Claim | Verdict | Result |
|---:|---|---|
| 1 | verified | One Go-compatible parser/classifier covers all four forms, present-empty values, missing values, stops and every role. |
| 2 | verified | Only normal parent owns one package root; every helper child count is zero. |
| 3 | verified | Unmarked R6 outer and marked receiver are exact across selector spellings. |
| 4 | verified | `R/S` are absent before either nested terminal worker. |
| 5 | verified | Production terminal-worker framing and protected source remain unchanged. |
| 6 | verified | Blocking, R6 and PIDFD spawns use one platform-aware eight-key filter. |
| 7 | verified | Missing/malformed, duplicate, partial, unknown, conflicting, platform and wrong-argv frames fail before setup; present-empty does not. |
| 8 | verified | Valid child `m.Run` failures retain their exact status. |
| 9 | verified | Every same-binary caller has a declared role and one parser/registry/filter owner. |
| 10 | verified | The four-file `_test.go` surface changes no external contract, schema, dependency, production behavior or live state. |

Result: 10 `verified`, 0 `failed`, 0 `not-verifiable`.

## Residual risk and next gate

This is design approval, not implementation proof. Backend implementation must
stay within the declared four `_test.go` files and execute every named positive
and adversarial guard. Any production-path edit, second parser/filter, omitted
same-binary caller, or behavior difference from this truth table returns to
architecture review.

## Gate

`PASS`

The bounded design now closes W8 F1 and all review findings through one
fail-closed test-only owner while preserving ordinary Go test behavior and all
protected production contracts. Backend implementation is authorized within the
accepted change surface.

## Terms and Abbreviations

- CST: CST Studio Suite electromagnetic solver.
- GUI: Graphical user interface.
- PIDFD: Linux process file descriptor.
- R6: real cross-process GUI recovery receiver regression scenario.
- W8: independent implementation architecture review phase.
