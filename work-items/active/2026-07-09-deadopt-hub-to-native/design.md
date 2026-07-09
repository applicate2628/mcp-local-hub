# Phase-2 de-adopt design memo

Repo state verified: `git master` HEAD in this repository during this session. This memo is read-only research plus design; no code changes are proposed as already implemented.

## Adopt-flow inventory

### Current surface reality

The current tree does not put the ADOPT action in `Servers.tsx`. `Servers.tsx` renders no-manifest client entries in `OtherMCPEntriesSection`, explicitly read-only because migrate/demigrate are no-ops without a manifest (`internal/gui/frontend/src/screens/Servers.tsx:1126-1137`). The current adopt UI is the Discovery/Migration screen: it imports `postAdopt`/`postAdoptPlan` (`internal/gui/frontend/src/screens/Migration.tsx:1-9`), shows `Adopt into hub` only for unknown stdio rows from adopt-supported clients (`internal/gui/frontend/src/screens/Migration.tsx:560-609`, `internal/gui/frontend/src/screens/Migration.tsx:739-749`), opens an adopt confirmation modal (`internal/gui/frontend/src/screens/Migration.tsx:756-870`), and submits `/api/adopt` after a successful plan (`internal/gui/frontend/src/screens/Migration.tsx:263-320`). The API client types and calls are `AdoptRequest`, `AdoptPlan`, `postAdoptPlan`, and `postAdopt` (`internal/gui/frontend/src/api.ts:77-84`, `internal/gui/frontend/src/api.ts:100-128`).

The existing Servers matrix has a different inverse-like surface: a `via-hub` cell can be unchecked and applied, which posts `/api/demigrate`, not a de-adopt operation (`internal/gui/frontend/src/screens/Servers.tsx:455-458`, `internal/gui/frontend/src/screens/Servers.tsx:540-576`, `internal/gui/frontend/src/screens/Servers.tsx:1487-1511`). That matters because `demigrate` restores/removes a client entry, but it does not delete an adopt-created manifest, remove routed adopt secrets, or release supervisor ownership.

### GUI and CLI entry points

The GUI backend registers adopt routes from `server.go` (`internal/gui/server.go:830-839`). `internal/gui/adopt.go` exposes POST `/api/adopt/plan` and POST `/api/adopt` (`internal/gui/adopt.go:46-49`). The plan route decodes the request and returns a preview from `buildGUIAdoptPlan` (`internal/gui/adopt.go:51-70`). The execute route builds the plan again, checks scoped symlink consent, calls `api.NewAPI().ExecuteAdoptWithOpts`, logs sanitized narration on failure, publishes an `operator-action` audit row with action `adopt`, and returns 201 on success (`internal/gui/adopt.go:73-123`).

The CLI has a real `adopt` command wired into the root command (`internal/cli/root.go:27-29`, `internal/cli/root.go:85`). `mcphub adopt <entry>` requires `--client`, accepts `--clients`, `--name`, `--port`, and `--yes`, builds the adopt plan, prints a dry-run unless confirmed, and then calls `a.ExecuteAdopt` (`internal/cli/adopt.go:13-57`).

### Adopt plan construction

`internal/api/adopt.go` is the owner of the backend ADOPT flow. It hard-limits supported adopt clients to stdio-capable client IDs such as `claude-code`, `codex-cli`, `cursor`, `vscode`, `gemini-cli`, `qwen-cli`, `antigravity`, `opencode`, and `mimocode` (`internal/api/adopt.go:15-32`). `AdoptPlan` contains the selected source client, entry name, manifest name, selected client set, same-name client diagnostics, explicit port, secret-routed key list, rendered manifest YAML, and an in-memory-only `secretValues` map (`internal/api/adopt.go:34-62`).

`BuildAdoptPlan` validates the source entry and client, defaults the manifest name to the entry name, rejects name mismatch, rejects existing embedded/disk manifest collisions, extracts a stdio entry from the source client, rejects disabled source entries, picks or validates an adopt port, rewrites sensitive env values, scans same-name entries across supported clients, normalizes the target client set, renders a strict stdio-bridge manifest with client bindings, validates it, and returns the plan (`internal/api/adopt.go:124-201`). The extracted source entry is a normalized command/args/env/disabled shape, not a raw pre-adopt client-config snapshot (`internal/api/scan.go:2291-2300`, `internal/api/scan.go:2303-2579`).

