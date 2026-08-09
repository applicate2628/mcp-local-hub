# Delivery plan: Cursor explicit opt-in review fixes

Date: 2026-07-26
Owner: `$planner`
Accepted inputs: `brief.md`, `research.md`, and `design.md` in this work item.

## 1. Scope, ownership, and non-negotiable constraints

This plan implements only the architect's Change-Surface Contract. The runtime membership owner remains `clientDescriptor.defaultInstall` in `clientRegistry()`; `clients.DefaultInstallClientNames()` remains the ordered derived read API; `buildDefaultClientBindings()` remains its Register-specific consumer. The admitted correction is two explanatory Go comments and one GUI Go-test fixture/nominal response. No executable policy, wire shape, persisted preference, scheduler, or process-lifecycle behavior is to change.

The following surfaces are protected in every phase: registry membership and order; `DefaultInstallClientNames`; `buildDefaultClientBindings`; ordinary and supervised Register fallback; install planner/default override behavior; client preference API, persistence, and wire shape; scheduler/live fleet; `internal/cli/register.go`; `internal/gui/client_install_prefs.go`; `internal/gui/frontend/src/components/settings/SectionClients.tsx`; `README.md`; `INSTALL.md`; `docs/supported-clients.md`; `docs/cli-reference.md`; and `internal/gui/assets/` unless a separately admitted frontend-source change makes regeneration necessary.

Hard execution constraints:

- Never run `go test ./...`; never start `mcphub`; never install/register/setup/supervise it; never kill or otherwise manage a process.
- `go build ./...` and `go vet ./...` are mandatory integration checks.
- Every Go test below uses its exact package and narrow `-run` pattern with `-count=1`; no broad `internal/api` or `internal/gui` test command is allowed.
- Frontend generation, typecheck, and test are inactive unless an actual frontend-source change is first proven and re-admitted. Generated assets are never hand-edited.
- Use `gofmt` only if a Go code structure changes. This plan changes comments and a test literal only, so no `gofmt` command is scheduled.
- No push, release, pull-request creation, or pull-request mutation is allowed.

## 2. Ordered delivery phases

### Phase A — backend/CLI explanatory corrections and narrow parity tests

Owner and integration owner: `$backend-engineer`.
Dependency: accepted `design.md`; no prior implementation phase.
Allowed files: exactly `internal/api/register.go` at the confirmed stale explanatory comment and `internal/cli/setup.go` at the confirmed stale setup-help comment. No Go test source is authorized to change in this phase; the named existing tests are execution evidence.

Must not break: the protected surfaces in section 1, especially `clients.DefaultInstallClientNames`, `buildDefaultClientBindings`, ordinary/supervised Register fallback, the install planner, client flags/manifests, and every external behavior. The comments must describe today's registry-derived two-client default set and keep Cursor explicit opt-in; they must not become a second policy list.

Diff-invisible invariants and named regression guards copied from the accepted design:

| Invariant | Named regression guard | Expected result |
| --- | --- | --- |
| The default binding construction happens at package initialization but its membership must still track the registry accessor. | `register-default-registry-parity`: `go test ./internal/api -run '^TestDefaultClientBindings_DerivedFromDefaultInstallSet$'` | Passes; binding names equal `DefaultInstallClientNames` minus relay-stdio and exclude Cursor. |
| Registry order and membership are shared by install, register, CLI/docs, and tests; a text-only correction must not mutate them. | `registry-default-membership`: `go test ./internal/clients -run '^TestDefaultInstallClientsExcludeOptInHeavyClients$'` | Passes with exactly `claude-code,codex-cli`; Cursor remains supported and absent from defaults. |
| A GUI test stub can bypass production API computation, so its nominal snapshot must still respect the canonical compile-default semantics. | `gui-default-snapshot-cursor-opt-in`: `go test ./internal/gui -run '^TestClientInstallPrefs_GetDefault$'` | Deferred to Phase B; no GUI file is touched in this phase. |
| Generated GUI assets are source derivatives, not editable policy owners. | `frontend-asset-conditional`: `git diff --name-only` followed by conditional `go generate ./internal/gui/...` only when frontend source changed. | No frontend source change means no asset change; this phase must leave both frontend source and assets unchanged. |

