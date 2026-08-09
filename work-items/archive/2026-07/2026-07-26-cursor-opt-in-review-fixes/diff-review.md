# Final diff review: Cursor opt-in consistency — revision 2

Revision 2 re-read the complete nine-file product diff after correction of the
revision-1 documentation finding.

## Files & symbols

- `static-read` — The compile-time default-install policy owner is `clientDescriptor.defaultInstall` in the canonical `clientRegistry()` table. Only `claude-code` and `codex-cli` set the flag; `cursor` does not. `DefaultInstallClientNames()` derives its result from those rows (`internal/clients/clients.go:824-838`, `internal/clients/clients.go:865-872`, `internal/clients/clients.go:952-964`).
- `runtime-verified` — A current-session descriptor count over `internal/clients/clients.go:865-937` found 47 registry rows, 2 with `defaultInstall: true`, and an explicit `mimocode` row. This supports the changed 47 / 2 / 45 counts and the added `mimocode` list entry in `docs/supported-clients.md:3-15`.
- `static-read` — The register fallback is derived by `buildDefaultClientBindings()` from `clients.DefaultInstallClientNames()`, filters relay-stdio adapters, and is consumed by both unsupervised and supervised empty-`client_bindings` paths (`internal/api/register.go:38-64`, `internal/api/register.go:789-792`, `internal/api/register_supervisor.go:268-270`).

### Object-axis record

| Axis | Verified object |
|---|---|
| Policy owner | `clientDescriptor.defaultInstall` rows in `clientRegistry()` |
| Derived accessor | `clients.DefaultInstallClientNames()` |
| Register projection | `buildDefaultClientBindings()` / `defaultClientBindings` |
| Empty-manifest consumers | `registerOneLanguage` and `registerOneLanguageSupervised` |
| User-facing baseline | 47 supported, 2 default-install, 45 opt-in; Cursor explicit opt-in |
| Falsifying probe | Add/remove `defaultInstall` on one descriptor and verify the derived accessor plus both register fallback consumers change together |

## Flows

- `static-read` — A manifest with explicit `client_bindings` retains those bindings. An empty list falls back to the registry-derived `defaultClientBindings`; both register implementations use the same value (`internal/api/register.go:789-799`, `internal/api/register_supervisor.go:268-278`).
- `static-read` — `setup --server` has a `--server` flag but no install-client selection flag; production routes through `runSetupInstallServer`, which calls the ordinary install path with default selection (`internal/cli/setup.go:439-449`, `internal/cli/setup.go:518-541`). Its revised help therefore correctly directs Cursor selection to a separate `mcphub install --server <name> --clients cursor` command (`internal/cli/setup.go:318-327`).
- `static-read` — The install command owns the plural `--clients` flag (`internal/cli/install.go:245-248`). No singular install `--client` alias is defined there.

## Contracts

- `static-read` — The changed register comment now matches the derived two-client fallback and states that Cursor requires an explicit manifest binding (`internal/api/register.go:135-141`).
- `static-read` — The two readiness-matrix documents and the adoption proposal now classify Claude Code and Codex CLI as default, and Cursor as opt-in (`docs/superpowers/plans/2026-05-04-ravitemer-mcp-hub-adoption-proposals.md:43-47`, `docs/superpowers/plans/2026-05-05-g1-readme-readiness-matrix.md:82-84`, `docs/superpowers/specs/2026-05-05-g1-readme-readiness-matrix-design.md:55-57`).
- `static-read` — The architecture-spec changes accurately identify `clientRegistry()` as the build-wide supported/default owner, distinguish the intentional seven-client Serena legacy reconciliation boundary, and state the verified 47 / 2 / 45 inventory (`docs/superpowers/specs/2026-06-10-clean-architecture-redesign.md:147-191`, `internal/api/serena_client_reconcile.go:139-168`).
- `static-read` — `docs/supported-clients.md:3-15` now states 47 supported, 2 default-install, and 45 opt-in clients, includes `mimocode`, and uses the real plural install selector `mcphub install --server X --clients <name>`. The command definition is the plural `--clients` flag (`internal/cli/install.go:245-248`).

