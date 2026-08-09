# Default-client inventory

Static investigation only; no runtime path, build, test, generator, package-manager, or product mutation was executed.

## Files & symbols

- `clients.clientRegistry` is the default-install policy owner: the `clientDescriptor.defaultInstall` field governs no-target install membership, and the registry has `claude-code` and `codex-cli` marked true while `cursor` has no flag. **static-read**: `internal/clients/clients.go:824-838`, `internal/clients/clients.go:865-872`.
- `clients.DefaultInstallClientNames` derives its ordered result from those descriptor flags. **static-read**: `internal/clients/clients.go:952-963`.
- `api.buildDefaultClientBindings` derives the workspace-register fallback from `clients.DefaultInstallClientNames`, filters relay-stdio adapters, and creates `/mcp` bindings. **static-read**: `internal/api/register.go:38-64`.

## Flows

- A workspace manifest with no `client_bindings` takes `defaultClientBindings` in both the ordinary register path and the supervised-register path. **static-read**: `internal/api/register.go:789-792`; `internal/api/register_supervisor.go:268-271`.
- The installation planner has the same compile-time fallback, after explicit clients and an operator override. **static-read**: `internal/api/install.go:1743-1759`.
- The CLI `register` help describes the matching policy: default bindings are `claude-code, codex-cli`; Cursor requires an explicit manifest binding. **static-read**: `internal/cli/register.go:35-38`.
- The dynamic execution of these Go paths is **ASSUMPTION (UNVERIFIED)** because the admitted procedure prohibits tests/runs. Resolving probe: the scoped register and client-registry tests named below.

## Contracts

- The compile-time registry contract is exactly two default clients and keeps Cursor supported but opt-in. **static-read**: `internal/clients/clients_test.go:351-381`.
- The direct register fallback contract equals the registry default set minus relay-stdio clients, includes today's two defaults, and rejects Cursor. **static-read**: `internal/api/register_test.go:30-79`.
- `internal/api/register.go:135-140` has a stale user-facing code comment: it says `gemini-cli` is default although the fallback code derives the two-client registry set. **static-read**.
- `internal/cli/setup.go:318-325` has a stale setup help comment: it names `claude-code,codex-cli,cursor` as default clients although `runSetupInstallServer` delegates to the same `api.Install` fallback and its local comment names only the two registry defaults. **static-read**: `internal/cli/setup.go:523-540`.

## Tests & coverage

- `internal/clients/clients_test.go:351-381`: current regression guard for registry membership; classification: **current**. **static-read**.
- `internal/api/register_test.go:30-79`: current regression guard for both registry-derived fallback and Cursor exclusion; classification: **current**. **static-read**.
- `internal/api/install_test.go:291-308`: current planner guard for two default updates and Cursor exclusion; classification: **current**. **static-read**.
- `internal/gui/client_install_prefs_test.go:35-61`: test fixture labels Cursor `CompileDefault: true` and selects it in its nominal default response; classification: **fix-required fixture**. **static-read**.
- No dedicated `SectionClients` frontend test, register fixture, or golden asserting a stale Cursor-default statement was found by the bounded searches; this is a negative static-search result, not a proof of absence. **ASSUMPTION (UNVERIFIED)**. Resolving probe: repeat the approved repository-wide tracked-text sweep after all edits.

## Similar implementations

- The install planner already consumes `clients.DefaultInstallClientNames`, matching the register fallback's owner. **static-read**: `internal/api/install.go:1743-1759`; `internal/api/register.go:55-64`.
- The GUI client-preference handler describes compile defaults as registry-derived and Cursor as opt-in. **static-read**: `internal/gui/client_install_prefs.go:3-21`, `internal/gui/client_install_prefs.go:69-107`.

## Inventory

