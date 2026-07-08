---
status: accepted (revised 2026-07-08 after a fable+codex+sonnet review panel + 3 independent live probes)
date: 2026-07-08
context: A2 H5 spike — is pipe-peer-liveness a viable safety gate for the auto-reaper?
supersedes: the 2026-07-08 v1 of this file, which over-claimed "pipe-peer is a dead end / config-presence is the reliable gate"
---

# Decision: the reaper's safety gate is a multi-condition predicate (config-file-absence + parent-dead + age + identity-reverify); PEB-isolated stdio pipe-state is an OPTIONAL fail-closed supplement; adopt is the primary fix

## H5 question
Can the auto-reaper (A2 PR5, kill orphaned bypass npx-stdio MCP servers whose parent
died) reliably determine, from OUTSIDE the process, that a LIVE client still holds the
other end of the server's stdio pipe — so it never friendly-fires an in-use server?

## Method (three INDEPENDENT live Go probes, this host, non-elevated, same-user)
1. Unfiltered `SystemHandleInformation` scan of the @mui orphan (PID 252772): 36 pipe
   handles; `GetNamedPipe*ProcessId` resolves peer PIDs on ~11/36, mostly DEAD npx
   ancestors + one live UNRELATED app (Antigravity, an inherited non-stdio handle);
   `NtQueryObject(ObjectName)` → STATUS_OBJECT_PATH_INVALID (anonymous, no name).
2. **PEB-isolated stdio probe** (`OpenProcess → NtQueryInformationProcess(PBI) → PEB →
   RTL_USER_PROCESS_PARAMETERS.StandardInput/Output/Error → DuplicateHandle → GetFileType
   → NtQueryInformationFile(FilePipeLocalInformation) NamedPipeState + GetNamedPipe*ProcessId`):
   isolates exactly the real 3 stdio handles.
   - Orphan 252772: all 3 stdio pipes **CLOSING**, readAvail 0, creator PID 179416 =
     **ZOMBIE** (exit 0, absent from the live process table) — correct verdict REAP.
     Reproduced independently by two panel lenses + the orchestrator.
   - Live hub-supervised `mui-mcp` node: stdin **CONNECTED**, readAvail 55, creator =
     mcphub.exe **STILL_ACTIVE** (the creator attribute sees through the npx/cmd
     intermediaries to the true owner) — correct verdict SPARE.

## Findings (triangulated across fable / codex / sonnet + orchestrator re-runs)
- The ORIGINAL design mechanism (unfiltered handle scan + same-Object-address peer
  correlation) is UNRELIABLE and, worse, FAIL-DANGEROUS: a CreatePipe pair is two
  distinct FILE_OBJECTs (read end + write end), so same-object matching is structurally
  blind to the client's peer end; in the dead-intermediary topology it finds no live
  holder and would KILL an in-use server. Abandoning THIS mechanism was correct.
- **PEB-isolated stdio `NamedPipeState` (CONNECTED vs CLOSING) IS a real per-instance
  signal** — it correctly separated the true orphan (CLOSING) from the live daemon
  (CONNECTED) in every run. The v1 claim "fd 0/1/2 are not externally distinguishable"
  is REFUTED (PEB read works, non-elevated, amd64).