Sensitive env routing is part of plan construction. Literal sensitive values and missing `secret:` references are rewritten to generated `secret:<ADOPTED_KEY>` manifest refs and the real values are held in `plan.secretValues`; shell/env placeholders stay as `${env:NAME}` and do not write to the vault (`internal/api/adopt_secret_route.go:17-52`, `internal/api/adopt_secret_route.go:55-65`).

The manifest renderer emits a global stdio-bridge manifest with `name`, `kind: global`, `transport: stdio-bridge`, `command`, `base_args`, `env`, one daemon named `default` on the selected port, `client_bindings`, and `weekly_refresh: false` (`internal/api/scan.go:2624-2654`). Adopt-specific client bindings always bind each selected client to daemon `default` at `/mcp` (`internal/api/adopt.go:482-492`).

### Adopt mutations

| Mutated surface | What adopt writes | Evidence | De-adopt inverse requirement |
|---|---|---|---|
| Secret vault | Persist routed adopt secrets before manifest create; on later manifest/create/install failure, cleanup removes the keys already written. | `ExecuteAdoptWithOpts` calls `persistAdoptRoutedSecrets` first and deletes on later failures (`internal/api/adopt.go:204-253`); secret persistence validates no overwrite and cleans partial writes (`internal/api/adopt_secret_route.go:122-188`). | Delete only adopt-created routed keys after the last adopted binding for that manifest is released. Keep keys when other adopted clients still need the manifest. |
| Disk manifest | Create a new disk manifest named `plan.ManifestName` via `ManifestCreate`; create rejects existing disk or embedded names. | Adopt create call and cleanup are in `internal/api/adopt.go:218-248`; manifest create semantics are in `internal/api/manifest.go:414-489`. | If all adopted clients are de-adopted and the manifest still matches the adopt-created hash, delete the manifest. If only a subset is de-adopted, edit `client_bindings` to remove those clients. |
| Client configs | Run `Install` for the adopt-created manifest and selected clients; install writes hub loopback client entries named after the manifest. | Adopt calls `a.Install` with `Server: plan.ManifestName` and `ClientsInclude: plan.AdoptClients` (`internal/api/adopt.go:230-235`). Install maps each binding to `ClientUpdateAddReplace` with `clients.HubLoopbackURL(daemon.Port, urlPath)` (`internal/api/install.go:1620-1675`). Client writes create `clients.MCPEntry` with URL, relay server/daemon metadata, and canonical exe path (`internal/api/install.go:2679-2692`). | Restore the exact saved pre-adopt entry for clients that had one; for clients explicitly adopted with no pre-existing entry, remove the hub-owned entry. |
| Client-config backups | Install backs up each client config before writing and rolls back from that backup on install failure. | Install calls `client.BackupKeep` before `AddEntryWithConfigWriter` and registers rollback with `RestoreEntryFromBackupForRollbackWithConfigWriter` (`internal/api/install.go:2642-2708`). Backup files are timestamped full copies; `BackupKeep` prunes according to keep count, while plain `Backup` does not prune (`internal/clients/clients.go:184-195`, `internal/clients/clients.go:1011-1050`). | De-adopt cannot rely on an unrecorded "newest backup" heuristic. Adopt must record a durable per-client pre-adopt snapshot or pinned backup ref. |
| Supervisor intent | Global stdio-bridge install writes descriptor rows into `supervisor-intent.json`; daemon start is delegated to the supervisor reconcile path. | Global parsed install builds supervisor intent descriptors (`internal/api/install.go:1582-1600`), writes merged intent under lock (`internal/api/install_parsed_manifest.go:586-663`), installs client config with scheduler tasks skipped (`internal/api/install_parsed_manifest.go:684-698`), records desired running state and nudges supervisor reconcile (`internal/api/install_parsed_manifest.go:752-783`). | If no bindings remain for the adopted manifest, remove the manifest's supervisor-intent descriptors, nudge supervisor reconcile, and kill removed targets only after the nudge path permits it. |
| Audit / event | Adopt logs a supervisor event and the GUI publishes an `operator-action` row. | Adopt event body includes client, entry, manifest, port, and secret-routed keys (`internal/api/adopt.go:527-552`); GUI publishes action `adopt` after commit (`internal/gui/adopt.go:106-116`). | De-adopt should emit symmetric backend event and GUI `operator-action` with target clients, manifest action, and restart-required state. |
| Managed-entry marker | Current adopt path does not record `managed-entries.json`; that marker is documented as written by migration after `AddEntry`, and observed callers are migrate/serena reconcile, not adopt. | Marker purpose and file shape are documented in `internal/api/managed_entries.go:1-44`; `RecordManagedEntry` writes a `(client, server)` tuple (`internal/api/managed_entries.go:169-203`); migrate calls it after adapter `AddEntry` (`internal/api/migrate.go:287-305`); serena reconcile does the same (`internal/api/serena_client_reconcile.go:389-403`). `ExecuteAdoptWithOpts` finishes with `a.Install`, event emission, and stdout, with no marker step (`internal/api/adopt.go:204-253`). | Phase 2 should add adopt-owned provenance, and should either also record managed-entry tuples or define a stronger adopt-provenance marker that de-adopt owns. |

