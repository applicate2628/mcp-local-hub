---
status: accepted
date: 2026-07-09
slug: absent-only-legacy-stop-watermarks
deciders: $architect (design), $lead (accepted recovery state)
context: PR #525 `fix/intent-collapse-stop-resurrection`
supersedes: none
superseded-by: none
---

# legacy_stop_watermarks representation decision

## Decision

Winner: **Lazy / absent-only**. Store `legacy_stop_watermarks` only for tasks absent from `stops`; for tasks present in `stops`, the `stops` entry itself is the durable watermark. This removes the current 2x persistence of the same `DaemonIntent` record under the shared 16 MiB intent-file cap while preserving exact-match stale-replay skipping and fail-toward-stop behavior.

Evidence: `DaemonIntent` is already minimal (`Desired`, `Reason`, `UpdatedAt`) in `internal/api/daemon_intent.go:234-238`; both daemon and supervisor intent files share `maxIntentFileBytes = 16 << 20` via `internal/api/state_read_caps.go:12-15` and `internal/api/state_read_caps.go:28-32`; collapse currently writes the same active legacy record to both `merged` and `mergedWatermarks` at `internal/api/intent_collapse.go:203-205`, `internal/api/intent_collapse.go:208-210`, and `internal/api/intent_collapse.go:214-216`; the watermark is consulted only when the task is absent from the merged `Stops` map at `internal/api/intent_collapse.go:197-205`.

Gate decision: **PASS**.

## Candidate Evaluation Table

| Candidate | P1 fix | Semantics | Abstraction | Churn / blast | Verdict |
|---|---|---|---|---|---|
| Lazy / absent-only | Yes. Active migrated stops are stored once in `stops`; a watermark exists only after that stop is later cleared. | Preserves exact equality through `daemonIntentRecordsEqual` at `internal/api/intent_collapse.go:279-280`. | Birth of watermarks moves to the stop-removal owner, `mutateStopSubBlock`, which already owns stop read-modify-write at `internal/api/stop_intent_subblock.go:270-286`. | Moderate test churn: tests that expected a watermark beside a present stop must now expect no watermark until a clear/re-enable occurs. | **Recommend**. |
|  | It directly eliminates the current duplicate map entry that is persisted at `internal/api/intent_collapse.go:506-507`. | Present stops remain authoritative through the delete gate's sub-block check at `internal/api/intent_collapse.go:610-615`; absent cleared stops still use matching watermarks at `internal/api/intent_collapse.go:614-615`. | Collapse remains a writer, but it only prunes redundant watermarks for present stops and never creates eager watermarks for present stops. | No new file, no new dependency, no public schema rename; the existing additive field in `internal/api/supervisor_intent.go:47-58` remains. |  |
| Separate capped file | Partially. Moving watermarks out of `supervisor-intent.json` avoids the supervisor-file doubling, but the active `stops` map still has to fit under the same read cap. | Can preserve exact semantics if the delete gate reads both files atomically. | Poor. Adds a second state file with its own read/write/flock/prune path and leaves the current multi-site eager watermark logic intact. | Higher operational blast: new file lifecycle, backup/delete behavior, restore behavior, and corruption handling. | Reject. |
|  | It also creates a new cap to tune and test. | It does not address the P2 rollback skip in `internal/api/register_supervisor.go:786`. | The existing state writer already has a hardened path; duplicating it for a sidecar is extra surface. | The failure mode moves from file size to split-brain state between supervisor intent and sidecar. |  |
| Compact digest | No, not fully. It shrinks each value but still duplicates one entry per active stop and one task key per active stop. | Exact record comparison becomes hash comparison unless the full record is still stored; that weakens the current exact-match invariant. | Does not fix the multi-site lifecycle smell. | Medium churn plus hash-version/collision handling. | Reject. |
|  | If `stops` itself approaches the cap, any O(N) duplicate map can still push the file over the cap. | The current equality is field-exact and time-aware at `internal/api/intent_collapse.go:279-280`; a digest would need a versioned canonical encoding contract. | P2 remains because rollback still has to restore or prune a separate watermark representation. | No advantage over absent-only for the real ownership problem. |  |