- **BUT the PEB signal is CONSTRAINED, not a clean win:**
  1. **The query HANGS on a healthy/CONNECTED pipe.** `GetNamedPipeInfo` /
     `NtQueryInformationFile` block indefinitely (reproduced >=12-15s on TWO independent
     live daemons, and on the orchestrator's live-node run) — exactly the "good" case
     the gate must clear fast. Windows offers no safe cancellation (TerminateThread is
     unsafe + leaks). Every query on a healthy-fleet daemon must be wrapped in a
     goroutine+timeout, and each timeout LEAKS a goroutine + a duplicated handle for the
     process's life. Acceptable one-shot for a MANUAL/operator-invoked reaper; a
     periodic maintenance-timer reaper would wedge/leak against the operator's own
     healthy fleet every tick — so unattended-timer use is a SEPARATELY-gated decision,
     not blessed here.
  2. **Offsets are undocumented internals** (RTL_USER_PROCESS_PARAMETERS
     StandardInput/Output/Error are not in the documented shape; the repo already treats
     PEB offsets as amd64-only — internal/gui/probe_windows.go). WOW64 (32-bit caller /
     64-bit target) needs the NtWow64* path; cross-session/service-account targets can
     fail PROCESS_VM_READ with access-denied. Any failure / non-amd64 → UNVERIFIED →
     observe-only.
  3. **Fail-closed direction:** CLOSING-returned-fast → reap-eligible; CONNECTED **or
     query-timeout or any PEB/dup/FSCTL failure** → SPARE.
- **A live-liveness correctness guard applies everywhere in this code (unanimous):** a
  process is alive iff `GetExitCodeProcess == STILL_ACTIVE`, NOT iff `OpenProcess`/
  `GetProcessTimes` succeed — a ZOMBIE satisfies the latter (PID 179416 did). This binds
  the parent-dead check too, not just pipe-peer.
- **Config-presence is NECESSARY but NOT SUFFICIENT** (corrects v1, which made it the
  sole gate). It is per-IDENTITY ("is this server still wanted?"); the leak is
  per-INSTANCE surplus of a still-wanted identity (65 @mui of one active entry) → a
  config-presence-only gate is INERT against the motivating bug, and has a FAIL-DANGEROUS
  T1 hole: post-adopt the hub signature (`npx -y @mui/mcp`) matches instances spawned by
  clients mcphub does NOT scan (Antigravity-native, Cursor, Windsurf) → "unreferenced" →
  parent-dead → KILLS a foreign client's in-use instance. It must key on config-FILE
  content (not "is the client process running now" — a closed-but-will-reopen client is
  handled by the parent-dead condition, a category-separate question), and an
  UNPARSEABLE/unknown/unreadable config must fail-closed to "do not reap".

## Decision
1. **Primary fix = ADOPT** (absorb the bypass server → supervised, single instance, never
   orphans). Reliable; now surfaced in the GUI (PR #516). Unanimous.
2. **A reaper is still worth building** as the fallback net (not-yet-adopted entries,
   third-party/remote clients mcphub can't scan/adopt, job_protection:false residue). But
   `CleanupOrphans` is currently CLI/GUI-invoked, NOT on an auto-timer — ship the
   corrected predicate for the **manual/operator-confirmed** path first; unattended-timer
   is a separate decision after real false-pos/neg rates are observed.
3. **Corrected safe predicate — kill iff ALL; any step-4/5 failure → observe-only:**
   1. exact T1/T2 command-signature match (T3 report-only; package-name-level, per the
      shipped `patternsFromClientStdio`/`patternIsTooBroad`);
   2. parent verified dead via `GetExitCodeProcess == STILL_ACTIVE`-false (NOT bare
      OpenProcess) OR PID-recycle-guarded by CreationDate;
   3. age > grace;
   4. candidate's signature absent from EVERY parseable on-disk client config file
      (keyed on file content; unparseable/unknown/unreadable → fail-closed = do NOT reap);
   5. OPTIONAL supplement: PEB-isolated stdio `NamedPipeState` — CONNECTED or
      query-timeout or PEB/dup/FSCTL failure → SPARE; both-ends CLOSING + creator
      not-STILL_ACTIVE (or start-time-recycled) → orphan-confirmed (amd64 only; else
      observe-only);
   6. exclusions (supervisor-state PIDs, Job.HasMember, mcphub basename) as designed;
   7. kill EXCLUSIVELY via the shipped `TerminatePIDWithIdentity` (proof captured at
      scan, re-verified on a held handle at kill — closes the PID-recycle race).
   Verdicts audited (orphan-reaped / -skipped-live / -unverified).
4. **The pure "pipe-peer via SystemHandleInformation same-object correlation" gate is
   REJECTED** (fail-dangerous). The PEB-stdio-state variant is an optional fail-closed
   supplement only, never the sole authority.

## Panel record
fable: REVISE (PEB-stdio-state gate viable + validated). codex: REVISE (PEB viable but a
fail-closed SUPPLEMENT — offsets undocumented/amd64-only; config-presence alone unsafe).
sonnet: AGREE-with-v1-direction (PEB narrows-but-the-isolated-stdio-IS-the-dead-ancestor-
handle; found the healthy-pipe query HANG disqualifier; corrected config-presence to
config-file-keyed + fail-closed-on-unparseable). Orchestrator reproduced both the
orphan-CLOSING and the live-pipe-hang independently. The synthesis above is stronger than
any single lens — the panel earned its keep.

## Residual ASSUMPTION (UNVERIFIED)
WOW64 (PEB32) targets and non-anonymous named-pipe client transports were not exercised;
the observe-only ladder in the predicate covers both, but the real-world hang-timeout /
handle-leak rate under a timer is unmeasured — the reason unattended-timer use stays a
separate gated decision.
