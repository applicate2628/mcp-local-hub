---
status: accepted
date: 2026-07-09
slug: test-leftover-reaper-peb-env-proof-preview-only
deciders: $architect (design), $knowledge-archivist (artifact sync)
context: work-items/active/2026-07-09-test-leftover-reaper/design.md
supersedes: none
superseded-by: none
---

# Decision: test-leftover reaper requires PEB env proof; preview-only without it

## Decision

The test-leftover reaper's apply-capable Windows path requires live target-process environment proof from a PEB-based reader. WMI/CIM is rejected for this proof because the feasibility probe found no process-environment surface there. The accepted apply contract is constrained to an amd64 Windows reader against amd64 and i386 same-user targets until any other architecture direction is implemented and tested.

Without verified `MCPHUB_STATE_DIR_OVERRIDE` proof, the lane may print preview diagnostics only. Path, argv, age, or executable-family evidence alone must not authorize termination of `mcphub.exe`, `mcphub`, `mcphub-reliability-*`, or `supervise` processes.

## Rationale

The 2026-07-09 feasibility probe verified on one host that a non-admin, medium-integrity amd64 Go process could read the same-user installed `mcphub.exe` daemon environment through PEB access and distinguish successful key absence from read failure. It also verified amd64 reader access to same-user WOW64 32-bit targets and confirmed access denial is distinguishable from absence.

The same probe left 32-bit reader -> 64-bit target support as `ASSUMPTION (UNVERIFIED)`. Protected or higher-integrity targets, read denial, target-exit races, malformed or truncated environment blocks, and host policy blocking all remain fail-closed refusal states.

## Consequences

- Branch-specific executable-family gates own basename matching; there is no shared common basename gate.
- `envProofGate` is tri-state: `OK with parsed map`, `OK with key absent`, or `ERROR / AMBIGUOUS`.
- `OK with key absent` refuses as `env-override-absent`; `ERROR / AMBIGUOUS` refuses as `env-read-error`.
- Unsupported architecture direction refuses or remains preview-only unless a tested Wow64-64 path is added.
- Operator-invoked cleanup remains separate from the unattended ticker.

Gate decision: **PASS**.