Acceptance criteria (append-only for Phase A):

- **A-AC1:** `internal/api/register.go` makes no claim that Gemini or Cursor is part of the bare/default Register binding set; its explanatory text names the registry-derived defaults `claude-code,codex-cli` and Cursor only as explicit opt-in where applicable.
- **A-AC2:** `internal/cli/setup.go` makes no claim that Cursor is installed by default; its explanatory text agrees with the registry-derived defaults `claude-code,codex-cli`.
- **A-AC3:** Both exact commands below exit 0 with one passing named test and no unrelated package test selection:

```powershell
go test ./internal/clients -run '^TestDefaultInstallClientsExcludeOptInHeavyClients$' -count=1
go test ./internal/api -run '^TestDefaultClientBindings_DerivedFromDefaultInstallSet$' -count=1
```

- **A-AC4:** The Phase A diff contains only the two authorized explanatory comment edits; it does not touch a protected runtime, test, asset, documentation, or configuration surface.

Completion evidence: read the edited comments and `git diff -- internal/api/register.go internal/cli/setup.go`; record the two zero exit codes and `PASS` lines from the exact tests. Revert grouping: standalone reversible commit, because no runtime, contract, generated output, or persisted-state change is introduced; reverting it restores only the prior explanatory text.

### Phase B — GUI test-fixture correction; frontend branch remains inactive

Owner and integration owner: `$backend-engineer`; `internal/gui/client_install_prefs_test.go` is a Go HTTP-handler/API test fixture, not TypeScript/React frontend implementation.
Dependency: Phase A accepted.
Allowed file: exactly `internal/gui/client_install_prefs_test.go` at the nominal no-override fixture/default-response expectation. Frontend source and `internal/gui/assets/` are explicitly not allowed in the normal path.

Must not break: `internal/gui/client_install_prefs.go`, its HTTP response fields and persisted preference semantics, registry membership/order, and the frontend source/assets named in section 1. The fixture must model—not replace—the production registry-derived API view: Cursor `CompileDefault: false` and `Selected: false` in the nominal no-override response.

Diff-invisible invariants and named regression guards copied from the accepted design:

| Invariant | Named regression guard | Expected result |
| --- | --- | --- |
| A GUI test stub can bypass production API computation, so its nominal snapshot must still respect the canonical compile-default semantics. | `gui-default-snapshot-cursor-opt-in`: `go test ./internal/gui -run '^TestClientInstallPrefs_GetDefault$'` | Passes with Cursor `CompileDefault=false` and `Selected=false` in the no-override response. |
| The default binding construction happens at package initialization but its membership must still track the registry accessor. | `register-default-registry-parity`: `go test ./internal/api -run '^TestDefaultClientBindings_DerivedFromDefaultInstallSet$'` | No `internal/api` source change is allowed; Phase D reruns the guard. |
| Registry order and membership are shared by install, register, CLI/docs, and tests; a text-only correction must not mutate them. | `registry-default-membership`: `go test ./internal/clients -run '^TestDefaultInstallClientsExcludeOptInHeavyClients$'` | No registry change is allowed; Phase D reruns the guard. |
| Generated GUI assets are source derivatives, not editable policy owners. | `frontend-asset-conditional`: `git diff --name-only` followed by conditional `go generate ./internal/gui/...` only when frontend source changed. | No frontend source change means no asset change; a frontend source change has matching regenerated assets and frontend checks. |

Acceptance criteria (append-only for Phase B):

- **B-AC1:** The test's nominal no-override response has exactly one Cursor descriptor with `CompileDefault: false` and `Selected: false`.
- **B-AC2:** The exact command below exits 0 with one passing named test:

```powershell
go test ./internal/gui -run '^TestClientInstallPrefs_GetDefault$' -count=1
```

