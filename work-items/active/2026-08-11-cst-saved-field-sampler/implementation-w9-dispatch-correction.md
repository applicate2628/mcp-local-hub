# W9 Test Helper Dispatch Correction

Date: 2026-08-13

Execution role: `$backend-engineer`

Target: successor candidate `310ea13bb63a6d4b072a2617ca3acec756f1a2b6`, tree
`88095de688c88a016dcce88e7aca7d0b22270022`.

Security REVISE input:
`E27FA419E0CDDBE2A2346A046F473CAD25CBF7D47331A597E429684692B73328`.
Prior correction input:
`423A714B6921EEDF3A37AFF2709A1A7EEC515598BA356C31C955EF4C77E94717`.

## Receiving-side echo

- Correct only the verified compiled-test-binary bootstrap defect. Do not
  broaden CST production authority, routing, deployment, or enablement.
- Bind test-helper branches to exact selectors, sentinel values and required
  fields; reject partial, unknown and conflicting state before package setup or
  a success exit.
- Preserve the established production same-binary workers: their contract is
  exact positional argv plus bounded, strictly decoded stdin framing. Bare or
  malformed input remains nonzero; no ambient test sentinel is invented for a
  producer contract that does not own one.
- Give the production-shaped test child one explicit test-only marker and exact
  argv grammar. Raw or incomplete `route` and `supervise` remain nonzero.
- Replace only the seven scanner-reported synthetic workspace literals in their
  exact fixture owner with a repository-neutral synthetic value. Preserve
  equality and opaque-value semantics.
- Do not stage, commit, push, install, register, or mutate live CST, Service
  Control Manager, App Control, virtual hard disk, CiTool, or hardware security
  module state.

## Root cause and corrected invariant

The CLI and GUI package `TestMain` functions independently branched on ambient
environment variables or a bare positional token. Several branches therefore
ran before the package sandbox without verifying the helper selector and
required framing. The exact immutable binaries reproduced exit `0` for two
spoofed sentinels, raw/incomplete `route`, raw `supervise`, and two GUI blocking
sentinel variants.

Each package now has one pre-root classifier. A helper route is selected only
when exactly one known protocol is active and its complete tuple validates.
Unknown values, missing fields, wrong or duplicated selectors, selector-only
invocations, conflicting helpers, and raw/incomplete production-shaped argv
exit `3` before helper work, package setup, or `m.Run`. Legitimate selected-test
helpers still bypass package-root construction, and the exact positional
workers retain their bounded input decoders as the framing authority.

## Exact allowlist and hashes

| Path | SHA-256 | Purpose |
|---|---|---|
| `internal/cli/settings_registry_test.go` | `57EC5F3C0D9987F6F194C36D6A223B5DC085C6E042DFA5F94D034DD49ADF4A41` | Single CLI pre-root classifier and fail-closed dispatch. |
| `internal/cli/supervise_overlay_marker_spawn_test.go` | `F5B63E18554EB7A7F96F4160FC7C70EF22400447330D9DA366FBB92BBC3B92AD` | Explicit marker for the exact selectorless Serena-shaped env-dump child and repository-neutral workspace fixture. |
| `internal/cli/supervise_reconcile_wiring_test.go` | `0205FB5B9BDDBCF584153D71800D7D566ACCCE08A5DB4B7E9EE46812A47094A4` | Seven repository-neutral synthetic workspace fixtures. |
| `internal/cli/testmain_helpers_test.go` | `18E7FA2E1030AD6D8F877902F6325671DECF81FCEB2AB53CC08DCD9CD2A02BDA` | CLI adversarial compiled-binary matrix and explicit supervisor helper marker. |
| `internal/gui/main_test.go` | `72E5D3FF693E5160705653FC641DDB2D3A238F11005F368652DFF131E641223F` | Single GUI pre-root classifier and fail-closed dispatch. |
| `internal/gui/testmain_dispatch_test.go` | `10D5896AC6B72568CB5C9D26494E725C74190425A12245D11D4A59AAC6D53AF1` | GUI adversarial compiled-binary matrix. |

No non-test Go source changed. The only new environment variable,
`MCPHUB_TEST_PRODUCTION_ARGV_HELPER`, exists in `_test.go` and is used solely by
compiled test binaries.

## TDD evidence

The new tests were written before the dispatcher implementation and observed
RED. CLI failed all eight adversarial rows because each malformed child exited
`0`; GUI failed the three cases that could previously return `0`. The two bare
GUI positional-worker rows already returned nonzero through their bounded stdin
decoder and remain regression falsifiers.

After implementation:

| Verification | Fresh result |
|---|---|
| CLI adversarial matrix plus package-root bypass and overlay helper protocols | PASS; focused package command exit `0`, `ok`, 0.654 s. |
| Compiled CLI binary direct probes | PASS; sentinel-only, wrong selector, raw route, incomplete route and raw supervise each exited `3`. |
| GUI adversarial, blocking helper, terminal-worker and R6 receiver protocols | PASS; focused package command exit `0`, `ok`, 0.315 s. |
| Full `internal/gui` | PASS, 56.255 s. |
| `go vet ./internal/cli ./internal/gui` | PASS. |
| CST focused preservation | PASS, 51 tests across frontend, daemon, broker, worker and production composition. |
| Native runtime verifier | PASS; image SHA-256 remains `38D87C50F716E334F89628D4F35604534C3A35BDCF35F378F1D939137BAB89E3`. |
| Publication scanner | PASS individually for all six changed paths; the seven prior drive-rooted synthetic fixture literal findings are absent from their exact owner. |
| `git diff --check` | PASS. |
| CodeGraph | `status -> sync -> status` fresh; 2,118 files, `[OK] Index is up to date`; post-edit dispatcher/caller query completed. |

Full `internal/cli` is explicitly **UNVERIFIED** in this correction run. One
attempt exceeded the 240-second shell bound; a diagnostic run with Go's own
90-second timeout stopped in
`TestMarketplaceGenerate_HttpEntryEmitsRemoteHTTPDraft` during a TLS handshake.
That test is outside the changed helper/fixture paths and passed alone in
0.115 seconds. No `cli.test` process survived either timeout. Focused affected
CLI surfaces passed; the successor candidate still requires the planned fresh
immutable full regression gate.

## Contract and risk notes

No HTTP, database, queue, cache, remote procedure call, endpoint,
authorization, wire shape, status code, retry policy, or production process
route changed. No new dependency was added. The fixture replacement changes
only synthetic opaque values from a workstation-shaped drive-root path to
`testdata/workspaces/alpha`; assertions and owner behavior are unchanged.

Residual risk is the unverified full CLI package run above. The correction is
not an immutable candidate and cannot authorize W8-W10 advancement until the
Lead assembles a successor commit and fresh independent gates bind to it.

Temporary immutable archives and compiled binaries created for the reproduction
were removed from `R:\Temp`; they were regenerable. Pre-existing unrelated
scratch paths were not touched. The Git index remains empty. No commit or push
occurred.

## Terms and Abbreviations

- CLI: Command-line interface.
- CST: CST Studio Suite.
- GUI: Graphical user interface.
- TDD: Test-driven development.

Gate: PASS:correction
