---
status: open
severity: low
filed: 2026-07-03
context: deep-audit finding (secure-write-dacl × correctness lens; DISPUTED — code divergence CONFIRMED, exploitability REFUTED 2/3)
---

# Client-config default-relax lane does not refuse a WRONG-OWNER parent, diverging from the state-file lane that does

## Finding (DISPUTED — real divergence, disputed impact)

The state-file write relax gate `stateFileParentGateAllowsDefaultRelax` (`state_file_helper.go:192-199`) explicitly HARD-FAILS (no relax) when the parent-gate error wraps `ErrWrongOwner` ("only an OWNER-CORRECT broadened parent may relax", `state_file_helper.go:132-134`). The **client-config** write lane does NOT apply that guard: `secureWritePathBased` (`client_write_init.go:653-688`) only checks `errors.Is(err, ErrSecureWriteParentInsecure)` (`:658`); on a wrong-owner parent the step-2 gate returns `fmt.Errorf("%w (path %s): %w", ErrSecureWriteParentInsecure, parentDirForDiag, ErrWrongOwner)` (`secure_write_windows.go:201`), which STILL satisfies `errors.Is(..., ErrSecureWriteParentInsecure)`, so the lane falls through to `secureWriteClientConfigSkipParentGate` (`:687`) and writes anyway. The symlink-resolved sibling lane (`:614-640`) has the same missing guard.

The **divergence is real and confirmed** (the correctness verifier confirmed every cited line). The disputed part is exploitability: on Windows a wrong-owner parent's foreign owner holds implicit WRITE_DAC/WRITE_OWNER and could rewrite the directory DACL to grant itself FILE_DELETE_CHILD, then swap the token-bearing client-config entry the external app (Claude Desktop/Codex) loads via an ordinary path read (nothing backstops the swap, unlike mcphub's own inode-anchored state reads). Two of three verifiers REFUTED the practical reachability/impact (hence DISPUTED, low severity).

## Suggested next step

Decide whether the client-config relax lane SHOULD mirror the state-file lane's wrong-owner refusal (symmetric hardening — the code's own comments imply the two lanes are meant to be symmetric). If yes, add the `errors.Is(err, ErrWrongOwner)` hard-fail to `secureWritePathBased` (+ the symlink-resolved sibling), matching `stateFileParentGateAllowsDefaultRelax`. Before implementing, resolve the exploitability dispute — the audit's own verifiers split on whether the swap is realistically reachable on a solo-dev host (the default posture) vs only on a genuinely multi-tenant/foreign-owned-parent host where `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` is the documented mitigation.

## Note

Surfaced during the 2026-07-03 multi-agent deep audit. The sibling STRONG finding (secrets-edit cleartext temp) was fixed in `fix/secrets-edit-owner-only-temp` (PR #497). This one is filed for a decision, not an immediate fix.