- **B-AC3:** `git diff --name-only` contains no path under `internal/gui/frontend/` or `internal/gui/assets/` after this phase.
- **B-AC4:** If a current-tree contradiction is alleged to require frontend-source change, stop this phase and send the concrete `path:line` contradiction to `$lead` for re-admission. Do not run `go generate`, frontend typecheck, or frontend tests and do not edit source/assets before that re-admission.

Conditional branch, inactive unless B-AC4 is re-admitted: allowed files must be named by the re-admission; then and only then run, from `internal/gui/frontend`, `npm run typecheck` and `npm test`, run `go generate ./internal/gui/...` from repository root, and directly read `git diff -- internal/gui/frontend internal/gui/assets`. Expected evidence is all three commands exit 0 and the generated asset diff is the direct derivative of the approved source diff. Generated assets remain forbidden from hand editing.

Completion evidence: inspect `git diff -- internal/gui/client_install_prefs_test.go`, the test output, and the name-only diff. Revert grouping: standalone reversible commit, provided B-AC3 holds. If the conditional branch is re-admitted, its source and generated assets are one atomic revert-group; the fixture correction remains separately revertible only if it does not depend on that source change.

### Phase C — user-facing exhaustive sweep and only newly proven stale live derivatives

Owner: `$knowledge-archivist`.
Dependency: Phases A and B accepted.
Allowed files at plan issue: none. `README.md`, `INSTALL.md`, `docs/supported-clients.md`, and `docs/cli-reference.md` are presently classified correct and stay untouched. A newly proven live explanatory derivative needs a concrete `path:line`, its classification, and `$lead` re-admission before its owning file is added to this phase; no runtime, test-fixture, frontend-source, or generated-asset path can be admitted through this documentation phase.

Must not break: every protected surface in section 1; in particular, no historical/provenance text is to be rewritten as if it were a live policy, no explicit `--clients cursor` example may be misclassified as a default, and no generated/minified output may become a policy owner.

Diff-invisible invariants and named regression guards copied from the accepted design:

| Invariant | Named regression guard | Expected result |
| --- | --- | --- |
| The default binding construction happens at package initialization but its membership must still track the registry accessor. | `register-default-registry-parity`: `go test ./internal/api -run '^TestDefaultClientBindings_DerivedFromDefaultInstallSet$'` | No Register implementation change is allowed; Phase D verifies the guard. |
| A GUI test stub can bypass production API computation, so its nominal snapshot must still respect the canonical compile-default semantics. | `gui-default-snapshot-cursor-opt-in`: `go test ./internal/gui -run '^TestClientInstallPrefs_GetDefault$'` | No GUI fixture/source change is allowed; Phase D verifies the guard. |
| Registry order and membership are shared by install, register, CLI/docs, and tests; a text-only correction must not mutate them. | `registry-default-membership`: `go test ./internal/clients -run '^TestDefaultInstallClientsExcludeOptInHeavyClients$'` | No registry change is allowed; Phase D verifies the guard. |
| Generated GUI assets are source derivatives, not editable policy owners. | `frontend-asset-conditional`: `git diff --name-only` followed by conditional `go generate ./internal/gui/...` only when frontend source changed. | This phase must leave frontend source/assets unchanged unless a separately re-admitted frontend change exists. |

Acceptance criteria (append-only for Phase C):

- **C-AC1:** The exact stale-only guard returns no output and exit code 0:

```powershell
git grep -n -I -E 'default clients claude-code,codex-cli,cursor|gemini-cli by default|Name: "cursor", CompileDefault: true' -- ':!work-items/archive/**' ':!docs/phase-*-verification.md' ':!docs/superpowers/**'
```

- **C-AC2:** Run the following exhaustive tracked-live inventory. It covers Cursor, default clients/counts, install-by-default language, and `client_bindings` across Go, TypeScript/TSX, Markdown, tests, fixtures, and goldens. It excludes only declared provenance (`work-items/archive/**`, `.plans/**`, `.reports/**`, `docs/phase-*-verification.md`, `docs/superpowers/**`) and generated-minified surfaces (`*.min.js`, `*.min.css`). Non-minified `internal/gui/assets/app.js` remains in scope as a generated derivative.

