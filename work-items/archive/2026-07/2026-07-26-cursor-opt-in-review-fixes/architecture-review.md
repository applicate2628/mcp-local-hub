# External architecture review

## Provenance

- Execution role: external-reviewer
- Assigned / replaced internal role: architecture-reviewer
- Requested provider: codex
- Resolved provider: Codex CLI
- Actual execution path: direct external CLI
- Model / profile used: `gpt-5.6-sol` / `xhigh`
- Launch flags: `--model gpt-5.6-sol -c model_reasoning_effort=xhigh --sandbox read-only --ephemeral`
- Run window: 2026-07-26 04:20:34–04:28:47 +03:00
- Raw report: `.scratch/external-reviews/architecture-r10.out`
- Deviation reason: none

## Reviewed revision

The reviewer independently reproduced the raw bytes from:

`git diff --binary origin/fix/cursor-not-default-install -- <15 product paths>`

Result: 48,364 bytes, plain SHA-1 `8568f0bbf84adabb3c21266819e78c49552ae9d6`.

## Findings

| Property | Verdict | Evidence summary |
| --- | --- | --- |
| Default-install ownership | PASS | `clientDescriptor.defaultInstall` in `clientRegistry()` is the sole runtime owner; only Claude Code and Codex CLI are defaults. |
| Register derivation | PASS | `DefaultInstallClientNames()` feeds `buildDefaultClientBindings()`; `effectiveClientBindings()` feeds both write and cleanup consumers. |
| Cohesion and dependency direction | PASS | Registry policy remains in `internal/clients`; the API imports the owner with no reverse dependency. |
| Blast radius | PASS | One runtime file; the remaining 14 product participants are tests, fixtures, comments, help, or documentation. |
| Derivative consistency | PASS | All 15 participants agree on 47 supported / 2 default / 45 opt-in and one `mimocode`. |
| Frontend generation ownership | PASS | Comment-only source change, successful generator evidence, and no generated-asset diff. |

Anti-layering verdict: `CLEAN-SINGLE-OWNER`.

## Advisory

The reviewer noted that the comment above `clientsForBindings()` says an uninstalled client is absent from that helper’s result, while adapter presence is filtered there and installation presence is checked later by `Exists()`. Runtime behavior is safe, the point is nonblocking, and it is outside the six admitted review findings; no revision-changing cleanup was added after the same-revision gate.

Gate: PASS

## Terms and Abbreviations

- `API`: Application Programming Interface.
- `CLI`: Command-Line Interface.
- `SHA-1`: Secure Hash Algorithm 1, used here only as a reproducible diff identity.
