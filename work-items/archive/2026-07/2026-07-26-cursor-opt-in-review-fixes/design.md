# Design: Cursor explicit opt-in single-owner correction

## 1. Decision and traceability

Adopt the existing registry as the only runtime default-install membership owner. `clientDescriptor.defaultInstall` in `clientRegistry()` owns membership; `clients.DefaultInstallClientNames()` is the ordered derived read API. This is already implemented: Cursor has no `defaultInstall` flag, while `claude-code` and `codex-cli` do (`internal/clients/clients.go:824-838`, `internal/clients/clients.go:865-872`); the accessor iterates those flags in registry order (`internal/clients/clients.go:952-964`).

No runtime fallback redesign is admitted. Register already derives its bindings from that read API and removes relay-stdio clients (`internal/api/register.go:38-64`), and install already consumes the same API (`research.md`, static-read: `internal/api/install.go:1743-1759`). Correct only the confirmed stale derivatives: the Register side-effect comment, the setup help comment, and the GUI handler test fixture/default response (`internal/api/register.go:135-140`, `internal/cli/setup.go:318-325`, `internal/gui/client_install_prefs_test.go:35-61`).

## 2. Change-Surface Contract

| Field | Design decision |
| --- | --- |
| Intended change surface | `internal/api/register.go:135-140` comment; `internal/cli/setup.go:318-325` help comment; `internal/gui/client_install_prefs_test.go:35-61` fixture and default-response expectation. Post-edit sweep may identify another *live stale derivative*; it requires planner/lead re-admission before expanding beyond a directly corresponding explanatory copy, test, fixture, or generated derivative. |
| Approved extension seam(s) | Membership: `clientDescriptor.defaultInstall` in `internal/clients/clientRegistry()`. Derived runtime read: `clients.DefaultInstallClientNames()`. Register-specific adaptation remains `buildDefaultClientBindings()` in `internal/api/register.go:55-64`, whose only additional policy is the capability filter `clients.IsRelayStdio`. |
| Protected / must-not-touch surfaces | Do not alter registry membership or order, `DefaultInstallClientNames`, `buildDefaultClientBindings`, register/supervised-register fallback flow, install planner/default override behavior, client preference API/persistence/wire shape, scheduler/live fleet, or current source/docs already consistent: `internal/cli/register.go`, `internal/gui/client_install_prefs.go`, `internal/gui/frontend/src/components/settings/SectionClients.tsx`, `README.md`, `INSTALL.md`, `docs/supported-clients.md`, `docs/cli-reference.md`, and generated `internal/gui/assets/` unless its frontend source changes. |
| Declared blast radius | Correct text/fixture semantics only: bare/default membership stays `{claude-code, codex-cli}`; Cursor remains supported but requires explicit `--clients cursor` or manifest selection. No intended executable behavior, external API, schema, preference persistence, scheduling, or process-lifecycle change. |

Implementation rule: no second literal default-policy list belongs in runtime logic. Literal names are permitted only as test expectations and explanatory text, and every user-facing explanatory copy is a derivative that must pass the post-edit sweep. The GUI fixture must represent Cursor as `CompileDefault: false` and unselected in its nominal no-override response, matching the registry-derived definition exposed by the production handler (`internal/gui/client_install_prefs.go:69-85`, `:103-107`).

## 3. Alternatives, dependencies, and contracts

| Option | Tradeoff | Decision |
| --- | --- | --- |
| Keep the registry-derived design; correct only stale derivatives | Preserves the existing single policy owner and limits executable change to no-op comments plus an honest fixture. | Chosen. It follows the verified owner/consumer relationship in `internal/clients/clients.go:835-838`, `:957-964`, and `internal/api/register.go:55-64`. |
| Add a local literal list to Register/setup/GUI code | Looks direct but creates a second policy owner that will drift when membership changes. | Rejected: the registry explicitly documents that there is no parallel list (`internal/clients/clients.go:824-830`), and the existing Register test guards derived equality (`internal/api/register_test.go:49-71`). |
| Change registry membership or add a persisted override migration | Would change live default-install behavior and/or preference semantics despite no evidence that the canonical owner is wrong. | Rejected: Cursor is already opt-in in the canonical descriptor (`internal/clients/clients.go:869-872`); the accepted scope is correction of stale derivatives. |

Dependency direction remains `clients registry -> DefaultInstallClientNames -> install/register consumers -> explanatory derivatives/tests`. The GUI handler remains a thin HTTP adapter over the API preference view; it does not own membership (`internal/gui/client_install_prefs.go:14-20`, `:51-62`).

Contract and persisted state: **no contract/persisted-state change**. Do not change HTTP response fields, CLI flags, manifest fields, preference file contents, or supported-client identifiers. There is therefore no migration, compatibility window, or state rollback procedure.

## 4. Failure modes, observability, and test strategy

