# Item-3 Unit B Phase H — frontend + progress + degrade messages — review record

Phase H (frontend SSE `gui-restart-progress` consumer + coarse progress + best-effort port-change navigation
+ two degrade literals + `events.go` classify rows + regenerated embedded bundle), default-OFF. Branch
`feat/gui-restart-unitb-gated`. codex implemented; $lead-verified + one fable security/correctness review.

## $lead verification
Frontend `npm run build` (vite, 268 modules) + `npm run test` (69 files / 1121 tests, 0 errors on re-run —
a transient env-setup "2 errors" did not recur) + `npm run typecheck` all green; `go generate ./internal/gui/...`
regenerated the bundle (the app.js delta vs codex's build is cosmetic minifier name-mangling, logic-identical);
`go build`/`go vet`/`gofmt` clean; AC-H1 `TestRestartV3_GraceNavigationIsBestEffortAndNeverClaimsCommit` +
the classify tests pass.

## fable review — no security blocker; every hostile-input surface fails safe (verified airtight)
- **Content/command injection: CLEAN.** The degrade text is two hardcoded module constants selected by
  exact-match `Set.has` allowlists on BOTH `reason_code` AND `operator_action` (+ a required pairing check);
  wire values are never echoed; text renders as a VDOM-escaped JSX node (no `dangerouslySetInnerHTML`); the
  only interpolated wire value is `targetPort`, gated by `validPort` (1-65535); `spawn_error` is escaped.
- **Navigation redirect: CLEAN.** Target is always the fixed literal `http://127.0.0.1:${port}/` with an
  integer-validated port; host/path/protocol never come from the wire; skips same-port + non-`reserved`;
  `assign` in try/catch. Worst case (attacker already owning the backend broadcaster): redirect to another
  loopback port.
- **SSE lifecycle / contract / best-effort: CLEAN.** `useEventSource` keys on `[url]` (no resubscribe churn),
  cleans up once; `/api/events` has no history replay so a stale `reserved` cannot re-trigger navigation on
  mount; `restartGui` still throws on non-2xx before the body read; 202 reconnect copy + 2xx spawn_error →
  "Restart incomplete" preserved; gate-OFF fully inert; no surface claims the child committed.

## $lead architectural reconciliation — the fable P2 cross-phase contract gap (AC-H3 RE-SCOPED)
fable proved the two AC-H3 degrade literals (`operator_action` → `mcphub gui` / `mcphub gui --force --kill`)
have NO live browser-reachable producer: the parent `publishProgress` emits
`{handoff_id, generation, phase, old_port, same_port, [new_port], [reason_code]}` — never `operator_action`;
the three degrade reason codes are produced only by the ensure-alive CLI one-shot, which writes to
`supervisor-events.log`, a channel the GUI never mirrors onto `/api/events`.

**Decision (architecture):** this is CORRECT, not a bug — the free-flock-interrupted and live-wedged degrade
scenarios are exactly when the GUI/browser is DEAD (the parent released the flock + exited with no child, or a
foreign process holds the single-instance lock). There is NO live-browser scenario that needs the degrade
`operator_action` (a proved rollback leaves the parent serving-full and needs no `mcphub gui` guidance). So
the degrade delivery channel is the CLI (ensure-alive → `supervisor-events.log` + the operator running
`mcphub gui` / `--force --kill`), consistent with design-B's "degrade-to-operator-visible". **AC-H3 is
re-scoped to enum-mapping-only:** the frontend DEFENSIVELY renders the two exact literals if an event ever
carries a valid `operator_action`, but is NOT the primary delivery channel; `operator_action` is deliberately
NOT wired into `publishProgress` (it would deliver to a dead browser). The browser-reachable degrade
(`spawn_error` → "Restart incomplete") works and is verified. This re-scope is recorded so Phase J's docs pass
does not re-open it.

## Fixes (2 non-blocking P3s → codex `phaseHfix`)
- **P3-2** (SectionGuiServer.tsx): the 202 `restarting:true` copy hardcoded "same port" even for a port
  change; now branches on `old_port !== target_port`.
- **P3-1** (events.go): the parent's terminal failure is carried as a `reason_code` inside the info-severity
  `gui-restart-progress` type; the fix documents/adjusts the observability severity honestly (no new
  never-emitted event type).

## Verdict
Phase H is commit-safe once the 2 P3 fixes land + re-verify. No security defect; the AC-H3 contract gap is a
$lead-reconciled re-scope (enum-mapping-only, CLI-primary degrade delivery), not a code fix.