| Classification | Surface | Finding | Evidence |
|---|---|---|---|
| current owner | registry descriptor | `defaultInstall` is the one membership flag; `claude-code` and `codex-cli` are true, Cursor is omitted. | static-read: `internal/clients/clients.go:824-838`, `:865-872`, `:952-963` |
| current consumer | register fallback | `buildDefaultClientBindings` consumes the registry result; both fallback reads use the built value. | static-read: `internal/api/register.go:49-64`, `:789-792`; `internal/api/register_supervisor.go:268-271` |
| fix-required | register comment | Stale claim that `gemini-cli` is default. | static-read: `internal/api/register.go:135-140` |
| current | register CLI help | States two defaults and explicit opt-in for Cursor. | static-read: `internal/cli/register.go:35-38` |
| fix-required | setup CLI help comment | Stale three-client default list including Cursor. | static-read: `internal/cli/setup.go:318-325` |
| current | setup execution comment/path | Delegates to `api.Install`; comment names two defaults. | static-read: `internal/cli/setup.go:516-540` |
| fix-required fixture | GUI API unit fixture | Cursor marked compile default and selected in nominal-default sample. | static-read: `internal/gui/client_install_prefs_test.go:35-61` |
| current | GUI source | Default text names the two clients and calls Cursor opt-in. | static-read: `internal/gui/frontend/src/components/settings/SectionClients.tsx:1-8`, `:118-153` |
| generated/current | GUI derivative | Generated `app.js` embeds the matching current `SectionClients` default/opt-in strings; `style.css` is the companion generated asset but contains no client-policy text. | static-read: `internal/gui/assets/app.js:110`; `internal/gui/assets/style.css` |
| current | install guide | Explicitly names two defaults and shows Cursor only in an explicit `--clients` example. | static-read: `INSTALL.md:99-124` |
| current | README | Names two defaults and Cursor opt-in. | static-read: `README.md:155-161`, `:279-283`, `:319-320` |
| current | supported-client document | Counts two default-install clients and lists Cursor under opt-in. | static-read: `docs/supported-clients.md:3-16`, `:69-74` |
| current/general | CLI reference | Says “default client configs” but does not enumerate a conflicting membership set. | static-read: `docs/cli-reference.md:12-19` |
| provenance | archived phase/design verification text | Historical phase documents contain older client lists; they are not current default-install contracts. | static-read: `docs/phase-2-verification.md:74`; `docs/phase-3-verification.md:247`; `docs/phase-3a-verification.md:168`; `docs/superpowers/**` |
| explicit/current | install example | `claude-code,codex-cli,cursor` occurs as an explicit `--clients` selection, not a default. | static-read: `INSTALL.md:105-112` |

### Object-axis C1 record

| Axis | Object | Canonical owner | Consumers/derivatives | Result |
|---|---|---|---|---|
| C1: default-install membership | ordered client-id set | `clientDescriptor.defaultInstall` in `clientRegistry()` | `DefaultInstallClientNames`, install planner, register fallback, GUI compile-default view, user-facing copies/tests | One verified runtime policy owner; two stale statements and one fixture retain duplicated, inconsistent values. |

## Constraints

- The admitted scope allows only this research memo and the analyst report; product, documentation, generated assets, and tests were not edited. **static-read**: `brief.md` (Scope and Constraints).
- The live fleet must remain untouched, and the named regression guard is a clean product/docs/assets diff attributable to this analyst stage. **static-read**: `brief.md` (Constraints and Critical risks).

## Change risks

- The default set changed in commit `5280b8aa` (Cursor made opt-in); `bff6b6bf` then fixed two register/GUI leaks. The remaining stale setup comment and GUI test fixture predate that correction, so textual/fixture drift is a concrete maintenance risk. **static-read**: focal `git log -n 5` and `git blame` for `internal/clients/clients.go`, `internal/api/register.go`, `internal/cli/setup.go` captured in this session.
- The generated `internal/gui/assets/app.js` is a derivative of `SectionClients.tsx`; direct modification would break the stated generation contract. **static-read**: `brief.md` (Scope).

## Unresolved questions

- No runtime test was run, so static equality between registry and fallback has not been execution-confirmed in this session. **ASSUMPTION (UNVERIFIED)**. Resolving probe: `internal/clients` default-client test and the narrowly targeted `internal/api` register test allowed by the brief.
- No post-edit sweep exists yet; the inventory is pre-change only. **ASSUMPTION (UNVERIFIED)**. Resolving probe: repeat the exact tracked-text searches after implementation and generated-asset regeneration.

## Research admission gates

- Regression risk: the present consumer code uses the registry-derived list, and focused tests statically encode Cursor exclusion. **static-read**: `internal/api/register.go:55-64`; `internal/api/register_test.go:30-79`.
- Metric alignment: not applicable to this factual inventory; no optimization objective is admitted. **static-read**: `brief.md` Goal and Acceptance criteria.
- Known limits: static search cannot prove runtime behavior or complete future generated output; both are listed as unresolved. **static-read**: this memo’s Unresolved questions.
- Bounded falsification: the brief names narrow touched-package tests and a post-edit repository sweep; no such command was run in this stage. **static-read**: `brief.md` Scope and Acceptance criteria.

## Adjacent findings

None within the bounded default-client inventory.

### Searched and excluded

- Searched tracked code/text for `cursor`; Cursor/Claude-Code/Codex-CLI co-enumerations; `by default`, default-client/default-install language, install-target claims, and `client_bindings`; inspected the registry, register paths/tests, CLI register/setup paths, GUI source/asset, README, INSTALL, supported-client document, and CLI reference.
- Excluded `.git`, `.scratch`, `.reports`, `.plans`, `work-items/archive`, `node_modules`, and minified generated assets from truth classification. The non-minified generated derivative `internal/gui/assets/app.js` was named and inspected solely as a generated derivative.
- Historical phase/design documents were classified as provenance rather than live behavior. Explicit client-selection examples were classified separately from defaults.
- Saturation stop: two consecutive widening checks (historical documentation and non-default Cursor co-enumerations) did not add another live default-policy owner, fallback reader, stale default statement, fixture, golden, or generated derivative.

## Gate

Gate: PASS
