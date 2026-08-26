# T14 — immutable local candidate and history seal

Execution role: `$toolchain-engineer` acting as the T14 candidate integration owner.

Gate: PASS — one immutable local candidate commit exists; exact staging, diff integrity, publication safety, protected-surface and post-commit reconciliation gates are complete.

## Accepted inputs

| Input | SHA-256 |
|---|---|
| `design.md` | `AFABC3C001169D5C571D7319EA2C751CDD228E46B335C9630C0516F6EBAE6DC9` |
| Accepted decision | `18307E933D393BBD0C6B0396F47FE6AAFB0C5AE94CE39E395F8EE948371BE92A` |
| `architecture-review.md` | `18499E40CC82236F9EA256F988BB7F48342806240A1EC710E7739978BCF7601E` |
| `security-constraints.md` | `A0F0D2CEF3BA016D4E4E607755D643F5415F2D56F2C9BF99E848481498A81A12` |
| `security-review-design.md` | `BFC9A0F36F7FF0E07ADE7E4DC79D507FBB1BBBDCD548713B263F7BC3FF14B84A` |
| Canonical `plan.md` | `3A0CB9AB98447A7A8ED63B2115F68007A9C78EDA77412E94B7A9F6FA90F1E8BD` |
| T13 PASS | `11C5506887E283A1E8284CA81EC49F2045C2D5B0DD0CF8D194FEFFE969FF1E48` |
| T14 knowledge-hygiene correction PASS | `067015EAADE312580C264F8F07941AFBB4179827F3D2EEA8BA1A9D2587403213` |

Candidate parent is the current local `HEAD` `048a30fabc10fa3e6bfc64facc9fb6da6ebe49da`. Candidate commit/tree identifiers are filled from Git only after the non-interactive local commit exists; this artifact is intentionally left outside that self-referential candidate commit.

## Pre-commit Bootstrap 0–5

| Step | Recorded evidence and decision |
|---|---|
| 0. Repository orientation | Scope is the live T00–T13 CST saved-field candidate and canonical work-item evidence. Workflow is `plan.md:282-294`; protected surfaces are unrelated dirty files, index, remotes, manifests and live services. Evidence: `design.md:242-278`, `plan.md:29-37,282-294`, T13 PASS. |
| 1. Diagnostic data | Correction-cycle-2 Git inventory began at `HEAD=048a30fa...`, empty index, 142 dirty leaves. Fresh classification produces 99 candidate leaves and 43 excluded leaves. The prior whitespace blocker is closed by accepted correction SHA `067015EA...`. CodeGraph exact-impact query was attempted first but reported auto-sync disabled due to an external file lock and mixed in unrelated source; it is rejected as current allowlist evidence. |
| 2. Hypotheses | H1: the freshly recomputed 99-leaf allowlist is exactly the admitted Change-Surface plus canonical evidence correction. H2: the three full-Go failures in T13 are exact pre-existing differentials rather than candidate defects. H3: the accepted correction changed knowledge records only; source/test bytes remain the T13-tested bytes. H4: a single local commit with no push is the T14 history seal. |
| 3. Verification | H1 is verified by phase artifacts T01–T13, accepted correction `067015EA...`, `design.md:242-278`, current `plan.md:29-37`, and fresh path-by-path Git classification. H2 is verified by T13's full 407-second run and exact focused differentials. H3 is verified by the correction's 16-path boundary and source/tests-untouched receipt. H4 is the exact `plan.md:282-294` contract. No decision-driving assumption remains. |
| 4. Scope proportionality | Stage only the freshly enumerated 99 leaves: 24 Go, 16 Python production, 16 Python tests, 35 active-item artifacts, seven related bug records, and one accepted decision. `implementation-t14.md` and `status.md` stay outside as post-commit receipts. Exclude the other 41 unrelated leaves plus those two receipts. |
| 4.5. No-kostyl check | The candidate contains the accepted owner-level topology and test corrections, not a timeout widening, error suppression, fallback, alias or compatibility route. T12's zero-residue proof and T13's complete regression/publication gate are the falsifiers. T14 changes no behavior. |
| 5. Recovery readiness | Before any push, the local candidate can be removed with reviewed `git reset --mixed 048a30fabc10fa3e6bfc64facc9fb6da6ebe49da`, which restores the candidate bytes as unstaged work while preserving unrelated dirty files. `git reset --hard` is forbidden in this dirty worktree. No amend, push, release, service mutation or deployment is authorized. |

## Candidate allowlist

| Class | Count | Admission evidence |
|---|---:|---|
| Go spawn/status/enrollment owners and tests | 24 | T01/T02 artifacts; design Change-Surface; exact current Git inventory |
| Python frontend/service/domain production | 16 | T03–T12 artifacts; design Change-Surface |
| Saved-field Python tests | 16 | T00 and T03–T13 test receipts |
| Active work-item canonical history | 35 | Physical active-item owner; accepted artifacts and phase receipts |
| Related bug records | 7 | Six CST finding records plus the routing baseline record cited by T13 |
| Accepted decision | 1 | Plan input SHA and accepted metadata |
| **Total** | **99** | Explicit path list is verified again from the staged index before commit |

