# Supplemental review: native efa73 window-policy delta

## Contents

1. [Scope and exact pins](#scope-and-exact-pins)
2. [Behavioral delta](#behavioral-delta)
3. [Review result and acceptance boundary](#review-result-and-acceptance-boundary)
4. [Applicability of the a217 findings](#applicability-of-the-a217-findings)
5. [Sources](#sources)

## Scope and exact pins

This is a bounded review of the **one changed source file** identified by the native qualification reviewer after the 31-file a217 Rust/ABI review. The entire supplied diff and its affected creation path were read. The native file was not counted as a second full-file review of all 1,085 lines, and the 64-file source review was not repeated. No build, native run, GUI probe or source mutation was performed.

**Table 1. Immutable source and artifact pins.**

| Item | Exact identity |
| --- | --- |
| Reviewed baseline | Commit `a217c7799a0214967bf86211542e508162c680b5`; tree `ca118ff1ec513cc830bb8b96b4d6782d23915a26` |
| Native build source | Commit `efa73c0e23f7af16b8aa2ccb047b745a58ba4f71` |
| Changed source | `experiments/rust-process-owner/src/windows.rs` |
| a217 file | 42,672 bytes; 1,078 lines; blob `63d1c91514f6f84753d50eb12a0a6bbe2b0ee514`; SHA-256 `4c031c1a3824b83c7cec4a63a61120c5d9597f893c0819f6dee8613cc14e90a9` |
| Native file | 43,064 bytes; 1,085 lines; blob `c279bfba630d1748c8894a41de69401a10a66165`; SHA-256 `110d7cec3dc450cf4049c8e8435fd54f50f734fb4c7d42bd126a75158459f499` |
| Native DLL receipt | 192,512 bytes; SHA-256 `dfa87a10a39e9110cda735ae6a987bfa64b03446529072710723e9b6ea0dca56` |
| Consumer private source | Commit `3ebe335a2f666a5279d7fafc31a6f4dc3feedff4`; tree `67c13b8fc1317b4fd60e3135a3eb8a376fe01817` |

The native qualification reviewer's [native-dll-source-map.json](native-dll-source-map.json) records 63 of the 64 inventory paths as byte-identical, with only the file above different. It also records the native artifact and receipt hashes; execution-environment locations are omitted from this public copy. This supplemental reviewer independently checked both local window-source digests, byte counts, the native Git blob hash, and that [native-vs-a217-windows.patch](native-vs-a217-windows.patch) exactly equals the diff of those local texts. The remaining 63 native file comparisons and DLL provenance are attributed to that supplied mapping, not independently re-executed here. **Full source byte equivalence is false.**

## Behavioral delta

The native file adds the `STARTF_USESHOWWINDOW` import at line 45 and this block in `NativeOwner::create_suspended_impl`, lines 455–460, immediately after initialization of `STARTUPINFOEXW` and before stdio attribute composition:

```rust
if !foreground {
    startup.StartupInfo.dwFlags |= STARTF_USESHOWWINDOW;
    startup.StartupInfo.wShowWindow = 0; // SW_HIDE
}
```

The existing `STARTF_USESTDHANDLES` assignment is also an OR operation, so the added show-window flag remains present when redirected stdio is supplied. The attribute list, borrowed source handles, Job, process-creation suspension and owner-publication mechanism are unchanged by the diff.

**Table 2. Startup policy from the actual branches.** Core line references below use a217; the new block uses native efa73 lines.

| Creation mode | Existing console creation flags | New native startup show policy |
| --- | --- | --- |
| `foreground = false` | `CREATE_NO_WINDOW`; parent-console observer is not called | `STARTF_USESHOWWINDOW`, `wShowWindow = SW_HIDE` |
| `foreground = true`, parent console attached | No `CREATE_NO_WINDOW` | No `STARTF_USESHOWWINDOW`; startup show value remains ignored |
| `foreground = true`, parent console absent | `CREATE_NO_WINDOW` | No `STARTF_USESHOWWINDOW`; GUI startup show policy remains unchanged |
| `foreground = true`, parent-console observation fails | Creation returns the observation error | No child creation to qualify |

The branch is driven by the requested foreground mode, not by whether `CREATE_NO_WINDOW` was ultimately selected. Therefore foreground creation without an attached parent console does not acquire the new hiding policy. This matches the added comment's distinction.

The core constructors pass `foreground = false` for ordinary/context/stdio/explicit-argv0 creation (`src/windows.rs:316–372`) and `true` only for `create_suspended_foreground`. In the FFI, request versions 1 and 2 select false, version 3 selects true (`ffi/src/lib.rs:406–410`, `482–505`). The new comment names background and explicit NoConsole consumers; the source-level claim here is limited to those actual false-valued branches. Product mode selection outside this inventory remains the consumer review's responsibility.

### The change also affects GUI startup

Microsoft documents `STARTF_USESHOWWINDOW` as making `wShowWindow` active. For a GUI process, that value controls its first `ShowWindow` call; later calls use it when requesting `SW_SHOWDEFAULT` [source 3]. In contrast, `CREATE_NO_WINDOW` alone is ignored for a non-console application [source 2]. Thus this diff has an additional documented consequence for a GUI child started with `foreground = false`: its initial show policy is hidden. This is a semantic change from a217, even if the intended motivation was a later console allocation. Subsequent child-controlled window operations are not a general invisibility guarantee.

### Later AllocConsole is a qualification claim, not established by this source read

Microsoft's startup-property documentation says supplied startup information also influences a console allocated later with `AllocConsole` [source 4]. Its listed console properties do not explicitly establish the full show/hide behavior asserted by the new source comment. The code demonstrably supplies the hide value; proving the target console host's visible result requires the appropriately bound native evidence. No such probe was executed by this reviewer, and source comments are not substituted for it.

## Review result and acceptance boundary

**No additional ownership, cleanup, ABI-layout or branch-composition defect was found in the exact delta.** The additive flag is scoped consistently to `!foreground` and coexists with stdio flags. This is a static conclusion about the supplied change, not a runtime PASS.

The material review note is the broader initial GUI-window policy described above. Whether hiding a nonforeground GUI child's first window is required behavior is a consumer contract decision. It is not labeled a defect here without a contradictory requirement. The concrete behavior should remain visible in the qualification description; “same behavior as a217” would be incorrect.

Existing `src/foreground_console_tests.rs` checks parent-console observation and `CREATE_NO_WINDOW` selection. It does not inspect the new `STARTUPINFO` show fields or a later `AllocConsole` result. All other inventoried test files are byte-identical according to the supplied map. Therefore this delta cannot claim fresh coverage merely because those earlier source tests were reviewed. If the qualification record contains an executed GUI/console fixture, use that exact artifact's original receipt and conditions; this supplemental review does not invent or upgrade one.

The narrow relevant behavioral distinctions are: background/NoConsole hidden startup, foreground with a parent console, and foreground without a parent console retaining its GUI policy. No additional native work is requested by this report, and no broad window-management guarantee is asserted.

## Applicability of the a217 findings

Both findings in [RUST-ABI-REVIEW.md](RUST-ABI-REVIEW.md) remain applicable to the mapped native source:

- **RUST-01:** the identity encoder, token-user error producer, identity header and Go decoder are all in unchanged files. The new window block does not change their data representation.
- **RUST-02:** the base encoder/header/Go validator are unchanged. The `windows.rs` termination and close branches cited in that finding are textually unchanged after the insertion; their native line numbers shift by seven, to 712–715 and 810–813 respectively.

This carries over the conditional source findings, not an assertion that native Win32 returned a zero last-error in the DLL's execution. No prior FAIL/UNKNOWN result, physical closure result or runtime qualification status is reclassified by this mapping.

## Sources

1. Local [native mapping and provenance](native-dll-source-map.json), [exact diff](native-vs-a217-windows.patch), and both source-file identities pinned in Table 1. The mapping was supplied by the native qualification reviewer; this reviewer checked the local delta as described above.
2. Microsoft Learn, [Process Creation Flags](https://learn.microsoft.com/en-us/windows/win32/procthread/process-creation-flags), `CREATE_NO_WINDOW`: the flag is ignored for applications that are not console applications. Retrieved 2026-11-26.
3. Microsoft Learn, [STARTUPINFOW](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/ns-processthreadsapi-startupinfow), `STARTF_USESHOWWINDOW` and `wShowWindow`: active show value, initial GUI show and later `SW_SHOWDEFAULT` behavior. Retrieved 2026-11-26.
4. Microsoft Learn, [Setting Window Properties Using STARTUPINFO](https://learn.microsoft.com/en-us/windows/win32/procthread/setting-window-properties-using-startupinfo): startup properties for initial GUI windows and newly allocated consoles, including later `AllocConsole`. Retrieved 2026-11-26.
