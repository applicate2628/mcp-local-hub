# W1 Backend Package — RED Native Runtime, Composition and Receipt Contracts

Execution role: `$backend-engineer`

Plan: `484883EDBAD02333162C61FAF78B99AA56C402FD64D5955F0E6B65BDDEC82E14`

Accepted design: `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED`

Accepted W0 package: `D70729E56345266D36EDB9043642385CE9782065B5F7CA266924FFAD192F564B`

## Receiving-side echo

- Task understood: execute W1 only by adding deterministic RED contracts for the native Portable Executable (PE), the existing Go frontend launch owner, fixed production roots, real local transport owners, serializable owner-local receipts, exact frontend/worker handle tuples, default-off behavior, and rejection of fake settlement.
- Inputs accepted: exact immutable plan/design/W0 hashes above and W0 RED SHA-256 `065CE479AF32B82B564AA46E8A7E8CFBEED8E7AF0C3DC97B76BF38C62457B75D`.
- Output: three standalone RED test files and this implementation package. No production implementation or production contract was changed.
- Constraints preserved: W0 RED remains byte-identical; existing-six paths are untouched; no broad Go rerun, Git index mutation, live CST, Service Control Manager, App Control, Virtual Hard Disk, Code Integrity tool, deployment, registration, publication, or push.
- Assumptions: none drive the W1 verdict. Every failure is a named assertion over an accepted missing contract.

## MCP ownership evidence

CodeGraph MCP was used before editing and after the final test edit. It returned the current on-disk Go path `StdioHost.Start -> prepareLaunchCapability -> windowsLaunchCapabilityPipe.apply`, with `cstLaunchCapabilityConfig` as the CLI composition owner. The post-edit query showed those production owners unchanged and no newly introduced raw launcher. The only pending-index banner named an unrelated GUI file; no W1-relevant result was stale.

## Added RED files

| Path | SHA-256 | Contract surface |
|---|---|---|
| `internal/daemon/cst_direct_contract_windows_test.go` | `355ED055AA4B16059511690D90AE2F8985B01A0AE3F6E6F9A4FAB4E8E49D86B4` | Existing Go spawn owner, exact direct-image admission, otherwise-compatible `SysProcAttr`, singleton additional handle, exact frontend four-tuple, conflicting field rejection. |
| `servers/electromagnetics-mcp/tests/test_cst_native_runtime.py` | `4C9F33DB8E4A9EA9A510FC1F77919B0118B0C1E5A47D45D81185F6893F6E68D6` | Package-owned native image/verifier/manifest, independent PE facts, deterministic unsigned bytes, entry disassembly receipt, frontend four-tuple and worker five-tuple. |
| `servers/electromagnetics-mcp/tests/test_cst_saved_field_w1_contracts.py` | `B3317498F458B39F4557E6401C492E290304373B9DCD6058D8CCAD20D4CA56C0` | Fixed non-injected roots, real local transport owners, closed serializable receipt wire, native pre-main bootstrap/receipt, exact topology, no fake settlement, explicit default-off owner. |

## Exact RED matrix

### Go: 3 RED test IDs

1. `TestCstDirectImageAdmissionIsOwnedByExistingSpawnPath`
2. `TestInheritedHandleFrontendTupleIsExactlyFour`
3. `TestInheritedHandleConflictingSysProcAttrFailsClosed`

All three fail on missing accepted behavior. The package compiles; `internal/api` passes and `internal/cli` passes with no matching tests.

### Python: 17 collected cases, 16 RED and 1 sensitivity PASS