| Failure mode | Observable discriminator | Containment / required guard |
| --- | --- | --- |
| A stale comment/help statement still treats Cursor or Gemini as default | Exact stale-pattern sweep returns a matching tracked live file/line. | Correct the derivative only; do not modify runtime policy. |
| GUI nominal-default fixture claims Cursor is compile-default or selected | `TestClientInstallPrefs_GetDefault` fails its Cursor `CompileDefault == false` and `Selected == false` assertion. | Update the test fixture/expectation to model the registry contract. |
| Register fallback reintroduces a parallel list or Cursor | `TestDefaultClientBindings_DerivedFromDefaultInstallSet` fails equality to `DefaultInstallClientNames` minus relay-stdio or detects Cursor. | Keep `buildDefaultClientBindings()` as a direct consumer of `DefaultInstallClientNames`. |
| Registry membership is accidentally changed while correcting derivatives | `TestDefaultInstallClientsExcludeOptInHeavyClients` reports a default-set mismatch or Cursor inclusion. | Treat registry as protected; revert/repair the off-scope edit. |
| Frontend source changes but embedded assets are stale | Diff shows a change below `internal/gui/frontend/` without regenerated `internal/gui/assets/` output and successful generator/check evidence. | If, and only if, frontend source changes, run `go generate ./internal/gui/...`, inspect the asset diff, and run the frontend checks selected by the planner. Never hand-edit generated assets. |

Required execution gates, owned by the later implementer/QA stages:

1. Never run `go test ./...`; never start `mcphub`; never kill or otherwise manage processes.
2. Run only these narrow Go tests for this correction:
   - `go test ./internal/clients -run '^TestDefaultInstallClientsExcludeOptInHeavyClients$'`
   - `go test ./internal/api -run '^TestDefaultClientBindings_DerivedFromDefaultInstallSet$'`
   - `go test ./internal/gui -run '^TestClientInstallPrefs_GetDefault$'`
3. Run the safe required repository checks `go build ./...` and `go vet ./...`.
4. Run the named post-edit tracked-text sweep before review. First, the stale-only guard must return no matches:
   - `git grep -n -I -E 'default clients claude-code,codex-cli,cursor|gemini-cli by default|Name: "cursor", CompileDefault: true' -- ':!work-items/archive/**' ':!docs/phase-*-verification.md' ':!docs/superpowers/**'`
   Then classify every remaining live `cursor` plus default/install/selection co-occurrence as either explicit opt-in, a correct negative assertion, or an out-of-scope historical record; an unclassified live occurrence is `REVISE`.
5. If frontend source actually changes, run the required generator and frontend typecheck/tests; otherwise leave frontend source and generated assets untouched and record that the conditional asset gate was not triggered.

No new log, event, metric, or status-code surface is required because the correction has no runtime execution-path change. Test failures and the named sweep output are the observability signals for this maintenance correction.

## 5. Diff-invisible invariants and claims

### Diff-invisible invariants

| Invariant | Named regression guard | Expected result |
| --- | --- | --- |
| The default binding construction happens at package initialization but its membership must still track the registry accessor. | `register-default-registry-parity`: `go test ./internal/api -run '^TestDefaultClientBindings_DerivedFromDefaultInstallSet$'` | Passes; binding names equal `DefaultInstallClientNames` minus relay-stdio and exclude Cursor. |
| A GUI test stub can bypass production API computation, so its nominal snapshot must still respect the canonical compile-default semantics. | `gui-default-snapshot-cursor-opt-in`: `go test ./internal/gui -run '^TestClientInstallPrefs_GetDefault$'` | Passes with Cursor `CompileDefault=false` and `Selected=false` in the no-override response. |
| Registry order and membership are shared by install, register, CLI/docs, and tests; a text-only correction must not mutate them. | `registry-default-membership`: `go test ./internal/clients -run '^TestDefaultInstallClientsExcludeOptInHeavyClients$'` | Passes with exactly `claude-code,codex-cli`; Cursor remains supported and absent from defaults. |
| Generated GUI assets are source derivatives, not editable policy owners. | `frontend-asset-conditional`: `git diff --name-only` followed by conditional `go generate ./internal/gui/...` only when frontend source changed. | No frontend source change means no asset change; a frontend source change has matching regenerated assets and frontend checks. |

### Claims

1. `{ guarantee: Default-install membership has exactly one runtime owner and Cursor remains explicit opt-in; single-owner: clientDescriptor.defaultInstall in clientRegistry(); enforcement-probe: registry-default-membership plus internal/clients/clients.go:865-872 and :957-964 }`
2. `{ guarantee: Workspace register fallback is derived from the membership owner rather than a second literal default list; single-owner: clients.DefaultInstallClientNames() read by buildDefaultClientBindings(); enforcement-probe: register-default-registry-parity and internal/api/register.go:55-64 }`
3. `{ guarantee: The three confirmed stale derivatives do not contradict the registry-derived default set after correction; single-owner: each derivative's owning source/test file, constrained by the registry policy; enforcement-probe: stale-only post-edit git grep plus gui-default-snapshot-cursor-opt-in }`
4. `{ guarantee: Existing consistent runtime, source, documentation, frontend, and generated-asset surfaces remain unchanged unless a post-edit sweep demonstrates a current-tree contradiction; single-owner: Change-Surface Contract in this design; enforcement-probe: scoped git diff review and classified tracked-text sweep }`
5. `{ guarantee: No generated asset is hand-maintained or left stale because of this item; single-owner: internal/gui frontend generation boundary; enforcement-probe: frontend-asset-conditional }`

This decision is local to the work item: it formalizes an already-existing owner and does not create a cross-cutting or long-lived new architecture rule. No `work-items/decisions/` entry is required.

Gate: PASS