### How adopted state is recognized today

Scan does not have a durable "adopted" marker. It classifies a row as `via-hub` when live client config points at the expected hub loopback/relay shape for a manifest daemon, and sets `Managed` to true only for `via-hub` (`internal/api/types.go:145-163`, `internal/api/scan.go:795-805`, `internal/api/scan.go:2012-2143`). `via-hub-inherited` is intentionally not managed because the hub cannot demigrate a layer it did not write (`internal/api/scan.go:2145-2157`; frontend mirror in `internal/gui/frontend/src/lib/routing.ts:481-515`).

The hub aggregate gate is manifest-driven. Gate-ON detection reads the live reserved `mcphub-hub` client entry rather than `managed-entries.json` (`internal/api/hub_gate_detect.go:1-23`, `internal/api/hub_gate_detect.go:33-65`). The resolver snapshot is populated from each manifest's `ClientBindings` (`internal/api/hub_mcp_resolver.go:181-188`, `internal/api/hub_mcp_resolver.go:223-235`, `internal/api/hub_mcp_resolver.go:279-299`). `/clients/<client>/mcp` maps to the bare client scope key (`internal/api/hub_mcp_handler.go:121-151`, `internal/api/hub_mcp_handler.go:904-915`), initialize captures `snap.Bindings[scopeKey]` as intended participants (`internal/api/hub_mcp_handler.go:609-636`), and tools/list drops bindings that disappeared from the current live snapshot (`internal/api/hub_mcp_aggregator.go:295-342`, `internal/api/hub_mcp_aggregator.go:345-387`, `internal/api/hub_mcp_aggregator.go:1414-1430`).

Manifest create/edit/delete GUI routes already republish the live resolver snapshot after `client_bindings` changes when the gate-ON listener is active; the failure shape is durable-write-success plus `restart_required` rather than a failed mutation (`internal/gui/manifest.go:169-224`, `internal/gui/manifest.go:269-275`, `internal/gui/manifest.go:356-365`, `internal/gui/manifest.go:499-508`). Adopt currently creates its manifest through `api.ManifestCreate` directly (`internal/api/adopt.go:221`), while the API manifest write itself only writes the file (`internal/api/manifest.go:457-489`). De-adopt must explicitly use the existing manifest-mutation republish/reconcile owner; otherwise the durable manifest edit/delete and live `/clients/<client>/mcp` aggregate can diverge.

## De-adopt reverse-mutation design

### Backend owner

The de-adopt owner should be the adopt pipeline in `internal/api/adopt.go` or a sibling `internal/api/deadopt.go` owned by the same API layer. The repo already states that CLI and GUI must call `internal/api` instead of reaching into clients/scheduler/config directly (`internal/api/api.go:1-11`). De-adopt should extend that owner instead of creating a separate state-sync path.

Use existing owners for sub-operations:

