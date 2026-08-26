# Backlog — `cst_solve` rejects the official CST `Third` tetrahedral order

- Filed: 2026-08-19 from the VFEM CST↔Fortran same-mesh validation.
- Server: `cst`.
- Status: candidate
- Priority: P1 — it blocks a requested run outright; there is no workaround through
  the MCP.
- No-epic rationale: one contract value on one tool; no cross-server coupling.

## Admission decision

**PASS — admit as an enhancement, narrowly scoped.** `cst_solve` cannot express a
solver order that CST itself supports, so a requested Order-3 sweep has no MCP path
at all. This is not a duplicate of the licence-reporting item
(`2026-08-17-cst-solve-reports-waiting-for-license-for-a-model-defect.md`), which
owns terminal evidence for a licence failure; this one owns the accepted value set.

## The defect

`Third` is refused in **two** independent places, so widening one is not enough:

| where | what it says |
|---|---|
| `servers/electromagnetics-mcp/src/mcphub_em_mcp/contracts.py:24` | `CSTTetOrder = Literal["First", "Second"]` |
| `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst.py:351` | `if order_tet not in {"First", "Second"}: raise ValueError(...)` |

Meanwhile the value is passed straight through to the macro at
`cst.py:174` — `f'    .OrderTet "{settings["order_tet"]}"'` — which is the CST macro
API's own token and would accept `Third` unchanged. The signature default sits at
`cst.py:319`, `order_tet: CSTTetOrder = "First"`.

**Evidence that `Third` is a real CST token, not a guess:** a macro carrying
`.OrderTet "Third"` was prepared and is tracked by SHA-256 in the consuming
repository's work-item
(`.scratch/cst-order3-own-adaptive/line10_order3_own_adaptive.bas`,
`D0F493341D2F5E9C7894359E3A51B632CB3E6BB08AB9E1E04CC86B152F81B300`). It has **not**
been executed — the launch died before entering the macro on `EXITCODE_NOLICENSE`,
so `Third` is **verified as accepted by the macro grammar's own vocabulary and as
what the operator requested**, `ASSUMPTION (UNVERIFIED)` that CST solves it on this
model. *Resolving step:* run it once the licence service is up; a `PASS` with
physical project/S2P/mesh settles it.

## What is asked for — and what is NOT

**Asked:** add `Third` to the literal and to the validator, keep them in step, and
add a preflight plus a test that pins the accepted set.

**Explicitly not asked: do not loosen the type to an arbitrary string.** The typed
literal is doing real work — a silent typo in a solver order would otherwise reach
the macro and change the physics without any diagnostic. The request is a *wider
enumeration*, not a *weaker contract*.

Worth checking while there: whether CST's own accepted set stops at `Third`. Filing
only the value that was needed would leave the next order to be discovered the same
way. `ASSUMPTION (UNVERIFIED)` that the set is exactly `First|Second|Third`;
*resolving step:* the CST macro API reference for `.OrderTet`.

## Why it matters to the caller

The blocked run is not incidental. It is a deliberately independent control: CST at
Order-3 **on its own adaptive mesh**, 1–20 GHz, with a converged propagation
constant, to be compared against a first-order same-mesh result whose first
divergence has already been localised to the 2D port eigensolve. Without it the
comparison rests on a single solver order.

## Terms and Abbreviations

- **CST** — CST Studio Suite.
- **`OrderTet`** — CST macro setting for the basis order on tetrahedra.
- **preflight** — a validation pass that rejects an inadmissible request before any
  solver is launched.
- **`ASSUMPTION (UNVERIFIED)`** — a claim believed but not measured, carrying the
  step that would settle it.