```powershell
git grep -n -I -i -E 'cursor|default([ -]?(client|install)|s)?|install(ed)? by default|client_bindings|claude-code|codex-cli|[0-9]+[[:space:]]+(default|default-install)' -- '*.go' '*.ts' '*.tsx' '*.md' '*fixture*' '*golden*' ':!work-items/archive/**' ':!.plans/**' ':!.reports/**' ':!docs/phase-*-verification.md' ':!docs/superpowers/**' ':!*.min.js' ':!*.min.css'
```

- **C-AC3:** Run the matching working-tree inventory (including an untracked live derivative, if any) and compare it with C-AC2:

```powershell
rg -n -I -i --glob '*.go' --glob '*.ts' --glob '*.tsx' --glob '*.md' --glob '*fixture*' --glob '*golden*' --glob '!work-items/archive/**' --glob '!.plans/**' --glob '!.reports/**' --glob '!docs/phase-*-verification.md' --glob '!docs/superpowers/**' --glob '!*.min.js' --glob '!*.min.css' 'cursor|default([ -]?(client|install)|s)?|install(ed)? by default|client_bindings|claude-code|codex-cli|[0-9]+[[:space:]]+(default|default-install)' .
```

- **C-AC4:** Classify every C-AC2/C-AC3 hit in the review record as exactly one of: canonical owner/consumer; correct current default statement; explicit opt-in selection; correct negative assertion; test/fixture/golden expectation; generated non-minified derivative; provenance; or newly proven stale live derivative. An unclassified live hit is a failed phase. A newly proven stale live derivative stops for lead re-admission; after admission, change only that named derivative and repeat C-AC1 through C-AC3.
- **C-AC5:** With no re-admitted stale live derivative, the documentation/user-facing product diff is empty; existing current documents remain untouched.

Completion evidence: preserve the classified inventory in the QA/review evidence, not as raw unredacted logs; direct-read any changed derivative. Revert grouping: no-op if no re-admission. A re-admitted explanatory derivative is standalone reversible with its own focused commit and repeat sweep; if it is a source derivative that requires generated output, it must use that source/generated atomic group instead of this phase.

### Phase D — backend integration and safe local verification

Owner and integration owner: `$backend-engineer`.
Dependency: Phases A, B, and C accepted (or any re-admitted narrow derivative accepted).
Allowed product files: none; this phase integrates the accepted edits without redesigning or expanding the surface. Evidence writes, if required by the lead, are limited to the work item and session report locations.

Must not break: all protected surfaces in section 1; all Go behavior remains unchanged except the corrected explanatory copies and test fixture. Do not run `gofmt` unless the actual diff contains a Go structural change; if it does, stop and diagnose the scope expansion rather than formatting broadly.

Diff-invisible invariants and named regression guards copied from the accepted design:

| Invariant | Named regression guard | Expected result |
| --- | --- | --- |
| The default binding construction happens at package initialization but its membership must still track the registry accessor. | `register-default-registry-parity`: `go test ./internal/api -run '^TestDefaultClientBindings_DerivedFromDefaultInstallSet$'` | Passes; binding names equal `DefaultInstallClientNames` minus relay-stdio and exclude Cursor. |
| A GUI test stub can bypass production API computation, so its nominal snapshot must still respect the canonical compile-default semantics. | `gui-default-snapshot-cursor-opt-in`: `go test ./internal/gui -run '^TestClientInstallPrefs_GetDefault$'` | Passes with Cursor `CompileDefault=false` and `Selected=false` in the no-override response. |
| Registry order and membership are shared by install, register, CLI/docs, and tests; a text-only correction must not mutate them. | `registry-default-membership`: `go test ./internal/clients -run '^TestDefaultInstallClientsExcludeOptInHeavyClients$'` | Passes with exactly `claude-code,codex-cli`; Cursor remains supported and absent from defaults. |
| Generated GUI assets are source derivatives, not editable policy owners. | `frontend-asset-conditional`: `git diff --name-only` followed by conditional `go generate ./internal/gui/...` only when frontend source changed. | No frontend source change means no asset change; a frontend source change has matching regenerated assets and frontend checks. |