## Winner Implementation Spec

### New invariant

Keep the existing `LegacyStopWatermarks map[string]DaemonIntent` JSON field, but change its invariant:

`keys(LegacyStopWatermarks)` contains only canonical task keys that are absent from `Stops`. A watermark value is the exact `DaemonIntent` record that previously occupied `Stops[task]` and was deliberately cleared. If `Stops[task]` exists, `LegacyStopWatermarks[task]` must be absent because `Stops[task]` is the authoritative watermark.

This preserves backward-compatible JSON shape: the field is still additive and `omitempty` as defined in `internal/api/supervisor_intent.go:47-58`, and empty supervisor intents still omit it per `internal/api/supervisor_intent_test.go:78-83`.

### Change-Surface Contract

`{ intended change surface: internal/api/intent_collapse.go, internal/api/stop_intent_subblock.go, internal/api/register_supervisor.go, internal/api/supervisor_intent.go comments, internal/cli/intent_collapse_cmd.go, and matching tests; approved extension seam(s): mergeDaemonIntentStops/deleteLegacyDaemonIntentIfMerged, mutateStopSubBlock, pruneLegacyStopWatermarksForRemovedSupervisorTargets, descriptor rollback helpers; protected / must-not-touch surfaces: DaemonIntent field shape, maxIntentFileBytes and state read caps, UnifiedStopsFile/IntentStillRunning stop-source semantics, Daemons/MaintenanceTimers/StrictMode/runtime_spec preservation, state-file hardened write pipeline; declared blast radius: supervisor-intent JSON bookkeeping semantics and tests only, no new state file, no external contract break beyond dry-run explanatory text. }`

### Files and functions

1. `internal/api/intent_collapse.go`

   - Update `DaemonIntentCollapseResult.MergedLegacyStopWatermarks` comments at `internal/api/intent_collapse.go:115-121` to state "absent-only cleared tombstones," not "every accounted legacy record."
   - In `mergeDaemonIntentStops`, seed `mergedWatermarks` from prior watermarks as it does now at `internal/api/intent_collapse.go:182-186`, but normalize it so any key present in the seeded `merged` stops map is removed.
   - Remove eager writes of active legacy records into `mergedWatermarks` at `internal/api/intent_collapse.go:203-205`, `internal/api/intent_collapse.go:208-210`, and `internal/api/intent_collapse.go:214-216`.
   - Keep the stale replay skip exactly where it is: active legacy + `!hadPrior` + matching watermark at `internal/api/intent_collapse.go:197-205` still skips adding the stale legacy stop.
   - When a missing/different watermark causes a legacy active stop to be added to `merged`, prune `mergedWatermarks[key]` because the task is now present in `Stops`.
   - When a legacy active stop updates an existing `Stops` entry, prune `mergedWatermarks[key]` for the same reason.
   - When an existing `Stops` entry wins because it is newer-or-equal, prune `mergedWatermarks[key]`; the delete gate already accepts identical/newer sub-block records at `internal/api/intent_collapse.go:610-615`.
   - Continue assigning `freshSupervisorIntent.Stops` and `freshSupervisorIntent.LegacyStopWatermarks` at `internal/api/intent_collapse.go:506-507`, but the latter is now the absent-only tombstone set, not a mirror of active stops.

