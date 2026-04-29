# C1 — `mcphub gui --force`: stuck-instance recovery

**Status:** rev 9. Codex r7 returned REVISE on rev 8 (no BLOCKER,
no SUGGEST blockers) — strict `argv[1] == "gui"` would refuse the
no-arg Explorer/Start-menu double-click path where
`cmd/mcphub/main.go:32` appends `"gui"` internally. Rev 9 widens
the gate to accept either `argv[1] == "gui"` OR `len(argv) == 1`
(the no-args internal-default case). All other r7 questions
returned PASS or SUGGEST (nice-to-have, not merge-blockers).
Design has converged across 7 review rounds.

**Scope:** PR #23 — replace the placeholder `--force` warning with a
genuinely useful stuck-instance recovery flow. Default `--force` shows
the diagnostic AND opens the lock folder in the OS file manager, so the
operator has both the explanation AND immediate one-click access to
the offending files. An explicit `--force --kill` adds opt-in
destructive automation: verify the recorded PID owns an `mcphub.exe`
image, kill it, wait for the kernel to release the flock, retry
acquire. Everything destructive is gated behind `--kill`; bare
`--force` is non-destructive.

## Why automation is opt-in (Codex r1-r4 carryover)

Four rounds of architecture review (r1, r2 Claude, r3, r4) established:

- After `flock.TryLock` returns busy, the OS guarantees a live
  process holds the lock. Pidport is metadata; it may not match the
  actual flock holder.
- "Remove `<pidport>.lock` then `TryLock`" splits ownership on POSIX
  in the inode-replace race window — Codex r4 #7. So we never
  delete the lock file directly.