## Tests & coverage

- `static-read` — The legacy-upgrade fixture comments now distinguish three explicit manifest bindings from the two default-install clients; the fixture still includes Cursor as an explicit binding, preserving the no-client-write negative-control breadth without calling it a default (`internal/cli/install_legacy_upgrade_classification_test.go:116-123`, `internal/cli/install_legacy_upgrade_classification_test.go:179-197`).
- `static-read` — The GUI preference fixture now marks Cursor `CompileDefault: false`, omits it from the default selected map, and asserts that it is neither compile-default nor selected (`internal/gui/client_install_prefs_test.go:35-43`, `internal/gui/client_install_prefs_test.go:56-61`, `internal/gui/client_install_prefs_test.go:82-95`).
- `runtime-verified` — `git diff --check` over the exact nine-file review set returned exit code 0.
- `ASSUMPTION (UNVERIFIED)` — No build, test, generation, npm, or mcphub command was run in this read-only review. The parent verification stage owns runtime gates.

## Similar implementations

- `static-read` — The install command already presents the correct plural client-selection surface and two-client default in help and flag metadata (`internal/cli/install.go:54-62`, `internal/cli/install.go:245-248`). This is the direct contract against which the changed setup and supported-clients prose were checked.
- `static-read` — `serenaReconcileClientSet()` is a separate migration-boundary inventory, not a competing build-wide default owner (`internal/api/serena_client_reconcile.go:139-168`, `internal/api/serena_client_reconcile.go:487-500`).

## Constraints

- `runtime-verified` — Review scope was exactly the nine files named by the Lead plus read-only owner/contract anchors in `internal/clients/clients.go`, `internal/api/register_supervisor.go`, `internal/api/serena_client_reconcile.go`, and `internal/cli/install.go`.
- No product file was edited by this reviewer. No runtime path, process operation, build, test, generator, npm command, or mcphub command was executed.
- Codegraph was not initialized because its connected-server policy reserves indexing to the operator.

## Change risks

- `static-read` — The nine-file diff contains no production behavior-logic change: `internal/api/register.go` changes only a comment; `internal/cli/setup.go` changes only Cobra help text; the remaining Go changes update test fixtures/assertions; all other changes are documentation.
- `static-read` — The revision-1 singular-flag defect is closed: `docs/supported-clients.md:6` now uses `--clients`, matching `internal/cli/install.go:247`.
- `static-read` — The large architecture-spec rewrite is internally aligned on the registry owner and verified counts, but it increases citation-drift exposure because it embeds many line anchors. That is a maintenance risk, not a defect in the reviewed Cursor claims.

## Unresolved questions

- No unresolved product defect remains in the nine-file review scope.
- Runtime verification remains outside this review by explicit instruction.

## Research admission gates

- **Regression risk:** `static-read` — The product diff is comments/help only; test fixtures are tightened to the current registry contract. No unrelated runtime behavior is changed.
- **Metric alignment:** `static-read` — The reviewed claims are evaluated against the actual registry descriptors, derived accessor, register fallback, and CLI flag definitions.
- **Known limits:** `ASSUMPTION (UNVERIFIED)` — Static review cannot prove test/build success; the parent verification stage must use the approved safe commands.
- **Bounded falsification:** `static-read` — The narrow falsifier is the existing default-binding test plus the setup/help and fixture tests; execution is intentionally deferred to the parent gate.

## Adjacent findings

- None.

### Searched and excluded

- Reviewed end to end: only the nine named diff files.
- Checked as source-of-truth anchors only: registry owner/accessor, supervised register consumer, Serena legacy boundary, and install flag definition.
- Excluded by instruction: all other product files, historical discovery, generated/minified assets, builds, tests, generators, npm, mcphub execution, and live-process surfaces.

Gate: PASS
