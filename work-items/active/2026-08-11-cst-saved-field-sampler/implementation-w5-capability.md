# W5 Broker Capability Owner Prerequisite

Execution role: `$platform-engineer`

Accepted design: `7423D56DD33394336A06AB8C515D12F4496B4AC0533F2901BF5A0EE1436756ED`

Prior W5 result: `1F24689B10D2774FB2FBB73E524E3F387AA15E04503766128C7CE71C60814BC6`

Native prerequisite: `79B69DD1914749FF5D4E18E7BAFD8C38B58AB004F8DA8EA6EAA6B42E4593016C`; image `38D87C50F716E334F89628D4F35604534C3A35BDCF35F378F1D939137BAB89E3`.

## Receiving-side echo

Implemented only the Windows broker-owned source/workspace root capability prerequisite. Existing six tools, W4, native image, vendor/application W6, live Service Control Manager/CST state, dependencies, Git index, and publication remain unchanged.

## Result

- Replaced reuse of generic `PolicyPlatform.hold_restricted_directory` with one broker capability owner.
- Source uses exact access `0x00120089`, `FILE_SHARE_READ`, `OPEN_EXISTING`, `FILE_FLAG_BACKUP_SEMANTICS|FILE_FLAG_OPEN_REPARSE_POINT`.
- Workspace uses exact access `0x0012019F`, `FILE_SHARE_READ|FILE_SHARE_WRITE`, the same disposition/flags, and no delete share.
- Both roles fail closed on reparse, final-path, file identity, protected owner/DACL, or original-inherit mismatch.
- Exact `DuplicateHandle` requests inheritability with options zero; `NtQueryObject`, object-type, handle flags, and duplicate identity are read back.
- `BrokerCapabilityReceiptV1` binds the correlation, exact tuples, identities, access readbacks, type and inherit/protect observations.
- The singleton non-reentrant `WorkerInheritanceEpoch` begins before capability duplicates and standard pipes and ends only after `CreateProcessW` returns and all five parent copies close. Failure settlement closes duplicates/originals and releases the epoch.
- Broker composition injects the capability opener for deterministic tests without adding a second production route.

## Changed paths

- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_containment_windows.py`
- `servers/electromagnetics-mcp/src/mcphub_em_mcp/cst_saved_field_broker_service_windows.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_containment_windows.py`
- `servers/electromagnetics-mcp/tests/test_cst_saved_field_integration.py`

These files already contained the accepted dirty W5 candidate; edits were applied as exact hunks and the reported Git diff stat includes prior W5 work.

## Verification

| Check | Result |
|---|---|
| Original safe Win32 reproduction | RED: `DuplicateHandle(...0x00120089, TRUE, 0)` through generic `0x001200A7` opener returned `WinError 5`. |
| Exact safe Win32 containment suite | PASS: `32 passed`. |
| Focused Win32 plus updated integration fixture | PASS: `33 passed`; one existing Pydantic unresolved-forward-reference warning. |
| W5 containment/pre-main/integration/vendor set | PASS: `89 passed`; same warning. |
| Scoped Ruff check / format check | PASS / PASS. |
| Python compile | PASS. |
| Git diff check | PASS. |
| CodeGraph | Fresh status, sync, fresh status, post-edit query; one broker `CreateProcessW` route through `CtypesWindowsKernel -> _invoke_atomic_job_process`. |

## Rollback

Rollback is the four-path capability-owner hunk group above. It was not exercised because the shared dirty W5 candidate must not be reset or overwritten. No external state was changed.

## Gate

`PASS:broker-capability-owner-prerequisite`

The previous exact `WinError 5` blocker is closed. Resume the W5 backend owner at W05-AC01/W05-AC02 using this owner and receipt; W6 remains out of this package.

## Adjacent findings

None.

## Terms and Abbreviations

- DACL: discretionary access-control list.
- SCM: Service Control Manager.
- W5/W6: ordered implementation phases in the accepted plan.
