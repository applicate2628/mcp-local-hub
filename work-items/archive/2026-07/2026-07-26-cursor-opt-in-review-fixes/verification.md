# Main-session integration verification

Refreshed for the final product revision on 2026-07-26.

## Fresh final-revision command evidence

| Command | Result |
| --- | --- |
| `go generate ./internal/gui/...` | PASS, exit 0; `tsc --noEmit` and Vite 8.0.16 production build completed, 269 modules transformed; emitted `index.html` 0.47 kB, `style.css` 120.96 kB, and `app.js` 584.89 kB; one non-failing chunk-size warning. |
| `git diff --name-only -- internal/gui/assets` | PASS, exit 0, no output; the required regeneration produced no generated-asset diff because the three frontend-source changes are comments only. |
| `go test -count=1 ./internal/clients -run '^TestDefaultInstallClientsExcludeOptInHeavyClients$'` | PASS, exit 0, `ok mcp-local-hub/internal/clients 0.049s`. |
| `go test -count=1 ./internal/api -run "^(TestDefaultClientBindings_DerivedFromDefaultInstallSet\|TestRegister_CleanupSkipsClientsThatGotNoReplacement)$"` | PASS, exit 0, `ok mcp-local-hub/internal/api 0.048s`; covers both the registry-derived fallback and the effective-binding cleanup boundary. |
| `go test -count=1 ./internal/gui -run '^TestClientInstallPrefs_GetDefault$'` | PASS, exit 0, `ok mcp-local-hub/internal/gui 0.041s`. |
| `go test -count=1 ./internal/cli -run '^TestUpgrade(InstallServer_PassesNoClientWriteOpts\|NoClientWriteSentinel_SelectsZeroClients)$'` | PASS, exit 0, `ok mcp-local-hub/internal/cli 0.078s`. |
| `go build ./...` | PASS, exit 0, no output. |
| `go vet ./...` | PASS, exit 0, no output. |
| `git diff --check` | PASS, exit 0, no output. |

No unscoped `go test ./...` was run. No `mcphub` command, scheduler action, client-config action, or live-fleet process action was run. One invalid external-review wrapper launched by this session was later stopped by its exact process identifier tree before relaunch; no image-name kill was used.

## Sweep and diff review

- The review/sweep correction loop found and closed:
  - two stale legacy-fixture comments;
  - stale default/current-state statements across four planning/specification documents;
  - supported-client inventory drift (46/44 and missing `mimocode`) corrected to 47/2/45;
  - the invalid singular install flag in `docs/supported-clients.md`;
  - six live frontend 46-client count comments;
  - the invalid singular install flag in `internal/api/install_hub_reconcile.go`;
  - the brittle `all 46 adapters` comment in `internal/clients/project_scope.go`;
  - two authoritative roadmap lines that still called shipped §9 open.
- The complete branch-to-worktree product diff relative to `origin/fix/cursor-not-default-install` contains exactly these 15 files:
  - `docs/supported-clients.md`
  - `docs/superpowers/plans/2026-05-04-ravitemer-mcp-hub-adoption-proposals.md`
  - `docs/superpowers/plans/2026-05-05-g1-readme-readiness-matrix.md`
  - `docs/superpowers/specs/2026-05-05-g1-readme-readiness-matrix-design.md`
  - `docs/superpowers/specs/2026-06-10-clean-architecture-redesign.md`
  - `internal/api/install_hub_reconcile.go`
  - `internal/api/register.go`
  - `internal/api/register_test.go`
  - `internal/clients/project_scope.go`
  - `internal/cli/install_legacy_upgrade_classification_test.go`
  - `internal/cli/setup.go`
  - `internal/gui/client_install_prefs_test.go`
  - `internal/gui/frontend/src/lib/matrix-columns.test.ts`
  - `internal/gui/frontend/src/lib/routing.test.ts`
  - `internal/gui/frontend/src/screens/Servers.tsx`
- The raw bytes from `git diff --binary origin/fix/cursor-not-default-install -- <the fifteen paths above>` have plain SHA-1 `8568f0bbf84adabb3c21266819e78c49552ae9d6` and length 48,364 bytes.
- The requested primary release/help surfaces (`README.md`, `INSTALL.md`, `servers/serena/README.md`, `internal/cli/register.go`, and `SectionClients.tsx`) were already correct at the starting revision and remain unchanged.
- The only preserved stale three-default snapshot is the self-closed historical Phase 3B-II record at `docs/superpowers/plans/phase-3b-ii-backlog.md:267-273`.

## Diff-invisible invariant results

| Invariant | Result |
| --- | --- |
| Registry default set remains exactly `claude-code,codex-cli`; Cursor remains opt-in. | Verified by `clientDescriptor.defaultInstall` rows and the scoped clients test. |
| Register fallback remains derived from `DefaultInstallClientNames()` and excludes Cursor. | Verified by `buildDefaultClientBindings()` and the scoped API test. |
| GUI nominal no-override snapshot reports Cursor as neither compile-default nor selected. | Verified by the scoped GUI test. |
| Explicit three-client legacy-upgrade fixture remains explicit and executable behavior is unchanged. | Verified by the scoped CLI tests and comment-only fixture diff. |
| Supported-client inventory matches the registry: 47 total, 2 default, 45 opt-in, with `mimocode` present once. | Verified by direct registry enumeration and the exhaustive sweep. |
| Frontend generated bundle is synchronized. | Verified by the required generator exit 0 and the absence of any generated-asset diff. |

Gate: PASS
