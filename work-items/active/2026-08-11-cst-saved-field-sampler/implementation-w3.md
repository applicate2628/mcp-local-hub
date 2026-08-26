# W3 Go Platform Package — Existing Direct-Image Launch Owner Integration

Execution role: `$platform-engineer`

Plan: `484883EDBAD02333162C61FAF78B99AA56C402FD64D5955F0E6B65BDDEC82E14`

Accepted design: `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED`

Accepted W2 package: `7FF74F7205D9F21F4E2EE9842C36773C330B43D0D445265B4B94F72E69D60BC1`

## Receiving-side echo

- Task understood: implement W3 only by extending the existing Go `exec.Cmd`/`StdioHost` launch owner with exact `cst-direct-v1` receipt admission, direct-image selection, singleton capability inheritance and immediate pre-start identity verification.
- Inputs accepted: exact plan/design/W2 hashes above; W2 image SHA-256 `CFCE49ED23D63D5F19F4CEBF386282916331255092A30E77382FA45F483C5E91`; W2 manifest SHA-256 `AEC9DE81AEE361C5FF1AEB2C126219781BC891E681D0C4D6402F9C4006C5AC56`; W1 Go RED SHA-256 before W3 `355ED055AA4B16059511690D90AE2F8985B01A0AE3F6E6F9A4FAB4E8E49D86B4`.
- Output: the seven scoped Go source/test paths below plus this implementation package. No Python composition/receipt test was made green; no manifest pin, dependency, live Service Control Manager state, Git index, commit, publication or push was changed.
- Boundary preserved: W2 remains truthfully `unprovisioned`. W3 creates no `ProvisionedPackageIdentityV1` and writes no provision receipt. Missing, unsafe or malformed `cst-direct-image-receipt-v1.json` leaves `cstLaunchCapabilityConfig` nil, retains the existing six-tool launch path and supplies no capability.
- Assumptions: no unverified premise drives the W3 gate. Target provisioning, signature/App Control proof and live exact-child observation remain later phases.

## MCP owner evidence

CodeGraph MCP was used before editing and again after editing. The fresh post-edit flow is still `cstLaunchCapabilityConfig -> HostConfig -> StdioHost.Start -> prepareLaunchCapability -> verifyCstDirectImage -> windowsLaunchCapabilityPipe.apply -> exec.Cmd.Start`. No W3-relevant pending-index banner appeared and no raw `CreateProcess` or parallel frontend launch owner was added.

## Changed paths and immutable hashes

| Path | SHA-256 | W3 responsibility |
|---|---|---|
| `internal/cli/daemon.go` | `419AB469926FD18E094F2A27D274BF5EB9A8CDDF95BC439868953CCEEE4866DE` | Exact CST/default/Windows admission, inode-anchored fixed receipt read, direct image/argv selection; absent receipt remains default-off. |
| `internal/cli/cst_direct_launch_windows_test.go` | `4AE05ADF6DAA05AEC3FEE695277B09F2AA97B4A9D54F829F259CAB44D9E3F553` | Wrong identity, absent receipt and closed receipt-schema tests. |
| `internal/daemon/host.go` | `1923A85DAACA72009BF36CC5AC86F5AEA2605B5FB8C4853C24C00B87594F0B10` | Reject capability-bearing wrapper/path/argv mismatch and close retained direct-image state on apply failure. |
| `internal/daemon/launch_capability.go` | `ADB3FF93177E8FDFFCCDAD9B0D7356DCF615B68B71DC5733BB8E3D8BEA68369B` | Closed typed receipt, held image/manifest identity, W2 manifest contract, enrollment integration, immediate pre-start recheck and all-return close/cancel. |
| `internal/daemon/launch_capability_windows.go` | `47E100AA8C3516A30D103B8C1617B1A98F523618FDA6486E9C639F18B6F7BA1D` | Share-read-only/non-reparse/final-path held opens and otherwise-compatible `SysProcAttr` plus exactly one additional handle. |
| `internal/daemon/launch_capability_other.go` | `20E2BFE75B7691A506B75CF5F4AADE4A86AC237501DD8857F90B1AEE30EF0DE7` | Non-Windows direct-image open fails closed while legacy nil-direct-image tests remain compatible. |
| `internal/daemon/cst_direct_contract_windows_test.go` | `C566DEFFCD473104005150B1F9A68DB17B144ACBE02070E073BA0BA86E2BA6FE` | W1 RED closure plus executable singleton/conflict and W2 image/manifest identity checks. |