| Test family | Cases | Result |
|---|---:|---|
| `test_w01_red_independent_pe_verifier_admits_exact_pre_entry_image` | 1 | RED: package-owned image/verifier/manifest absent. |
| `test_w01_red_native_manifest_binds_deterministic_image_and_disassembly` | 1 | RED: manifest absent. |
| `test_w01_red_native_manifest_declares_exact_role_handle_tuple` | 2 | RED: frontend/worker manifest rows absent. |
| `test_w01_red_production_entrypoints_are_fixed_non_injected_roots` | 3 | RED: daemon/broker/worker still accept injected Python composition. |
| `test_w01_red_production_roots_materialize_real_local_transport_owners` | 3 | 2 RED for missing named-pipe owners; 1 PASS for the existing containment owner. |
| `test_w01_red_owner_local_receipts_are_closed_serializable_wire_values` | 4 | RED for missing/incomplete native-bootstrap, pre-main and split transport receipt wires. |
| `test_w01_red_worker_contract_declares_exact_five_handle_tuple` | 1 | RED: native worker receipt/tuple absent. |
| `test_w01_red_no_fake_settlement_or_default_on_composition` | 1 | RED: fixed default-off composition owner absent; literal fake settlement checks remain clean. |
| `test_w01_red_production_topology_is_exactly_three_endpoints_and_four_schemas` | 1 | RED: closed topology contract absent. |

The single PASS is intentional sensitivity evidence: the matrix does not fail merely because it is RED; it recognizes the already-real `WindowsContainedInvocation` owner.

## Fresh verification

| Check | Result |
|---|---|
| Focused Go W1 command from the plan | Expected RED: 3 named test failures; `internal/api` PASS; `internal/cli` PASS/no matching tests; no compile error. |
| W1-only Python run | Expected RED: 16 failed, 1 passed; no collection, import, or fixture error. |
| Combined W0 + W1 + T15 + integration run | Expected RED: 24 failed = preserved 9 W0 plus the then-current 15 W1 failures; selected implemented cases pass. The final added topology guard brings final W1-only RED to 16. |
| `ruff check` on both W1 Python files | PASS. |
| `ruff format --check` on both W1 Python files | PASS. |
| `tests/test_servers.py tests/test_stdio.py` | PASS: 6 tests. |
| W0 RED hash recheck | PASS: remains `065CE479AF32B82B564AA46E8A7E8CFBEED8E7AF0C3DC97B76BF38C62457B75D`. |

## Contract statement

No Hypertext Transfer Protocol, Remote Procedure Call, database, queue, cache, command-line, authorization, pagination, retry, persistence, or production wire contract changed. W1 adds only RED test contracts. The planned future receipt wire is additive and closed; exact before/after values are not claimed until their production owner phase implements and verifies them.

## W2 toolchain handoff

W2 must satisfy the native-only subset first, without weakening any assertion:

1. Materialize the package-owned `native/cst-runtime/` image, independent verifier and deterministic manifest.
2. Make the verifier parse AMD64 entry/import/delay-import/TLS/CLR/bound-import/section/mitigation facts independently and bind disassembly through the last role-required revocation.
3. Emit exact role rows for frontend `stdin,stdout,stderr,capability-read` and worker `stdin,stdout,stderr,source-root,workspace-root`.
4. Preserve the W1 RED state for Go composition/receipts/default-off; those belong to W3-W6, not W2.
5. Return a fresh native build/verifier manifest and run only the native W1 cases plus the W0 preservation oracle before handoff.

## Risks, rollback and next gate

- Residual risk: source-presence checks in the Go RED file are an admission guard, not runtime proof; later phases must pair them with actual executable/handle tests.
- Adjacent findings: none.
- Rollback: remove only the three W1 test files and this artifact. No production or unrelated path requires restoration.
- Next owner: `$toolchain-engineer` for W2, using the exact native handoff above.

Gate: PASS

## Terms and Abbreviations

- CodeGraph: repository code-intelligence MCP used for current owner and call-path evidence.
- MCP: Model Context Protocol.
- PE: Windows Portable Executable.
- RED: a test deliberately demonstrated failing before implementation.
- TLS: Thread Local Storage in the PE load-time sense.
- CLR: Common Language Runtime PE directory.
- W0/W1/W2: ordered implementation phases in the accepted plan.
