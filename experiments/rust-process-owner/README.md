# RX-02: isolated Windows process-owner experiment

**Not a Hub backend, not a replacement for `internal/process`, and not admitted to production.** This standalone crate is a reviewable first slice of the [MCPHub Rust plan](https://github.com/applicate2628/mcp-local-hub/pull/623). The existing web UI, Go policy/supervisor, application state and installed binaries are unchanged.

## Scope

The experiment owns one original process handle, primary thread and unnamed Windows Job. Creation uses `PROC_THREAD_ATTRIBUTE_JOB_LIST` and `CREATE_SUSPENDED`: membership is applied at creation rather than after an already running child. A single-use admission operation exposes a borrowed, observation-only `ProcessView` and then resumes the retained thread.

`wait_root` observes the original handle; the numeric exit code `259` is not mistaken for a running process. `finish` distinguishes a pending root, an exited root with active descendants, a settled Job, and failed native close attempts. Pending results keep the same owner. Repeated terminal observations are cached; a failed/ambiguous native action is not automatically retried through another implementation.

Rust's `Drop` closes the kill-on-close Job as a fallback but does **not** claim wait/reap or successful cleanup. Only explicit `finish` can produce `Complete`. Failed close attempts are recorded once and are not repeated on a potentially recycled numeric handle. Fault-injection qualification of that path is still required.

## Implemented first slice

- Absolute executable path and lossless UTF-16 command-line quoting; embedded NULs and overlong command lines rejected.
- At-create Job membership, suspended creation and original process observation.
- Admission rejection leaves the original suspended owner; it cannot be retried as a new approval.
- Deadline-bounded wait/settlement and explicit Job termination without PID reopening.
- Original root result survives later descendant termination and cleanup.
- Safe Rust borrowing prevents retaining the observation past the admission call; a compile-fail example checks this boundary.

The Windows target uses only the existing cached `windows-sys = 0.61.2` bindings. `publish = false`; this crate has no connection to the Hub build, installer, dependency files or runtime. No additional COM client or intermediary daemon is introduced.

## Real checks performed

On Windows 11 build 26100, x86_64, Rust 1.99.1 (`x86_64-pc-windows-msvc`):

- 13 native tests passed using only the local `owner_child` helper.
- They cover suspended admission and image/PID observation, denied/repeated admission, finite pending state, root+descendant termination, a completed root with a live descendant, preserved nonzero/259 exit status, wide arguments, invalid arguments, missing executable, and stable handle count over 24 complete lifetimes.
- One compile-fail doctest rejects a borrowed `ProcessView` escaping admission.
- `cargo clippy --offline --locked --all-targets -- -D warnings` passed.
- Negative stub execution failed nine native cases as expected. A temporary mutation treating root exit as whole-Job settlement was detected; the original source was restored and all checks repeated.

These checks are evidence for their actual scenarios, not a proof of all possible schedules or native API failures.

## Reproduce without installs

Use an already available compatible Rust toolchain and cached pinned dependencies. If the cache is missing, stop this task rather than downloading dependencies silently.

```powershell
$env:CARGO_NET_OFFLINE = 'true'
$env:MCPHUB_RX02_FIXTURES = Join-Path $env:TEMP ('mcphub-owner-fixtures-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $env:MCPHUB_RX02_FIXTURES | Out-Null
cargo test --offline --locked -- --test-threads=1
cargo clippy --offline --locked --all-targets -- -D warnings
cargo fmt --all -- --check
```

Run native tests serially: the handle-count check concerns the test process, not unrelated concurrent tests. Each invocation gets a new owned fixture directory. The fixtures are intentionally retained for review and trash-first disposition, never automatically deleted. Only the explicitly compiled local child executable is launched; it has a finite fallback lifetime and no shell, network, solver or model invocation. Windows child creation requests no console window.

## Remaining gates before any integration

1. Explicit environment/current-directory policy and inherited/borrowed standard streams with an exact inheritance allowlist.
2. Cancellation during admission/resume/settlement, native failure injection at every acquisition/close step, and owner-crash behavior.
3. A reviewed Go/Rust ABI and real calling-side tests: safe Rust's borrowing does not prove the foreign caller's correctness.
4. Equal-contract comparison with the accepted Go owner and the current selected-activation/source identity requirements.
5. Independent lifecycle/security review and the existing repository integration/merge gates.
6. Native Linux/macOS contracts and tests. Non-Windows strict creation currently returns `Unsupported` before side effects; that is **not** a functional Unix backend.

This first slice has no stdio capture, log pumping, restart policy, persisted state, scheduler, runtime migration or deployment. It does not establish a sandbox or stronger permissions than the launching user. The CMake/relay measurements and mesh-export work remain separate responsibilities.

## Source layout

`src/windows.rs` owns the native mechanism; `src/lib.rs` exports the experiment's result types and explicit unsupported route. `src/bin/owner_child.rs` is a finite fixture executable. `tests/lifecycle.rs` validates actual native outcomes. There is deliberately no generic platform framework, callback registry, global owner table or production C ABI.