- Client config writes: client adapters and config-lock wrappers, because `AddEntryWithConfigWriter`, `RestoreEntryFromBackupWithConfigWriter`, and `RemoveEntry` already serialize per config path (`internal/clients/config_lock.go:32-50`, `internal/clients/config_lock.go:207-238`, `internal/clients/config_lock.go:247-268`).
- Manifest mutation: manifest edit/delete helpers, with hash checking for edited manifests (`internal/gui/manifest.go:321-365`, `internal/api/manifest.go:774-800`).
- Supervisor intent release: a strict API-level variant of the existing uninstall cleanup that removes server descriptors, nudges reconcile, and kills removed targets after the nudge result permits it (`internal/api/install_parsed_manifest.go:1885-1909`, `internal/api/install_parsed_manifest.go:1914-2023`, `internal/api/install_parsed_manifest.go:2064-2093`).
- Gate-ON aggregate convergence: reuse `BuildHubReconcilePlan` and `ApplyHubReconcileInOrder`; that planner is the current owner of gate ON/OFF transitions (`internal/api/install_hub_reconcile.go:1-10`, `internal/api/install_hub_reconcile.go:32-42`, `internal/api/install_hub_reconcile.go:69-85`, `internal/api/install_hub_reconcile.go:326-329`).

### Required adopt-side provenance

Current adopt is not sufficient for a clean inverse. `AdoptPlan` stores normalized source command/args/env and rendered manifest data, not a raw pre-adopt entry snapshot (`internal/api/adopt.go:44-58`, `internal/api/adopt.go:154-187`). Install makes backups for rollback but does not return or persist a backup ref to adopt (`internal/api/install.go:2642-2708`). Sensitive literals may be rewritten into hub vault refs before manifest creation (`internal/api/adopt_secret_route.go:17-52`), so reconstructing native config from the generated manifest can lose the original literal-vs-secret/env spelling.

Design input required before safe de-adopt:

1. Add an adopt provenance file in the hardened state-dir pattern, e.g. `<state-dir>/adopted-entries.json`, owned by the adopt API. The state-file helper already exists for hardened JSON state and explicitly says cross-file consistency should serialize at a higher level when needed (`internal/api/state_file_helper.go:1-10`, `internal/api/state_file_helper.go:70`).
2. Record one row per adopt-created manifest:
   - `manifest_name`, source entry name, source client, selected clients, selected port.
   - Adopt-generated manifest hash and current expected hash.
   - Per-client original state: `present` with a pinned backup ref or serialized adapter snapshot, or `absent` for explicit fanout clients that had no pre-adopt entry.
   - Per-client original config-shape hash and expected hub-managed live shape.
   - Adopt-created routed secret keys.
   - Operation state: `adopting`, `adopted`, `de_adopting`, `closed`.
3. Prefer a pinned backup ref over duplicating raw config blobs. Backups are already exact full-file copies with 0600 mode (`internal/clients/clients.go:1011-1050`), and restore helpers already know how to put one named entry back or remove it if absent (`internal/clients/clients.go:220-252`). If a client adapter cannot restore byte-equivalent state from a backup, the row must mark the restore mode as functional-equivalent only.
4. Write a pending provenance row before the first irreversible adopt mutation, then mark it `adopted` only after install succeeds. Existing adopt cleanup removes routed secrets and manifest on downstream failure (`internal/api/adopt.go:218-248`), so the provenance cleanup should be part of the same failure handling. A crash with `adopting` plus live hub shape is recoverable: de-adopt can resume using the saved snapshots, or adopt can mark the row committed after verifying manifest/client shapes.

ASSUMPTION (UNVERIFIED): every client adapter can restore a byte-equivalent entry from a pinned backup for the adopted entry name. The code proves backup/restore support exists at the interface level (`internal/clients/clients.go:114-195`, `internal/clients/clients.go:220-252`), but byte-equivalence should be verified per adapter before promising it in user-facing copy.

### Trigger surfaces

GUI:

- Add an explicit `De-adopt to native` action for adopt-provenance rows. Do not overload the existing Servers matrix uncheck-to-demigrate flow, because that flow calls `/api/demigrate` and has narrower semantics (`internal/gui/frontend/src/screens/Servers.tsx:540-576`, `internal/api/demigrate.go:42-70`).
- Show the action on `via-hub` rows/cells only when backend scan/de-adopt plan says the server is adopt-owned by this hub. `via-hub-inherited` must remain read-only because scan and routing already define it as not hub-ownable (`internal/api/scan.go:2145-2157`, `internal/gui/frontend/src/lib/routing.ts:481-515`).
- The most consistent current placement is the Discovery/Migration screen's managed/via-hub section plus a matching Servers row/cell action. The current adopt affordance is in Discovery/Migration (`internal/gui/frontend/src/screens/Migration.tsx:560-609`), while Servers already carries hub routing state and per-cell actions (`internal/gui/frontend/src/screens/Servers.tsx:413-430`, `internal/gui/frontend/src/screens/Servers.tsx:1487-1511`).
- Add POST `/api/deadopt/plan` and POST `/api/deadopt` or `/api/adopt/deadopt/plan` and `/api/adopt/deadopt`; keep Same-Origin and request/response style identical to adopt (`internal/gui/adopt.go:46-123`).
- Return `restart_required`/`hub_live` when a durable manifest binding mutation succeeded but live gate-ON republish failed, matching manifest create/edit/delete routes (`internal/gui/manifest.go:169-224`).

