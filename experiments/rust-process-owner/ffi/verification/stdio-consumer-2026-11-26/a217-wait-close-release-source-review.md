# a217 source review of wait, close and release

**Static review, 26 November 2026.** The reviewed source path preserves the distinction between observing exit, attempting native cleanup, and releasing the language-boundary allocation. No defect was established in the five-file slice below.

## Source identity

The reviewed commit is `a217c7799a0214967bf86211542e508162c680b5`, tree `ca118ff1ec513cc830bb8b96b4d6782d23915a26`. The preserved archive contains 64 native-component source files totaling 649,751 bytes. Archive SHA-256 is `7176c8987e29f409c57bd26e88f6228af8b5a77eef39bc48c928b87d2fb8c3e0`. Readback recomputed the lengths, Git blob identifiers and SHA-256 digests of all 64 files against the acquisition inventory and the preserved historical digests. SHA-256 is the 256-bit Secure Hash Algorithm. The source review below follows selected functions in five files. [Acquisition and inventory](https://github.com/applicate2628/mcp-local-hub/blob/2a0657728b45d7dc1c654101c323f883424b7da3/docs/modernization/verification/residual-next-closeout-20261122/C-source-triage/A217-SOURCE-ACQUIRED.md) · [Preserved archive](https://github.com/applicate2628/mcp-local-hub/blob/2a0657728b45d7dc1c654101c323f883424b7da3/docs/modernization/verification/residual-next-closeout-20261122/C-source-triage/source-evidence.zip)

**Table 1. Reviewed source paths and digests.** Paths are relative to `experiments/rust-process-owner/` in a217. Archive entries use the additional prefix `source-a217c779/`.

| File | SHA-256 | Reviewed functions or contract |
| --- | --- | --- |
| `ffi/include/owner_v1.h` | `441ee6dfb105e00c3073d42bd69d828864711cc3053aafa3676f9da928e60615` | Paired extension and release declarations, lines 123–150 |
| `ffi/go/wait_windows.go` | `e68d3c473974ca6d22964f1d211b50edb020663c3c1a669152499f6421ba6925` | `exitResult`, lines 26–71 |
| `ffi/go/bridge_windows.go` | `c2e6055dbe2874dcc9544fc455b8a190cc2f75c9b02ea01b62a4ea12fab91811` | Symbol loading and dispatch, lines 67–113. `Library.Close`, lines 268–282. Owner operations, lines 423–507 |
| `ffi/src/lib.rs` | `4bbb985928887c11b311baa7194a38de39c6e4fbdaf8480a4bd25700a27fb025` | Outcome mapping and snapshot, lines 202–247. Wait, close, finish and release exports, lines 646–792 |
| `src/windows.rs` | `4c031c1a3824b83c7cec4a63a61120c5d9597f893c0819f6dee8613cc14e90a9` | Original root wait, lines 665–686. Job observation and terminal cleanup, lines 720–821 |

## Observed control flow

1. **Wait.** The Go bridge admits the split operation only when the dynamic-link library exposes both `mh_wait1` and `mh_close1`. The Rust implementation waits on the original root process handle, then observes the active member count of its Windows Job using the remaining budget. The `EXITED` observation requires a known root exit and zero active members. The Go decoder verifies the version and 32-byte output layout, then checks status, flags and counts. The wait path retains the native handles and the owner allocation.
2. **Close.** `CloseExited` refuses an active member observation. For a live owner it rechecks root and Job exit with zero wait before consuming any handles. Cleanup consumes the original thread, process and Job handles with one close attempt per handle. The terminal result preserves close failures and is cached for later calls. The application binary interface leaves product permission to close with the caller.
3. **Release.** `mh_release1` requires a terminal owner. It reports the cached settlement, consumes the Rust allocation and clears the caller's pointer. The Go bridge tracks actual pointer clearing even when outcome decoding fails, and the library refuses to unload while it still has live owners. Native cleanup status and allocation release remain separate facts.

This conclusion covers the archived a217 slice under the documented exclusive-owner and buffer contracts. The other 59 preserved files were hash-checked. The slice does not establish runtime behavior, a complete source review, or equivalence to the later private Go-consumer source or its library. No tests, builds or native experiments were executed for this source review. The [consumer experiment](README.md) retains its separate `NOT_RUN` comparison outcome.