2. `internal/api/stop_intent_subblock.go`

   - Make `mutateStopSubBlock` the owner of watermark birth and prune for ordinary stop lifecycle transitions. It already owns all public stop writes: active/inactive `WriteStopIntent` at `internal/api/stop_intent_subblock.go:86-96`, guarded idle writes at `internal/api/stop_intent_subblock.go:154-175`, compare-and-clear at `internal/api/stop_intent_subblock.go:211-217`, and blind clear at `internal/api/stop_intent_subblock.go:252-254`.
   - Extend the lock-held body to copy `intent.LegacyStopWatermarks` alongside `intent.Stops` after the fresh read at `internal/api/stop_intent_subblock.go:303-317`.
   - After the caller mutates the copied `stops` map, apply one per-task transition rule:
     - `before != nil && after == nil`: set `LegacyStopWatermarks[taskName] = *before`.
     - `after != nil`: delete `LegacyStopWatermarks[taskName]`.
     - `before == nil && after == nil`: leave any existing absent watermark untouched.
   - Nil `Stops` and `LegacyStopWatermarks` when empty before writing, matching the existing `Stops` omitempty handling at `internal/api/stop_intent_subblock.go:338-343`.
   - A watermark-only normalization write must not emit a stop audit entry. Current callers use `changed` for audit at `internal/api/stop_intent_subblock.go:105-107`, `internal/api/stop_intent_subblock.go:180-183`, and `internal/api/stop_intent_subblock.go:258-266`; keep that boolean as "stop entry changed" or split it from "file wrote."

3. `internal/api/install_parsed_manifest.go`

   - Keep `pruneLegacyStopWatermarksForRemovedSupervisorTargets` as the uninstall/decommission owner for cleared tombstones whose daemon target is removed. It already delegates to task-key pruning at `internal/api/install_parsed_manifest.go:1177-1178`.
   - Keep the existing call sites that prune both stops and watermarks for removed supervisor targets: server merge at `internal/api/install_parsed_manifest.go:325-327`, full install at `internal/api/install_parsed_manifest.go:624-626`, and uninstall cleanup at `internal/api/install_parsed_manifest.go:1983-2003`.
   - Under absent-only, those call sites are still necessary: a cleared task has no `Stops` entry, so uninstall must prune the watermark by task ownership or the cleared tombstone can outlive the daemon row.

4. `internal/api/supervisor_intent.go`

   - Update the field comment at `internal/api/supervisor_intent.go:47-58` to say the map holds only cleared legacy stop tombstones for tasks currently absent from `Stops`.
   - Do not change the JSON field name, field type, or `omitempty`.

5. `internal/cli/intent_collapse_cmd.go`

   - Update the bookkeeping-only dry-run text at `internal/cli/intent_collapse_cmd.go:118-123`. The current message says "watermark self-heal" and describes recording a missing bookkeeping watermark. Under absent-only, the common no-entry bookkeeping delta is compaction/pruning of redundant watermarks for tasks already present in `Stops`, not self-healing a present stop.

### Collapse match and fail-toward-stop polarity

The collapse match remains exact: `daemonIntentRecordsEqual` compares `Desired`, `Reason`, and `UpdatedAt.Equal` at `internal/api/intent_collapse.go:279-280`.

The skip condition remains narrow: it fires only for active legacy records whose task is absent from the current merged stops map and whose absent-only watermark exactly matches the legacy record at `internal/api/intent_collapse.go:197-205`.

If the watermark is missing, lost by an old writer, or different, collapse still adds the active legacy stop to `Stops`. That preserves the existing documented polarity at `internal/api/intent_collapse.go:149-152`: fail toward respecting the stop, not silently starting a possibly stopped daemon.

The delete gate remains valid without eager present-task watermarks. For present tasks, `deleteLegacyDaemonIntentIfMerged` already accepts an identical or strictly newer sub-block stop at `internal/api/intent_collapse.go:610-615` and `internal/api/intent_collapse.go:641-642`. For absent cleared tasks, it still accepts the matching watermark at `internal/api/intent_collapse.go:614-615`.

### P1 size behavior