CLI:

- Add `mcphub de-adopt <server>` with alias `deadopt`.
- Flags: `--client <name>` (repeatable or comma list), `--clients <list>`, `--all`, `--yes`, `--dry-run`, and an explicit legacy escape hatch such as `--reconstruct-legacy` for no-provenance rows. Default must be dry-run unless `--yes`, matching adopt (`internal/cli/adopt.go:13-57`).
- CLI should use `api.BuildDeAdoptPlan` and `api.ExecuteDeAdoptWithOpts`. If gate ON is active and the live GUI hub must republish an in-memory resolver, the CLI must either call an existing live control-plane endpoint or report that a hub restart/reconcile is required. Current CLI manifest delete only removes the manifest file and does not republish another process's live snapshot (`internal/cli/manifest.go:187-209`, `internal/api/manifest.go:774-800`); de-adopt should not silently inherit that limitation for the aggregate requirement.

### Reverse mutation algorithm

Plan phase:

1. Load adopt provenance. If no committed provenance row exists for `(manifest, client)` then fail closed. Existing demigrate can remove hub-owned entries with marker corroboration, but it cannot restore adopt-created native snapshots or release the adopt manifest (`internal/api/demigrate.go:271-295`, `internal/api/demigrate.go:309-348`).
2. Load the current manifest and verify the hash equals the adopt-created or last de-adopt-updated hash. If an operator edited the manifest since adopt, fail closed and show a merge prompt. Manifest edit hash conflicts are already a first-class conflict surface in the GUI (`internal/gui/manifest.go:337-345`).
3. Validate the live target client shape:
   - Gate OFF: the per-server client entry must still be the expected hub loopback/relay shape for that manifest binding. Existing demigrate refuses marker-backed removal when the live URL no longer matches the manifest-managed shape (`internal/api/demigrate.go:417-429`).
   - Gate ON: the client may only have the aggregate `mcphub-hub` entry; gate detection is based on that reserved entry (`internal/api/hub_gate_detect.go:1-23`, `internal/api/hub_gate_detect.go:33-65`). The manifest binding and live resolver snapshot are the authoritative aggregate membership source (`internal/api/hub_mcp_resolver.go:279-299`, `internal/gui/hub_listener.go:891-900`).
4. Build a restore plan from provenance:
   - If the original client entry was present, restore that snapshot/backup.
   - If the original entry was absent, remove the hub-owned per-server entry in gate-OFF mode or only remove aggregate membership in gate-ON mode. This case is required because adopt can intentionally fan out to an entryless client (`internal/api/adopt_test.go:1510-1539`).
5. Build a manifest mutation plan:
   - Remove target clients from `client_bindings`.
   - If bindings remain, write the edited manifest and keep supervisor intent and routed vault keys.
   - If no bindings remain, delete the adopt-created manifest, remove supervisor intent descriptors, forget managed/provenance ownership, and delete adopt-created routed keys.
6. Build a gate reconcile plan:
   - Gate OFF: no aggregate rewrite is needed beyond restoring/removing the target per-server entry.
   - Gate ON: after manifest binding removal, republish the resolver snapshot so `/clients/<target>/mcp` no longer contains the de-adopted server. Existing sessions also revalidate tools/list against the current snapshot and drop removed bindings (`internal/api/hub_mcp_aggregator.go:345-387`).
   - If the target client has no remaining bindings, current gate-ON `BuildHubReconcilePlan` will not emit a removal for `mcphub-hub`, because it only iterates clients with at least one binding (`internal/api/install_hub_reconcile.go:121-149`, `internal/api/install_hub_reconcile.go:181-260`). De-adopt needs a small extension in the hub reconcile owner to prune a stale aggregate for selected clients with zero remaining bindings, mirroring the existing gate-OFF all-client aggregate sweep (`internal/api/install_hub_reconcile.go:150-179`).