- Cross-platform holder-proof is unavailable in public APIs:
  `gofrs/flock` v0.13.0 uses `flock(2)` on Linux (incompatible with
  `fcntl(F_GETLK)` queries — Codex r4 #1); Windows has no public
  API to enumerate `LockFileEx` holders.

The only **safe** automation that doesn't depend on holder-proof is
KILLING the holder process: when the holder process exits, the OS
releases the flock unconditionally — no inode races, no split
ownership. Risk shifts from "lock-domain corruption" to "killed the
wrong process". We mitigate the wrong-process risk via a **three-part
identity gate** before SIGKILL/TerminateProcess (Codex r5 #1 — image
basename alone is insufficient because Task Scheduler `mcphub.exe
daemon ...` children share the same image):

1. **Image basename canonical match.** Resolve the process executable
   path (`QueryFullProcessImageName` on Windows; `/proc/<pid>/exe`
   readlink on Linux); accept iff its base filename is exactly
   `mcphub.exe` on Windows (case-insensitive) OR `mcphub` on POSIX
   (exact, no `.exe` extension — Codex r6 #6 fix; Linux/macOS
   binaries don't have the suffix).
2. **Command-line subcommand match.** Read the full argv via
   `NtQueryInformationProcess(ProcessBasicInformation)` →
   `PEB.ProcessParameters.CommandLine` on Windows (split honoring
   `CommandLineToArgvW` quoting rules); `/proc/<pid>/cmdline` on
   Linux (NUL-delimited, already split). Codex r6 #5: substring
   matching `gui` was unsafe — paths like `C:\users\guido\mcphub.exe
   daemon ...` or daemon args containing the literal "gui" would
   pass. Required gate: **`argv[1] == "gui"` (cobra subcommand
   token equality) OR `len(argv) == 1`** (the no-args Explorer/
   Start-menu double-click path — `cmd/mcphub/main.go:32` appends
   `"gui"` to `os.Args` internally when invoked with no command,
   so the EXTERNAL cmdline is just the executable path while the
   process is functionally a gui invocation; Codex r7 #6 fix).
   Anything else with explicit non-`gui` argv[1] (`daemon`,
   `install`, `uninstall`, etc.) → fail closed.
3. **Start-time order check.** Read process start time
   (`GetProcessTimes` CreationTime on Windows; `/proc/<pid>/stat`
   field 22 starttime on Linux). Accept iff start-time ≤ pidport
   mtime + 1 second tolerance (a recycled PID belongs to a process
   that started AFTER our pidport was written; if we see start-time
   strictly later than pidport mtime, identity has changed). The
   1-second tolerance covers clock jitter between `os.WriteFile` and
   the kernel's processed start-time stamp.

All three must pass for `--kill` to fire. Any rejection routes to
exit 7 (kill-refused) with a diagnostic explaining which gate failed
("recorded PID is `mcphub.exe daemon` not `gui`", "recorded PID
started after pidport — recycled", etc.).

The remaining residual risk (a competing `mcphub.exe gui` invocation
that we'd kill instead of the stuck one — meaning another operator on
the same machine is also trying to use the GUI) is unrecoverable
without coordination and is documented as a known limitation.

## Interface

```
mcphub gui --force         # diagnostic + open lock folder; non-destructive
mcphub gui --force --kill  # diagnostic + kill recorded PID + retry startup
mcphub gui --force --kill --yes  # same as --kill but skip confirmation prompt
```

`--force` alone is hidden from `--help` (Codex r1 SUGGEST 1; remains a
runbook escape hatch). `--kill` is documented in `--force`'s long help
text but not advertised in the help summary.

## Default `--force` flow

1. Acquire flock as today. If success → normal startup; `--force` had
   no effect (no stuck lock). Silent, exit 0.
2. If `ErrSingleInstanceBusy`:
   a. Read pidport (PID, port, mtime).
   b. Probe `processID(pid)` → alive, denied, imagePath.
   c. Probe `pingIncumbent(port)` → matched-PID OR error.
   d. Print structured diagnostic block (see §"Diagnostic format"
      below).
   e. Open the lock folder in the OS file manager:
      - Windows: `explorer.exe /select,<pidportPath>` highlights
        the pidport file.
      - macOS: `open -R <pidportPath>`.
      - Linux: `xdg-open <dir>` (best-effort; falls back to
        printing the path if `xdg-open` is unavailable).
   f. Exit 2. Operator inspects the diagnostic, optionally manages
      files in the now-open folder, and reruns `mcphub gui` (with
      `--kill` for automation, or after manual recovery).

## `--force --kill` flow

1. Acquire flock as today. If success → exit 0; `--kill` was unnecessary.
2. If busy:
   a. Same diagnostic as above (so the operator sees what we're
      about to kill).
   b. **Healthy-incumbent early-exit** (Codex r5 #7b + r6 #2):
      run the same `pingIncumbent(port)` probe used by the bare-`--force`
      diagnostic. If `200 + matched-PID == recordedPID`, the recorded
      gui is healthy — running normally. `--kill` MUST NOT kill a
      healthy incumbent. Print a one-line notice to stdout —
      "incumbent is healthy (PID `<pid>`); activating instead of
      killing" — then route to the existing `TryActivateIncumbent`
      handshake (same as if `--kill` was omitted), exit 0. The
      notice tells operators who typed `--kill` why the kill didn't
      fire (Codex r6 #2: silent activation hides the override
      decision).
   c. **Three-part identity gate** (Codex r5 #1 fix; argv-token
      tightened in r6 #5; Explorer no-arg branch added in r7 #6).
      Run all three checks from §"Why automation is opt-in":
        - image basename canonical match (`mcphub.exe` Windows /
          `mcphub` POSIX),
        - argv subcommand: `argv[1] == "gui"` OR `len(argv) == 1`
          (the Explorer no-args branch the binary internally
          defaults to gui),
        - process start time ≤ pidport mtime + 1s tolerance.
      Any failure → exit 7 with a diagnostic naming WHICH gate
      tripped ("recorded PID image is not mcphub.exe",
      "argv[1] is `daemon …`, not `gui`", "process started after
      pidport — PID recycled").
   d. **Confirmation prompt — only when `--yes` was NOT passed.**
      Codex r6 #6a clarification: `--yes` ALWAYS skips the prompt,
      including under non-TTY (the whole point of `--yes` is
      scripted use). The non-TTY guard fires only when `--yes` is
      ALSO absent: if non-TTY AND no `--yes` → exit 6 (the operator
      can't see or answer the prompt; they should add `--yes`).

      With TTY and no `--yes`:

      `Kill PID <pid> (mcphub gui)? [y/N]:`

      EOF or non-`y` → exit 0 (cancelled).
   e. SIGKILL/TerminateProcess on the PID. (Linux: `unix.Kill(pid,
      unix.SIGKILL)`; Windows: `OpenProcess(PROCESS_TERMINATE)` +
      `TerminateProcess`.) Errors → exit 4 (kill-failed).
   f. Wait for kill to register: poll `processID(pid).alive == false`
      with a 5-second deadline.
   g. **Acquire-poll loop** (Codex r5 #2 fix — replaces the prior
      fixed 200ms sleep). Retry `AcquireSingleInstance(port)` in a
      `for` loop with 50 ms back-off, until either:
        - TryLock succeeds → proceed with normal startup; log
          "force-killed previous incumbent PID `<pid>` and acquired
          lock"; exit 0.
        - 2-second deadline elapses → exit 3 (race: kernel released
          the flock but a competitor won the new acquire; rerun
          without `--kill` to handshake with the new incumbent).
      `TryLock` is the source of truth for "lock is mine" — no
      fixed sleep guesses what the kernel needs.

`--kill` deletes nothing. The destructive operation is the kill, not a
file remove. The OS handles flock release as a side effect of process
termination, which is the only race-free path on both Linux and Windows.

## Diagnostic format

Single block emitted before any optional Explorer/kill action:

```
Cannot acquire mcphub gui single-instance lock.

Lock file:  C:\Users\dima_\AppData\Local\mcp-local-hub\gui.pidport.lock
Pidport:    C:\Users\dima_\AppData\Local\mcp-local-hub\gui.pidport
  recorded PID:  4128
  recorded port: 9125
  pidport mtime: 2026-04-29T08:14:32Z

Live-holder probe:
  PID 4128 status:    alive
  PID 4128 image:     C:\Users\dima_\.local\bin\mcphub.exe
  /api/ping on 9125:  connection refused

Recovery options:
  Automatic — kill PID 4128 (image verified as mcphub.exe), wait
  for the kernel to release the lock, retry startup:

      mcphub gui --force --kill

  Manual — close any open Chrome dashboard window, then:
      Windows: Task Manager → End Task on PID 4128
      Linux/macOS: kill -9 4128
  Then rerun:
      mcphub gui

  If PID 4128 does not appear in Task Manager / `ps` but this
  message persists, reboot the OS — a stuck file handle survives
  user-mode cleanup only across a session reset.
```

When the recorded PID is dead OR fails the three-part identity gate,
the "Automatic" section reads:

> **Refusing automatic recovery** — recorded PID `<pid>` is `<reason>`
> (e.g., "no longer alive", "is `mcphub.exe daemon` not `gui`",
> "started after pidport — recycled to an unrelated process"). The
> live process holding the flock is not the gui we recorded; mcphub
> cannot identify it. Identify the holder manually:
>
>   Windows: Sysinternals `handle.exe -a <lock-path>` (download from
>            https://learn.microsoft.com/sysinternals/downloads/handle).
>            **Requires an elevated/admin shell to enumerate handles
>            owned by other users.**
>   Linux:   `lsof <lock-path>` or `fuser <lock-path>`. Without admin
>            you'll only see your own processes; use `sudo` if the
>            holder belongs to a different user.
>
> Codex r6 #4: when admin tooling isn't available (locked-down
> machine, missing handle.exe), reboot the OS — a stuck file handle
> survives user-mode cleanup only across a session reset and
> reboot is the universally available recovery path.
>
> Then `kill -9 <pid>` (Linux) or Task Manager → End Task on that
> PID (Windows). DO NOT delete the lock file; the kernel handles
> flock release as a side effect of process exit, and deleting the
> file under a live holder splits ownership.

Codex r5 #7a fix: prior rev told users to "manually delete
`<lock-path>`" — that contradicts the no-remove invariant which is
unsafe for both us AND the operator (same inode-replace race). The
correct manual recovery is to identify and kill the actual holder,
not delete the file.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | Success — normal startup OR healthy incumbent activated OR `--kill` recovered |
| 1 | Non-force startup error (config, port bind for first acquirer, etc.) |
| 2 | Stuck-lock detected; bare `--force` exited after diagnostic + folder-open |
| 3 | `--kill` race — kernel released flock but a competitor won the new acquire first; rerun |
| 4 | `--kill` failed terminating the recorded PID (permissions, process ended mid-call, etc.) |
| 6 | Non-interactive shell with `--kill` and no `--yes` |
| 5 | RESERVED — not emitted by PR #23. Reserved for a future migrate-needed flow if pidport format is ever extended (Codex r5 #5). |
| 7 | `--kill` refused — three-part identity gate failed (image not `mcphub.exe`, OR command line not `gui`, OR start-time postdate pidport mtime) |

## Verdict struct (kept for A4-b future)

```go
type Verdict struct {
    Class     VerdictClass // see consts below
    PID       int
    Port      int
    Mtime     time.Time
    PIDAlive  bool
    PIDImage  string  // empty if denied or dead
    PIDCmdline string // truncated to 1KB for safety
    PIDStart   time.Time
    PingMatch bool    // ping returned PID matching recorded
    // Diagnose / Hint are derived. JSON marshalling skips them
    // (`json:"-"`); HTTP API formats from structured fields, CLI
    // calls a Format(v Verdict) string helper. Codex r5 #4: storing
    // a pre-formatted human block in struct is wasteful for
    // A4-b's JSON path and risks staleness if structured fields
    // change.
}

const (
    VerdictHealthy        VerdictClass = iota
    VerdictLiveUnreachable
    VerdictDeadPID
    VerdictMalformed
    VerdictKilledRecovered
    VerdictKillRefused      // image not mcphub.exe
    VerdictKillFailed
    VerdictRaceLost
)
```

A4-b's forthcoming `POST /api/force-kill` HTTP handler returns the same
`Verdict` as JSON. Both surfaces share the contract — UI doesn't
reverse-engineer strings.

## Files

- `internal/gui/probe.go` (cross-platform) +
  `probe_windows.go` / `probe_unix.go` —
  `processID(pid int) (alive bool, denied bool, imagePath string)`,
  `pingIncumbent(port int) (matchedPID int, err error)`,
  `killProcess(pid int) error`. ~140 LOC.
- `internal/gui/openfolder.go` (cross-platform dispatch) +
  `openfolder_windows.go` / `openfolder_other.go` — wraps
  `explorer.exe /select,<path>` / `open -R` / `xdg-open`. ~50
  LOC.
- `internal/gui/single_instance.go` — adds
  `BuildVerdict(p string) Verdict`,
  `KillRecordedHolder(p string, opts ...) (Verdict, error)`.
  Pidport format unchanged. ~120 LOC.
- `internal/cli/gui.go` — replace placeholder; map Verdict to exit
  codes; integrate `--kill` and `--yes`; conditional folder-open. ~140
  LOC.
- `internal/cli/gui_force_test.go` — 8 scenarios. ~220 LOC.
- `CLAUDE.md` — runbook section. ~30 LOC.
- `docs/phase-3b-ii-verification.md` — D2 row update. ~15 LOC.
- `docs/superpowers/plans/phase-3b-ii-backlog.md` — A4-b row
  forward-reference to Verdict + force-kill HTTP contract. ~5 LOC.

**Total ~830 LOC** across 8 files (rev 7 +90; rev 8 +20 for argv
parsing helper that splits CommandLineToArgvW on Windows and
NUL-array on Linux, plus the healthy-kill notice + admin/reboot
wording in diagnostic templates).

## Tests

8 scenarios:

1. **Healthy `--force` no-op** — pre-bind real listener; populate
   pidport with the listener's PID. Run `--force`. Assert: handshake
   activates, exit 0, no folder open invoked, no diagnostic emitted.
   Companion: same setup, run `--force --kill --yes`. Assert: NO
   kill attempted (Codex r5 #7b — healthy incumbent must not be
   killed even when `--kill` was passed); handshake activates; exit
   0.
2. **Stuck — bare `--force` shows diagnostic + opens folder** —
   populate pidport with `os.Getpid()` (alive, mcphub-test-binary as
   image). Run `--force`. Assert: diagnostic includes "alive" + image
   path; folder-open helper called via test seam; exit 2; lock NOT
   removed.
3. **`--force --kill` happy path** — Codex r5 #6: this scenario
   replaced with a test seam that mocks the three-gate verifier so
   it returns "all gates pass" against the test binary's own PID.
   The REAL behaviour is exercised in scenario 8 below (real
   `cmd/mcphub` subprocesses with independent flock state).
4. **`--force --kill` refuses non-mcphub image** — populate pidport
   with `os.Getppid()` (typically the shell, not mcphub). Run
   `--force --kill --yes`. Assert: exit 7, no kill attempted.
5. **`--force --kill` non-interactive without `--yes`** — close
   stdin. Run `--force --kill`. Assert: exit 6, no kill attempted.
6. **`--force --kill` race-lost** — pre-acquire flock; populate
   pidport accordingly. Run `--force --kill --yes` with a test
   seam that, after the kill but before the retry, lets a separate
   goroutine acquire the now-free flock. Assert: exit 3.
7. **Malformed pidport** — write garbage. Run `--force`. Assert:
   diagnostic mentions "pidport unreadable"; exit 2.
8. **Real subprocess E2E** (Codex r4 #6) — `go build ./cmd/mcphub`
   via the existing `daemon_reliability_test.go::ensureMcphubBinary`
   pattern. Pre-acquire flock from one subprocess; run a second
   subprocess with `--force --kill --yes`. Assert: first subprocess
   killed, second subprocess starts (we send it SIGTERM after
   ready); exit 0 from the recovery path.

## CLAUDE.md runbook

Add to "## GUI frontend":

```
### When `mcphub gui` won't start

If `mcphub gui` exits with the structured "Cannot acquire mcphub
gui single-instance lock" block, run `mcphub gui --force` for the
diagnostic — it also opens the lock folder in your file manager
so the offending files are visible.

To recover automatically:
    mcphub gui --force --kill   (prompts before killing)
    mcphub gui --force --kill --yes   (no prompt; for scripts)

`--kill` only kills the recorded PID after a three-part identity
gate: (1) image basename is `mcphub.exe` (Windows) or `mcphub`
(POSIX); (2) `argv[1]` (cobra subcommand) equals `gui` exactly —
NOT a substring match (Codex r6 #5); (3) process start time
precedes pidport mtime. If any gate fails (e.g., PID recycled
to a `mcphub.exe daemon` Task Scheduler child), `--kill` refuses
with exit 7. In that case identify the actual flock holder
via OS tools and kill it manually:

  Windows: download Sysinternals `handle.exe`, then
           `handle.exe -a "<lock-path>"` (REQUIRES ELEVATED
           shell to see other-user handles).
  Linux:   `lsof "<lock-path>"` or `fuser "<lock-path>"`
           (use `sudo` for cross-user holders).

Then `kill -9 <pid>` (Linux) or Task Manager → End Task on
that PID (Windows). DO NOT delete the lock file; the kernel
handles flock release as a side effect of process exit, and
deleting the file under a live holder splits ownership.

If admin tooling isn't available, **reboot is the universally
available recovery** — stuck file handles survive user-mode
cleanup only across a session reset.
```

## Decisions confirmed

1. **`--force` stays hidden in `--help`** (Codex r1 SUGGEST 1).
   `--kill` documented in `--force`'s long help only.
2. **No file deletion ever** (Codex r4 #7). Destructive automation
   = kill the holder; OS releases flock as a side effect.
3. **Image-path check** before kill is the single mitigation for
   pidport-stale-vs-real-holder mismatch. Operator confirms.
4. **Stdin readline + IsTerminal guard** for `--kill` confirmation;
   bare `--force` doesn't prompt (no destructive action to confirm).
5. **Folder-open default** for bare `--force` — UX feedback that
   bare diagnostic was insufficient.

## Out of scope

- macOS `processID` and `killProcess` — Windows-first; Linux gets a
  full implementation. macOS lacks `/proc/<pid>/{exe,cmdline,stat}`,
  so the Linux-style identity probe is unimplemented; `probe_darwin.go`
  returns an explicit "not supported on macOS" sentinel that
  `probeOnce` surfaces as a `Malformed`-class verdict with a clear
  diagnostic. `mcphub gui --force` (Probe-only) still produces a
  diagnostic block on macOS, but `--force --kill` is unsupported in
  this PR — reboot is the recovery path. A future libproc/sysctl-
  based macOS probe is tracked in the Phase 3B-II backlog. (Codex
  PR #23 P2 #3 iter-2 corrected the previous "best-effort same as
  Linux via posix kill" framing — sharing the Linux probe via
  `//go:build !windows` made every macOS kill refusal look like
  "image is not an mcphub binary" because /proc reads silently
  returned empty fields.)
- A4-b's Settings UI for stuck-instance recovery (PR #24).
- Automatic recovery for "PID dead but flock still held by stuck
  kernel handle" — only `reboot` clears that, not in scope.

## Review trail

- rev 1-4: see prior memo history (`/.reports/2026-04/`).
- rev 5: pure diagnostic-only path Y. User feedback: insufficient UX,
  needs quick access to files + instructions.
- rev 6: Path Y+ — diagnostic AND folder-open by default; opt-in
  `--kill` for automation. No file delete (Codex r4 #7 invariant
  preserved). Image-path was the sole safety gate.
- rev 7 (this): Codex r5 — BLOCKER on rev 6 (image-path alone
  insufficient: TS daemon children share the same image; "manually
  delete" instructions contradicted no-remove invariant; missing
  healthy-incumbent early-exit on `--force --kill`; fixed 200ms
  sleep was a guess). Folded:
  - **Three-part identity gate** (image basename + command-line
    `gui` substring + start-time ≤ pidport mtime + 1s tolerance).
    Rejects TS-launched daemon children, install/uninstall
    invocations, and PID-recycled unrelated processes.
  - **Healthy-incumbent early-exit on `--force --kill`** (ping
    matches recorded PID → handshake + exit 0; never kill a healthy
    gui).
  - **Acquire-poll loop** with 50 ms back-off + 2-second deadline
    instead of fixed 200 ms sleep — TryLock is the source of truth
    for "lock is mine".
  - **No "manually delete <lock-path>" instructions** anywhere.
    Replaced with "identify holder via OS tools (handle.exe / lsof)
    and kill the holder manually" — preserves the no-remove
    invariant for the operator too.
  - **Verdict.Diagnose / Hint marked derived** (`json:"-"`); JSON
    API formats from structured fields. + extended Verdict with
    PIDCmdline, PIDStart for the three-part gate decisions.
  - Exit 5 documented as RESERVED in the table.
  - Test 3 simplified to a seam-only test; the real proof is in
    test 8 (real `cmd/mcphub` subprocesses).
- rev 8 (this): Codex r6 — BLOCKER on substring `gui` cmdline
  match; REVISE on `--yes`/non-TTY wording and POSIX basename;
  SUGGEST on healthy-kill notice and admin/reboot recovery
  wording. Folded:
  - **Argv-token equality**: `argv[1]` (cobra subcommand) must
    equal `"gui"` exactly. No substring matching. Parse via
    CommandLineToArgvW on Windows; NUL-delimited array on
    Linux. Rejects paths and arg values that happen to contain
    `gui`.
  - **POSIX basename = `mcphub`** (no `.exe`).
  - **`--yes` always skips prompt** including non-TTY. The
    non-TTY guard fires only when `--yes` is also absent.
  - **Healthy-kill notice**: "incumbent is healthy (PID `<pid>`);
    activating instead of killing" before TryActivateIncumbent.
  - **Manual recovery instructions** document admin shell
    requirement for handle.exe / lsof and explicitly suggest
    reboot when admin tooling is unavailable.
- rev 9 (this): Codex r7 — REVISE on a single edge case; no
  BLOCKER. Folded:
  - **Argv gate widened**: accept `argv[1] == "gui"` OR
    `len(argv) == 1` (no-args Explorer double-click path
    where `cmd/mcphub/main.go:32` appends `"gui"` internally,
    so the EXTERNAL cmdline visible to our verifier is just
    the executable path). Without this widening, a real GUI
    incumbent launched from Explorer would be refused
    automatic recovery via `--force --kill` (exit 7
    fail-closed), forcing the operator to manual recovery
    unnecessarily.
  - All other r7 concerns (CommandLineToArgvW correctness,
    POSIX exact `mcphub`, healthy-kill notice clarity, no
    prevention-advice creep) returned PASS or SUGGEST.