The current write persists both `Stops` and `LegacyStopWatermarks` at `internal/api/intent_collapse.go:506-507`, and the writer marshals the full supervisor intent at `internal/api/supervisor_intent.go:461-469`. Because the state reader rejects files above the kind-specific cap on both POSIX and Windows (`internal/api/hub_mcp_state_read_inode_posix.go:160-178`, `internal/api/hub_mcp_state_read_inode_windows.go:220-244`), writing a doubled supervisor intent can make the next `ReadSupervisorIntent` fail. The absent-only invariant removes the duplicated active-stop payload; a large legacy stop map collapses to one `stops` copy plus only cleared tombstones.

The regression must use the real indented JSON shape. Daemon intent writes also use `json.MarshalIndent` at `internal/api/daemon_intent.go:982-993`, and supervisor intent writes use `json.MarshalIndent` at `internal/api/supervisor_intent.go:461-462`, so the size test should not rely on compact JSON.

## P2 Guard Fix

Do not leave P2 as a one-line boolean patch if this file is already being touched. Replace the positional stop/watermark rollback booleans with a small owned artifact concept, for example a `supervisorStopArtifacts` value captured from `Stops` and `LegacyStopWatermarks` using `supervisorStopForTask`.

Current evidence:

- Upsert captures both prior stop and prior watermark at `internal/api/register_supervisor.go:500-501`.
- Descriptor removal captures both at `internal/api/register_supervisor.go:693-694`.
- `removeSupervisorIntentDescriptorAndStop` returns early on `!removed && !restoreStop` at `internal/api/register_supervisor.go:786-787`, so `restoreWatermark=true` / `restoreStop=false` can be skipped.
- The helper then prunes watermarks at `internal/api/register_supervisor.go:790-791` and restores them only later at `internal/api/register_supervisor.go:798-802`.

Spec:

- Introduce a local rollback artifact abstraction in `register_supervisor.go` that carries optional stop plus optional watermark and exposes a single "has any artifact" predicate.
- Change the early return to use that predicate: if no descriptor was removed and there is no stop or watermark artifact to restore, return; otherwise continue.
- Restore through one helper that normalizes absent-only on write:
  - If restoring a stop, write `Stops[task]` and do not write a watermark for the same task.
  - If restoring only a watermark, write `LegacyStopWatermarks[task]`.
  - If both are present because an old eager file had both, compact to the new invariant by restoring the stop and dropping the redundant watermark.
- Keep descriptor uninstall/removal pruning in this helper family; descriptor removal still owns pruning task-scoped stop artifacts when the daemon row is removed at `internal/api/register_supervisor.go:696-703`.

This makes the P2 class structurally harder to repeat because future callers pass "restore artifacts" instead of keeping `restoreStop` and `restoreWatermark` in separate guard logic.

## Test Plan

1. Add a P1 size-bound regression in `internal/api/intent_collapse_test.go`.

   - Generate a large `DaemonIntentFile{Tasks: ...}` with active stops and marshal it with `json.MarshalIndent`, matching `internal/api/daemon_intent.go:982-993`.
   - Choose the count so the raw daemon-intent fixture is under `maxIntentFileBytes` but large enough that the old eager duplicated supervisor intent would exceed the cap.
   - Write it as `daemon-intent.json`, run `RunDaemonIntentCollapse`, read raw `supervisor-intent.json`, assert `len(raw) <= maxIntentFileBytes`, and then call `ReadSupervisorIntent` successfully.
   - Assert the supervisor has all stops, zero watermarks for those present stops, and the legacy daemon-intent file was deleted.
   - Existing large-file tests cover read and backup above the small state cap at `internal/api/intent_collapse_test.go:84-115`, `internal/api/intent_collapse_test.go:117-156`, and `internal/api/intent_collapse_test.go:158-168`; this new test covers the missing collapse-output cap.

