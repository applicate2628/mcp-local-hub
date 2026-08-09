# Backend implementation: Cursor explicit opt-in derivatives

## Scope result

Implemented the three initially approved stale derivatives plus the one Lead-re-admitted Phase C derivative. Runtime default membership, Register fallback construction, API and persisted preference behavior, frontend source, and generated assets are unchanged.

| Changed file | Change | Evidence |
| --- | --- | --- |
| `internal/api/register.go` | Replaced the stale Gemini default claim with the registry-derived `claude-code` and `codex-cli` default set; Cursor is explicitly bound only. | `internal/api/register.go:135-140` |
| `internal/cli/setup.go` | Corrected setup help from a three-client default to `claude-code,codex-cli` and made Cursor explicit opt-in through `--clients cursor`. | `internal/cli/setup.go:318-325` |
| `internal/gui/client_install_prefs_test.go` | Made Cursor non-compile-default and unselected in the nominal no-override snapshot; asserted that result. | `internal/gui/client_install_prefs_test.go:35-43`, `:56-95` |
| `internal/cli/install_legacy_upgrade_classification_test.go` | Reclassified both descriptions of the legacy-upgrade fixture's three bindings as explicit test inputs used to exercise zero-client-write behavior; fixture values and executable code are unchanged. | `internal/cli/install_legacy_upgrade_classification_test.go:118-120`, `:181-185` |

No HTTP, command-line, manifest, persistence, or API wire surface changed; a wire before/after is therefore not applicable.

## Verification

| Named regression guard | Expected | Actual | Result |
| --- | --- | --- | --- |
| `go test ./internal/clients -run '^TestDefaultInstallClientsExcludeOptInHeavyClients$' -count=1` | One named test passes; defaults remain `claude-code,codex-cli`, excluding Cursor. | Exit 0: `ok mcp-local-hub/internal/clients 0.019s` | PASS |
| `go test ./internal/api -run '^TestDefaultClientBindings_DerivedFromDefaultInstallSet$' -count=1` | One named test passes; fallback equals the registry default set minus relay-stdio and excludes Cursor. | Exit 0: `ok mcp-local-hub/internal/api 0.023s` | PASS |
| `go test ./internal/gui -run '^TestClientInstallPrefs_GetDefault$' -count=1` | One named test passes; nominal Cursor is not compile-default and not selected. | Exit 0: `ok mcp-local-hub/internal/gui 0.033s` | PASS |
| `go test ./internal/cli -run '^TestUpgrade(InstallServer_PassesNoClientWriteOpts|NoClientWriteSentinel_SelectsZeroClients)$' -count=1` | Both named legacy-upgrade tests pass; explicit fixture bindings still exercise zero-client-write behavior. | Exit 0: `ok mcp-local-hub/internal/cli 0.024s` | PASS |
| Scoped `git diff --check` | No whitespace error. | Exit 0, no output. | PASS |
| Initial Phase A+B scoped `git diff --name-only` | Only the three initially approved product paths. | `internal/api/register.go`; `internal/cli/setup.go`; `internal/gui/client_install_prefs_test.go`. | PASS |

`go build ./...` and `go vet ./...` were intentionally not run: the assigned phase permits only the three exact narrow tests; integration owns those later checks.

## Claims and receiving-side echo

| Diff-invisible invariant | Status | Evidence |
| --- | --- | --- |
| Register fallback remains `DefaultInstallClientNames` minus relay-stdio and excludes Cursor. | Verified | Named API guard passed; implementation remains the direct reader at `internal/api/register.go:55-64`. |
| Registry set remains exactly `claude-code,codex-cli`; Cursor is opt-in. | Verified | Named clients guard passed; owner remains `internal/clients/clients.go:865-872`, `:957-964`. |
| GUI nominal default has exactly one Cursor row with `CompileDefault: false` and `Selected: false`. | Verified | Named GUI guard passed; fixture/expectation at `internal/gui/client_install_prefs_test.go:35-43`, `:56-95`. |
| The legacy-upgrade fixture's three bindings are explicit test inputs, not a restatement of default-install membership; zero-client-write behavior is preserved. | Verified | Both descriptions are corrected at `internal/cli/install_legacy_upgrade_classification_test.go:118-120`, `:181-185`; the exact two-test CLI guard passed. |
| No frontend-source or generated-asset path changed. | Verified | Scoped product diff names no `internal/gui/frontend/` or `internal/gui/assets/` path. |

| Defect-class participant | Classification | Evidence |
| --- | --- | --- |
| Register explanatory comment | Fixed stale derivative; not a runtime-policy owner. | `internal/api/register.go:135-140` |
| Setup CLI help text | Fixed stale user-facing derivative; not a runtime-policy owner. | `internal/cli/setup.go:318-325` |
| GUI nominal-response fixture/expectation | Fixed stale test derivative; not a runtime-policy owner. | `internal/gui/client_install_prefs_test.go:35-43`, `:56-95` |
| Legacy-upgrade fixture comments | Fixed stale explanatory derivatives; the three bindings remain explicit fixture data, not a runtime-policy owner. | `internal/cli/install_legacy_upgrade_classification_test.go:118-120`, `:181-185` |
| Runtime default list | Not affected; no additional runtime list was introduced. | Product changes are comments plus the already-approved GUI test fixture; existing owner remains `clientDescriptor.defaultInstall` at `internal/clients/clients.go:831-838`, `:865-872`. |

| Lens | Primary object examined | Adjacent object classes re-aimed at | Decision facts proved | Result + evidence |
| --- | --- | --- | --- | --- |
| C1 default-install membership | `clientDescriptor.defaultInstall` registry metadata | Register fallback; explanatory comments; GUI and legacy-upgrade test derivatives | Membership stays in the registry, with its ordered derived accessor as the sole runtime read path; explicit test bindings do not restate membership. | Verified by `internal/clients/clients.go:865-872`, `:957-964`; API and re-admitted CLI guards passed. |

## Risks and unknowns

- The knowledge-archivist must repeat/complete the exhaustive live-tree sweep after the external-review correction at line 119; this implementation does not claim that final sweep is complete.
- Build and vet remain unexecuted in this phase by the explicit command boundary above.
- No runtime execution, installation, registration, setup, process management, or generated-asset operation was performed.

## Handoff

Recommended next role: `$knowledge-archivist` to repeat and complete the accepted Phase C exhaustive live-tree sweep. The receiver should accept this package only if the four recorded test commands and updated invariant echo are sufficient for the sweep handoff.

Gate: PASS