Acceptance criteria (append-only for Phase D):

- **D-AC1:** Each exact scoped command exits 0 with its single named test passing:

```powershell
go test ./internal/clients -run '^TestDefaultInstallClientsExcludeOptInHeavyClients$' -count=1
go test ./internal/api -run '^TestDefaultClientBindings_DerivedFromDefaultInstallSet$' -count=1
go test ./internal/gui -run '^TestClientInstallPrefs_GetDefault$' -count=1
```

- **D-AC2:** The required safe commands each exit 0:

```powershell
go build ./...
go vet ./...
```

- **D-AC3:** The final `git diff --name-only` contains only Phase A/B files and any specifically re-admitted Phase C derivative; it contains no registry, runtime fallback, API/persistence, scheduler, frontend-source, or generated-asset change unless separately re-admitted under the conditional branch.
- **D-AC4:** `git diff --check` exits 0, followed by an end-to-end human read of `git diff --` that confirms comments/fixture semantics, exact scope, and absence of unintended behavior changes.
- **D-AC5:** If frontend source changed through a re-admitted branch, its generator/typecheck/test and direct generated-diff inspection from Phase B all have fresh zero-exit evidence; otherwise the recorded condition is “inactive: no frontend-source change,” with no generator/frontend command executed.

Completion evidence: retain command status/output summary, the name-only diff, `git diff --check`, and end-to-end diff reading in the review evidence. Revert grouping: no new implementation commit; integration validates the standalone groups from A/B and any separately re-admitted C group. If a combined local commit is later used, it is still safely reversible as one text/fixture-only unit before push.

### Phase E — external QA and architecture review, local commit, closure/archive

Owners: `$qa-engineer` through the configured `$external-reviewer` route, then `$architecture-reviewer` through the configured `$external-reviewer` route; `$lead` performs commit and closure/archive only after both reviews accept the same final diff.
Dependency: Phase D fresh evidence.
Allowed product files: none. Allowed task-memory transitions: the work item's status/closure material and its move from `work-items/active/` to the configured `work-items/archive/YYYY-MM/` location only after all required gates and the local commit succeed.

Must not break: the reviewed diff must still obey every protected surface and hard constraint in section 1. External reviewers may not widen the implementation; a finding is either a bounded correction through the owning earlier phase or a returned `REVISE` to `$architect`/`$lead`.

Diff-invisible invariants and named regression guards copied from the accepted design:

| Invariant | Named regression guard | Expected result |
| --- | --- | --- |
| The default binding construction happens at package initialization but its membership must still track the registry accessor. | `register-default-registry-parity`: `go test ./internal/api -run '^TestDefaultClientBindings_DerivedFromDefaultInstallSet$'` | Review accepts fresh Phase D evidence showing registry parity and Cursor exclusion. |
| A GUI test stub can bypass production API computation, so its nominal snapshot must still respect the canonical compile-default semantics. | `gui-default-snapshot-cursor-opt-in`: `go test ./internal/gui -run '^TestClientInstallPrefs_GetDefault$'` | Review accepts fresh Phase D evidence showing Cursor `CompileDefault=false` and `Selected=false`. |
| Registry order and membership are shared by install, register, CLI/docs, and tests; a text-only correction must not mutate them. | `registry-default-membership`: `go test ./internal/clients -run '^TestDefaultInstallClientsExcludeOptInHeavyClients$'` | Review accepts fresh Phase D evidence showing exactly `claude-code,codex-cli`. |
| Generated GUI assets are source derivatives, not editable policy owners. | `frontend-asset-conditional`: `git diff --name-only` followed by conditional `go generate ./internal/gui/...` only when frontend source changed. | Review accepts either no frontend source/assets diff or the re-admitted source/generated evidence. |

Acceptance criteria (append-only for Phase E):