## Acceptance result

| Criterion | Evidence |
|---|---|
| W03-AC01 exact identity/default-off | Only exact Windows `cst/default` reads the fixed protected state receipt; strict JSON rejects unknown fields; image, manifest, provision-binding hash and `--role=frontend` are mandatory. Absent receipt leaves legacy command unchanged and capability nil. |
| W03-AC02 exact handle list input | `windowsLaunchCapabilityPipe.apply` accepts only existing `HideWindow + CREATE_NO_WINDOW`, zero token/parent/security/CmdLine, `NoInheritHandles=false`, and empty additional list; it then assigns one capability handle. Mutation and repeat-apply tests pass. |
| W03-AC03 lifecycle | Existing CNG generation, 32-byte write/EOF, secure zero and cancel-once remain the sole owner. W3 adds retained image/manifest close to prepare/apply/verify/start failure and success paths. Existing lifecycle tests and full daemon package pass. |
| W03-AC04 immediate verification | Held share-read-only, non-reparse files are hashed and W2 manifest tuple is checked at admission, then rehashed immediately before `exec.Cmd.Start`. The native child still owns four flag clear/readbacks; W2 real-child probes pass. |
| W03-AC05 preservation | No `internal/process`, HTTPHost, manifest, Python service, worker containment or unrelated daemon file was changed. Native W1/W2 and existing-six fixtures pass. |

## Fresh verification

| Check | Result |
|---|---|
| W1 focused Go RED before mutation | Expected RED: three tests failed only for missing direct receipt/verifier and strict `SysProcAttr`. |
| Plan-focused Go command after mutation | PASS: `internal/daemon`, `internal/api`, `internal/cli`. |
| W3-only daemon and CLI tests | PASS. |
| Full `internal/daemon` | PASS in 41.910 seconds. |
| Focused hardened-state/enrollment `internal/api` | PASS. |
| Native W1/W2 plus existing-six Python fixtures | PASS: 21 tests; one pre-existing Pydantic forward-reference warning. |
| `go vet ./internal/daemon ./internal/api ./internal/cli` | PASS. |
| `git diff --check` | PASS. |
| W2 image/manifest hash preservation | PASS: exact accepted hashes unchanged. |

One broad `internal/cli -run Test.*Daemon` wrapper timed out at 61.2 seconds after the inner test process printed `ok ... 58.854s`; that invocation is treated as UNVERIFIED. It is not used for this gate. The exact new CLI tests and plan-focused affected selection were rerun separately and passed.

## Contract, risk and rollback

The existing six MCP schemas, response/error text, ports and stdio framing are unchanged. The new receipt is a local, owner-only control-plane input, not an MCP wire. Its provision identity digest is binding input only; W3 does not claim to validate a nonexistent X1/X2 App Control or signing receipt.

Residual risk: W3 has no genuine provisioned receipt or signed live bundle to launch, so direct-child PID/image/parent enrollment and target App Control facts remain unverified by design. W4 must preserve this default-off polarity and implement only Python composition/transport receipts; it must not invent or select a provision receipt.

Rollback mechanism: revert the seven W3 Go paths as one atomic group; W2 stays inert/unselected and the legacy existing-six command remains the only path. Rollback was not exercised because the repository worktree contains unrelated user changes; it remains `ASSUMPTION (UNVERIFIED)` until exercised in an isolated lower-environment copy.

Adjacent findings: none.

Next owner: `$backend-engineer` for W4, consuming W3 only as the frontend launch/enrollment boundary and retaining the independent worker five-handle phase for W5.

Gate: PASS

## Terms and Abbreviations

- CNG: Windows Cryptography Next Generation random-number API.
- MCP: Model Context Protocol.
- PE: Portable Executable.
- SCM: Windows Service Control Manager.
- W1/W2/W3/W4: ordered implementation phases in the accepted plan.
