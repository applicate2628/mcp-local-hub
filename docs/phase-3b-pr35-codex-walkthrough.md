# PR #35 Codex Review Walkthrough — Security Batch Consolidation

PR #35 consolidated 28 open Codex-auto-generated security PRs (#6–#28, #30–#34; #29 closed as obsolete) into one reviewable bundle. This walkthrough captures the lessons from the 6 Codex review rounds that followed, organized by the failure patterns behind each finding.

**Scope:** 6 rounds, 6 unique findings, 5 fix commits on top of the 28 cherry-picks.

---

## 1. Cherry-pick hygiene failures

The two most dangerous findings were introduced **by the batch consolidation itself**, not by any upstream Codex PR. They illustrate two classic bulk-rebase traps.

### 1.1 Silent drop on whole-file conflict resolution

**Symptom** (caught offline, pre-PR):
When PR #14's version of `internal/api/install.go` was taken wholesale to resolve a conflict with PR #17, two independent helpers from overlapping PRs were lost:

- PR #17's `maxHealthProbeResponseBytes` constant and the bounded `io.ReadAll(io.LimitReader(...))` in `singleHealthProbe`.
- PR #16's `validateClientURLPath` helper and its call-site in `BuildPlan`.

The compiler only caught one of them (`TestInstallAllInstallsEverything` still compiled, so the URL-path regression hid until a test run). The health-probe constant was caught by an undefined-name compile error.

**Lesson:**
> When a conflict involves multiple overlapping PRs, "take one side wholesale" is a lie. The "losing" side frequently owns an orthogonal helper that gets silently deleted. Always `grep` for the lost side's symbols and for any undefined-name compile errors after a bulk rebase.

Fixed in commit `03c1b1c` (constants) + `a0d661d` (url_path validator) before the PR was even opened.

### 1.2 Cap-exceeded error swallowed by exit code

**Symptom** (caught by the test suite):
`TestLLVMObjdump_DisassemblesBinary` started failing with `tool returned IsError=true: llvm-objdump exited 74` even though the handler had code to report "output exceeded N bytes". PR #19's `runCaptureLimited` had a logic bug:

```go
err := cmd.Run()
if ee, ok := err.(*exec.ExitError); ok {
    res.ExitCode = ee.ExitCode()
    return res, nil        // ← takes this branch, loses the cap signal
}
if errors.Is(err, errOutputLimitExceeded) {
    return nil, errOutputLimitExceeded
}
```

When the capped writer returns `errOutputLimitExceeded`, Go's exec package closes the reader side of the stdout pipe. The child then gets SIGPIPE / ERROR_BROKEN_PIPE on its next write and exits with a platform-dependent code (74 here). `cmd.Run()` sees an `*exec.ExitError` and the cap-exceeded error is never surfaced — the caller just sees "exited 74, stderr empty".

**Fix:** track cap state on the writer itself and surface it before the exit-error branch:

```go
type cappedBuffer struct {
    buf      bytes.Buffer
    limit    int
    exceeded bool  // ← new
}

err := cmd.Run()
if stdout.exceeded || stderr.exceeded {
    return nil, errOutputLimitExceeded   // ← must come first
}
if ee, ok := err.(*exec.ExitError); ok { ... }
```

**Lesson:**
> Returning `(len(p), err)` from an `io.Writer` looks right, but `os/exec` collapses that error under `*exec.ExitError` whenever the child dies from a broken pipe. If you need the writer's error to survive, track the condition on the writer object, not in the return value.

### 1.3 `-s -w` strip trap in test binaries

**Symptom:**
After updating the llvm-objdump test to narrow the disassembly via `--disassemble-symbols=main.main` (to stay under the cap), the test still failed — llvm-objdump printed only the file-format header.

**Root cause:** `go test` links its own test binary with `-s -w` (strip symbol table + strip DWARF), so `main.main` is gone from the classic COFF/ELF symbol table even though it's declared in the source. The symbol table stub visible via `llvm-objdump -t` came from a **different** manually compiled binary.

**Fix:** build a small dedicated binary in `t.TempDir()` with a plain `go build` (no strip) and disassemble that:

```go
exe := filepath.Join(tmp, "hello.exe")
build := exec.Command("go", "build", "-o", exe, src)
build.Env = append(os.Environ(), "GOFLAGS=")  // ← clears global -ldflags
```

**Lesson:**
> Any test that does `os.Executable()` and expects named-symbol disassembly will silently fail under `go test`. `-s -w` is the default linker config. Build your own test artifact.

---

## 2. Defer-close swallows errors (R1)

**File:** `internal/clients/clients.go:164-182`
**Severity:** P2

PR #19's new `copyFile` helper used `defer out.Close()` and returned success as soon as `io.Copy` finished:

```go
out, err := os.OpenFile(dst, ..., perm)
if err != nil { return err }
defer out.Close()

if _, err := io.Copy(out, in); err != nil {
    return err
}
return nil
```

Disk-full and NFS-fsync errors typically surface **at close time**, not during `io.Copy` — the kernel buffers writes. A deferred Close with a discarded error reports success on what could be a truncated file, and `writeBackup` happily tells the user the backup succeeded.

**Fix:** close explicitly and propagate:

```go
if _, err := io.Copy(out, in); err != nil {
    _ = out.Close()
    return err
}
if err := out.Close(); err != nil {
    return err
}
```

**Lesson:**
> `defer file.Close()` is fine for readers. For writers, close explicitly and check the return. The buffered-write flush path is a real failure mode that `defer` silences.

---

## 3. SSE fan-out silently dropped by session-scoping refactor (R2)

**File:** `internal/daemon/host.go`
**Severity:** P1

PR #30 added session validation to the SSE endpoint (good) — but its diff also **removed the per-subscriber channel mechanism entirely**:

```go
// Before PR #30:
sseMu      sync.Mutex
sseClients []chan []byte
// handleSSE: register channel, select on it, write `data:` frames

// After PR #30 (pre-fix):
sseActive atomic.Int32    // just a counter
sessionID string
// handleSSE: wait for cancel/exit, never write anything
```

`readStdoutLoop` was also changed to no longer fan out unrouted lines. Any MCP backend that emits a JSON-RPC notification (message without `id`) — progress events, log updates, server-pushed state changes — was silently dropped on the floor.

**Fix:** restore `sseMu + sseClients + per-subscriber buffered channel` alongside PR #30's session-validation and subscriber cap. Session scoping gates **who can subscribe**; the fan-out reaches every validated subscriber as before.

**Lesson:**
> When a refactor's purpose is "add X", review the diff for "did it also silently remove Y". Session scoping and broadcast are orthogonal features that can coexist. A PR description that says "add session scoping" and a diff that deletes 40 lines of broadcast code is a red flag.

---

## 4. Retired-manifest uninstall asymmetry (R4)

**File:** `internal/api/install.go`
**Severity:** P1

PR #13 removed `servers/gdb/manifest.yaml` to prevent gdb from being installed as a shared long-running daemon (security concern: stateful gdb sessions are hijackable). Install path was correctly blocked. **Uninstall was not.**

```go
func (a *API) Uninstall(server string) (*UninstallReport, error) {
    data, err := loadManifestYAMLEmbedFirst(server)
    if err != nil {
        return nil, fmt.Errorf("load manifest %s: %w", server, err)
        // ↑ users who installed gdb before the upgrade are stuck here,
        //   with stale scheduler tasks and client entries and no
        //   supported way to clean them up
    }
    ...
}
```

**Initial fix (R4):** fall back to a manifest-less cleanup path that deletes tasks by prefix and removes client entries across all known clients.

**Lesson from R4:**
> A security-motivated "we no longer ship X" change must still answer "how do existing users of X get to a clean state?". Install/uninstall asymmetry is a migration-path bug, not a feature decision.

---

## 5. Unbounded-trust fallback = destructive prefix match (R5)

**File:** `internal/api/install.go`
**Severity:** P1

My R4 fix had a subtle but severe regression: the manifest-less fallback ran for **any** `ENOENT`, combined with scheduler-task `List(prefix)` that matches `HasPrefix`:

```go
// R4 version:
if os.IsNotExist(err) {
    return a.uninstallWithoutManifest(server)
}
// uninstallWithoutManifest:
prefix := "mcp-local-hub-" + server     // ← no trailing dash
tasks, _ := sch.List(prefix)
for _, t := range tasks {
    sch.Delete(t.Name)                    // ← deletes whatever matches
}
```

Consequence: `mcphub uninstall --server se` → prefix `mcp-local-hub-se` → matches `mcp-local-hub-serena-default`, `mcp-local-hub-serena-codex`, any future `mcp-local-hub-secrets-*` — **all deleted**.

**Fix (R5):**
1. Allowlist explicit retired-server names:
   ```go
   var retiredServerNames = map[string]bool{
       "gdb": true,  // removed in PR #13
   }
   if os.IsNotExist(err) && retiredServerNames[server] {
       return a.uninstallWithoutManifest(server)
   }
   return nil, fmt.Errorf("load manifest %s: %w", server, err)
   ```
2. Narrow the fallback prefix to match PR #31's `"mcp-local-hub-<name>-"` shape (trailing dash) so `gdb-default` matches but `gdbtool-*` does not.

**Lessons:**
> - **Tolerant error handling on unvalidated input is a vulnerability class.** When the normal path's validation (here, manifest presence + `ParseManifest`) is also the input-validation step, falling back to a "try harder" path on ENOENT turns every typo into a blast radius.
> - **Two recent fixes conflict when you forget one of them.** PR #31 had already narrowed the main path's prefix to `"mcp-local-hub-<name>-"`. My R4 fallback reintroduced the over-match that PR #31 fixed. Any new code path that touches scheduler deletion must honor the same boundary convention.
> - **`retiredServerNames` is a zero-cost future-proofing surface.** The next time a manifest retires, add one line; every other unknown name still fails fast.

---

## 6. Test portability (R4b)

**File:** `internal/perftools/exec_test.go`
**Severity:** P3

`TestRunCaptureLimited_EnforcesStdoutLimit` hard-coded `bash -lc 'printf ...'` — fine for a dev Linux/macOS machine, broken on standard Windows CI runners without bash installed.

**Fix:** `exec.LookPath("bash")` + `t.Skip` when missing. The production logic under test (`runCaptureLimited`) is shell-agnostic; the test's need for an external process was the only reason to reach for a shell at all.

**Lesson:**
> Integration tests that shell out to `bash`, `sh`, or any specific binary should either resolve via `exec.LookPath` + `Skip`, or use a portable helper binary compiled inline from the test. Windows CI is a first-class target.

---

## Meta-patterns across the 6 rounds

### Bulk-rebase workflow

| Trap | Detection signal | Prevention |
|---|---|---|
| Whole-file conflict drops helper from losing side | Undefined-name compile error; silent test failure | After every conflict resolution, `grep -rn "<lost-side-symbol>"` and run the full suite |
| Multiple PRs add the same feature (#7 vs #18, #15 vs #20) | Semantic duplicate, not textual conflict | Inspect commits for overlapping goals before cherry-picking; prefer the more complete version and skip duplicates |
| Retained test references a renamed symbol | Compile error in test files | Look for test files touching the conflicted symbol — they weren't in the diff |

### Tolerant-error-handling trap

Several rounds circled the same anti-pattern: "when X fails, try Y instead" without gating Y on the expected precondition. R5 was the sharpest instance. The safe form always has three parts:

1. What error are we tolerating (`os.IsNotExist`)?
2. What input invariant makes the fallback safe (`retiredServerNames[server]`)?
3. What happens if the invariant fails (explicit error, not a wider fallback)?

### Codex cadence observations

- First review typically arrives **7–10 min** after `@codex review`.
- Codex occasionally silently drops a trigger; a second `@codex review` comment usually wakes it up.
- Unresolved review threads appear on the latest commit via the GitHub API, not their original commit. Use `created_at` for real recency, not the `commit_id` association.
- A CLEAN verdict is posted as an issue-level comment, not a formal review. Filter on the `chatgpt-codex-connector[bot]` username and look for "Didn't find any major issues" as the clean signal.