- **E-AC1:** External QA receives the final diff plus Phase D evidence and returns an explicit clean verdict; an errored, timed-out, or silent route is unverified, not clean.
- **E-AC2:** External architecture review independently verifies the single-owner seam, protected surfaces, classified live-tree sweep, and final diff, and returns an explicit clean verdict.
- **E-AC3:** After E-AC1/E-AC2 pass, create one focused local commit whose message names the duplicated-policy-drift root cause, `clientDescriptor.defaultInstall` as the single owner, and any remaining `ASSUMPTION (UNVERIFIED)` item. Do not push or perform any pull-request action.
- **E-AC4:** Before archive, reconcile original scope, the final diff, Phase D checks, both external verdicts, local-commit presence, no-push/no-PR status, and residual risk. Write closure material, move the item to the configured archive location, and update the recovery index/status. Any reviewer `REVISE` returns to the owning prior phase; no commit/archive occurs first.

Completion evidence: review artifacts tied to the same final diff, commit hash, closure/reconciliation record, archive move, and recovery-index update. Revert grouping: the focused local commit is reversible locally before any push; if a gate disproves its premise, use the local rollback path before closure rather than publishing corrective churn.

## 3. Verification matrix and receiving contract

The receiving role sequence is: `$backend-engineer` (Phase A) → `$backend-engineer` (Phase B) → `$knowledge-archivist` (Phase C) → `$backend-engineer` (Phase D integration) → `$qa-engineer` external review → `$architecture-reviewer` external review → `$lead` commit/closure/archive. A `$frontend-engineer` is involved only if future current-tree evidence is explicitly re-admitted for the conditional frontend-source branch.

| Evidence package | Required receiver decision |
| --- | --- |
| Phase A exact diff plus two narrow test outputs | Accept only if A-AC1 through A-AC4 are evidenced; otherwise return to `$backend-engineer`. |
| Phase B fixture diff, GUI test output, and name-only diff | Accept only if B-AC1 through B-AC4 hold; a frontend-source allegation requires re-admission, not an implementation shortcut. |
| Phase C stale-only output and classified dual inventory | Accept only if every live hit is classified; an unknown hit or stale derivative is `REVISE`/re-admission. |
| Phase D three scoped test outputs, build, vet, diff check, full diff read, and conditional frontend record | Accept only if D-AC1 through D-AC5 are fresh and exact; no substitute broad Go suite is valid. |
| Phase E same-diff external QA and architecture verdicts | Commit/close only on two explicit clean verdicts; timeout/error/silence remains unverified. |

## 4. Risks, stop conditions, and rollback

| Risk or stop condition | Required response |
| --- | --- |
| A command would start or manage `mcphub`, kill a process, or run `go test ./...` | Do not execute it; retain the safe command set in this plan. |
| A change would touch a protected runtime/persistence/scheduler/registry surface | Stop and return `REVISE` to `$architect`; do not normalize expanded scope. |
| A frontend-source or generated-asset change appears | Stop unless concrete current-tree evidence is re-admitted; then apply the conditional source/generator/typecheck/test/diff route as one atomic group. |
| A new live documentation derivative is found | Classify it, obtain lead re-admission, correct only that derivative, and repeat the entire Phase C sweep. |
| A named test/build/vet/sweep/review fails | Preserve the specific output, return to the phase owner, and re-run the same exact guard after the bounded correction. |
| A local commit premise is disproved before push | Reset/revert the local focused commit, repair through the owning phase, and do not publish intermediate churn. |

## 5. Explicit non-goals and handoff

This work item does not change default-install membership, default ordering, runtime fallback logic, CLI flags, manifests, HTTP response shapes, preferences, scheduler state, or the live installed fleet. It does not hand-edit generated assets, run broad Go tests, launch the application, or push/create/update a pull request. Current correct frontend and documentation statements are not cleanup opportunities.

Next action: dispatch Phase A to `$backend-engineer` with this plan, `design.md`, and the two exact allowed comment locations. The receiving engineer must treat all behavior claims as still requiring the exact Phase D evidence; this plan makes no unverified runtime correctness claim.

Gate: PASS