Execute phase:

1. Acquire the de-adopt/adopt provenance lock. For gate-ON live aggregate changes, also use the existing `hub-mcp.lock` publish/reconcile owner; resolver publish is explicitly serialized under that lock (`internal/api/hub_mcp_resolver.go:423-545`).
2. Mark provenance row `de_adopting` with a resume step and expected before/after hashes.
3. Restore native client config before removing the hub binding from durable topology. This follows the existing "adds before removes" safety rule in hub reconcile, where AddReplace runs before Remove within a client config rewrite (`internal/api/install_hub_reconcile.go:231-260`, `internal/api/install_hub_reconcile.go:326-329`). The short duplicate-routing window is safer than an outage window.
4. Mutate the manifest binding set or delete the adopt-created manifest. If deleting, remove supervisor intent via the existing uninstall cleanup and nudge/kill sequence (`internal/api/install_parsed_manifest.go:1885-1909`, `internal/api/install_parsed_manifest.go:1999-2023`, `internal/api/install_parsed_manifest.go:2064-2093`).
5. If gate ON is live, republish the resolver snapshot and run the hub reconcile extension so aggregate config and in-memory bindings converge. Manifest routes already treat republish failure as restart-required rather than rolling back the durable mutation (`internal/gui/manifest.go:192-224`); de-adopt should return the same shape.
6. Forget managed-entry/provenance rows for completed clients. Existing `ForgetManagedEntry` is idempotent for absent rows (`internal/api/managed_entries.go:206-231`).
7. Delete adopt-created routed secret keys only when the manifest no longer has any remaining adopted binding.
8. Emit backend event and GUI `operator-action` audit row.

### In-flight client sessions

De-adopt is a config/topology mutation, not a live client-session migration. Existing client sessions pointed at a per-server hub URL or at `/clients/<client>/mcp` may continue until the client disconnects. New sessions should use the restored native entry after the client reloads config. For gate ON, existing aggregate sessions revalidate tools/list against the current resolver snapshot and drop removed bindings when the snapshot changes (`internal/api/hub_mcp_aggregator.go:345-387`), but tools/call can still reject moved/removed routes with `-32601` after a binding leaves scope (`internal/api/hub_mcp_aggregator.go:43-50`, `internal/api/hub_mcp_aggregator.go:809-816`). The GUI/CLI should explicitly tell the operator to restart or reconnect the client to complete the switch.

## Round-trip invariants + failure modes

### Round-trip invariant

Primary invariant: `adopt -> de-adopt` restores every selected client's MCP config to the pre-adopt entry state for that server, and releases every hub-owned artifact that adopt created for that client.

- If adopt records a pinned backup/snapshot, the target is byte-equivalent for the restored entry where the adapter supports byte-equivalent restore.
- If byte-equivalence is impossible for an adapter, the target is functional equivalence: same server name, transport, command, args, env values, disabled state, and adapter-owned metadata needed by the client. This fallback must be declared in the de-adopt plan.
- For explicit fanout clients that had no pre-adopt entry, the restored state is absence of that server entry.
- If no bindings remain for the adopt-created manifest, de-adopt must remove the manifest, supervisor-intent descriptors, routed adopt secrets, and ownership/provenance records.
- If bindings remain for other clients, de-adopt must remove only target client bindings/provenance and leave the daemon/manifest/secrets running for remaining clients.

### Failure modes

