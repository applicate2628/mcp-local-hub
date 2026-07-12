# Bug: the adopt data-preservation refusal is redacted to a generic `internal error` in the GUI

Status: open
Filed: 2026-07-12
Severity: P2 (Sol) / P1 (Terra error-prop) — diagnosability/UX gap, NOT data-loss (the gate protects regardless; the redaction actually hides the path, making manual deletion HARDER)
Source: Sol + Terra error-propagation lanes, #532 commission
Context: the GUI redaction is PRE-EXISTING general adopt-error behavior; #532 adds a new safety-critical refusal that deserves better than the generic mapping. Filed separately per user decision 2026-07-12.

## The gap

#532's capture-UPSERT refusal returns actionable recovery guidance
(`internal/api/adopted_entries.go:585-590` — "do NOT delete the snapshot dir if the prior
adopt committed; ..."). But the Discovery GUI adopt route maps EVERY adopt error to
`500 ADOPT_FAILED` and redacts the body to `internal error` for path-safety
(`internal/gui/adopt.go:238`, `:261`; `scan.go:117-124`; the
`TestAdoptRouteExecuteFailureRedactsAbsolutePath` behavior). So a GUI operator who hits a
committed-but-manifest-deleted prior adopt sees only `internal error` and cannot:

- distinguish this data-preservation refusal from an ordinary I/O failure;
- follow the CLI-only preservation guidance.

## Why NOT data-loss (severity nuance)

The gate's protection holds for the GUI too — the auto-reap is refused regardless of the
message. For the operator to actually LOSE data they would have to manually delete the
`adopt-provenance/<manifest>/` dir — and the redaction *strips the path*, making that harder,
not easier. So this is a diagnosability/UX gap (operator doesn't understand WHY adopt
failed), not a data-loss amplifier. Hence P2 (Sol) over P1 (Terra).

## Fix

Return a STABLE, path-free refusal signal the GUI can map to a distinct, actionable response
(e.g. HTTP 409 Conflict) with a safe operator-facing message ("this server has a prior
in-progress/committed adopt; resolve it before re-adopting"). Requires:
- a typed/sentinel error from `captureAdoptProvenance`'s refusal (so the GUI can recognize it
  without string-matching or leaking the raw message);
- GUI handling of that signal → 409 + safe copy (no filesystem paths);
- (composes with the audit-event classification added in #532 — same "distinguish this
  refusal" need on the operator surface).

Touches `internal/gui/adopt.go` + a typed error at the API boundary — a distinct surface from
#532's capture-lane guard, so a separate PR.
