# Phase 3B-II A3-a — Secrets registry screen (design memo)

**Status:** design memo (pre-plan)
**Author:** Claude Code (brainstorm + Codex Q1–Q6 consensus, 2026-04-25)
**Predecessor:** PR #17 (Phase 3B-II B1 — Servers matrix uncheck-to-demigrate, merged at `fa2012c`)
**Backlog entry:** `docs/superpowers/plans/phase-3b-ii-backlog.md` §A row A3
**Original spec reference:** `docs/superpowers/specs/2026-04-17-phase-3-gui-installer-design.md` §5.6

## 1. Summary

A3-a delivers the GUI Secrets registry screen at `#/secrets`. It lists vault keys with their "Used by" counts, exposes Add / Rotate / Delete operations, and surfaces vault-state errors honestly. The backend gains 5 HTTP endpoints under `/api/secrets/*` and 5 `api.go` wrappers that thread through the existing `internal/secrets` package — closing a long-standing CLI ≡ GUI parity gap (CLI has been calling `secrets.OpenVault` directly since Phase 2; the GUI now does the same through proper API wrappers). "Used by" counts are computed by a tolerant manifest scan that aggregates env values starting with the `secret:` prefix.

A3-b — the `env.secret:KEY` picker inside `AddServer.tsx` / `EditServer.tsx` forms — is **explicitly deferred** to a follow-up phase. A3-a builds the registry surface; A3-b adds consumer UX. This split was the Q1 decision and is locked.

This memo encodes:

- The Q1–Q6 brainstorming consensus as locked design decisions D1–D6.
- Four implementation-structure decisions D7–D10 not covered by Q1–Q6 (screen organization, modal primitive, restart-result granularity, scan boundaries).
- The exact API contracts, including the registry envelope shape that handles four distinct vault states (`ok` / `missing` / `decrypt_failed` / `corrupt`) and ghost-reference rows.
- Implementation risks and the tests required to ship.

## 2. Context recon

### 2.1 Vault backend — already implemented

`internal/secrets/vault.go` defines an age-encrypted key/value store:

```go
type Vault struct {
    identity  age.Identity
    recipient age.Recipient
    vaultPath string
    data      map[string]string
}

func InitVault(keyPath, vaultPath string) error
func OpenVault(keyPath, vaultPath string) (*Vault, error)
func (v *Vault) Set(key, value string) error
func (v *Vault) Get(key string) (string, error)
func (v *Vault) Delete(key string) error
func (v *Vault) List() []string                 // sorted alphabetically
func (v *Vault) ExportYAML() ([]byte, error)
func (v *Vault) ImportYAML(raw []byte) error
```

Three behaviors A3-a relies on:

1. `InitVault` **fails if either file already exists** ([vault.go:28-33](../../../internal/secrets/vault.go#L28-L33)). Naive "POST init" twice in a row is a hard error. The A3-a `SecretsInit` wrapper must idempotently no-op when `OpenVault` already succeeds (Q2 contract).
2. `Set` silently overwrites existing keys ([vault.go:84-87](../../../internal/secrets/vault.go#L84-L87)). The `POST /api/secrets` (Add) handler must therefore reject duplicates explicitly with a 409 — vault layer can't help here.
3. `Delete` returns "not found" if key is absent ([vault.go:99-105](../../../internal/secrets/vault.go#L99-L105)). The `DELETE /api/secrets/:key` handler should map this to 404 (idempotent retry need not surface as success but should be unambiguous).

Path resolution lives in `internal/secrets/paths.go`:

- `DefaultKeyPath()` → `<UserDataDir>/.age-key` (private identity)
- `DefaultVaultPath()` → `<UserDataDir>/secrets.age` (encrypted store)
- `UserDataDir()` → OS-canonical (`%LOCALAPPDATA%\mcp-local-hub` on Windows, `$XDG_DATA_HOME/mcp-local-hub` on Linux, `~/Library/Application Support/mcp-local-hub` on macOS)
- `resolveSecretPath` walks a search list (canonical → exe-sibling → exe-parent → CWD) and returns canonical if none hit. `InitVault` always lands in canonical.

These functions stay the source of truth for A3-a; the new `api.SecretsInit` wrapper calls them directly and does not introduce parallel path logic.

### 2.2 Resolver and existing scan surface

`internal/secrets/resolver.go` parses manifest env values into resolution categories:

| Prefix    | Source                                     |
|-----------|--------------------------------------------|
| `secret:` | `vault.Get` (this is what A3-a's scan tracks) |
| `file:`   | local config map (`config.local.yaml`)     |
| `$VAR`    | `os.LookupEnv`                             |
| (literal) | returned as-is                             |

`internal/api/install.go:checkSecretRefs` ([install.go:966-995](../../../internal/api/install.go#L966-L995)) is the canonical pattern for `secret:` detection. It does:

```go
if strings.HasPrefix(v, "secret:") {
    // resolver.Resolve, fail-fast on missing
}
```

A3-a's `api.ScanManifestEnv` (new helper, §3.1 backend item #1) inherits this exact prefix test and the same "tolerant on parse error" framing.

### 2.3 CLI surface — pre-existing, non-conformant

`internal/cli/secrets.go` exposes 7 cobra subcommands: `init`, `set`, `get`, `list`, `delete`, `edit`, `migrate`. Every command calls `secrets.OpenVault(defaultKeyPath(), defaultVaultPath())` **directly** ([secrets.go:84](../../../internal/cli/secrets.go#L84), [121](../../../internal/cli/secrets.go#L121), [144](../../../internal/cli/secrets.go#L144), [172](../../../internal/cli/secrets.go#L172), [195](../../../internal/cli/secrets.go#L195), [213](../../../internal/cli/secrets.go#L213), [312](../../../internal/cli/secrets.go#L312)) — bypassing `api.NewAPI()`. This is the same CLI-≡-GUI parity gap acknowledged in the B1 memo §2.4 and the softened comment in `internal/api/api.go`.

A3-a does **not** retrofit the CLI to go through `api.go` — that's a separate cleanup. But the new GUI wrappers (`api.SecretsInit` / `api.SecretsListWithUsage` / `api.SecretsSet` / `api.SecretsRotate` / `api.SecretsDelete`) establish the canonical pattern future CLI work should adopt. A3-a's CLI surface is unchanged.

### 2.4 GUI surface — empty

- No `/api/secrets/*` endpoints. Routing in `internal/gui/server.go` does not register any.
- No `Secrets.tsx` screen. `internal/gui/frontend/src/screens/` contains Servers / Migration / AddServer / EditServer / Dashboard / Logs.
- No sidebar nav link. Nav lives in `internal/gui/frontend/src/app.tsx` (no `components/Sidebar.tsx` exists — Codex memo-R1 P2). Current 5 nav entries; "Secrets" is missing.
- `internal/gui/frontend/src/hooks/useRouter.ts` is the routing hook (no separate `routing.ts` registry — Codex memo-R4 P2). The hash-route switch lives directly in `app.tsx`'s render branch on `currentRoute`.

A3-a creates all of these.

### 2.5 Manifest scan source — embed-first, disk fallback

**Codex memo-R1 P1 caught a wrong-source assumption in the original draft.** Manifests in this repo live at `servers/<name>/manifest.yaml` (one directory per server, not flat `*.yaml` files). Production read paths use the embed-first / disk-fallback pattern in `internal/api/manifest_source.go` ([manifest_source.go:51-93](../../../internal/api/manifest_source.go#L51-L93)):

- `servers.Manifests` is the `embed.FS` baked into the binary at compile time. Subdirectories under it are server names; each contains a `manifest.yaml`.
- Disk fallback: `defaultManifestDir()` resolves to a sibling `servers/` directory next to the running binary (or repo root in dev). On installed binaries with no source tree, the disk path typically does not exist — embed is authoritative.
- `listManifestNamesEmbedFirst()` returns the **union** of embed names and disk names, deduped and sorted.
- `loadManifestYAMLEmbedFirst(name)` reads `<name>/manifest.yaml` from embed first, then falls back to disk.

A3-a's `ScanManifestEnv` MUST use this same pair (or call into them directly) so its scan results match what the rest of the GUI sees. Scanning `<UserDataDir>/servers/*.yaml` would return zero entries on most installs and produce empty `used_by` arrays everywhere.

The "Used by" aggregation needs only two fields per file:

```yaml
name: <server-name>
env:
  KEY1: secret:vault_key_1   # ← scan target
  KEY2: file:local_key       # ← ignore
  KEY3: $HOME                # ← ignore
  KEY4: literal              # ← ignore
```

Other fields (`kind`, `transport`, `command`, `daemons`, `client_bindings`) are not even unmarshaled by the scan helper. The scan uses a deliberately narrow typed struct `{Name string \`yaml:"name"\`, Env map[string]string \`yaml:"env"\`}` so unrelated malformed fields don't break the scan — which means malformed unrelated fields (e.g., a broken `daemons` block) are NOT reported in `manifest_errors`. **Codex memo-R4 P3 clarified:** `manifest_errors[]` reports failures of `Name` or `Env` parsing only; full-schema validation is the responsibility of the existing manifest-validation paths (`api.ManifestGetWithHash` etc.), not the secrets scan. Delete fail-closed therefore protects against `Name`-or-`Env` parse failures, not arbitrary schema drift in a manifest.

### 2.6 Restart API — pre-existing, aggregated-error

`internal/gui/server.go:realRestarter.Restart` ([server.go:204-218](../../../internal/gui/server.go#L204-L218)) returns a single aggregated error string when one or more daemons fail to restart:

```go
if len(failed) > 0 {
    return fmt.Errorf("restart failed: %s", strings.Join(failed, "; "))
}
```

The `/api/servers/:name/restart` handler ([servers.go:22-32](../../../internal/gui/servers.go#L22-L32)) returns 500 on this aggregated error with a `RESTART_FAILED` code. There is **no per-daemon outcome surface** today.

A3-a's Rotate flow (Q4 D4 below: "Save and restart" with per-daemon result list) needs granular results. D9 below picks the integration approach (refactor existing handler vs. add a parallel one).

## 3. Scope

### 3.1 In scope (A3-a)

**Backend (Go):**

1. `internal/api/secrets_scan.go` (new — **Codex memo-R2 P1 fix:** placing in `internal/api` avoids the import cycle that would arise if a `secrets` package helper imported `api` for `manifest_source.go` access; `internal/api` already imports `internal/secrets`, not the other way). Defines `ScanManifestEnv() (map[string][]UsageRef, []ManifestError, error)` reusing `listManifestNamesEmbedFirst()` + `loadManifestYAMLEmbedFirst()` from `manifest_source.go` directly. `UsageRef{Server, EnvVar}` and `ManifestError{Name, Path, Error}` are defined in `internal/api/secrets.go` alongside the wrappers. Per-manifest YAML parse errors are collected separately and surfaced in `manifest_errors`. **Memo-R1 P1: must NOT scan `<UserDataDir>/servers/*.yaml`; that path doesn't exist on installed binaries — see §2.5.**
2. `internal/api/secrets.go` (new): 5 wrappers — `SecretsInit`, `SecretsListWithUsage`, `SecretsSet`, `SecretsRotate`, `SecretsDelete` — plus `UsageRef`, `ManifestError`, `SecretsEnvelope`, `SecretRow`, `SecretsInitResult`, `SecretsRotateResult` types. Wrappers call `secrets.OpenVault` / `secrets.InitVault` directly and call `ScanManifestEnv` (defined in the sibling `secrets_scan.go`).
3. `internal/gui/secrets.go` (new): HTTP handlers for the 5 endpoints. **Same-origin guard via existing `requireSameOrigin` middleware ([csrf.go:27](../../../internal/gui/csrf.go#L27)) on every endpoint including GET** — Codex memo-R2 P2 corrected an earlier wrong assumption: `/api/scan` DOES enforce same-origin in this codebase. The middleware emits `CROSS_ORIGIN` (not a secrets-specific code) on rejection. Error code catalog (§5.7).
4. `internal/gui/server.go`: register the new routes; add `s.secrets` adapter field analogous to `s.demigrater`.
5. **D9 refactor:** `realRestarter.Restart` returns `([]api.RestartResult, error)` instead of aggregated `error`; handler exposes the per-task shape using the existing `api.RestartResult{TaskName, Err}` type (D9 details below — no new types invented).
6. Go unit tests for handlers and scan logic.

**Frontend (TypeScript + Preact):**

1. `internal/gui/frontend/src/screens/Secrets.tsx` (new): main screen with 4-state rendering (`not-init` / `init-empty` / `init-keyed` / `broken`).
2. `internal/gui/frontend/src/components/AddSecretModal.tsx` (new).
3. `internal/gui/frontend/src/components/RotateSecretModal.tsx` (new): 3-button modal with `[Cancel | Save without restart | Save and restart]`.
4. `internal/gui/frontend/src/components/DeleteSecretModal.tsx` (new): differential typed-confirm.
5. `internal/gui/frontend/src/lib/secrets-api.ts` (new): typed fetch wrappers for the 5 endpoints.
6. `internal/gui/frontend/src/app.tsx`: add 6th nav link (`#/secrets`) and the `case "secrets"` route entry to the existing screen-switch (Codex memo-R1 P2: there is no `components/Sidebar.tsx`; nav lives in `app.tsx`).
7. `internal/gui/frontend/src/app.tsx`: extend the route switch to recognize `secrets` as a route key (no separate `routing.ts` registry exists — Codex memo-R4 P2; routing is the `useRouter` hook in `hooks/useRouter.ts` plus the screen switch in `app.tsx`).
8. `internal/gui/frontend/src/lib/use-secrets-snapshot.ts` (new): hook that fetches `/api/secrets`, polls on focus, exposes `{state, data, error, refresh}`.
9. Vite build + `go generate ./internal/gui/...` produces `internal/gui/assets/{index.html,app.js,style.css}`.
10. E2E scenarios in `internal/gui/e2e/tests/secrets.spec.ts` (new spec file).

**Documentation:**

1. `CLAUDE.md` E2E coverage section updated for the new `secrets.spec.ts` count.
2. `internal/api/api.go` package comment unchanged from B1's softened state (A3-a uses the per-request `api.NewAPI()` pattern; the comment already says capabilities live in `api` so both surfaces *can* reach them).

### 3.2 Explicitly out of scope — deferred to A3-b

- **`env.secret:KEY` picker in `AddServer.tsx` / `EditServer.tsx` forms.** Today's env-pair editor is a plain `<input type="text">` for `KEY = value`. A3-b adds a dropdown of vault keys when the value field is focused, type-ahead populates `secret:<key>`, and unknown-secret entries surface a "Create this secret first" link to `#/secrets`. This is the actual consumer UX wave; A3-a builds the registry it consumes.

### 3.3 Explicitly out of scope — deferred to future phases

- **Cascade delete** (vault delete + manifest env-line removal + dependent daemon restart). Q5 D5 explicitly deferred this. Different operational risk class.
- **Spawn `$EDITOR` from backend** (Q6 D6 banner-not-button). If ever revisited, requires: vault locking, mutation-disable while editing, secure tempfile handling, atomic re-encrypt, partial-save recovery, cross-platform GUI-vs-terminal editor detection, server-side process lifecycle. Not A3-a.
- **Vault import/export through GUI.** CLI `secrets edit` already exists; A3-a's banner points users at it.
- **CLI `mcphub secrets *` retrofit** to go through `api.go`. Tracked separately; not blocking A3-a.
- **Vault hash/version field for concurrent-edit detection.** R1 below acknowledges concurrent edits are last-write-wins. A future hash-mismatch shape (analogous to manifest's `expected_hash`) belongs in its own design.

## 4. Decisions (Q1–Q6 locked + D7–D10 implementation structure)

### D1 — Scope split between A3-a and A3-b (Q1)

**Chosen:** A3-a builds the registry. A3-b builds the consumer picker. They ship as separate phases.

The split aligns with Codex's framing: "secret registry vs consumer UX" rather than "backend vs frontend". A3-a's API contract is consumed by a clean A3-b implementation that doesn't leak vault internals into the AddServer form. Dogfooding A3-a's UX (admin opens registry, adds keys, rotates them) before committing to A3-b's picker shape is a deliberate design-validation step. If A3-a's "Used by" envelope surfaces real-world friction (e.g., users want to filter by server, search by key prefix), A3-b can absorb the lessons.

### D2 — Vault state machine + idempotent init (Q2)

**Chosen:** four-state envelope, `POST /api/secrets/init` idempotent (no-op when `OpenVault` succeeds), mutating ops fail-fast on uninitialized vault.

| State                | UX                                                                | API path                                                                                      |
|----------------------|-------------------------------------------------------------------|-----------------------------------------------------------------------------------------------|
| `not-initialized`    | Empty-state + "Initialize secrets vault" button                  | `POST /api/secrets/init` → 200 with `{vault_state:"ok"}`. Subsequent calls also 200 (no-op).  |
| `initialized-empty`  | "No secrets yet" + "Add secret" button                            | `GET /api/secrets` returns `{vault_state:"ok", secrets:[…manifest-only refs as ghosts…]}`.    |
| `initialized-keyed`  | Table of key names + Used-by + per-row Rotate/Delete actions      | Standard list rendering.                                                                       |
| `broken`             | Actionable error banner (e.g., decrypt failure)                   | `GET /api/secrets` returns `{vault_state:"decrypt_failed", secrets:[…manifest-only refs…]}`.  |

**Init idempotency rule** — `secrets.InitVault` refuses if EITHER `keyPath` or `vaultPath` already exists ([vault.go:28-33](../../../internal/secrets/vault.go#L28-L33)). Order of operations inside `InitVault`: write key file first ([vault.go:38-42](../../../internal/secrets/vault.go#L38-L42)), then call `v.save()` which encrypts to a buffer and writes via `os.WriteFile(vaultPath, buf.Bytes(), 0o600)` ([vault.go:118-134](../../../internal/secrets/vault.go#L118-L134)). Either step can fail (disk full, permission, IO interruption). If the second step fails, `os.WriteFile` may have created and truncated `secrets.age` to a partial state OR not created it at all — both possibilities must be cleaned up. The `SecretsInit` wrapper is responsible for cleanup-aware retry.

1. **`OpenVault` succeeds** → vault is functional. No-op. Return `vault_state:"ok"`. (Idempotent path: clicking Init on an already-initialized system is harmless.)
2. **`OpenVault` fails AND both files are missing** → safe to call `InitVault`. On success return `vault_state:"ok"`. **On `InitVault` failure** (Codex memo-R2 P1 expanded): the wrapper attempts cleanup of EVERY artifact `InitVault` may have created — first `os.Remove(vaultPath)` (in case `os.WriteFile` partially wrote it), then `os.Remove(keyPath)` (in case the key was written but vault failed). Cleanup uses `os.IsNotExist(err)` tolerance so a missing artifact is not a cleanup failure. The wrapper reports `cleanup_status: "ok" | "failed"`:
   - `cleanup_status: "ok"` → both removals succeeded (or the artifacts didn't exist). User can retry; HTTP 200 with `{vault_state: "missing", cleanup_status: "ok", error, code}` so the body still surfaces the original `SECRETS_INIT_FAILED` reason.
   - `cleanup_status: "failed"` → at least one removal failed. HTTP 500 + `SECRETS_INIT_FAILED` + `orphan_path` field naming the artifact that requires manual cleanup.
3. **`OpenVault` fails AND both files exist** (decrypt failure, identity mismatch, corrupt vault) → do NOT call `InitVault` (it would refuse anyway). Return **409** + `SECRETS_INIT_BLOCKED` with a message pointing the user at `mcphub secrets edit` for recovery, or at deleting the files manually if the key is truly lost.
4. **`OpenVault` fails AND exactly one file exists** (orphan key without vault, or orphan vault without key) → **409** + `SECRETS_INIT_BLOCKED`. Message names which file is the orphan and asks the user to delete it manually before retrying.

**Never silently destroy existing data.** Cases 3 and 4 require explicit user action; A3-a does not implement automated cleanup of pre-existing orphan / corrupt files. The wrapper does cleanup ONLY for orphans **it just created itself** in case 2's partial-failure path. The asymmetry between case 2 (auto-init + auto-cleanup-of-our-orphan) and cases 3/4 (refuse) is the safety boundary.

**Mutation fail-fast rule:** `POST /api/secrets` (Add), `PUT /api/secrets/:key` (Rotate), `DELETE /api/secrets/:key` all fail with **409** + error code `SECRETS_VAULT_NOT_INITIALIZED` if the vault is missing. The frontend's mutating modals first GET `/api/secrets` to read `vault_state`; if not `ok`, they refuse to render the form and route the user back to the empty-state init flow. Backend protection ensures a CLI / direct-API caller hitting POST / PUT / DELETE without init also gets a clean 409 instead of partial-init surprises.

### D3 — Registry envelope with vault-state and ghost-refs (Q3)

**Chosen:** `GET /api/secrets` returns a registry snapshot envelope, not a flat list.

```json
{
  "vault_state": "ok | missing | decrypt_failed | corrupt",
  "secrets": [
    {"name": "OPENAI_API_KEY", "state": "present", "used_by": [{"server": "server-a", "env_var": "OPENAI_API_KEY"}, {"server": "server-b", "env_var": "OPENAI_API_KEY"}]},
    {"name": "WOLFRAM_APP_ID", "state": "referenced_missing", "used_by": [{"server": "server-c", "env_var": "WOLFRAM_APP_ID"}]}
  ],
  "manifest_errors": [
    {"path": "servers/broken.yaml", "error": "yaml: line 12: did not find expected node content"}
  ]
}
```

Three classes of `secrets[].state`:

- `present` — key exists in vault. `used_by` may be empty (orphan in vault but no manifest refs).
- `referenced_missing` — manifest references `secret:KEY` but vault doesn't have the key. **Used only when `vault_state == "ok"`** (we can verify absence). UI shows warning icon, suggests "Add this secret" or "Remove the reference from server".
- `referenced_unverified` — manifest references `secret:KEY` but `vault_state != "ok"` so we cannot verify whether the vault has the key. **Codex memo-R1 P2 corrected the original draft's collapse of `referenced_missing` and `referenced_unverified`.** UI copy is softer ("Vault unavailable; this key is referenced by N servers but its vault status cannot be verified"); Add/Rotate/Delete actions are disabled for these rows until vault becomes `ok`.
- (Future) `unreadable` for keys present but with corrupted values. Not in A3-a — the vault encrypts the whole file, so partial corruption surfaces as `vault_state:"corrupt"`, not per-key.

`manifest_errors[]` is per-file. A YAML parse failure on one file does NOT abort the entire registry — other manifests still contribute their refs. Users see a small "1 manifest had errors" indicator with expandable details.

**Read-only endpoint degrades gracefully; mutating endpoints fail-fast** (already in D2). These error contracts are deliberately asymmetric: a broken vault should still let users see what they have configured; only writes need atomic correctness.

### D4 — Rotate as 3-button modal with best-effort restart (Q4)

**Chosen:** `[Cancel | Save without restart | Save and restart]` modal. Best-effort restart sequence: vault is updated first; restart attempts on each affected daemon; per-daemon results displayed. **No vault rollback on restart failure.**

Modal copy:

> Rotating `OPENAI_API_KEY`. **Currently referenced by 5 daemons across 2 servers; 3 daemons are running and can be restarted now.**
>
> Stopped daemons will pick up the new value automatically on next start.
>
> *(value field, masked)*
>
> [Cancel] [Save without restart] [Save and restart]

Behavior:

1. **Cancel** — no vault write, no restart. Modal closes.
2. **Save without restart** — `PUT /api/secrets/:key` with `{"value":"…","restart":false}`. Vault updated. Modal closes.
   - If at least 1 running daemon would be affected: persistent CTA appears at top of Secrets screen: "Vault updated. N running daemons still using the previous value. [Restart now] [Dismiss]". Persistent (not toast) so user can defer until convenient maintenance window.
   - **If 0 running daemons** (Codex memo-R1 P3): the CTA is suppressed entirely; instead a brief toast confirms "Vault updated. No running daemons need restart." Stopped daemons will pick up the new value on next start; surfacing a useless "0 running daemons" CTA would be noise.
3. **Save and restart** — `PUT /api/secrets/:key` with `{"value":"…","restart":true}`. Backend writes vault first, then attempts restart on each affected daemon (those bound to a server whose manifest references this key AND whose status is "running"). Response includes per-daemon results array. If any restart fails, the response banner shows: "Vault updated. 3/5 daemons restarted. 2 still need restart to use the new value." with a list of failed daemons and a "Retry failed restarts" button.

**No rollback on restart failure.** If 2 of 5 restarts fail, the vault still has the new value. Rolling vault back to the old value would mean: the 3 successfully-restarted daemons now use a value that's no longer authoritative, the 2 failed daemons still have the old value, and the next restart of any daemon that picks the new value pulls something stale. Best-effort writes are robust because they preserve the vault as the source of truth and let the user decide when to retry restart attempts.

**Stopped vs. running distinction in the counter** is essential. A user who sees "5 daemons reference this key" might expect 5 restarts; clarifying "3 are currently running" makes the modal honest. Stopped daemons pick up the new value naturally on their next start.

### D5 — Delete with two-layer guard + differential typed confirm + escalation flow (Q5)

**Chosen:** Backend 409 guard is the source of truth. Frontend ALWAYS calls without `?confirm=true` first; on 409 escalates to typed-confirm using the FRESH refs returned in the 409 body.

**Codex memo-R1 P1 fix:** the original draft pre-decided in the GUI based on the cached snapshot's `used_by`, then sent `?confirm=true` for the simple-delete path. That bypasses the backend guard if `used_by` changed between snapshot read and DELETE call (e.g., someone just added a manifest referencing the key in another tab). Fix: simple-delete path NEVER sends `?confirm=true`; if backend says 409, GUI escalates.

API layer (precedence: `manifest_errors` → `used_by` → delete; the scan-incomplete check ALWAYS wins because we can't trust `used_by` from an incomplete scan):

- `DELETE /api/secrets/:key` (no confirm):
  1. If `manifest_errors != []` → 409 + `{error: humanMessage, code: "SECRETS_USAGE_SCAN_INCOMPLETE", manifest_errors: [...]}` — **fail-closed.** A key referenced ONLY by a broken manifest must not be silently deleted (Codex memo-R1 P1).
  2. Else if `used_by != []` → 409 + `{error: humanMessage, code: "SECRETS_HAS_REFS", used_by: [{server, env_var}, ...]}`.
  3. Else → 204 No Content (deleted).
- `DELETE /api/secrets/:key?confirm=true` (force):
  - Bypasses both `SECRETS_HAS_REFS` and `SECRETS_USAGE_SCAN_INCOMPLETE` guards.
  - If vault is `ok` and key exists: 204.
  - If key missing: 404 + `SECRETS_KEY_NOT_FOUND`.
  - If vault not initialized: 409 + `SECRETS_VAULT_NOT_INITIALIZED`.

Frontend escalation flow:

1. User clicks Delete. GUI fires `DELETE /api/secrets/:key` (no confirm).
2. **204** → success. Snapshot refresh; row disappears.
3. **409 with `SECRETS_HAS_REFS`** → open modal with the exact `server / env_var` ref list FROM THE 409 RESPONSE BODY (fresh refs, not cached). Copy:
   > Deleting `<key>` will leave broken references in:
   > - `server-a` (env: `OPENAI_API_KEY`)
   > - `server-b` (env: `OPENAI_API_KEY`)
   >
   > Manifests will not be modified. Running daemons will not restart, but future installs and restarts of these servers will fail until you provide the secret again or remove the references.
   >
   > Type **DELETE** to confirm.
4. **409 with `SECRETS_USAGE_SCAN_INCOMPLETE`** → open modal with copy:
   > Some manifests couldn't be scanned (`manifest_errors`). We can't verify whether `<key>` is referenced. Type **DELETE** to delete anyway.
5. **404** → toast "Already deleted" + snapshot refresh.
6. After typed-confirm, GUI fires `DELETE /api/secrets/:key?confirm=true`. Expects 204 (or 404 if it was deleted in another tab — handled gracefully).

**Why escalation works:** the GUI is no longer the source of truth for "should this require confirmation". The backend's 409 guard runs **at the moment of the actual delete**, against the current scan state. Race window between snapshot and delete is closed. Differential UX (no modal for unreferenced delete) is preserved because the happy path is single-click → 204.

**Friction is now ALWAYS proportional to current risk**, not stale-snapshot risk.

### D6 — Edit vault as banner-not-button (Q6)

**Chosen:** Info banner at top of the Secrets screen with the CLI command and a copy-to-clipboard button. **No "Edit vault" button.**

Banner copy:

> Need bulk operations? Run the CLI command in a terminal:
>
> `mcphub secrets edit`
> [📋 Copy command]

A button would imply the GUI owns the lifecycle of the editor session — it does not, and won't (see §3.3 for the preconditions a future C-style implementation would need). Banner makes the boundary clear: this operation lives outside the GUI. Discoverability is preserved without implementation cost.

### D7 — Single Secrets.tsx with state-machine guard (Alt 1)

**Frontend recon update (Codex memo-R1 P2):** there is no `internal/gui/frontend/src/components/Sidebar.tsx`. Nav and the screen-switch live in `app.tsx`. A3-a does NOT introduce a new Sidebar component — instead it adds a route entry to the existing `app.tsx` switch and a nav link to whatever the current sidebar markup is in `app.tsx`. §3.1 frontend item #6 ("`components/Sidebar.tsx`: add 6th entry") is therefore corrected to: "`internal/gui/frontend/src/app.tsx`: add the `#/secrets` case to the route switch and the corresponding link element."

**Chosen:** One file, `Secrets.tsx`, with a top-level `switch (vault_state)` rendering different shapes.

Three rejected alternatives:

1. **Split per state**: `SecretsEmptyState.tsx`, `SecretsList.tsx`, `SecretsBroken.tsx`, plus a Container that picks. Adds 3 component files, more imports, no clarity gain — the rendering branches are ~30 LOC each and live naturally in one file.
2. **State-machine library** (xstate, @reduxjs/toolkit): overkill for 4 states with no nested transitions. The `useSecretsSnapshot` hook already returns `{state, data}`, which is the whole machine.
3. **Conditional component composition with HOCs**: rejected as over-engineering.

Rationale for chosen: the screen is small (estimated ~250 LOC including modals' invocation logic). Keeping it in one file matches the AddServer / EditServer / Migration patterns in this codebase. State branching with `switch` (or early-return ladder) is idiomatic.

### D8 — Modals via native `<dialog>` element (Alt 2)

**Codex memo-R1 P2 corrected the original draft's claim that EditServer already uses `<dialog>` — it does not.** EditServer's "save-confirm" / "force-save" surfaces are inline in-screen sections, not modals. There is no existing modal primitive in the codebase. A3-a is therefore **introducing** the modal pattern.

**Chosen:** native `HTMLDialogElement`. The drawbacks of the alternatives (floating div with manual focus trap, headless UI / Radix dependency) outweigh the cost of being first.

Native `<dialog>` provides:

- Built-in focus trap.
- ESC-to-close via `cancel` event.
- `::backdrop` pseudo-element for overlay styling.
- Z-index always on top (no z-index war).
- ARIA `role="dialog"` automatic.

Drawbacks (acceptable for A3-a):

- Older Safari support is irrelevant (we ship Vite-built ES2020+ targeting modern browsers; same constraint already applied across the GUI).
- Animations are limited without polyfills.
- This is the **first** modal in the codebase — its CSS / a11y patterns become precedent for future modals (Settings, Edit-vault if ever revived). The plan should call out the pattern explicitly so it can be lifted into a shared helper component if/when a second modal use-case appears.

### D9 — Restart-result granularity: REUSE existing `api.RestartResult` (Alt 3)

**Codex memo-R2 P1 caught the most important fix:** `api.RestartResult` already exists at [install.go:1456-1460](../../../internal/api/install.go#L1456-L1460) with shape `{TaskName, Err}`. `api.Restart` already returns `[]RestartResult` (verified at the same file). A3-a MUST reuse this exact type. Inventing a parallel `{server, daemon, ok, error}` shape would either break CLI callers or create two competing contracts.

**Chosen:** reuse `api.RestartResult` everywhere. The wrapper `api.SecretsRotate` returns `(SecretsRotateResult, error)` where `SecretsRotateResult.RestartResults` is `[]api.RestartResult` (see §5.6 for full struct). The plan must verify and add `json:"task_name"` / `json:"error,omitempty"` to `api.RestartResult` if not already present — current struct has no JSON tags so will marshal as `TaskName` / `Err` until tags are added.

**Plan task 1 (backend scaffold) MUST audit `api.RestartResult`** before the new wrappers ship: confirm field names, add JSON tags if missing, verify CLI's `internal/cli/restart.go` consumers use field accessors (not JSON marshaling) so the JSON tag addition is non-breaking.

Two rejected alternatives:

1. **Parallel endpoint** `POST /api/servers/:name/restart-with-results`. Code duplication; two adapter pathways; future drift risk.
2. **Define a new `RestartResult` shape** with richer fields (`{server, daemon, ok, error}`). Rejected per Codex memo-R2: `api.RestartResult` already exists; reusing it preserves CLI behavior and keeps one source of truth.

Refactor shape (verified against existing code):

```go
// internal/api/install.go (already exists)
type RestartResult struct {
    TaskName string  // e.g. "mcp-local-hub-memory-default"
    Err      string  // empty on success
}

// internal/gui/server.go — refactor realRestarter
type restarter interface {
    // RETURNS the per-task results from api.Restart, NOT an aggregated error.
    // The aggregation happens in the handler (success vs partial vs all-fail).
    Restart(server string) ([]api.RestartResult, error)
}
```

Existing handler at `servers.go:22-32` calls `s.restart.Restart(name)`. The handler today only checks the `error`. The change:

- Handler returns `[]api.RestartResult` in a 200 JSON body when ALL `Err` fields are empty.
- Handler returns **207 Multi-Status** with `[]api.RestartResult` when ANY task has a non-empty `Err` — including the all-failed case. 207 is reserved for per-task failures when the orchestration loop completed; orchestration failures (loop did not complete) use 500 + `RESTART_FAILED` as described above. The frontend distinguishes "all OK" vs. "any failed" by inspecting the `error` JSON field of each result, plus checking the response status code (207 = per-task failures, 500 = orchestration failure with possible partial results).
- Handler returns **500 + `RESTART_FAILED`** when the orchestration itself failed (scheduler unavailable, manifest read failed, status query failed, mid-loop `api.Restart` call returned error). In this case `vault_updated:true` is always set (vault.Set ran before restart attempts), and `restart_results` carries any partial results gathered before the abort. Helper signature `restartServersForKey(key) ([]api.RestartResult, error)` returns both partial results and error so the handler can include them.

The aggregate-error behavior (single-string error from CLI's `internal/cli/restart.go`) is preserved at the **CLI layer**: `restart.go` aggregates `api.Restart`'s `[]RestartResult` into a string for human-readable output. The change is GUI-adapter-side only.

For A3-a's rotate flow, the new helper `restartServersForKey(key string) ([]api.RestartResult, error)` lives in `internal/api/secrets.go` (Codex memo-R3 P1: error return is required so the handler can distinguish per-task failures from orchestration failures) and:

1. Reads manifests via `listManifestNamesEmbedFirst()` + `loadManifestYAMLEmbedFirst()`, finds servers whose env references this key. **Manifest read failure** here returns `(nil, err)` — orchestration failure.
2. Reads daemon status (existing `api.Status()` or equivalent), filters to running daemons. **Status query failure** returns `(nil, err)` — orchestration failure.
3. Calls `api.Restart(server, "")` for each affected server. Per-server `api.Restart` errors are themselves orchestration failures and abort the loop, returning `(partial-results, err)` so the handler can surface "vault written but restart sequence aborted". Per-task failures inside a successful `api.Restart` call appear as `Err`-populated entries in the returned slice; those do NOT cause the helper to return an error.
4. Returns the flattened slice + nil error to `api.SecretsRotate` on the happy path.

`api.SecretsRotate` propagates the helper's error to the handler. The handler:

- helper returned no error → 200 (all `Err` empty) or 207 (any `Err` non-empty).
- helper returned error → **500 + `RESTART_FAILED`**, body is `{vault_updated:true, error:"…", code:"RESTART_FAILED", restart_results:[…partial results…]}`. Partial results are whatever the helper accumulated before the orchestration failure aborted the loop. Frontend should show partial results inline alongside the error so users see which daemons did succeed before the abort.

### D10 — Used-by scan boundaries: inline per GET (Alt 4)

**Chosen:** Scan inline on every `GET /api/secrets` call. No cache, no invalidation hooks.

Cost analysis (corrected per Codex memo-R1 P2):

- Typical home: ~10 manifests (current repo has exactly 10 totaling ~10 KB embed), ~5 env vars each = 50 env entries scanned per call.
- Worst plausible: ~100 manifests × ~10 env vars = 1000 entries.
- Each entry is a `strings.HasPrefix(value, "secret:")` check and a struct append into a `map[string][]UsageRef`.
- Realistic order of magnitude: **low milliseconds** (FS / embed traversal + 10–100 YAML parses dominate; the prefix check is trivial). Codex caught the original "microseconds" claim — that ignored YAML parse cost.
- Vault decryption (age + `json.Unmarshal`) on a small vault is also low milliseconds. Both are user-imperceptible, but the cost is "ms" not "μs".

Cache + invalidation rejected as premature optimization. If profiling later shows scan dominates (e.g., users with hundreds of manifests), add a `sync.Map` keyed by `(dir, mtime-checksum)` or invalidate on `ManifestCreate` / `ManifestEdit` / `ManifestDelete` events.

## 5. API contracts

### 5.1 `POST /api/secrets/init`

**Request body:** `{}` (empty JSON object).

**Behavior** (per D2 four-case classifier):

- Case 1 — `OpenVault` succeeds → no-op. **200** with `{"vault_state":"ok"}`.
- Case 2a — `OpenVault` fails AND both files are missing AND `InitVault` succeeds → **200** with `{"vault_state":"ok"}`.
- Case 2b — partial init AND wrapper cleanup succeeded → **200** with `{"vault_state":"missing", "cleanup_status":"ok", "error":"…", "code":"SECRETS_INIT_FAILED"}`. The 200 is deliberate: the system is back to the pre-init clean state and a retry will work; the body still surfaces what happened so the GUI can show "init failed; click retry".
- Case 2c — partial init AND wrapper cleanup ALSO failed → **500** + `{"error":"…", "code":"SECRETS_INIT_FAILED", "cleanup_status":"failed", "orphan_path":"/…/secrets.age"}`. The 500 reflects "manual intervention required to clean up the orphan path before retry can succeed".
- Case 3 — `OpenVault` fails AND both files exist (decrypt failure, corrupt vault, identity mismatch) → **409** + `SECRETS_INIT_BLOCKED` with a recovery hint.
- Case 4 — `OpenVault` fails AND exactly one of the two files exists (orphan from prior run, NOT this wrapper's partial init) → **409** + `SECRETS_INIT_BLOCKED` naming the orphan.

**Status mapping is single-sourced here.** §5.1 endpoint and §5.7 catalog must mirror this exactly. Codex memo-R3 P1 caught the prior version's drift; tests in §7.1 verify each case independently.

**Response shapes** (all use `writeAPIError`'s envelope `{"error": humanMessage, "code": CODE}` plus optional extras — Codex memo-R2 P2):

```json
// 200 OK
{"vault_state": "ok"}

// 200 OK — partial-failure cleanup ok
{"vault_state": "missing", "cleanup_status": "ok", "error": "init failed: …", "code": "SECRETS_INIT_FAILED"}
// (200 because the orphan was cleaned; user can retry. Body still flags the failure via error+code.)

// 500 SECRETS_INIT_FAILED — partial-failure cleanup also failed
{"error": "init failed: …", "code": "SECRETS_INIT_FAILED", "cleanup_status": "failed", "orphan_path": "/path/to/.age-key"}

// 409 SECRETS_INIT_BLOCKED (cases 3 + 4)
{"error": "vault file exists but cannot be opened: …", "code": "SECRETS_INIT_BLOCKED"}
```

**Idempotency:** safe to call repeatedly. Used at screen mount when `vault_state === "missing"` to bootstrap.

### 5.2 `GET /api/secrets`

**Behavior:** Returns the registry snapshot envelope (D3). Always 200 unless an unexpected internal error occurs (then 500 + `SECRETS_LIST_FAILED`). **No `SECRETS_VAULT_NOT_INITIALIZED` on this endpoint** — missing vault is signalled via `vault_state:"missing"` in the body, not via 4xx.

**Implementation order:**

1. `OpenVault` — determines `vault_state`. If success, `keys = vault.List()`. If file missing, `keys = nil`, `vault_state = "missing"`. If decrypt fails (wrong identity), `keys = nil`, `vault_state = "decrypt_failed"`. If JSON-decode fails post-decrypt, `keys = nil`, `vault_state = "corrupt"`.
2. `ScanManifestEnv` — independent of vault state. Returns `{key → []UsageRef{server, env_var}, []ManifestError}`.
3. **Merge** — the row state depends on BOTH vault_state AND key presence:
   - `vault_state == "ok"`:
     - For each `k in keys`: emit `{name: k, state: "present", used_by: usage[k] ?? []}`.
     - For each `k in usage` not in `keys`: emit `{name: k, state: "referenced_missing", used_by: usage[k]}`.
   - `vault_state != "ok"` (vault unavailable — keys cannot be enumerated):
     - For each `k in usage`: emit `{name: k, state: "referenced_unverified", used_by: usage[k]}`. We cannot say `referenced_missing` because the vault may have the key under another identity.
     - No "present" rows (vault is opaque).
4. Sort `secrets[]` alphabetically by `name`.

**Response:**

```json
{
  "vault_state": "ok|missing|decrypt_failed|corrupt",
  "secrets": [
    {"name":"K1","state":"present","used_by":[{"server":"s1","env_var":"K1"}, {"server":"s2","env_var":"K1"}]},
    {"name":"K2","state":"referenced_missing","used_by":[{"server":"s3","env_var":"K2"}]}
  ],
  "manifest_errors": [{"name":"x","path":"servers/x/manifest.yaml","error":"…"}]
}
```

### 5.3 `POST /api/secrets`

**Request body:** `{"name":"OPENAI_API_KEY","value":"sk-…"}`

**Behavior:**
- Same-origin guard: required.
- `OpenVault`: if missing → 409 + `SECRETS_VAULT_NOT_INITIALIZED`.
- Validate `name`: matches `^[A-Z][A-Z0-9_]*$` (env-var convention). On mismatch: 400 + `SECRETS_INVALID_NAME` with allowed regex in message.
- Validate `value`: non-empty. 400 + `SECRETS_EMPTY_VALUE` if empty.
- Duplicate check: `vault.Get(name)` returning success → 409 + `SECRETS_KEY_EXISTS`.
- `vault.Set(name, value)` — on success 201 Created with empty body.

**Response shapes** (all error responses use `writeAPIError`'s `{"error": humanMessage, "code": CODE}` envelope):

```text
201 Created  (no body)
400 {"error":"…","code":"SECRETS_INVALID_NAME"}
400 {"error":"…","code":"SECRETS_EMPTY_VALUE"}
400 {"error":"…","code":"SECRETS_INVALID_JSON"}
403 {"error":"…","code":"CROSS_ORIGIN"}
409 {"error":"…","code":"SECRETS_KEY_EXISTS"}
409 {"error":"…","code":"SECRETS_VAULT_NOT_INITIALIZED"}
500 {"error":"…","code":"SECRETS_SET_FAILED"}
```

### 5.4 `PUT /api/secrets/:key`

**Path:** `:key` is the secret name (URL-escaped if needed; only `[A-Z0-9_]` realistically used).

**Request body:** `{"value":"new-value","restart":true|false}`

**Behavior:**

- Same-origin guard: required.
- `OpenVault`: if missing → 409 + `SECRETS_VAULT_NOT_INITIALIZED`.
- Validate `value` non-empty.
- Existence check: `vault.Get(key)` failing with not-found → 404 + `SECRETS_KEY_NOT_FOUND`.
- `vault.Set(key, value)` — overwrite. On failure: 500 + `SECRETS_SET_FAILED`. **Vault commits BEFORE any restart attempt** (D4 best-effort). Once `vault.Set` succeeds, the response body always carries `vault_updated:true` regardless of subsequent restart outcomes.
- If `restart === true`:
  1. Call `restartServersForKey(key)` (helper from §D9), which iterates affected running daemons and calls `api.Restart(server, "")` per server.
  2. Returns `[]api.RestartResult` from §D9 (uses existing `{TaskName, Err}` shape; empty `Err` = success).
  3. **If orchestration itself fails** (scheduler unavailable, status query crashes): 500 + `RESTART_FAILED` with body `{vault_updated:true, error, code}`. **NOT for per-task failures** — those use 207 below.
  4. **If list is empty OR all `Err` empty:** 200 OK with body `{vault_updated:true, restart_results:[…]}`.
  5. **If ANY `Err` non-empty** (including all-failed): **207 Multi-Status** with body `{vault_updated:true, restart_results:[…]}`. Frontend distinguishes "all OK" vs. "some failed" by inspecting `restart_results[].error` (JSON field), not HTTP status. **Codex memo-R2 P1: 207 covers per-task failures including all-failed.** Note: 500 + `RESTART_FAILED` is reserved for ORCHESTRATION failures (loop crash, scheduler unavailable) — see step 3 above; that 500 path also carries `vault_updated:true` because the vault write happened before the orchestration broke. The distinction: 207 = per-task failures (orchestration loop completed), 500 = orchestration loop did not complete.
- If `restart === false`: 200 OK with body `{vault_updated:true, restart_results:[]}`. Frontend shows the persistent CTA per D4 (or suppresses it when 0 daemons running).

**Response shapes:**

**Wire shape**: each `restart_results` entry is the JSON-marshalled `api.RestartResult` with the JSON tags `task_name` / `error` (D9 mandates these tags be added to the existing struct in plan task 1). Go-side field names are `TaskName` / `Err`; JSON-side field names are `task_name` / `error`. **Frontend parsing uses `task_name` and `error` JSON keys, never `TaskName` / `Err`.**

```text
200 OK            {vault_updated:true, restart_results:[{task_name:"…", error:""}, …]}    (all OK, restart:true and at least one daemon)
200 OK            {vault_updated:true, restart_results:[]}                                  (restart:false, or 0 daemons reference key)
207 Multi-Status  {vault_updated:true, restart_results:[{task_name:"…", error:"…"}, …]}    (any per-task error non-empty, including all-failed)
404               {"error":"…", "code":"SECRETS_KEY_NOT_FOUND"}
409               {"error":"…", "code":"SECRETS_VAULT_NOT_INITIALIZED"}
500               {"error":"…", "code":"SECRETS_SET_FAILED"}                                (vault.Set failed; no vault_updated key)
500               {"error":"…", "code":"RESTART_FAILED", "vault_updated":true, "restart_results":[…partial…]}  (orchestration crash post-vault-write)
```

### 5.5 `DELETE /api/secrets/:key[?confirm=true]`

**Behavior:** D5 differential confirm + escalation — see §D5 above. Endpoint summary:

- Same-origin guard: required.
- `OpenVault`: if missing → 409 + `SECRETS_VAULT_NOT_INITIALIZED`.
- Existence check: 404 + `SECRETS_KEY_NOT_FOUND` if absent.
- Runs `ScanManifestEnv` to compute `used_by` and `manifest_errors`.
- **No `?confirm=true`** (precedence: `manifest_errors` check FIRST, refs check SECOND — Codex memo-R2 P2):
  1. If `manifest_errors != []` → **409 + `SECRETS_USAGE_SCAN_INCOMPLETE`** + `{manifest_errors:[…]}`. Fail-closed; if scan was incomplete, we can NEVER trust `used_by[]` even if it appears empty, because the broken manifest may have referenced this key. (memo-R1 P1)
  2. Else if `used_by[key]` non-empty → **409 + `SECRETS_HAS_REFS`** + `{used_by:[{server, env_var},…]}`.
  3. Else: `vault.Delete(key)`. **204 No Content** on success. **500 + `SECRETS_DELETE_FAILED`** if `vault.Delete` itself fails (disk error, encryption error, etc.).
- **With `?confirm=true`:**
  - Bypasses both scan-incomplete and refs guards.
  - `vault.Delete(key)`. **204 No Content** on success. **500 + `SECRETS_DELETE_FAILED`** if `vault.Delete` fails. **404 + `SECRETS_KEY_NOT_FOUND`** if just-deleted by another caller. **409 + `SECRETS_VAULT_NOT_INITIALIZED`** if vault disappeared.

### 5.6 `api.go` wrappers

```go
// internal/api/secrets.go (new file)
package api

type SecretsEnvelope struct {
    VaultState     string          `json:"vault_state"`
    Secrets        []SecretRow     `json:"secrets"`
    ManifestErrors []ManifestError `json:"manifest_errors"`
}

type SecretRow struct {
    Name   string     `json:"name"`
    State  string     `json:"state"` // "present" | "referenced_missing" | "referenced_unverified"
    UsedBy []UsageRef `json:"used_by"`
}

type UsageRef struct {
    Server string `json:"server"`
    EnvVar string `json:"env_var"`
}

type ManifestError struct {
    Name  string `json:"name,omitempty"`  // server name when extractable
    Path  string `json:"path"`            // full path or embed-relative path
    Error string `json:"error"`
}

// SecretsInitResult is the body of POST /api/secrets/init.
// VaultState uses omitempty so case 2c (cleanup-failed 500) can omit it
// — the vault state is undefined when manual cleanup is required.
// Codex memo-R5 P2: §5.1 case 2c body must NOT include vault_state.
type SecretsInitResult struct {
    VaultState    string `json:"vault_state,omitempty"`    // "ok" (case 1, 2a) | "missing" (case 2b, cleanup-ok); OMITTED on case 2c
    CleanupStatus string `json:"cleanup_status,omitempty"` // "ok" | "failed" — populated only on D2 case 2b/2c
    Error         string `json:"error,omitempty"`          // human message; populated on 2b (200) and 2c (500)
    Code          string `json:"code,omitempty"`           // "SECRETS_INIT_FAILED" on 2b/2c
    OrphanPath    string `json:"orphan_path,omitempty"`    // populated only on 2c (cleanup failed); names the artifact requiring manual removal
}

// SecretsRotateResult is the body of PUT /api/secrets/:key.
type SecretsRotateResult struct {
    VaultUpdated   bool            `json:"vault_updated"`
    RestartResults []RestartResult `json:"restart_results"` // existing api.RestartResult from install.go:1456 — see D9
}

func (a *API) SecretsInit() (SecretsInitResult, error)
func (a *API) SecretsListWithUsage() (SecretsEnvelope, error)
func (a *API) SecretsSet(name, value string) error
func (a *API) SecretsRotate(name, value string, restart bool) (SecretsRotateResult, error)
func (a *API) SecretsDelete(name string, confirm bool) error
```

`RestartResult` is the EXISTING type from [internal/api/install.go:1456-1460](../../../internal/api/install.go#L1456-L1460) (D9 reuses it). Plan task 1 audits its JSON tags and adds them if missing.

Each wrapper opens the vault (or returns the appropriate state for `SecretsInit` / `SecretsListWithUsage`), performs the operation, and returns typed errors. The handlers map these errors to HTTP status codes via the existing `writeAPIError` pattern, which emits `{"error": humanMessage, "code": CODE}` per [internal/gui/scan.go:28-35](../../../internal/gui/scan.go#L28-L35). When extra fields need to be attached (`used_by`, `manifest_errors`, `cleanup_status`, etc.), the response is a hand-built JSON object that includes the same `error` + `code` keys plus the extras.

### 5.7 Error code catalog

| Code                                | HTTP | Used by                           |
|-------------------------------------|------|-----------------------------------|
| `SECRETS_VAULT_NOT_INITIALIZED`     | 409  | Mutating endpoints (POST/PUT/DELETE). **GET does NOT use this code** (GET degrades gracefully via `vault_state:"missing"` in the envelope; Codex memo-R1 P2). |
| `SECRETS_INIT_FAILED`               | 200 (case 2b: cleanup ok, retryable) OR 500 (case 2c: cleanup also failed) | `POST /api/secrets/init`, partial init. Body includes `cleanup_status` + on 500 `orphan_path`. |
| `SECRETS_INIT_BLOCKED`              | 409  | `POST /api/secrets/init` — vault/key present but unreadable (cases 3 + 4). **409 not 500** — resource-state conflict. |
| `SECRETS_INVALID_NAME`              | 400  | `POST /api/secrets` — name regex mismatch. |
| `SECRETS_EMPTY_VALUE`               | 400  | `POST /api/secrets`, `PUT /api/secrets/:key` — empty value field. |
| `SECRETS_INVALID_JSON`              | 400  | Any endpoint with malformed JSON body. |
| `CROSS_ORIGIN`                      | 403  | Existing code emitted by `requireSameOrigin` middleware ([csrf.go:36](../../../internal/gui/csrf.go#L36)) on any endpoint when same-origin checks fail. **NOT a secrets-specific code** (Codex memo-R2 P2). |
| `SECRETS_KEY_EXISTS`                | 409  | `POST /api/secrets` — duplicate. |
| `SECRETS_KEY_NOT_FOUND`             | 404  | `PUT /api/secrets/:key`, `DELETE /api/secrets/:key`. |
| `SECRETS_HAS_REFS`                  | 409  | `DELETE /api/secrets/:key` without confirm — body includes `used_by:[{server, env_var},…]`. |
| `SECRETS_USAGE_SCAN_INCOMPLETE`     | 409  | `DELETE /api/secrets/:key` without confirm AND scan returned `manifest_errors != []` — fail closed (Codex memo-R1 P1). |
| `SECRETS_SET_FAILED`                | 500  | Any `vault.Set` failure (post-init). |
| `SECRETS_DELETE_FAILED`             | 500  | Any `vault.Delete` failure (post-init). |
| `SECRETS_LIST_FAILED`               | 500  | `GET /api/secrets` unexpected error. |
| `RESTART_FAILED`                    | 500  | Existing code, inherited from D9 refactor — used in PUT path when restart **orchestration** failed (scheduler unavailable, manifest read failed, status query failed, mid-loop crash). Body always carries `vault_updated:true` AND `restart_results` (partial results gathered before abort). Per-task failures use 207 + `restart_results[].Err`, NOT 500. |

**Error envelope shape:** all error responses follow the existing `writeAPIError` ([scan.go:28-35](../../../internal/gui/scan.go#L28-L35)) pattern of `{"error": humanMessage, "code": CODE}`. Endpoints that need to attach extra fields (e.g., `used_by`, `manifest_errors`, `cleanup_status`) extend the envelope with additional top-level keys.

**Frontend wrapper convention** (Codex memo-R1 P2): `secrets-api.ts` wrappers treat any 2xx HTTP status as success — including 207 Multi-Status from PUT. The response body is always parsed; `{vault_updated, restart_results}` is returned to the caller regardless of whether the status was 200 or 207. Frontend logic distinguishes "all OK" vs. "partial failure" by inspecting `restart_results[].error` (JSON key — empty string = success, non-empty = that task failed) — NOT by HTTP status code, NOT by an `ok` boolean (Codex memo-R3 P1: there is no `ok` field; the existing `api.RestartResult` Go struct is `{TaskName, Err}`, and after D9 adds JSON tags, the wire shape is `{task_name, error}`).

## 6. Risks

### R1 — Concurrent vault edits are last-write-wins

The vault has no version field analogous to manifests' `expected_hash`. Two GUI tabs (or a GUI tab + CLI invocation) racing on the same vault produce last-write-wins semantics. Specifically:

- Tab A reads the registry snapshot at T=0. Vault has `{KEY1:"oldA"}`.
- Tab B reads the same snapshot at T=1. Same content.
- Tab A rotates `KEY1` to `"newA"` at T=2. Vault now `{KEY1:"newA"}`.
- Tab B rotates `KEY1` to `"newB"` at T=3, unaware of Tab A's change. Vault now `{KEY1:"newB"}`.
- Tab A's "newA" is silently lost.

**Decision: ship A3-a without optimistic concurrency.** Cost of a proper fix (vault hash field + bump on every Set + 409 on mismatch) is comparable to a small feature in itself, has cross-CLI ramifications (CLI doesn't pass hashes), and the failure mode is "user re-rotates" which is recoverable. Documented as known limitation in `work-items/bugs/a3a-vault-concurrent-edit-lww.md` (created with the plan).

A future hash-mismatch shape, if pursued, would mirror the manifest's `expected_hash` pattern: vault file gets a hash, every Set computes a new hash, every mutation requires `If-Match: <hash>`. CLI would need to know the hash too. Out of A3-a scope.

### R2 — Vault key file lost is irrecoverable

The `.age-key` is the only way to decrypt `secrets.age`. Lose it → vault data is unrecoverable. The CLI's existing `secrets init` ([secrets.go:75-91](../../../internal/cli/secrets.go#L75-L91)) prints stern warnings. A3-a's UI **must surface the same warning at init time**:

> ⚠️ **Important.** This will create your private encryption key at:
>
> `<UserDataDir>/.age-key`
>
> If you lose this file, all encrypted secrets are unrecoverable. **Back it up via password manager / encrypted USB / secure scp** when moving to a new machine. Never commit it to git.
>
> [Cancel] [I understand — initialize vault]

The init modal requires explicit acknowledgment via the secondary confirm button. A3-a does NOT add backup-management features (export-key, key-rotation). Backup is operator's responsibility, surfaced clearly.

### R3 — Tolerant manifest scan must NOT silently lose `secret:` refs

**Codex memo-R3 P1 caught a contradiction:** the original "skip malformed env silently" prose contradicted the delete fail-closed contract. If `ScanManifestEnv` silently skips a malformed `env` block, `manifest_errors` stays empty, and `DELETE /api/secrets/:key` would happily delete a key that was actually referenced from the broken section. The fix is to **record a `ManifestError` whenever the narrow `{Name, Env}` projection fails to parse cleanly** (top-level YAML syntax error, missing `name`, or env-block strict-typing failure). Full-schema drift in unrelated fields is NOT recorded — see §2.5 and the scope note below.

Concrete behavior of `ScanManifestEnv` (scope: only `Name` and `Env` projections; full-schema validation is NOT in scope per §2.5):

1. For each manifest path, attempt `yaml.Unmarshal` into the narrow typed struct `{Name string, Env map[string]string}`. Fields outside this projection are ignored entirely — malformed `daemons` / `client_bindings` / etc. do NOT trigger a `ManifestError`.
2. If the whole document fails to parse (top-level YAML syntax error) → record `ManifestError{Name: "", Path: path, Error: err.Error()}`. Skip the manifest.
3. If parsed but `Name` is empty → record `ManifestError{Path: path, Error: "missing name field"}`. Skip the manifest.
4. If parsed but `Env` failed strict typing (any value not a string, malformed map, etc.) → record `ManifestError{Name: name, Path: path, Error: "env block did not unmarshal as map[string]string: …"}`. Skip the env scan for this manifest. **The manifest itself is still recorded as scanned (`Name` is known) but its env contributions are unknown.**
5. If both `Name` and `Env` parse cleanly → for each env value with `secret:` prefix, append `UsageRef{Server: name, EnvVar: envKey}` to `usage[trimmedKey]`.

**Codex memo-R5 P3:** the wording is deliberately scoped to `{Name, Env}` parse failures — NOT "ANY part of the manifest". Full-schema drift (e.g., a malformed `daemons` block) does not surface in `manifest_errors`. That's a deliberate scope choice: the secrets scan is responsible only for accurately aggregating `secret:` refs from valid `Name`+`Env` pairs; full-manifest validation is the existing `api.ManifestGetWithHash` path's job. Delete fail-closed therefore protects against `Name`+`Env` parse failures specifically — which is the failure mode that could conceal `secret:` refs.

Steps 2/3/4 all populate `manifest_errors`. The delete fail-closed check in §5.5 (`if manifest_errors != [] → 409`) protects against the "broken env may have referenced this key" case. Tests `TestScanManifestEnv_TolerantOnYAMLError` and `TestScanManifestEnv_MalformedEnvProducesManifestError` verify steps 2 and 4 respectively.

### R4 — D9 refactor breaks existing CLI restart aggregation

`internal/cli/restart.go` calls `api.Restart(server, daemon)` and aggregates the existing `[]api.RestartResult` into a multi-line output. The D9 refactor changes the **GUI restart adapter** (`realRestarter.Restart`), not `api.Restart` itself. CLI surface is untouched. Existing GUI tests for `/api/servers/:name/restart` need updating to match the new response shape (200 → 207 Multi-Status semantics, and `[]api.RestartResult` body with `{TaskName, Err}` fields).

Plan task #2 (D9 refactor commit) must include regression coverage for both the all-success path and the partial-failure path on `/api/servers/:name/restart`. CLI `mcphub restart` E2E or smoke verification confirms no CLI regression.

### R5 — Cross-platform path differences

`secrets.UserDataDir()` already handles Windows / Linux / macOS. A3-a inherits all path resolution from `internal/secrets/paths.go`. No new path logic in handlers or scan code — they call `secrets.DefaultKeyPath()` / `secrets.DefaultVaultPath()` / `api.defaultManifestDir()` and don't construct paths manually.

E2E tests on Windows runner only (matches existing CI policy — see `CLAUDE.md` GUI E2E section). Linux/macOS coverage deferred until scheduler test seam exists.

### R6 — Banner copy might be confusing for users without CLI access

The Q6 D6 banner points at `mcphub secrets edit` — but the CLI must be on the user's PATH for that command to work. In a typical install (where `mcphub` is on PATH), this is fine. In a niche setup (mcphub.exe in a custom dir, not on PATH), the banner copy is incomplete.

Mitigation: A3-a's banner copy stays generic. Future work could detect PATH coverage and surface "Add mcphub to your PATH" guidance, but that's a separate UX concern (CLI bootstrapping) not specific to secrets.

### R7 — Init partial-failure leaves orphan artifacts

`InitVault` writes `.age-key` first then `secrets.age` ([vault.go:38-49](../../../internal/secrets/vault.go#L38-L49)). Either `os.WriteFile(keyPath)` or the subsequent `v.save()` (which `os.WriteFile`s `vaultPath`) can fail mid-init, leaving orphan artifacts in inconsistent states:

- Key written, vault not even started → orphan key only.
- Key written, vault `os.WriteFile` truncated then failed → orphan key + zero-byte vault.
- Key write itself failed → no orphans (best case).

Without cleanup, the next `SecretsInit` call hits D2 case 4 (orphan present) and returns `SECRETS_INIT_BLOCKED` — making init non-idempotent.

Mitigation in D2 + §5.1: the wrapper detects partial failure and attempts removal of BOTH `vaultPath` and `keyPath` (in that order — vault first because the key file is needed to read the vault, so cleaning vault first is safer if cleanup is itself interrupted). Cleanup tolerates `os.IsNotExist`. The response includes `cleanup_status: "ok"` (artifacts cleared; user can retry) or `"failed"` (cleanup also failed; user must clean up the orphan path manually — path is in the response).

Tests verify both branches and both orphan shapes (`TestSecretsInit_PartialFailureCleansBothArtifacts`, `TestSecretsInit_PartialFailureKeyOnly`, `TestSecretsInit_PartialFailureCleanupAlsoFails` in §7.1).

## 7. Testing

### 7.1 Go unit tests (new)

`internal/api/secrets_scan_test.go` (Codex memo-R4 P2: file lives in `internal/api`, NOT `internal/secrets`, mirroring the production code's package placement to avoid the import cycle):

- `TestScanManifestEnv_AggregatesSecretRefs` — three manifests, two reference `KEY1`, one references `KEY2`, one literal env value, one `file:` ref, one `$VAR` ref. Assert returned `usage["KEY1"] == [{Server:"server-a", EnvVar:"…"}, {Server:"server-b", EnvVar:"…"}]` and `usage["KEY2"] == [{Server:"server-c", EnvVar:"…"}]`. Assert `manifest_errors == []`.
- `TestScanManifestEnv_TolerantOnYAMLError` — one valid manifest, one broken (truncated YAML). Returned map contains the valid manifest's refs. `manifest_errors` has one entry naming the broken file. Function does not return an error.
- `TestScanManifestEnv_IgnoresNonSecretPrefixes` — manifest with `file:`, `$VAR`, literal values only. Returned map is empty. No errors.
- `TestScanManifestEnv_SortsServersWithinKey` — two manifests reference `KEY1`, manifest order is server-z then server-a on disk. Returned `usage["KEY1"] == [{Server:"server-a", EnvVar:"KEY1"}, {Server:"server-z", EnvVar:"KEY1"}]` (sorted by Server name).
- `TestScanManifestEnv_MalformedEnvProducesManifestError` (Codex memo-R3 P1) — manifest has valid `name` but `env` is itself a YAML mapping, not a string-to-string map. Assert returned `manifest_errors[]` contains an entry with `Name == "<server>"`, `Path == "<path>"`, `Error` mentions env unmarshal. Assert `usage[]` does NOT include this manifest's would-be refs (i.e., delete fail-closed will trigger via `manifest_errors != []`, not via leaked partial refs).

`internal/api/secrets_test.go`:
- `TestSecretsInit_IdempotentOnExistingVault` — pre-create vault. Call `SecretsInit`. Assert no error, `vault_state == "ok"`, vault contents unchanged.
- `TestSecretsInit_FailsOnUnreadableVault` — pre-create vault with a different identity. Call `SecretsInit`. Assert error matching `SECRETS_INIT_BLOCKED`. Assert vault file unchanged.
- `TestSecretsInit_OrphanKey` (D2 case 4) — pre-create only `.age-key`, no vault. Call `SecretsInit`. Assert error matching `SECRETS_INIT_BLOCKED` with the orphan-key path in the message. Assert key file unchanged.
- `TestSecretsInit_OrphanVault` (D2 case 4) — pre-create only `secrets.age`, no key. Call `SecretsInit`. Assert error matching `SECRETS_INIT_BLOCKED` with the orphan-vault path in the message.
- `TestSecretsInit_PartialFailureCleansBothArtifacts` (D2 case 2 partial — Codex memo-R2 P1) — both files initially missing. Stub `os.WriteFile(vaultPath, …)` to fail AFTER it has truncated the vault file. Call `SecretsInit`. Assert error matching `SECRETS_INIT_FAILED`, `cleanup_status == "ok"`, neither key file nor vault file exists post-call (both removed), retry succeeds.
- `TestSecretsInit_PartialFailureKeyOnly` — both files initially missing. Stub vault write to fail BEFORE creating the file. Call `SecretsInit`. Assert error, `cleanup_status == "ok"`, key file removed, no orphan vault file present (it never existed), retry succeeds.
- `TestSecretsInit_PartialFailureCleanupAlsoFails` — same setup as `CleansBothArtifacts`, additionally stub `os.Remove(vaultPath)` to fail. Call `SecretsInit`. Assert error matching `SECRETS_INIT_FAILED`, `cleanup_status == "failed"`, response includes `orphan_path` field naming the partial vault file. HTTP status is 500 (cleanup failed signal).
- `TestSecretsInit_CorruptVaultJSON` — pre-create a vault file with valid age encryption but garbage JSON inside. Call `SecretsInit`. Assert `SECRETS_INIT_BLOCKED`.
- `TestSecretsListWithUsage_ReturnsEnvelope` — pre-create vault with `{K1:"v"}`, two manifests (one references `K1` under env `OPENAI_API_KEY`, one references `K2` under env `WOLFRAM`). Assert envelope: `vault_state:"ok"`, `secrets[0] == {K1, "present", [{server:"server-a", env_var:"OPENAI_API_KEY"}]}` and `secrets[1] == {K2, "referenced_missing", [{server:"server-b", env_var:"WOLFRAM"}]}`, sorted alphabetically, `manifest_errors == []`. **Asserts UsageRef object shape, not flat strings (Codex memo-R2 P1).**
- `TestSecretsListWithUsage_DegradedOnMissingVault` — no vault. Two manifests reference `K1`, `K2`. Assert envelope: `vault_state:"missing"`, both rows have `state:"referenced_unverified"` (NOT `referenced_missing` because we cannot verify absence — Codex memo-R2 P1), no error.
- `TestSecretsListWithUsage_DegradedOnDecryptFail` — pre-create vault with one identity, replace `.age-key` with a different identity. Manifest references `K1`. Assert envelope: `vault_state:"decrypt_failed"`, row has `state:"referenced_unverified"`.
- `TestSecretsSet_RejectsInvalidName` — call with `name="lower-case"`. Assert error matching `SECRETS_INVALID_NAME`.
- `TestSecretsSet_RejectsEmptyValue` — assert `SECRETS_EMPTY_VALUE`.
- `TestSecretsSet_RejectsDuplicate` — pre-create vault with `K1`. Call `SecretsSet("K1", "newval")`. Assert `SECRETS_KEY_EXISTS`.
- `TestSecretsRotate_OverwritesExisting` — pre-create with `K1:"old"`. Call `SecretsRotate("K1", "new", false)`. Assert `SecretsRotateResult{vault_updated:true, restart_results:[]}`, no error, vault contains `K1:"new"`.
- `TestSecretsRotate_RestartTrue_AttemptsBoundDaemons` — pre-create vault, two manifests both referencing `K1` with running daemons. Call with `restart:true`. Assert returned `restart_results` has two `api.RestartResult` entries, both with empty `Err` (success). Restart adapter mock asserts `api.Restart` called twice.
- `TestSecretsRotate_PartialRestartFailureKeepsVaultUpdated` — same setup, restart adapter returns error for one daemon. Assert vault still has new value, `restart_results` has one entry with empty `Err` and one with non-empty `Err`, no error returned from wrapper. **Handler maps per-task failures (orchestration loop completed) to 207 Multi-Status; orchestration failures use 500 + `RESTART_FAILED` separately. This test covers the 207 path specifically.**
- `TestSecretsRotate_AllRestartsFailReturns207` — same setup, restart adapter returns error for ALL daemons. Wrapper returns no error; handler returns 207 with all `restart_results[].Err` populated and `vault_updated:true`. Asserts the all-failed-restart case is 207 not 500.
- `TestSecretsRotate_OrchestrationFailureReturns500` — restart adapter's `api.Restart` itself fails (e.g., scheduler unavailable) before per-task results. Handler returns 500 + `RESTART_FAILED`. Body includes `vault_updated:true` because the vault write happened first. Asserts the 500 path is reserved for orchestration failure ONLY.
- `TestSecretsDelete_RequiresConfirmWithRefs` — pre-create vault with `K1`, manifest references it. Call `SecretsDelete("K1", false)`. Assert error matching `SECRETS_HAS_REFS` with `used_by[0] == {server: "X", env_var: "OPENAI_API_KEY"}`. Vault unchanged.
- `TestSecretsDelete_FailsClosedOnScanIncomplete` — pre-create vault with orphan `K1`. Seed one valid manifest (no refs to `K1`) AND one corrupt manifest (broken YAML). Call `SecretsDelete("K1", false)`. Assert error matching `SECRETS_USAGE_SCAN_INCOMPLETE` with `manifest_errors[0]` naming the corrupt path. Vault unchanged. (Codex memo-R1 P1: scan must fail closed.)
- `TestSecretsDelete_NoRefsNoConfirm` — pre-create vault with orphan `K1`. All manifests valid, no refs. Call `SecretsDelete("K1", false)`. Assert no error, vault no longer has `K1`.
- `TestSecretsDelete_WithConfirmDeletesEvenWithRefs` — pre-create vault + referencing manifest. Call `SecretsDelete("K1", true)`. Assert no error, vault no longer has `K1`. Manifest is unchanged.
- `TestSecretsDelete_WithConfirmBypassesScanIncomplete` — same scan-incomplete fixture as above. Call `SecretsDelete("K1", true)`. Assert success. Vault no longer has `K1`.

`internal/gui/secrets_test.go`:
- HTTP-level coverage of every endpoint × every error code combination. **Same-origin guard rejection emits `CROSS_ORIGIN` (NOT `SECRETS_CROSS_ORIGIN`)** (Codex memo-R2 P2) — single test asserts this on one endpoint as smoke; the middleware itself has its own coverage. Method-not-allowed (GET on POST endpoints, etc.). Body shape assertions on responses, especially:
  - `/api/secrets` GET returns `{vault_state, secrets:[…UsageRef objects…], manifest_errors}`.
  - `/api/secrets/init` returns 200 on idempotent path, 200 on cleanup-ok partial-failure, 500 on cleanup-failed, 409 on `SECRETS_INIT_BLOCKED`.
  - `/api/secrets/:key` PUT returns 200 on all-success, 207 on partial-OR-all-task-failure (NOT 500), 500 only on orchestration failure.
  - `/api/secrets/:key` DELETE returns 204 on no-refs-no-confirm, 409 + `SECRETS_USAGE_SCAN_INCOMPLETE` BEFORE 409 + `SECRETS_HAS_REFS` (precedence).

### 7.2 Frontend unit tests

`internal/gui/frontend/src/lib/secrets-api.test.ts`:
- Each typed wrapper (`secretsInit`, `getSecrets`, `addSecret`, `rotateSecret`, `deleteSecret`) tested against fetch-mocked responses. Asserts request shape (method, headers, body) and response parsing (envelope unmarshaling, error code extraction).
- Vitest: ~12 test cases.

`internal/gui/frontend/src/lib/use-secrets-snapshot.test.ts` — render the hook in isolation, assert state transitions on fetch / refetch / focus events.

### 7.3 E2E (Playwright)

New file: `internal/gui/e2e/tests/secrets.spec.ts`. Uses the same fixture infrastructure (`global-setup.ts`, `MCPHUB_E2E_SCHEDULER=none`, per-test temp `HOME`).

**Scenarios (initial set):**

1. **"Empty-state init flow"** — fresh tmpHome (no `.age-key` / `secrets.age`). Navigate to `#/secrets`. Assert empty-state banner with "Initialize secrets vault" button. Click. Assert `POST /api/secrets/init` fires. After response, screen renders the "No secrets yet" state with an Add button. Tmpfs verifies key + vault files are created.

2. **"Add first secret"** — initialized empty vault. Navigate to `#/secrets`. Click "Add secret". Modal opens. Type name `OPENAI_API_KEY` and value `sk-test`. Click Save. Assert `POST /api/secrets` fires with correct body. After response, table shows one row with `OPENAI_API_KEY`, `Used by: 0`, Rotate / Delete actions.

3. **"Used-by counts populate from manifest scan"** — initialized vault with `{K1:"v"}`. Seed two manifests, both with env `KEY: secret:K1`. Navigate to `#/secrets`. Assert table row shows `K1`, `Used by: 2`. Hover on count → tooltip lists `server-a`, `server-b`.

4. **"Ghost ref displays for manifest-only key"** — empty vault. Seed manifest with env `WOLFRAM: secret:WOLFRAM_APP_ID`. Navigate. Assert table row shows `WOLFRAM_APP_ID` with warning icon, `Used by: 1`, "Add this secret" link in the row.

5. **"Decrypt-failed vault shows broken state but keeps refs visible"** — pre-create vault with one identity, replace `.age-key` with another identity. Seed manifest referencing one key. Navigate. Assert `vault_state` banner indicates decryption failure (`vault_state == "decrypt_failed"`). Table still shows rows from manifest scan, but state is `referenced_unverified` (NOT `referenced_missing` — we cannot verify the vault contents when decrypt failed; Codex memo-R3 P2). Add / Rotate / Delete buttons disabled for `referenced_unverified` rows.

6. **"Rotate Save without restart — 0 running suppresses CTA"** (D4 + Codex memo-R1 P3) — vault with `K1`, no daemons running. Click row's Rotate. Modal shows counter (`1 daemon references this key; 0 currently running`). Type new value. Click "Save without restart". Assert `PUT /api/secrets/K1` fires with `{value, restart:false}`. Modal closes. Brief toast appears: "Vault updated. No running daemons need restart." Persistent CTA is NOT shown. After 4 seconds toast auto-dismisses.

7. **"Rotate Save without restart — N running shows persistent CTA"** — vault with `K1`, two daemons referencing it, both running. Click Rotate, type new value, "Save without restart". Assert persistent CTA appears: "Vault updated. 2 running daemons still using the previous value. [Restart now] [Dismiss]". Click Dismiss → CTA disappears. (The "Restart now" semantics — re-PUT or separate restart-batch endpoint — is a plan-time refinement; this test asserts the Dismiss path only. The Restart-now path is exercised by test 8 below.)

8. **"Rotate Save and restart with partial failure"** — vault with `K1`, two manifests referencing it, both with one running daemon each. Stub `PUT /api/secrets/K1` to return 207 with `{vault_updated:true, restart_results:[{task_name:"mcp-local-hub-server-a-default", error:""}, {task_name:"mcp-local-hub-server-b-default", error:"timeout"}]}` (using existing `api.RestartResult{TaskName, Err}` JSON tags `task_name`/`error` per D9). Trigger Rotate → Save and restart. Assert response banner: "Vault updated. 1/2 daemons restarted. 1 still need restart." Failed task listed. "Retry failed restarts" button visible.

9. **"Delete unreferenced secret — single-click confirm"** — vault with orphan `K2`. All manifests valid; no refs to `K2`. Click Delete on its row. Modal shows simple "Delete secret K2? This cannot be undone." Click Delete. Assert ONE request: `DELETE /api/secrets/K2` (NO confirm flag — backend's lack of refs returns 204 directly). Row disappears from table.

10. **"Delete with refs — escalation flow"** — vault with `K1`, manifest references it. Click Delete on row. Assert FIRST request: `DELETE /api/secrets/K1` (NO confirm flag). Assert response: 409 + `SECRETS_HAS_REFS` + `used_by:[{server:"server-a", env_var:"OPENAI_API_KEY"}]`. Modal opens with the FRESH refs from the response body. Modal shows danger copy with `server-a` listed and "Type DELETE to confirm" input. Primary button initially disabled. Type "delete" (lowercase) → still disabled. Type "DELETE" → enabled. Click. Assert SECOND request: `DELETE /api/secrets/K1?confirm=true`. Row updates: now `referenced_missing` when `vault_state == "ok"`.

11. **"Delete fails closed when scan incomplete"** — vault with orphan `K1`, no manifests reference it, but seed one corrupt manifest YAML in the disk fallback dir. Click Delete on `K1`. Assert FIRST request gets 409 + `SECRETS_USAGE_SCAN_INCOMPLETE` + `manifest_errors[]`. Modal opens with copy "Some manifests couldn't be scanned. We can't verify whether K1 is referenced. Type DELETE to delete anyway." After typed confirm: SECOND request with `?confirm=true` succeeds.

12. **"Delete with refs — direct backend 409 verification"** — direct API call (Playwright `request.delete()`) without `?confirm=true` against a referenced key. Assert 409 + `SECRETS_HAS_REFS` body + `used_by` array of `{server, env_var}` objects.

13. **"Banner shows mcphub secrets edit command"** — navigate to `#/secrets`. Assert the info banner contains the literal text `mcphub secrets edit`. Click copy button. Assert clipboard receives the command (uses Playwright's clipboard API).

14. **"Sidebar Secrets link routes correctly"** — navigate to `#/dashboard`. Click "Secrets" link in `app.tsx` nav. Assert URL is `#/secrets` and screen renders.

**Target count:** 14 new E2E tests in `secrets.spec.ts`. Total suite: 52 (current) → **66**.

### 7.4 CLAUDE.md update

The `## What's covered` E2E coverage block in `CLAUDE.md` adds a `secrets` line:

> - Secrets: empty-state init, Add modal, Used-by counts from manifest scan, ghost-refs for manifest-only keys, decrypt-failed degraded view, Rotate Save-without-restart with persistent CTA, Rotate Save-and-restart with 207 partial-failure handling, Delete differential typed-confirm (single-click for unreferenced / typed DELETE for referenced), backend 409 guard verification, sidebar nav link, mcphub secrets edit banner.

And the count line: `66 smoke tests total (3 shell + 8 servers + 6 migration + 13 add-server + 17 edit-server + 2 dashboard + 3 logs + 14 secrets)`.

## 8. Handoff to writing-plans

The plan should derive from this memo with the following commit breakdown. Each commit is independently buildable and testable; the order respects type dependencies.

1. **Backend scaffold (api.go wrappers + scan helper).** Creates `internal/api/secrets.go` (wrappers + types) and `internal/api/secrets_scan.go` (scan helper). **Both in `internal/api`, NOT `internal/secrets`** — Codex memo-R3 P1: keeping the scan in `internal/api` avoids the import cycle that would arise if a `secrets` package helper imported `api` for `manifest_source.go` access; `internal/api` already imports `internal/secrets`, not the other way. Pure additions, no behavior change to existing surfaces. Audits `api.RestartResult` JSON tags (D9 prerequisite) — adds `json:"task_name"` / `json:"error,omitempty"` if missing. Adds Go unit tests for both files. **Acceptance:** `go test ./internal/api/...` green; new wrappers usable by would-be CLI / handlers; existing CLI behavior unchanged.

2. **D9 restart-result granularity refactor.** `internal/gui/server.go`: refactor `restarter` interface to return `([]api.RestartResult, error)` using the existing `api.RestartResult{TaskName, Err}` type from `internal/api/install.go:1456`. Update `realRestarter` implementation. Update existing `/api/servers/:name/restart` handler at `internal/gui/servers.go:22-32` to return 200 (all empty `Err`), 207 Multi-Status (any non-empty `Err`, including all-failed), or 500 + `RESTART_FAILED` (orchestration crash before per-task results). Update existing tests for the new shape. **Acceptance:** `go test ./internal/gui/...` green; CLI `mcphub restart` unaffected (verify with E2E or smoke).

3. **HTTP handlers + routing.** `internal/gui/secrets.go` (new): registers all 5 endpoints, wires same-origin guard. `internal/gui/server.go`: register routes and add `s.secrets` adapter field. Includes Go handler tests for every endpoint × every error code (§7.1). **Acceptance:** all 5 endpoints respond per §5; error codes per §5.7.

4. **Frontend scaffold (no UI yet).** `internal/gui/frontend/src/lib/secrets-api.ts` (new) + Vitest unit tests. `internal/gui/frontend/src/lib/use-secrets-snapshot.ts` (new) + Vitest unit tests. No UI changes. **Acceptance:** `npm test` (Vitest) green for the new files.

5. **Secrets screen + nav entry.** `internal/gui/frontend/src/screens/Secrets.tsx` (new) renders the 4-state machine. `internal/gui/frontend/src/app.tsx` adds the `#/secrets` nav link AND the `case "secrets"` route entry to the existing screen-switch (no `Sidebar.tsx` exists — Codex memo-R3 P2). No modals yet — Add / Rotate / Delete buttons are placeholders that log to console. Banner from D6 included. **Acceptance:** screen renders all 4 states correctly with seed data; manual smoke confirms; `go generate ./internal/gui/...` regenerates assets.

6. **Add Secret modal.** `AddSecretModal.tsx` + integration into `Secrets.tsx`. Validation for name (regex) and value (non-empty). Connects to `POST /api/secrets`. **Acceptance:** Add flow works end-to-end against a real backend; modal handles 400 / 409 errors gracefully.

7. **Rotate Secret modal (3-button).** `RotateSecretModal.tsx`. Persistent CTA banner component. `PUT /api/secrets/:key` integration with both `restart:true` and `restart:false`. Per-daemon results banner for 207 responses. **Acceptance:** Rotate flow works; 207 partial-failure renders the per-daemon list with retry button.

8. **Delete Secret modal (differential).** `DeleteSecretModal.tsx`. Reads `used_by` from the cached snapshot. Differential rendering (simple vs. typed-confirm). `DELETE /api/secrets/:key?confirm=true` integration. **Acceptance:** Delete flow works in both unreferenced and referenced cases; backend 409 verified by the dedicated E2E test.

9. **Asset regeneration + E2E suite.** `go generate ./internal/gui/...`. Add `secrets.spec.ts` with all 14 scenarios (§7.3). Update `CLAUDE.md` E2E coverage block + count. **Acceptance:** Playwright suite passes 66/66.

10. **Documentation alignment + final smoke.** Verify `CLAUDE.md` count line matches actual test count post-merge. Verify backlog entry §A row A3 is marked done with link to this memo + the resulting PR. Manual smoke against a real running `mcphub gui` for the full Init → Add → Rotate (both branches) → Delete (both branches) flow. **Acceptance:** documentation freshness + manual smoke passes.

**Note on commit ordering (Codex memo-R1 P3):** the work-items entry for the LWW limitation (`work-items/bugs/a3a-vault-concurrent-edit-lww.md`) is folded into commit 1 (the "backend scaffold" commit), NOT a final commit. This way the durable record lands BEFORE the behavior ships, not as a trailing note after the limitation is already in production. Commit 1's full scope:

- `internal/api/secrets.go` (new) with the 5 wrappers + types
- `internal/api/secrets_scan.go` (new) with `ScanManifestEnv` + tests (located in `internal/api`, NOT `internal/secrets`, to avoid import cycle — see commit 1 acceptance)
- `work-items/bugs/a3a-vault-concurrent-edit-lww.md` (new) describing R1
- Go unit tests for all of the above

**Total:** 10 commits across one branch (Codex memo-R3 P3 collapsed duplicate "documentation alignment" commits). Estimated 8–14 days of focused work. Quality gates: spec-then-quality review per task per `superpowers:subagent-driven-development`; pre-execute Codex memo and plan reviews per established A2b/B1 rhythm.