| Failure mode | Behavior |
|---|---|
| Native config externally edited since adopt | Fail closed before mutation if live target does not match expected hub shape or if restoring would overwrite a different direct entry. Surface a compare/merge prompt in GUI. Existing demigrate already fails closed on marker-backed removal when the live entry no longer matches expected hub-managed shape (`internal/api/demigrate.go:417-429`). |
| Manifest externally edited since adopt | Fail closed on manifest hash mismatch. User must choose: keep manifest and only restore native client, edit manifest manually, or force a merge that removes only matching target bindings. Existing manifest edit has an expected-hash conflict path (`internal/gui/manifest.go:337-345`). |
| No adopt provenance | Fail closed by default. Current adopt does not persist a raw snapshot or adopt marker (`internal/api/adopt.go:44-58`, `internal/api/adopt.go:204-253`). A legacy `--reconstruct-legacy` mode may create a functional native stdio entry from manifest command/args/env, but it cannot guarantee original secret spelling after adopt secret routing (`internal/api/adopt_secret_route.go:17-52`). |
| Hub never fully adopted the server | Fail closed unless the provenance row is in a recoverable pending state and live disk state corroborates it. Adopt cleanup already removes created manifest/secrets when install fails (`internal/api/adopt.go:218-248`), so a missing manifest plus pending provenance should be treated as abortable cleanup, not as a de-adopt candidate. |
| De-adopt while daemon is quarantined/backing off | Allow de-adopt if provenance and disk shapes are valid. Removing desired supervisor intent is independent of daemon health; the supervisor reconcile path already treats active stops/quarantine as runtime state, while descriptor removal cleanup owns teardown (`internal/cli/supervise_reconcile.go:13-23`, `internal/api/install_parsed_manifest.go:2015-2093`). Do not require the daemon to be healthy to release ownership. |
| Gate ON with other clients still routing | Remove only target client bindings from the manifest/provenance. Republish the resolver so `/clients/<target>/mcp` drops that server, but preserve other clients' bindings and aggregate entries. If target has no remaining hub bindings, prune `mcphub-hub` for that client via the hub reconcile owner extension. |
| Partial failure after native restore but before manifest update | Leave a recoverable `de_adopting` row. A retry sees native snapshot restored plus hub binding still present and resumes at manifest/aggregate release. This may temporarily expose both native and hub aggregate routes, but avoids losing the server. |
| Partial failure after manifest delete but before supervisor intent removal | Keep the provenance row until supervisor intent cleanup completes. Retry can remove descriptors by server name using the existing cleanup core even if the manifest is gone (`internal/api/install_parsed_manifest.go:1914-2023`). |
| Live resolver republish fails after durable mutation | Report success-with-restart-required, matching manifest mutation routes (`internal/gui/manifest.go:192-224`). Do not roll back the durable restore; tell the operator the live aggregate may be stale until restart/republish. |

## Test plan

API/unit tests:

1. `TestExecuteDeAdopt_RoundTripRestoresNativeEntry`: seed a codex/claude stdio entry, run adopt, run de-adopt, assert the client entry equals the saved pre-adopt snapshot. This extends the existing adopt full-path test that asserts manifest/intent/client URL changes (`internal/api/adopt_test.go:105-185`).
2. `TestExecuteDeAdopt_EntrylessFanoutRemovesEntry`: adopt into an explicitly requested client with no original entry, then de-adopt that client and assert the server entry is absent. This falsifies mishandling of the adopt fanout case pinned by `TestBuildAdoptPlanPreservesFanoutToEntrylessClient` (`internal/api/adopt_test.go:1510-1539`).
3. `TestExecuteDeAdopt_SubsetKeepsManifestAndSupervisor`: adopt into two clients, de-adopt one, assert manifest still exists with only the remaining `client_bindings`, routed secrets remain, and supervisor intent remains.
4. `TestExecuteDeAdopt_AllClientsDeletesManifestIntentSecrets`: de-adopt the last client, assert manifest dir is gone, supervisor-intent rows are gone, adopt-routed vault keys are gone, and managed/provenance rows are closed.
5. `TestBuildDeAdoptPlan_NoProvenanceFailsClosed`: create a hub-looking manifest/client entry without adopt provenance and assert no mutation occurs.
6. `TestExecuteDeAdopt_LiveConfigEditedFailsClosed`: change the live client entry after adopt to a non-matching URL or direct entry and assert de-adopt refuses before restoring/removing anything.
7. `TestExecuteDeAdopt_ManifestHashMismatchFailsClosed`: edit the adopt-created manifest after adopt and assert de-adopt refuses or requires explicit merge.
8. `TestExecuteDeAdopt_ResumeAfterRestoreBeforeManifest`: inject a failure after client restore; retry must complete manifest/provenance/supervisor cleanup without corrupting the restored native entry.
9. `TestExecuteDeAdopt_GateOnDropsTargetFromAggregateOnly`: seed gate ON with two clients, de-adopt one target, republish resolver, assert `Bindings[target]` lacks the server while the other client still has it. Use resolver/aggregate tests as the model because current aggregate sessions read from `ResolverSnapshot.Bindings` (`internal/api/hub_mcp_resolver_test.go:10-35`, `internal/api/hub_mcp_aggregator_test.go:531-579`).
10. `TestExecuteDeAdopt_GateOnLastBindingPrunesAggregate`: de-adopt the last binding for a client and assert the new hub reconcile extension removes `mcphub-hub` only for that client.
11. `TestExecuteDeAdopt_QuarantinedDaemonReleasesIntent`: simulate descriptor present with runtime not healthy; de-adopt should still remove supervisor intent and not attempt a daemon health precondition.