2. Rewrite collapse tests that currently expect eager present-task watermarks.

   - `TestRunDaemonIntentCollapse_MintsSupervisorIntentForLegacyOnlyActiveStop` at `internal/api/intent_collapse_test.go:276-311`: keep the stop assertion, add that no watermark exists for the present task.
   - `TestRunDaemonIntentCollapse_LegacyStopWatermarkSelfHealsExistingSubBlockStop` at `internal/api/intent_collapse_test.go:384-403`: change the expectation from "write missing watermark" to "sub-block already accounts for the legacy stop; delete is allowed; no watermark beside present stop."
   - `TestRunDaemonIntentCollapse_LegacyStopWatermarkLossFailsTowardRespectingStop` at `internal/api/intent_collapse_test.go:406-445`: keep fail-toward-stop; after the add, assert no watermark because the task is present in `Stops`.
   - `TestRunDaemonIntentCollapse_LegitimateRestopAfterClearAddsDifferentLegacyRecord` at `internal/api/intent_collapse_test.go:448-468`: assert the new stop is present and no watermark remains for that present task.
   - Fresh reread/newer sub-block tests at `internal/api/intent_collapse_test.go:1129-1176` and `internal/api/intent_collapse_test.go:1179-1215`: stop expecting collapse to write a missing present-task watermark; present sub-block records already satisfy the delete gate.
   - E2 tests at `internal/api/intent_collapse_e2_test.go:96-131` and `internal/api/intent_collapse_e2_test.go:134-164`: same rewrite - no eager watermark for present stops.

3. Preserve and sharpen stale replay tests.

   - `TestRunDaemonIntentCollapse_LegacyStopWatermarkBlocksStaleReplayAfterClear` at `internal/api/intent_collapse_test.go:313-337` stays conceptually valid: seed only an absent watermark and no stop, then assert stale legacy replay is skipped and the task is allowed to run.
   - `TestRunDaemonIntentCollapse_LegacyStopWatermarkSurvivesRealReenableWriter` at `internal/api/intent_collapse_test.go:340-380` should now assert first collapse produces no watermark while the stop is present, then `WriteStopIntent(... DesiredRunning ...)` creates the watermark when it removes the stop.
   - `TestDeleteLegacyDaemonIntentIfMerged_AllowsMatchingLegacyStopWatermark` at `internal/api/intent_collapse_e2_test.go:205-228` remains valid as an absent-watermark delete-gate test.

4. Add/adjust stop-sub-block lifecycle tests.

   - Active `WriteStopIntent` prunes an existing watermark for the same task.
   - Inactive `WriteStopIntent` and `ClearStopIntent` snapshot the departing `before` stop into `LegacyStopWatermarks`.
   - `ClearStopIntentIfReason` snapshots only when it actually removes the matching current stop; when it refuses because reason differs, it leaves both stop and watermark state unchanged.
   - Idempotent clear of an already absent task preserves an existing absent watermark and emits no clear audit.

5. Rewrite rollback tests in `internal/api/register_supervisor_rollback_test.go`.

   - Existing tests seed both `Stops` and `LegacyStopWatermarks` for the same task at `internal/api/register_supervisor_rollback_test.go:156-157` and `internal/api/register_supervisor_rollback_test.go:215-218`; change those to invariant-compliant setups.
   - For descriptor removal rollback, present-stop prior state should restore the stop and not restore a redundant watermark. Current assertions expecting both at `internal/api/register_supervisor_rollback_test.go:194-197` must change accordingly.
   - Add a dedicated watermark-only prior-state regression: seed `LegacyStopWatermarks[task]` with no `Stops[task]`, run the upsert/remove rollback path, and assert the watermark is restored. This catches the exact `!removed && !restoreStop` guard bug at `internal/api/register_supervisor.go:786-787`.

6. Keep uninstall/decommission tests, but make seed data absent-only where possible.

   - Existing tests asserting removed task watermarks are pruned and sibling watermarks survive should remain: examples include `internal/api/install_parsed_manifest_test.go:2420-2424`, `internal/api/install_parsed_manifest_lane_b_test.go:433-440`, `internal/api/phase_f_lifecycle_test.go:149-160`, and `internal/api/supervisor_intent_ownership_disambiguator_test.go:510-519`.

