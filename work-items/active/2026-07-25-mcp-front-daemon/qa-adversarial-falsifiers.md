# QA adversarial falsifiers — architecture A2 and A3

Target: detached `d6c0501f5866644849423d12583ad4e8f4b0c696`.

Participants: QA engineer; lead as the accepting parent role.

QA gate: **PASS**. The three requested deterministic falsifiers were decisive:
both read-only resolver families published an older generation over a newer
generation, and the startup seeder returned a stale caller generation rather
than the generation committed under the intent lock.

Product closure: **REVISE**. Architecture findings A2 and A3 are runtime-proven
product defects, not merely static concerns.

## Scope and safety

- The accepted claims are A2 and A3 from
  `work-items/active/2026-07-25-mcp-front-daemon/architecture-adversarial-reverify.md:266-343,345-436`.
- Temporary tests and the two pause hooks existed only in one detached
  worktree at `.scratch/qa-accept-inc1/adversarial-a2a3-20260726-01/`.
- The worktree was created with the required marker:
  `git worktree add --detach $wt HEAD # orchestrarium:requested-isolation-worktree`.
- Each command selected exactly one anchored test in exactly one approved
  package. No whole-package suite, `go test ./...`, real daemon start/kill, or
  live state access was used.
- The tests and instrumentation were read after formatting and before
  execution. `git diff --check` was clean in the detached worktree.
- The previous strict-port and persistence nil-input probes were not rerun.
- Raw output and the complete temporary harness source are under
  `.scratch/qa-accept-inc1/adversarial/`.

## Criterion adequacy

| Claim | Weak criterion that could pass the defect | Decisive criterion used |
|---|---|---|
| A2 resolver generation ordering | A race-only run could report no data race while an older complete snapshot still overwrites a newer cache. | Pause reload A after it copies generation A but before cache publication; publish B through an overlapping reload on the same resolver; verify B is cached; release A; require the final cache and modification-time token to remain B. |
|  | A same-modification-time update would mix the independent invalidation-token defect into the ordering result. | Publish seed, A, and B with distinct explicit modification times separated by two seconds. |
| A3 committed-generation return | Checking disk alone would pass because the locked mutation correctly preserves B on disk. | Seed stale caller A and newer disk B with an extra daemon, a stop, and strict mode; seed the built-in route; require both the returned value and disk to be identical, retain B, and contain exactly one canonical route row. |

## Result matrix

Counts are top-level Go tests. No subtests, skips, or expected-failure facility
were used.

| Claim / package | Result | Decisive observation | Mechanism |
|---|---|---|---|
| A2 Serena read-only resolver | 0 pass, 1 fail, 0 skip; exit 1 | B port `19102` was observed and cached before A was released. A then overwrote the final cache with port `19101` and regressed `lastMtime` from `02:47:58` to `02:47:56` local time. | **HELD / CONFIRMED** |
| A2 LSP read-only resolver | 0 pass, 1 fail, 0 skip; exit 1 | B port `19202` was observed and cached before A was released. A then overwrote the final cache with port `19201` and regressed `lastMtime` from `02:48:37` to `02:48:35` local time. | **HELD / CONFIRMED** |
| A3 startup seeder | 0 pass, 1 fail, 0 skip; exit 1 | Disk committed B plus one route row. The return contained stale A plus one route row, omitted B's extra daemon and stop, retained `StrictMode=false`, and was not deeply equal to disk. | **HELD / CONFIRMED** |

The non-zero exits are the intended correctness assertions failing against the
current product. They are evidence of the defects, not harness or environment
failures.

## Exact commands and timing

Serena A2:

`go test -race -tags=test_state_path_env -count=1 -run '^TestQAAdversarialSerenaReadOnlyReloadCannotRegress$' ./internal/api/serena_routing`

- Started: `2026-07-26T03:47:29.6011257+03:00`
- Ended: `2026-07-26T03:47:54.9171507+03:00`
- Wall time: `25.310 s`; test-reported time: `0.17 s`; package time:
  `0.412 s`; exit `1`.
- Raw: `.scratch/qa-accept-inc1/adversarial/a2-serena.txt`.

LSP A2:

`go test -race -tags=test_state_path_env -count=1 -run '^TestQAAdversarialLSPReadOnlyReloadCannotRegress$' ./internal/api/lsp_routing`

- Started: `2026-07-26T03:48:11.8612882+03:00`
- Ended: `2026-07-26T03:48:34.1519343+03:00`
- Wall time: `22.286 s`; test-reported time: `0.28 s`; package time:
  `0.366 s`; exit `1`.
- Raw: `.scratch/qa-accept-inc1/adversarial/a2-lsp.txt`.

CLI A3:

`go test ./internal/cli -run '^TestQAAdversarialSeederReturnsCommittedGeneration$' -tags=test_state_path_env -count=1`

- Started: `2026-07-26T03:48:44.6223100+03:00`
- Ended: `2026-07-26T03:49:08.6309158+03:00`
- Wall time: `24.003 s`; test-reported time: `0.02 s`; package time:
  `0.069 s`; exit `1`.
- Raw: `.scratch/qa-accept-inc1/adversarial/a3-cli.txt`.

## A2 finding

The temporary hook ran after `Registry.Load` and the family-specific entry copy,
but before the resolver cache lock and publication. The deterministic sequence
was:

1. Prime the resolver from a seed generation.
2. Publish A with a later modification time.
3. Start reload A and pause it after its complete A entry slice is copied.
4. Publish B with a modification time two seconds newer than A.
5. Start reload B on the same resolver and require it to return B.
6. Inspect the private cache under the resolver mutex and require B.
7. Release A, then require the final cache and token to remain B.

Steps 5 and 6 passed in both families; step 7 failed in both. This directly
confirms the out-of-order cache-publication mechanism described at
`architecture-adversarial-reverify.md:275-288,333-343`.

Both commands used `-race`. Neither output contained a race-detector warning.
That does not disprove A2: the engineered schedule pauses A after its registry
copy, so the decisive failure is generation-order regression under overlapping
reload lifetimes, not a requirement for simultaneous field writes.

The same-modification-time behavior at
`internal/api/serena_routing/resolver_test.go:460-510` is independent. This
falsifier used distinct seed/A/B times; it neither depends on nor attempts to
close that separate invalidation-token defect.

## A3 finding

The temporary CLI test supplied:

- stale caller generation A with daemon `\qa-generation-a`;
- newer on-disk generation B with daemon `\qa-generation-b-extra`, stop
  `\qa-generation-b-stopped`, and `StrictMode=true`;
- a deterministic strict port result of `9137`.

The locked transaction correctly committed B plus exactly one canonical
`\mcp-local-hub-route-front` row. The returned pointer also had exactly one
route row, but it was A plus that row: it omitted B's daemon and stop, retained
A's update marker, and retained `StrictMode=false`. Deep equality with the
committed disk generation failed. This confirms the stale reconstruction path
described at `architecture-adversarial-reverify.md:354-382`.

The sibling nil-input failure behavior is cited, not rerun: the previous QA
artifact recorded that both strict-port and persistence failure return a
non-nil empty intent from nil input while creating no route/file or success
event (`qa-reverify.md:140-152,211-214`).

## Evidence manifest

| File | SHA-256 | Purpose |
|---|---|---|
| `.scratch/qa-accept-inc1/adversarial/a2-serena.txt` | `6974C25951E831EACFE9486F3AD80D69F0E810B4FAAB78D739B840DD1CD53903` | Exact Serena command, timestamps, exit, and assertion output. |
| `.scratch/qa-accept-inc1/adversarial/a2-lsp.txt` | `0581595173DA36C7C28FCB176EFBCB8127B9EDDCC4CBE7DFA031B38C95E5931E` | Exact LSP command, timestamps, exit, and assertion output. |
| `.scratch/qa-accept-inc1/adversarial/a3-cli.txt` | `B7F1FFD9C9603B39CD765345AB38B1D7E692FC8DFF270EEE8616A8376AC6F92C` | Exact CLI command, timestamps, exit, and assertion output. |
| `.scratch/qa-accept-inc1/adversarial/harness-source.txt` | `AF7C7D41E77FECC6F48D66AE60154F7EB43D89898E8E1A8035183D8DEDC8ACCC` | Detached HEAD, temporary status, resolver-hook diff, and all three test sources as read before execution. |
| `.scratch/qa-accept-inc1/adversarial/cleanup.txt` | `3AC289D35A47D8F22BC2A17E793829F38199DEE1FAE552D2CE435E834D54ABBB` | Worktree absence and registration cleanup. |
| `.scratch/qa-accept-inc1/adversarial/main-product-status-before.txt` | `53F13255A05BAD7B4D7AF92DBF56D1D91F23ACC5E6AC6005F428221B8455A74C` | Empty main-worktree product diff/status before cleanup and artifact write. |
| `.scratch/qa-accept-inc1/adversarial/main-product-status-after.txt` | `53F13255A05BAD7B4D7AF92DBF56D1D91F23ACC5E6AC6005F428221B8455A74C` | Byte-identical empty main-worktree product diff/status after cleanup. |

## Cleanup and integrity

- The detached worktree was removed with
  `git worktree remove --force $wt`.
- `.scratch/qa-accept-inc1/adversarial/cleanup.txt` records
  `Temporary worktree exists: False` and no matching worktree registration.
- Main HEAD remained
  `d6c0501f5866644849423d12583ad4e8f4b0c696`.
- The before/after product-status snapshots were byte-identical and empty for
  `internal/`, `cmd/`, `go.mod`, and `go.sum`. No product or permanent test
  file was changed.

## Gate

**PASS for the QA artifact.** A2 and A3 each have deterministic runtime
falsification with exact anchored commands and preserved raw evidence.

**REVISE for product closure.** The resolver reload owner must prevent an older
generation from publishing over a newer one in both families, and the startup
seeder must return the exact committed supervisor-intent generation. The
same-modification-time invalidation defect and the previously proven nil-input
return-shape defect remain separate open product concerns.

## Terms and Abbreviations

- **A2 / A3** — finding identifiers in the adversarial architecture review.
- **HELD** — the proposed defect mechanism survived its falsifying test.
- **LSP** — Language Server Protocol.
- **MCP** — Model Context Protocol.
- **PASS** — the assigned QA evidence gate is complete and satisfied.
- **REVISE** — product behavior still needs correction before closure.
- **SHA-256** — Secure Hash Algorithm 256-bit evidence digest.
