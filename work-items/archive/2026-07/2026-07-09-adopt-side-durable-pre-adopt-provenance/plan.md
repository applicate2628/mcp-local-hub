# Plan — adopt-side durable pre-adopt provenance

Role: $planner. Source of truth: the ACCEPTED, review-cleared `design.md` (arch
PASS; security 0-P0/P1; all 6 MUST-FIX folded) + the ACCEPTED store-shape decision
`work-items/decisions/2026-07-10-adopt-provenance-store-shape.md`. No implementation
code here. All `file:line` anchors re-verified on-disk at repo HEAD `cea9219f`
(the design's `ceb01c18` anchors are unchanged in these files at HEAD).

This plan breaks the design into small, independently-reviewable delivery phases.
It changes NO architecture — the store shape, seam, schema, and blast radius are
the architect's; this plan allocates work WITHIN them.

## Scope-boundary reminders carried in (from the reviews — DO NOT normalize away)

These are the architect's Change-Surface Contract (`design.md` "Change-Surface
Contract", "Protected / must-not-touch surfaces", claims 6-9,13) + the store-shape
decision. Every phase preserves them; any phase that cannot fit inside them is a
`REVISE`-to-architect, not a plan expansion.

- **This item OWNS:** the `<state-dir>/adopted-entries.json` store (+ `.lock`), the
  `<state-dir>/adopt-provenance/<manifest>/<client>.snapshot` snapshot dir, the
  ADOPT-side capture lifecycle (`adopting`→`adopted`→abort), the fail-closed capture
  seam in `ExecuteAdoptWithOpts`, the capture UPSERT + `gcOrphanedAdoptingProvenance`,
  and `ReadAdoptProvenance` (real body, in-scope).
- **This item MUST NOT build (F7 anti-layering):** the de-adopt-owned mutators
  `MarkAdoptProvenanceDeAdopting` / `UpdateAdoptExpectedManifestHash` /
  `CloseAdoptProvenance`. They are DECLARED as **comments only** in the new file so
  the schema supports them; **no empty/stub bodies**. The `de_adopting`/`closed`
  states are schema-declared, not implemented here.
- **Must NOT touch / must NOT weaken:**
  - `internal/api/install.go` per-client block (`:2632-2710`) + shared rollback
    contract (`:2702-2708`). Capture stays in the adopt owner; NOT threaded through
    `Install` (design Alt-1 rejected; claim 6).
  - `internal/api/managed_entries.go` — `ManagedEntry` (`:120-124`), schema version
    (`:75`), demigrate readers `IsManagedEntry` (`:249-266`), `ForgetManagedEntry`
    (`:213-232`), `RecordManagedEntry` (`:181-204`), `backfillMarkerIfEntryMatchesManifest`
    (`:312-335`). Not extended (store-shape decision; claim 7).
  - The client backup lane — `writeBackup` / `pruneOldTimestamped` / `BackupKeep`
    (`clients.go:1021-1051,1145-1191`). Not modified. Only the RESTORE extraction
    `clients.RestoreEntryFromBackupForRollbackWithConfigWriter` (`clients.go:353-362`)
    is reused DOWNSTREAM by de-adopt — read-only, not this item.
  - `BuildAdoptPlan` (`adopt.go:126-202`) stays side-effect-free; capture belongs to
    Execute, never Build.
  - `adopt_secret_route.go` (`rewriteAdoptSensitiveEnv`, `persistAdoptRoutedSecrets`)
    — unchanged; ordering unchanged.
  - Default/aggressive cleanup readers; `managed-entries.json` readers.
  - Adopt CLI/API/GUI request+response shapes — byte-unchanged (capture is
    backend-internal; claim 9).
- **Snapshot hardening is mandatory (security):** the snapshot is secret-bearing
  and MUST go through `WriteStateFileBytesAtomic` (`state_file_helper.go:86`),
  owner-only handle-bound DACL + parent-gate posture, NEVER the backup lane's plain
  0600 copy (claim 8). Event bodies carry names/counts/paths only — no secret
  values, no config contents.

## Resolved implementation facts (verified this planning session)

- `validateStateFileName` (`state_paths.go:167-174`) is a **single-component
  validator, NOT an enumerated allowlist** — `adopted-entries.json` /
  `adopted-entries.lock` need **no allowlist edit** to pass through
  `writeHubMcpStateFile`/`readHubMcpStateFile` (`hub_mcp_state.go:79,166`). The
  snapshot writer takes a FULL path via `WriteStateFileBytesAtomic` (which does its
  own `MkdirAll(dir,0700)` + per-file `<path>.lock` flock), so it bypasses the
  name-validator entirely and can live under `adopt-provenance/<manifest>/`.
- Storage template to COPY (not share): `managed_entries.go` flock+read+write —
  `withManagedEntriesLock` (`:99-115`), `readManagedEntries` (`:135-156`, missing →
  empty{Version:1}, schema-version reject `:152-154`), `writeManagedEntries`
  (`:160-167`), in-place upsert (`:191-202`).
- Event emit precedent: `emitAdoptExecutedEvent` (`adopt.go:527-553`) — resolves
  state dir, `OpenSupervisorEventLog`, `SupervisorEvent` envelope, `Source:"adopt"`,
  body with `secret_routed_keys` NAMES only. New provenance events mirror this.
- `ManifestHashContent(data []byte) string` (`manifest_hash.go:17`) — SHA-256 of raw
  bytes; hash `plan.ManifestYAML` (`adopt.go:55`) at capture for BOTH manifest hashes.
- Test harness: `setupAdoptTestEnv` (`adopt_test.go:24`) redirects the state dir
  (LOCALAPPDATA + XDG_STATE_HOME) to a temp root; `failClientConfigWritesForAdoptTest`
  (`:1191`) injects client-config write failures; `mustReadFileForAdoptTest` (`:1209`);
  entryless-fanout model `TestBuildAdoptPlanPreservesFanoutToEntrylessClient`
  (`:1514-1537`); literal-secret-env adopt model near `:1722`.

## Phase overview

| Phase | Title | Files (C=create, E=edit) | Depends on | Worktree |
|---|---|---|---|---|
| A | Storage layer + `ReadAdoptProvenance` | C `internal/api/adopted_entries.go`, C `internal/api/adopted_entries_test.go` | — | yes |
| B | Capture + lifecycle functions (unwired) | E `adopted_entries.go`, C `internal/api/adopt_provenance_events.go`, C `internal/api/adopt_provenance_test.go` | A | yes |
| C | Fail-closed seam wiring into `ExecuteAdoptWithOpts` | E `internal/api/adopt.go`, E/C adopt-seam integration test | B | yes |
| D | Orphan GC + Execute-start reaper | E `adopted_entries.go`, E `adopt_provenance_events.go`, E `adopt.go` | B (funcs); sequence after C (shares `adopt.go`) | yes |
| E | Canonical docs (CLAUDE.md) | E `CLAUDE.md` (+ optional `docs/supervisor-architecture.md`) | A-D | optional |
| F1* | OPTIONAL — `managed-entries.json` tuple-recording | E `adopt.go` | C | (decision required) |
| F2* | OPTIONAL — `mcphub supervise` startup GC hook | E supervisor startup (`runSupervise`) | D | (decision + architect confirm) |

\* F1/F2 are FLAGGED decisions the orchestrator must resolve BEFORE implementing —
see "Flagged decisions". Default recommendation: DEFER both.

Execution order: **A → B → C → D → E**. NO phases run in parallel — the write
boundaries overlap (A/B/D all edit `adopted_entries.go`; C/D both edit `adopt.go`),
and the design fixes no disjoint write boundaries for parallel work, so serialize.

---

## Phase A — Storage layer + `ReadAdoptProvenance`

Pure new file. Fully self-contained and reviewable with zero edit to any existing
`.go` file. Proves the store + snapshot artifact are durable/hardened before any
adopt-path behavior depends on them.

**File scope**
- CREATE `internal/api/adopted_entries.go`:
  - Schema types (design "API-contract sketch"): `AdoptOperationState`,
    `AdoptOriginalState`, `AdoptRestoreMode`, `AdoptClientProvenance`,
    `AdoptProvenanceRecord`, `AdoptedEntries` — exact JSON tags per design
    `design.md:426-459`.
  - Consts: `adoptedEntriesFileLeaf = "adopted-entries.json"`,
    `adoptedEntriesLockFileLeaf = "adopted-entries.lock"`,
    `adoptedEntriesSchemaVersion = 1`, snapshot subdir leaf `"adopt-provenance"`.
  - `adoptedEntriesMu sync.Mutex` + `withAdoptedEntriesLock(fn func() error) error`
    — COPY `managed_entries.go:84,99-115` (in-proc mutex FIRST, then flock over
    `<state-dir>/adopted-entries.lock`).
  - `readAdoptedEntries() (*AdoptedEntries, error)` — COPY `managed_entries.go:135-156`
    (via `readHubMcpStateFile(adoptedEntriesFileLeaf)`; missing → empty{Version:1};
    version 0 normalize; version != 1 hard error).
  - `writeAdoptedEntries(m *AdoptedEntries) error` — COPY `managed_entries.go:160-167`
    (via `writeHubMcpStateFile`).
  - Snapshot helpers: `adoptSnapshotDir(manifestName string) (string, error)`
    (`<state-dir>/adopt-provenance/<manifest>`); `writeAdoptClientSnapshot(manifestName,
    client string, configBytes []byte) (ref, sha256Hex string, err error)` (→
    `WriteStateFileBytesAtomic` `state_file_helper.go:86`; whole-file
    `ManifestHashContent`-equivalent sha256 over `configBytes`; ref is state-dir-relative);
    `removeAdoptSnapshots(manifestName string) error` (`os.RemoveAll` the manifest dir).
  - `ReadAdoptProvenance(manifestName string) (*AdoptProvenanceRecord, bool, error)`
    — REAL body (in-scope): read store, linear-find by `ManifestName`, return
    (rec, found, nil); propagate read errors (fail-closed).
  - COMMENT-ONLY block declaring the three de-adopt-owned mutators
    (`MarkAdoptProvenanceDeAdopting`, `UpdateAdoptExpectedManifestHash`,
    `CloseAdoptProvenance`) per `design.md:480-486` — **no bodies (F7)**.
- CREATE `internal/api/adopted_entries_test.go`.

**Dependencies:** none (first phase).

**Acceptance criteria**
- **A1** `go build ./...` + `go vet ./...` clean; `git diff` shows ONLY the two new
  files (no edit to `adopt.go`/`install.go`/`managed_entries.go`/`clients.go`/`manifest*.go`).
- **A2** Round-trip: `writeAdoptedEntries` a record → `readAdoptedEntries` → deep-equal.
- **A3** Schema-version reject: a stored file with `version` ∉ {0,1} → read error
  (mirror `managed_entries.go:152-154`); `version:0` accepted + normalized to 1.
- **A4** Missing file → `readAdoptedEntries` returns `&AdoptedEntries{Version:1}, nil`
  (not an error).
- **A5** `writeAdoptClientSnapshot` writes to
  `<state-dir>/adopt-provenance/<manifest>/<client>.snapshot`, returns the
  state-dir-relative ref + a whole-file sha256 that equals sha256 of the on-disk
  bytes; the file is owner-only DACL/mode (reuse existing state-file hardening
  assertions — same posture proven for other `WriteStateFileBytesAtomic` outputs).
- **A6** `removeAdoptSnapshots` deletes the entire `<manifest>` snapshot dir
  (incl. any `<client>.snapshot.lock` sidecar); the snapshot path carries NO
  `.bak-mcp-local-hub-` prefix and is not a sibling of a client-config path (proves
  non-prunable location — claim 3).
- **A7** `ReadAdoptProvenance` returns (rec,true,nil) for present, (nil,false,nil)
  for absent, and propagates a read error for a corrupt store (fail-closed).
- **A8** F7: `grep -n 'func .*MarkAdoptProvenanceDeAdopting\|func .*UpdateAdoptExpectedManifestHash\|func .*CloseAdoptProvenance'`
  finds NO Go function definition — only comments.

**Tests (add):** `TestAdoptedEntriesRoundTrip` (A2), `TestAdoptedEntriesSchemaVersionReject`
(A3), `TestAdoptedEntriesMissingFileEmpty` (A4), `TestWriteAdoptClientSnapshotHardenedAndHashed`
(A5), `TestRemoveAdoptSnapshots` (A6), `TestReadAdoptProvenancePresentAbsentError` (A7).

**Checks:** `go build ./...`; `go vet ./...`;
`go test -count=1 -timeout 5m ./internal/api/ -run 'AdoptedEntries|AdoptClientSnapshot|RemoveAdoptSnapshots|ReadAdoptProvenance'`;
then sweep test daemons: `Get-Process -Name 'mcphub' -ErrorAction SilentlyContinue | Stop-Process -Force`.

**Risk / rollback:** Low. Additive new file. Rollback = delete the two files;
nothing else references them yet.

---

## Phase B — Capture + lifecycle functions (unwired)

Adds the core provenance logic + its event emitters, exercised by direct unit
tests but NOT yet called from `ExecuteAdoptWithOpts`. This isolates the correct
storage-lifecycle logic (the "core" change) from the hot-path seam edit (Phase C),
per the "split enabling phases with tighter review" rule. The functions are unused
by adopt.go this phase but are NOT dead code (their own tests call them).

**File scope**
- EDIT `internal/api/adopted_entries.go` (append):
  - `captureAdoptProvenance(plan *AdoptPlan) (*AdoptProvenanceRecord, error)` — the
    UPSERT (design "Fail-closed capture seam" + "Orphan lifecycle + upsert"):
    run the whole body under `withAdoptedEntriesLock`; FIRST remove any prior row
    for `plan.ManifestName` AND `removeAdoptSnapshots` (emit orphan-reaped
    `trigger:"upsert"` when a prior row existed); THEN for each selected client
    read its live config (`clients.AllClients()[client].ConfigPath()` → read bytes;
    classify via `client.GetEntry(entryName)` per design anchors `clients.go:136-138,
    208-209`): `present` = same-name entry exists → pin hardened snapshot +
    whole-file sha256; `absent` = config parses cleanly AND no same-name entry →
    empty snapshot_ref/sha; **a `GetEntry`/config-read/parse ERROR = capture failure
    (F4): abort with zero side effects, NEVER guessed `absent`**; compute BOTH
    `adopt_manifest_hash` + `expected_manifest_hash` = `ManifestHashContent(plan.ManifestYAML)`
    (F1); write the row `operation_state:"adopting"` (no `expected_hub_shape` — F3);
    emit captured.
  - `promoteAdoptProvenanceToAdopted(manifestName string) error` — flip
    `adopting`→`adopted` ONLY (writes no hashes); idempotent (already-`adopted` →
    no-op success); MAY re-verify `adopt_manifest_hash` vs on-disk manifest as a
    consistency check; emit committed.
  - `abortAdoptProvenance(rec *AdoptProvenanceRecord) error` — delete row +
    `removeAdoptSnapshots`; idempotent + best-effort (an abort failure is logged +
    surfaced, never masks the caller's original error); emit abort.
- CREATE `internal/api/adopt_provenance_events.go` — emit helpers mirroring
  `emitAdoptExecutedEvent` (`adopt.go:527-553`), `Source:"adopt"`, path-free +
  secret-value-free bodies: `emitAdoptProvenanceCaptured`, `emitAdoptProvenanceCommitted`,
  `emitAdoptProvenanceCaptureFailed`, `emitAdoptProvenanceAbort`,
  `emitAdoptProvenanceCommitFailed`, `emitAdoptProvenanceOrphanReaped`
  (the last is used by capture's upsert here and by GC in Phase D).
- CREATE `internal/api/adopt_provenance_test.go`.

**Dependencies:** Phase A.

**Acceptance criteria**
- **B1** build/vet clean; `git diff` shows changes only to `adopted_entries.go` +
  the new events file + the new test file; **`adopt.go` UNTOUCHED this phase**.
- **B2** (T-hash-at-capture / F1) After `captureAdoptProvenance(plan)`, the on-disk
  `adopting` row carries BOTH `adopt_manifest_hash` and `expected_manifest_hash`,
  each == `ManifestHashContent(plan.ManifestYAML)`, BEFORE any promote.
- **B3** (T-capture-upsert / F2) Pre-seed an orphan `adopting` row + snapshot dir
  for the manifest; call capture → exactly ONE row for the manifest remains, the
  stale snapshot dir was replaced (not duplicated, not two rows).
- **B4** (T-capture-read-error-fail-closed / F4) Make one selected client's config
  unreadable/corrupt so `GetEntry` returns `(nil,err)` → capture returns an error;
  the client is NEVER classified `absent`; NO committed provenance row/snapshot for
  the manifest is left behind (capture's own writes rolled back / not persisted).
- **B5** (T-secret-spelling / limit ii) Capture a plan whose source entry has a
  literal secret `env` value → the pinned snapshot preserves the ORIGINAL literal
  (not a `secret:` ref).
- **B6** Present/absent classify: a `present` client → `original_state:"present"` +
  snapshot_ref+sha; an `absent` (valid config, entry absent) client →
  `original_state:"absent"` + empty snapshot_ref/sha.
- **B7** `promoteAdoptProvenanceToAdopted` flips state, writes no hashes, idempotent;
  `abortAdoptProvenance` removes row+dir, idempotent (second call = no-op success).
- **B8** Emitted event bodies are redacted: manifest/client NAMES, counts, snapshot
  PATHS, key NAMES only — no secret VALUES, no config contents; upsert-triggered
  orphan-reaped carries `trigger:"upsert"`.

**Tests (add):** `TestCaptureAdoptProvenanceHashesAtCapture` (B2),
`TestCaptureAdoptProvenanceUpsertReapsOrphan` (B3),
`TestCaptureAdoptProvenanceReadErrorFailClosed` (B4),
`TestCaptureAdoptProvenancePreservesSecretLiteral` (B5),
`TestCaptureAdoptProvenancePresentAbsentClassify` (B6),
`TestPromoteAndAbortIdempotent` (B7), `TestAdoptProvenanceEventBodiesRedacted` (B8).

**Checks:** `go build ./...`; `go vet ./...`;
`go test -count=1 -timeout 5m ./internal/api/ -run 'CaptureAdoptProvenance|PromoteAndAbort|AdoptProvenanceEvent'`;
sweep daemons.

**Risk / rollback:** Low-medium. Functions are inert (uncalled by adopt.go).
Rollback = revert this phase's edits; Phase A storage remains intact.

**Design ambiguity (flag — do not invent):** the capture-body **atomicity boundary**.
Recommended reading: run the ENTIRE capture (remove-prior → per-client snapshots →
row write) inside `withAdoptedEntriesLock` so a provenance mutation is atomic w.r.t.
concurrent provenance ops; the nested per-snapshot flock (`WriteStateFileBytesAtomic`
takes `<snapshot>.lock`) is a consistent, deadlock-free lock order
(`adopted-entries.lock` → `<snapshot>.lock`, never reversed). Implementer confirms;
if a real reverse-order acquisition appears, escalate.

---

## Phase C — Fail-closed seam wiring into `ExecuteAdoptWithOpts`

The single behavioral-risk phase: it rewires the hot adopt path. Isolated so the
fail-closed ordering is reviewed on its own, and so a revert restores the exact
pre-item adopt behavior.

**File scope**
- EDIT `internal/api/adopt.go` `ExecuteAdoptWithOpts` (`:211-253`), per design
  "Fail-closed capture seam":
  1. Insert capture **before** `persistAdoptRoutedSecrets` (`:218`):
     `rec, err := a.captureAdoptProvenance(plan)`; on error →
     `emitAdoptProvenanceCaptureFailed(...)` + `return fmt.Errorf("adopt: capture
     pre-adopt provenance: %w", err)` (nothing irreversible has run — zero side
     effects; a currently-successful adopt is NOT regressed).
  2. **Abort on EACH of the THREE failure branches** (see ambiguity flag):
     - `persistAdoptRoutedSecrets` error path — currently the **bare `return err` at
       `:218-220` with no cleanup block** → add `abortAdoptProvenance(rec)` before it.
     - `ManifestCreate` error branch (`:222-228`) → add `abortAdoptProvenance(rec)`.
     - `Install` error branch (`:236-249`) → add `abortAdoptProvenance(rec)`.
     Abort is idempotent + best-effort; an abort failure appends to the existing
     operator error message (same shape as the current secret/manifest-cleanup notes
     at `:226,239,244`), never masking the original error.
  3. After `Install` success, **before** `emitAdoptExecutedEvent` (`:250`):
     `if err := promoteAdoptProvenanceToAdopted(rec.ManifestName); err != nil {
     emitAdoptProvenanceCommitFailed(rec.ManifestName, err) }` — NON-FATAL: Install
     committed, so a flip-write failure downgrades to a recoverable `adopting` state;
     adopt still returns success (claim 10).
- EDIT/CREATE the adopt-seam integration tests (extend `adopt_test.go` or add
  `internal/api/adopt_provenance_seam_test.go`), using `setupAdoptTestEnv` +
  `failClientConfigWritesForAdoptTest`.

**Dependencies:** Phase B (capture/promote/abort + emit helpers).

**Acceptance criteria**
- **C1** (T-capture-persisted / T1) Seed one stdio entry → `ExecuteAdopt` → open the
  store from a FRESH `API` instance (no shared in-memory state) and assert:
  (a) `adopted-entries.json` has an `adopted` record with the mapped fields;
  (b) the pinned snapshot exists at `snapshot_ref` and `snapshot_sha256` matches its
  bytes; (c) the snapshot's parsed entry equals the pre-adopt entry. FAILS if any
  provenance is only in memory.
- **C2** (T-capture-fail-closed / T3) Inject a snapshot-write failure (unwritable
  snapshot path) → `ExecuteAdopt` returns an error AND no vault key was written, no
  manifest was created, no client config changed.
- **C3** (T-abort-cleanup / T4) Inject an `Install` failure after
  capture+secrets+manifest → the pending row AND snapshot dir are removed, no orphan
  remains. ALSO assert the persist-failure and manifest-create-failure branches each
  leave no provenance orphan (all three abort sites exercised).
- **C4** (T-present-absent / T5) Adopt with an entryless-fanout client (model
  `adopt_test.go:1514`) → fanout client row `original_state:"absent"` (empty
  snapshot_ref), source client `present` (snapshot present).
- **C5** (T-promote-recoverable / T8) Inject a flip-write failure after `Install`
  success → row stays `adopting`, its `adopt_manifest_hash` + `expected_manifest_hash`
  are BOTH populated (usable by de-adopt's hash-gate), and `ExecuteAdopt` returns
  success.
- **C6** (F1 cross-check) The committed row's `adopt_manifest_hash` == the hash of
  the manifest `ManifestCreate` actually wrote to disk.
- **C7** (protected surfaces / claims 6-9) `git diff` shows NO change under
  `install.go` per-client block (`:2632-2710`) / rollback contract (`:2702-2708`),
  `managed_entries.go` (struct/schema/readers), `clients.go` backup lane, `manifest*.go`;
  adopt CLI/API/GUI request+response structs byte-unchanged (`internal/cli/adopt.go`,
  `internal/gui/adopt.go`, `api.ts` untouched).

**Tests (add):** `TestExecuteAdoptPersistsProvenanceAcrossFreshAPI` (C1),
`TestExecuteAdoptCaptureFailClosed` (C2), `TestExecuteAdoptAbortOnEachFailureBranch`
(C3), `TestExecuteAdoptPresentAbsent` (C4), `TestExecuteAdoptPromoteRecoverable` (C5),
`TestExecuteAdoptManifestHashMatchesRow` (C6).

**Checks:** `go build ./...`; `go vet ./...`;
`go test -count=1 -timeout 5m ./internal/api/ -run 'ExecuteAdopt'` (broad — reruns
ALL existing adopt tests to catch regression) + the new run set;
`go test -count=1 -timeout 5m ./internal/cli/` (confirm the adopt CLI still passes —
it is untouched but this proves it); sweep daemons.

**Risk / rollback:** HIGHEST of the code phases — edits the successful adopt path.
Named regression risk (research gate 1): a capture-step failure must NOT regress a
currently-successful adopt → C2 is the fail-closed guard. Rollback = revert the
`adopt.go` seam edits ONLY; Phases A+B remain inert (unused funcs), so reverting C
restores the exact pre-item adopt behavior with no residue.

**Design ambiguity (flag — do not invent):** the design prose says "the three
existing failure branches (`:222-228`, `:237-248`)" but lists only TWO ranges. The
`persistAdoptRoutedSecrets` error path is a **bare `return err` at `:218-220` with no
cleanup block today** — after capture is inserted before `:218`, that path DID mutate
(the provenance row + snapshots exist), so it MUST become a THIRD abort site. The
pseudocode's step-1 ("on error → abortAdoptProvenance") implies it; the prose
under-counts it. Implementer MUST add abort to all THREE sites; C3 asserts all three.

---

## Phase D — Orphan GC + Execute-start reaper

Adds the second orphan owner (the design's "(b) cross-manifest hard-crash orphan —
bounded GC"). Distinct concern from Phase B's UPSERT (the "(a) same-manifest re-run"
owner). Sequenced after C because both edit `ExecuteAdoptWithOpts`.

**File scope**
- EDIT `internal/api/adopted_entries.go` (append): `gcOrphanedAdoptingProvenance(olderThan
  time.Duration) error` — under `withAdoptedEntriesLock`: read store; for each record
  with `operation_state == "adopting"` AND `updated_at` older than `olderThan`,
  delete the row + `removeAdoptSnapshots` + emit orphan-reaped `trigger:"gc"`. Never
  reaps `adopted`/`de_adopting`/`closed` rows or a fresh `adopting` row.
- EDIT `internal/api/adopt.go` `ExecuteAdoptWithOpts` — prepend at step 0a (BEFORE
  capture): a best-effort `a.gcOrphanedAdoptingProvenance(adoptOrphanGCThreshold)`
  (default threshold 24h, a package const). A GC error is logged (warn) and
  NON-FATAL — it must NOT block a fresh adopt.
- (Uses `emitAdoptProvenanceOrphanReaped` already added in Phase B.)
- EXTEND the provenance test file.

**Dependencies:** Phase B (store + emit helper). **Sequence after Phase C** (both
edit `ExecuteAdoptWithOpts`; serialize to avoid a write conflict).

**Acceptance criteria**
- **D1** (T-gc-orphan / T12) Seed an `adopting` row + snapshot dir with `updated_at`
  older than the threshold; run `gcOrphanedAdoptingProvenance(threshold)` → the row +
  snapshot dir are gone AND a fresh (recent `updated_at`) `adopting` orphan is
  PRESERVED; an `adopted`/`de_adopting`/`closed` row is never reaped.
- **D2** The Execute-start GC call is best-effort: an injected GC read error does NOT
  fail `ExecuteAdopt` (adopt still proceeds to capture+install).
- **D3** orphan-reaped events fire for BOTH triggers — `gc` (sweep) and `upsert`
  (capture replace, from Phase B) — with bodies carrying `manifest`, `age_seconds`,
  `trigger` only (no secret values, no user-home paths).
- **D4** build/vet clean; `git diff` shows only `adopted_entries.go` + `adopt.go`
  (the one prepended line) + the test file changed.

**Tests (add):** `TestGcOrphanedAdoptingProvenance` (D1), `TestExecuteAdoptGCNonFatal`
(D2), `TestAdoptProvenanceOrphanReapedEvents` (D3).

**Checks:** `go build ./...`; `go vet ./...`;
`go test -count=1 -timeout 5m ./internal/api/ -run 'GcOrphaned|ExecuteAdoptGC|OrphanReaped|ExecuteAdopt'`;
sweep daemons.

**Risk / rollback:** Low-medium. GC is additive + best-effort at Execute start.
Rollback = revert the GC function + the one prepended line; capture/promote/abort
(Phase B/C) unaffected.

**Design ambiguity (flag — do not invent):** the design's GC guard "…AND no in-flight
adopt for that manifest." Recommended realization: this is satisfied by (i) the age
threshold — a genuinely in-flight `adopting` row is younger than 24h — and (ii) GC
running under `withAdoptedEntriesLock` at step 0a BEFORE the current adopt captures.
No separate in-flight registry is planned. Implementer confirms; if a real
concurrent-same-manifest window exists (adopt-v1 disallows it — `ManifestCreate`
refuses a pre-existing disk manifest, `adopt.go:148-152`), escalate rather than add a
new mechanism.

---

## Phase E — Canonical docs (CLAUDE.md)

Docs-only. Updates the project's canonical source-of-truth for the new durable
surfaces (canonical-source maintenance discipline).

**File scope**
- EDIT `CLAUDE.md` (repo root): add an "Adopt provenance (v0.7)" subsection
  (near the supervisor "State files" block) documenting: the `adopted-entries.json`
  (+ `.lock`) store, the `adopt-provenance/<manifest>/<client>.snapshot` snapshot dir,
  the `adopting`→`adopted` lifecycle THIS item owns (and that `de_adopting`/`closed`
  are de-adopt's), the 6 provenance events, the orphan GC (24h threshold, Execute-start
  reaper) + the residual secret-bearing-orphan boundary + the
  `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` mitigation, and the fail-closed capture ordering
  (durable before `install.go:2689`). Reference `design.md` + the store-shape decision.
- OPTIONAL consistency: if `docs/supervisor-architecture.md` enumerates state files
  (it lists `managed-entries.json` at `:46`), add the new files there too — flagged as
  optional canonical-source alignment, not required for correctness.

**Dependencies:** A-D (documents their delivered surfaces). Execute LAST.

**Acceptance criteria**
- **E1** CLAUDE.md documents `adopted-entries.json` + `adopt-provenance/` + the 6
  events + orphan GC + residual + fail-closed ordering; a `grep -n
  'adopted-entries\|adopt-provenance'` pass finds no stale/contradictory text
  (surgical-edit consistency, CLAUDE.md Step 8).
- **E2** No machine-local absolute paths introduced; state-dir-relative wording used.
- **E3** No code file changed in this phase.

**Checks:** none code (docs). Optional markdown grep per CLAUDE.md Step 8.

**Risk / rollback:** None (docs). Rollback = revert the CLAUDE.md edit.

---

## Flagged decisions (orchestrator MUST resolve before implementing — NOT invented here)

- **F1 — `managed-entries.json` tuple-recording (design Findings-for-user #1;
  decision file "Separately (orthogonal, additive)").** Adopt could additionally call
  `RecordManagedEntry(client, manifestName)` per adopted client after Install success
  (mirrors `migrate.go:287-305`) so the demigrate ownership marker stays consistent.
  It is **NOT required for de-adopt correctness** — de-adopt restoration relies solely
  on `adopted-entries.json`, and `ForgetManagedEntry` on an absent row is a harmless
  no-op (`managed_entries.go:210`).
  **Planner recommendation: DEFER** (do not include). If the orchestrator wants it,
  fold it as a 3-line additive edit into Phase C after `Install` success (touches the
  adopt owner only; inside blast radius). Keep it from growing the change surface.
  **Decision required: include (fold into C) or defer.**

- **F2 — `mcphub supervise` startup GC hook (design "Orphan lifecycle" (b): "SHOULD
  also be wired at `mcphub supervise` startup (planner decision)").** A one-line
  `gcOrphanedAdoptingProvenance` call in `runSupervise` startup so hosts that adopt
  once and never again still reap orphans. This TOUCHES supervisor startup — a surface
  OUTSIDE the design's declared blast radius (adopt Execute path + one new file). The
  per-adopt GC (Phase D) already bounds the residual.
  **Planner recommendation: DEFER to a separate tightly-scoped follow-up.** If
  included, it is a `REVISE`-to-architect to confirm the supervise-startup touch is
  inside the accepted Change-Surface Contract (the design pre-authorized it as a
  "planner decision" but it is a distinct file/owner from the declared blast radius).
  **Decision required: defer, or include-with-architect-confirmation.**

## Explicitly out of this item's scope (do NOT pull in)

- The de-adopt reverse operation and its consumer obligations (fail-closed snapshot
  gate, `liveEntryMatchesManifestBinding` shape recheck, `ManifestDelete` hash-gate,
  routed-key deletion, shared-key scan) — those are `2026-07-09-deadopt-hub-to-native`.
  This item only PRODUCES the provenance they consume. The de-adopt tamper-gate test
  (swap snapshot bytes → refuse restore) is enabled by the whole-file hash written
  here but belongs to de-adopt.
- The de-adopt-owned mutators — comment-declared only (F7).
- Per-adapter byte-equivalence probe (adjacent finding) — separate `work-items/bugs/`.
- Minimal-entry snapshot / P3-1 (Alt-2) — STANDING follow-up work-item.

## Key risks + rollback (summary)

| Risk | Owner phase | Mitigation / rollback |
|---|---|---|
| Capture failure regresses a currently-successful adopt | C | Fail-closed return before `:218` (nothing irreversible run); AC C2. Revert C → pre-item behavior. |
| Missed abort site leaves a secret-bearing orphan on a failed adopt | C | THREE abort sites (incl. the bare persist-error return); AC C3. |
| Committed-but-`adopting` row carries empty hash → de-adopt hash-gate silently SKIPs | B/C | BOTH hashes at capture (F1); AC B2 + C5. |
| Secret-bearing snapshot written with weaker-than-state-file DACL | A/B | Mandatory `WriteStateFileBytesAtomic`; AC A5. |
| Hard-crash orphan snapshot lingers | B (upsert) + D (GC) | UPSERT reaps on same-manifest retry; GC reaps cross-manifest at Execute start; ACs B3, D1. Residual documented (E1). |
| Write conflict from parallel phases | all | Serialize A→B→C→D→E (shared write boundaries). |

## Recommended next-role sequence

1. **$backend-engineer** implements A → B → C → D → E in an **isolated git worktree**
   (all code phases), TDD per phase (tests before/with impl), one phase = one
   reviewable commit.
2. **$security-reviewer** — MANDATORY gate on **Phase A** (snapshot DACL hardening,
   claim 8) and **Phase C** (secret-bearing snapshot lifecycle + fail-closed
   ordering). This design originated from a security REVISE; the snapshot is
   secret-bearing.
3. **$architecture-reviewer** — gate on **Phase C** (hot-path seam edit; protected
   surfaces claims 6-9,13 untouched) and confirm F2 if the orchestrator elects to
   include it.
4. **$qa-engineer** — gate EACH phase against its AC-ids + named tests; run the
   scoped `go test ./internal/api/` set; verify `git diff` respects the protected
   surfaces.
5. Before any push: the repo pre-push gate (CLAUDE.md Step 1) — `go build ./... &&
   go vet ./... && go test -count=1 -timeout 5m ./...` AND
   `go test -tags=test_state_path_env -count=1 -timeout 5m ./internal/api/ ./internal/cli/`,
   then the sweep + the Codex bot PASS loop + deep-security pass (Steps 2-7). (Push
   is out of scope for this plan.)

## Gate decision

**PASS.** Every phase is small, independently committable/reviewable/rollbackable,
with explicit file scope, dependencies, per-phase append-only AC-ids, named tests,
and scoped repo-standard checks. The fail-closed ordering is testable (C2/C3), the
protected-surface set is carried in and asserted (C7 + per-phase `git diff` ACs), F7
anti-layering is enforced (A8), and no phase exceeds the architect's Change-Surface
Contract — the two surface-adjacent options (F1 tuple-recording, F2 supervise-GC) are
FLAGGED as orchestrator decisions with a DEFER default, not normalized into the plan.
No implementation code is included. Next stage: $backend-engineer (Phase A).