Excluded from the candidate: 43 leaves — 41 unrelated leaves plus the two post-commit receipts. The two tracked protected changes are `internal/api/port_alloc_excluded_windows.go` and `work-items/README.md`; all remaining unrelated exclusions are PR, registry, archive, liveness, autostart or scheduler records. `servers/cst/manifest.yaml` is unchanged.

## Checks and immutable candidate receipt

| Field | Immutable value |
|---|---|
| Commit | `43fee019d46c69522ebe79be952d5f139bd4854f` |
| Parent | `048a30fabc10fa3e6bfc64facc9fb6da6ebe49da` |
| Tree | `29dada47e1c7d597e5567a66f68b506dc4576cad` |
| Commit-content SHA-256 | `396AF5958891A093ECE6FAADE88A6D2393EEDA8E3FB2C0017809DF61E4E0657C` |
| Subject | `feat(cst): add contained saved-field sampler candidate` |
| Candidate paths | 99 |

| Check | Result |
|---|---|
| Exact explicit staging | PASS: freshly requested 99, actual staged 99; set comparison had zero extras and zero omissions. The two post-commit receipts were not staged. |
| Cached integrity | PASS: `git diff --cached --check` returned exit 0 after accepted correction `067015EA...`; staged/worktree divergence was zero. |
| Installed publication-safety scanner | PASS: all 99 staged leaves scanned; zero findings. The scanner's `--range` mode later rejected local commit IDs at its remote-selection gate, so it is not claimed as a second PASS. Binding is instead exact: the scanned index equaled the allowlist, had zero worktree divergence, and became the unchanged 99-path commit tree. |
| Current SHA closure | PASS: zero stale current-reference hits outside the accepted correction's explicitly labeled provenance map. |
| Protected/dependency surfaces | PASS: no candidate `internal/api/port_alloc_excluded_windows.go`, `work-items/README.md`, `servers/cst/manifest.yaml`, dependency manifest, PR, registry, archive, autostart or scheduler path. Protected worktree blob IDs remain `5214ec62...` and `0e94cee5...`. |
| Commit range | PASS: `git diff --check 048a30fa... 43fee019...` returned exit 0; `git diff-tree` reports exactly 99 paths and zero forbidden paths. |
| Git object connectivity | PASS: `git fsck --connectivity-only` exited 0. Existing dangling objects were informational and unrelated; the candidate commit/tree are connected from `HEAD`. |
| Post-commit settlement | PASS: `HEAD` is the candidate, index is empty, 43 excluded dirty leaves remain, and no candidate path has a post-commit worktree delta. |

T13 remains the fresh behavior gate for these unchanged source/test bytes: Python 574/574, saved-field 531/531, existing-six 8/8, Ruff/format/vet, native/WSL guards, and the 407-second full-Go exact differential are recorded in accepted `implementation-t13.md`. Cycle 2 changed canonical knowledge bytes only; it did not require or authorize another full suite.

## Receiving-side echo

| Required invariant or guard | State before commit | Expected falsifier |
|---|---|---|
| Named regression guard: exact index allowlist | VERIFIED: 99/99 exact in commit; post-commit index empty | Any commit path outside the 99-leaf list, missing allowlisted leaf, or non-empty index |
| Named regression guard: publication safety | VERIFIED for exact staged bytes: 99/99 clean and identical to commit | Any scanner finding or staged/commit byte divergence |
| Existing-six/tool/schema/error equivalence | VERIFIED by accepted T13: 8/8 exact sample and saved-field/full Python matrices | Any T15–T18 candidate-bound regression invalidates this gate |
| Unrelated dirty paths remain byte-identical and unstaged | VERIFIED: 43 excluded leaves remain; protected blob IDs unchanged; index empty | Any excluded path in the candidate or lost worktree byte |
| No manifest/pin/live/remote mutation | VERIFIED: none occurred | Diff in `servers/cst/manifest.yaml`, remote update, or service/process mutation |
| Known failure class | Two stale routing source anchors and one TempDir cleanup race are classified not-affected by T13; no correction is staged | Any additional full-suite failure or an impact edge from candidate paths returns to implementation |

## Toolchain-specific boundary

No compiler, linker, dependency, build flag, cache key or packaging graph changes in T14. Therefore observed flag-delta and two-build artifact-hash probes are not triggered; the reproducibility claim is limited to an immutable Git tree/commit binding, verified below by Git object IDs and range inspection.

## Rollback

While the candidate remains local and before any dependent work is published, reviewed rollback is `git reset --mixed 048a30fabc10fa3e6bfc64facc9fb6da6ebe49da`. This removes the candidate commit while restoring its bytes as unstaged work and preserving unrelated dirty leaves. Never use `git reset --hard` in this worktree.

## Terms and Abbreviations

- CST: Computer Simulation Technology.
- SCM: Service Control Manager.
- QA: quality assurance.
- SHA-256: Secure Hash Algorithm 256-bit digest.
