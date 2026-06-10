# Stale deleted-watchdog-file comment references (adjacent finding)

**Severity:** P3 (comment-only; no behavior change, no compile impact)

**context:** adjacent-finding

**status:** closed

**closed-at:** 2026-06-10

**closed-by:** comment-only sweep on branch `worktree-agent-a36a59ea1dc5f5c93`
(re-pointed the active "mirrors X:line" / "X indexes" design notes at their
surviving owners). No behavior change; build matrix + vet + gate-D green.

**Found:** 2026-06-10 while fixing the 4 named Phase-3b P3 stale-comment
findings (re-pointing comments at the deleted watchdog files
`watchdog_log.go`, `watchdog_xml_validator.go`, `recovery.go`). Those 4
findings were scoped to specific production file:line targets and are now
fixed. While running the final
`grep -rn 'watchdog_log\.go\|watchdog_xml_validator\.go\|recovery\.go'
internal/ --include='*.go'` gate, additional comments still naming a
deleted file surfaced. They are the SAME defect class (a comment pointing
at a file Phase 3b removed) but were NOT among the 4 named findings, so
per the adjacent-findings protocol they are filed here rather than fixed
in-scope.

## Surviving homes (for the eventual fix)

- `splitJSONLines` → `internal/api/jsonlines.go:14`
- `stripXMLDeclaration` (only surviving def) → `internal/migration/classify_xml.go:521`
- `IsRealFailure` / `isMaintenanceTaskName` / `ServerFromTaskName`
  → `internal/api/task_classifiers.go:70,107,126`
- intent-key indexer (`intent.Tasks[row.TaskName]`) now owned by the
  supervisor reconcile loop → `internal/cli/supervise_reconcile.go:146`
- identity-cap / 10 MB rotation / fsync-on-append discipline now lives in
  `internal/api/supervisor_events.go` + `internal/api/intent_audit.go`
  (the supervisor log self-documents its cap against `intent_audit.go:98`)

## Remaining stale references (NOT fixed — adjacent)

Active "mirrors X:line" / "X indexes" design notes whose cited
`watchdog_log.go` / `recovery.go` line numbers are now dead:

- `internal/api/supervisor_events.go:30,76,88,173,250` — "mirrors
  watchdog_log.go:25-36 §35" et al. Re-point to the surviving
  `intent_audit.go` / `supervisor_events.go` self-discipline (the file
  already cross-references `intent_audit.go:98` at line 96).
- `internal/api/supervisor_events_test.go:76,123,254` — "per the
  watchdog_log.go precedent" / "Mirrors the watchdog_log.go discipline".
- `internal/api/hub_mcp_log.go:304` — "Sync to match watchdog_log.go
  durability". Re-point at `supervisor_events.go` fsync-on-append, or
  reword to drop the deleted-file equivalence claim.
- `internal/api/codex_followup_test.go:7,52,141` — "recovery.go indexes
  `intent.Tasks[row.TaskName]`". Same obsolete-indexer pattern fixed in
  the 4 production findings; re-point to
  `internal/cli/supervise_reconcile.go`.

## Intentional history mentions — KEEP (do not touch)

These correctly cite the deleted file as migration provenance (the new
home of a migrated symbol explaining where it came from); they are the
"intentional Legacy/history mentions" the gate explicitly allows:

- `internal/api/jsonlines.go:5` — "migrated here from watchdog_log.go …"
- `internal/api/liveness_task.go:31` — "Migrated here from
  watchdog_xml_validator.go …"
- `internal/api/task_classifiers.go:3` — "migrated here from recovery.go …"

## Suggested fix

A single comment-only sweep re-pointing the active-mirror references above
at their surviving owners, leaving the three migration-history breadcrumbs
intact. No code change, no test impact.

## Closure note (2026-06-10)

Comment-only sweep complete. All active "mirrors `watchdog_log.go`" /
"`recovery.go` indexes" design notes re-pointed at their surviving owners;
the three intentional migration breadcrumbs left untouched.

Surviving-owner map applied (verified by reading the surviving files, not
the bug doc's suggestions — one correction below):

- identity-cap / per-entry-cap / identity-oversize §35/§51 discipline →
  `intent_audit.go` (`AuditEntryMaxBytes` :91, `AuditIdentityFieldByteCap`
  :98, `ErrIdentityOversize` :118).
- rotation (`.1` rename) precedent → `gui_event_log.go:166-199` (surviving)
  plus `intent_audit.go:545-560` (replacing the dead
  `watchdog_log.go:237-241`).
- blocking-Lock JSONL sibling → `gui_event_log.go:161` (surviving;
  replacing the dead `watchdog_log.go` claim).
- append-fsync durability → `intent_audit.go:630` (the surviving append-log
  that calls `f.Sync()`). **Correction to the bug doc's suggested fix:**
  it proposed re-pointing `hub_mcp_log.go:304` at "`supervisor_events.go`
  fsync-on-append", but `supervisor_events.go` has NO `f.Sync()` call, so
  that would have re-pointed at a non-existent contract. Re-pointed at the
  real surviving fsync-on-append owner `intent_audit.go:630` instead.
- intent-key indexer (`intent.Tasks[row.TaskName]`) → the supervisor
  reconcile loop `internal/cli/supervise_reconcile.go:146`
  (`daemonIntent.Tasks[d.TaskName]`).

Files changed (comment-only):

- `internal/api/supervisor_events.go` — :30, :76, :88, :173, :250.
- `internal/api/supervisor_events_test.go` — :76, :123, :254.
- `internal/api/hub_mcp_log.go` — :304.
- `internal/api/codex_followup_test.go` — header Finding 1 (~:7), :52, :141.

Verification:

- `go build ./...` + `GOOS=linux GOARCH=amd64 go build ./...` +
  `GOOS=darwin GOARCH=amd64 go build ./...` — all exit 0.
- `go vet ./internal/...` — clean.
- `go test ./internal/cli/ -run 'GateD|NoWatchdog' -count=1` — ok.
- Final gate
  `grep -rn 'watchdog_log\.go\|watchdog_xml_validator\.go\|recovery\.go'
  internal/ --include='*.go'` now shows ONLY the three intentional
  migration breadcrumbs: `jsonlines.go:5`, `liveness_task.go:31`,
  `task_classifiers.go:3`.

Doc moved to `work-items/bugs/closed/` per repo convention.