7. Update CLI dry-run test.

   - `TestIntentCollapseCmd_CheckExplainsWatermarkOnlyDelta` at `internal/cli/intent_collapse_cmd_test.go:70-104` currently asserts "watermark self-heal." Rename/rewrite it to assert a generic bookkeeping-only/compaction message, or remove it if the API result is extended to expose a more precise bookkeeping reason.

Recommended verification commands after implementation:

- `go test ./internal/api -run 'IntentCollapse|StopIntent|SupervisorIntent|RegisterSupervisor|ParsedManifest|PhaseF'`
- `go test ./internal/cli -run 'IntentCollapse|Supervise'`
- If runtime budget allows, `go test ./internal/api ./internal/cli`

## Claims

1. `{ guarantee: A task present in SupervisorIntentFile.Stops is never persisted with a redundant LegacyStopWatermarks entry after collapse or stop-sub-block mutation; single-owner: absent-only watermark normalization in mergeDaemonIntentStops plus mutateStopSubBlock; enforcement-probe: new/rewritten tests assert no watermark for present stops in internal/api/intent_collapse_test.go and stop_intent_subblock_test.go. }`

2. `{ guarantee: A cleared task retains an exact legacy-stop watermark that blocks stale daemon-intent replay only when Stops lacks the task; single-owner: mutateStopSubBlock creates the watermark on stop removal, mergeDaemonIntentStops reads it only in the active-and-absent branch; enforcement-probe: TestRunDaemonIntentCollapse_LegacyStopWatermarkSurvivesRealReenableWriter and TestRunDaemonIntentCollapse_LegacyStopWatermarkBlocksStaleReplayAfterClear. }`

3. `{ guarantee: Lost or mismatching watermarks still fail toward respecting the legacy stop; single-owner: mergeDaemonIntentStops; enforcement-probe: TestRunDaemonIntentCollapse_LegacyStopWatermarkLossFailsTowardRespectingStop rewritten to assert Stops contains the stop and no redundant watermark. }`

4. `{ guarantee: A large valid legacy daemon-intent stop map collapses to a supervisor-intent file readable under maxIntentFileBytes without duplicating active stop records; single-owner: absent-only representation in mergeDaemonIntentStops; enforcement-probe: new size-bound regression in internal/api/intent_collapse_test.go using ReadSupervisorIntent. }`

5. `{ guarantee: Descriptor rollback cannot skip restoring a watermark-only prior artifact; single-owner: register_supervisor.go rollback artifact helper; enforcement-probe: new register_supervisor_rollback_test.go watermark-only rollback regression targeting the prior guard at internal/api/register_supervisor.go:786-787. }`

6. `{ guarantee: No new state file, cap, dependency, or sidecar lifecycle is introduced; single-owner: existing SupervisorIntentFile schema; enforcement-probe: grep shows no new legacy-stop sidecar leaf and the JSON field remains LegacyStopWatermarks in internal/api/supervisor_intent.go. }`

## Re-commission Needed?

Recommended: **narrow re-commission for the P1 size/representation and P2 rollback-abstraction slice only**.

Reason: this decision changes the representation invariant from eager "all accounted active records" to absent-only "cleared tombstones." Any prior commission that explicitly blessed eager collapse writes at `internal/api/intent_collapse.go:203-216` is invalid for the representation. The core safety semantics remain the same: exact-match stale replay skip at `internal/api/intent_collapse.go:197-205`, fail-toward-stop on missing/different watermark at `internal/api/intent_collapse.go:149-152`, and delete-gate protection at `internal/api/intent_collapse.go:610-615` / `internal/api/intent_collapse.go:614-615`.

Do not re-commission the whole resurrection design unless the prior reviewers' acceptance depended on "watermarks mirror every active stop." Re-commission the changed claims: absent-only invariant, size-bound regression, `mutateStopSubBlock` ownership, and rollback artifact guard.