GUI tests:

1. Add route tests mirroring `internal/gui/adopt_test.go`: plan returns preview without mutation, execute calls the API, symlink/snapshot conflicts map to actionable errors, and operator-action audit is emitted (`internal/gui/adopt_test.go:104-151`, `internal/gui/adopt_test.go:527-562`).
2. Add frontend tests showing `De-adopt to native` only for adopt-provenance `via-hub` rows, not direct, unknown, external, or `via-hub-inherited` rows. Existing Migration tests already pin adopt button visibility (`internal/gui/frontend/src/screens/Migration.test.tsx:254-353`).
3. Add Playwright flow: adopt seeded unknown stdio row, de-adopt it, assert manifest/client config/scan state return to native. Existing e2e adopt verifies the adopted config has URL and no command (`internal/gui/e2e/tests/migration.spec.ts:154-187`).
4. Add restart-required UI test for gate-ON republish failure, matching manifest mutation response behavior.

CLI tests:

1. Cobra command parses `de-adopt/deadopt`, dry-run prints plan, `--yes` executes.
2. Missing provenance exits non-zero with no mutation.
3. Gate-ON live-republish unavailable returns a restart-required or actionable "restart hub/reconcile" message instead of claiming full live aggregate convergence.

## Scope + prereqs + recommendation

Scope classification: full-delivery, not quick-fix. The change crosses backend state/provenance, adopt execution, manifest mutation, supervisor intent teardown, hub aggregate reconcile, CLI, GUI, and tests. It also changes the semantics of adopt by adding durable provenance, so it needs architecture and QA gates rather than a local patch.

Change-surface contract:

- Primary owner: adopt/de-adopt API pipeline in `internal/api`.
- Existing sub-owners to reuse: clients adapters/config locks, manifest edit/delete, supervisor-intent cleanup, hub reconcile, GUI event/audit.
- New durable surface: adopt provenance state file, or a schema-compatible extension of managed entries. Do not infer adopt ownership solely from hub URL shape.
- No breaking change to existing adopt CLI/API responses unless adding optional fields. New provenance should be written for future adopts; legacy adopted entries without provenance remain fail-closed by default.
- No new parallel state-sync mechanism for aggregate membership. Manifest `client_bindings` remain the source of `/clients/<client>/mcp` membership.

Prerequisites:

1. Add adopt provenance capture before implementing user-facing de-adopt.
2. Decide whether adopt also records `managed-entries.json` tuples. Recommendation: yes, for consistency with existing demigrate ownership language, but de-adopt should rely on stronger adopt provenance for restoration.
3. Add a manifest-binding edit helper that removes selected clients with expected-hash protection, instead of hand-editing YAML in the de-adopt handler.
4. Extend hub reconcile to remove stale `mcphub-hub` for selected clients whose binding set becomes empty under gate ON.
5. Decide legacy behavior. Recommendation: default fail-closed for no provenance; offer explicit `--reconstruct-legacy` only after warning that secret spelling and byte equivalence are not recoverable.

Recommended delivery sequence:

1. Phase 2a: adopt provenance and ownership marker. Add tests that a new adopt writes provenance, marker rows, and a pinned restore reference, and that a failed adopt cleans pending provenance.
2. Phase 2b: backend de-adopt plan/execute. Implement restore, manifest-binding removal/delete, supervisor intent cleanup, secret cleanup, idempotent resume.
3. Phase 2c: gate-ON reconcile/publish. Add resolver republish and aggregate-prune behavior with restart-required reporting.
4. Phase 2d: CLI and GUI. Add explicit de-adopt affordance and CLI verb after the backend plan is falsifiable.
5. Phase 2e: e2e round-trip. Adopt from native, de-adopt to native, verify client config, manifest, supervisor intent, aggregate membership, and scan classification.

Recommendation: do not ship de-adopt until adopt records durable pre-adopt provenance. Without that prerequisite, the current tree can remove some hub-shaped entries via demigrate, but it cannot guarantee an adopt -> de-adopt round-trip to the original native client config.
