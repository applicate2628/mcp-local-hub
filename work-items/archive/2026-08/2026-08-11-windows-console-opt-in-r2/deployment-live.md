# Windows live deployment gate

Date: 2026-08-11
Execution role: `$platform-engineer`
Common verification skill: `$windows-gui-manual-testing`
Target commit: `b87dc8ddc30d4aba815790f6a5a8b88fb37884c1`
Terminal gate: **PASS**

## Receiving-side echo

| Input / boundary | Received and applied |
|---|---|
| Active status | `status.md`, SHA-256 `80673F7980A85CF2F9574D787437E8990328B4387F93DE20C3411256DD1ACAC9`; Phase H published and live install/restart/observation remained. |
| Accepted integration | `implementation-integration-final.md`, SHA-256 `D3C40A5EC01F3E4F87A16B5DC9797BB829622E7F2D836D47A3AA7A6F6E93C082`; accepted published source boundary and Windows console invariant. |
| Accepted QA | `qa-final-r2.md`, SHA-256 `846B1E39DB1FCF604E143CD008C0FB08A756555016548466860D173F7D1976EF`; terminal `PASS`. |
| Published source | `HEAD` exactly `b87dc8ddc30d4aba815790f6a5a8b88fb37884c1` (`feat: make Windows console explicitly opt-in`). |
| Live scope | Build the published source; install through the existing `install --upgrade` owner; restore the background GUI with `gui --no-browser`; prove binding, readiness, recovery, and ten minutes without a visible console. |
| Protected scope | No source edit; no status/ledger/lifecycle edit; no stage/commit/push/publication; no Sandbox; no PR action. PR #598/#600 work-items/worktrees were not read, executed, mutated, removed, or rebased. Unrelated dirty files were preserved. |
| Console policy | `--debug-console` was never passed. No interactive or visible console launch was used. |

Repository orientation used the live root workflow documented by `README.md:11-16`, the active recovery state at `status.md:8-26`, and the upgrade owner at `internal/cli/install_upgrade.go:285-486`. The Windows successor launch remains owned by `UpgradeDeps.StartSupervisor` and the no-window detached adapter described at `internal/cli/install_upgrade.go:421-437`.

## Baseline and rollback admission

| Check | Exact result |
|---|---|
| Installed path | `%USERPROFILE%\.local\bin\mcphub.exe` (resolved current-user canonical install). |
| Installed version before | `0.4.28`, commit `dcc41eb8`, build date `2026-08-10T12:37:21Z`, Go `1.26.5`, `windows/amd64`. |
| Installed SHA-256 before | `E13717882E3953613847C60C474F6E5138C59A7C4EFD1F3E9110D1CFEB090895`. |
| Baseline owner / critical PIDs | Supervisor `81960`; GUI `13216`; CodeGraph `84088`; HFSS `100240`; CST `3948`. |
| Baseline health | `mcphub status` exit `0`; GUI `/api/status` HTTP `200`; HFSS, CST, CodeGraph and the supervised fleet were present and running. |
| Baseline visible-window oracle | Win32 `EnumWindows` + `IsWindowVisible`: 47 installed `mcphub` processes, 214 correlated descendants, zero correlated visible top-level windows, zero visible `ConsoleWindowClass` windows. |
| Manual-test environment | Windows light application theme, dark system theme, system DPI `96` (100%). The background `--no-browser` GUI owned no visible HWND, so there was no GUI surface to capture or crop. |
| Debug-console guard | Zero live `mcphub` command lines contained `--debug-console`. |
| Rollback admission | A temporary exact copy of the installed binary was hash-verified and admitted as PE subsystem `2` before mutation. The upgrade owner later retained the same prior binary at `%USERPROFILE%\.local\bin\mcphub.exe.old-20260811T150939Z`, SHA-256 `E13717882E3953613847C60C474F6E5138C59A7C4EFD1F3E9110D1CFEB090895`, length `32107520`. |

The admitted rollback route is the same owner path, not a manual overwrite: stop the running GUI only after its PID/image/argv identity is proved, then execute the retained prior binary with `install --upgrade`. That route performs rename-aside, supervisor quiesce/exit, replacement, owner restart, readiness wait, and automatic rollback on failed successor readiness.

## Build and install binding

| Command / probe | Result |
|---|---|
| `pwsh ./build.ps1` | Exit `0`; built `bin/mcphub.exe` as version `0.4.28`, commit `b87dc8dd`, and built `bin/mcphub-pe-admit.exe`. Build output reported `PE subsystem 2 (WINDOWS_GUI)`. |
| `bin/mcphub-pe-admit.exe bin/mcphub.exe` | Exit `0`; candidate admitted as subsystem `2`. |
| `bin/mcphub-pe-admit.exe <retained-prior>` | Exit `0`; rollback binary admitted as subsystem `2`. |
| Candidate SHA-256 | `193969E1B34ED816313BB4C3EE516288E4BB0FCDA8F0CFE898B3535330CDC2E6`, length `32141312`. |
| First `bin/mcphub.exe install --upgrade` | Refused before mutation because GUI PID `13216` held the canonical target. The guard explicitly required tray Quit or identity-bounded `Stop-Process`, preventing the unsafe StopAll-before-copy sequence. |
| Guard-directed GUI stop | PID `13216` was re-identified by canonical image and `gui --no-browser` argv, then stopped. GUI and tray tree reached zero within ten seconds; supervisor `81960`, HFSS `100240`, and CST `3948` remained alive before handoff. API alone was temporarily unavailable, as expected after GUI stop. |
| Canonical upgrade retry | Candidate `install --upgrade`, launched with no console and waited through `System.Diagnostics.Process`: exit `0`, stdout `v0.5 upgrade complete.` |
| GUI restore | Canonical `%USERPROFILE%\.local\bin\mcphub.exe gui --no-browser`; exit `0` from the launching surface. Replacement GUI PID `119604`; `/api/status` recovered to HTTP `200`. |
| Installed binding after | Installed SHA-256 exactly equals candidate SHA-256 `193969E1B34ED816313BB4C3EE516288E4BB0FCDA8F0CFE898B3535330CDC2E6`; `version` reports `0.4.28`, commit `b87dc8dd`, build date `2026-08-11T15:04:54Z`. |
| Installed PE admission | New adapter against installed canonical binary: exit `0`, subsystem `2 (WINDOWS_GUI)`. |
| API build binding | `/api/health`: hub version `0.4.28`, commit `b87dc8dd`, start time `2026-08-11T18:11:42+03:00`, daemon error count `0`. |

The build regenerated its normal resource/build outputs but introduced no tracked source delta. The pre-existing dirty inventory remained unchanged until this artifact was added.

## Immediate live readiness

| Surface | Result |
|---|---|
| Supervisor owner | Replacement PID `118144`; scheduled owner `mcp-local-hub-supervisor` state `Ready`. |
| Configured daemon recovery | 36 API rows `Running`; critical recovered PIDs: CodeGraph `95552`, HFSS `99888`, CST `116092`; route-front and all pre-upgrade running rows recovered. The maintenance row remained non-running by design. |
| HFSS / CST MCP health | `mcphub status --health --json`: both `ok:true`, six tools each. |
| GUI/API | `/api/ping`, `/api/status`, and `/api/health` all HTTP `200`; final `/api/status` body length `12723`; API daemon error list empty. |
| CodeGraph | Wrapper PID `95552` owned listener `127.0.0.1:9303`; `codegraph status` reported the 2,041-file index up to date. A real connected `mcp__codegraph__codegraph_explore` call passed both before and after the observation (0.9 s and 0.8 s). |
| Runtime origin | 47 final `mcphub.exe` processes, all from the canonical installed image; zero build, scratch, test, or other executable paths; zero `--debug-console` arguments. |

`mcphub status --health` returned `initialize: HTTP 502` for the generic CodeGraph health probe (and independently for LLDB and Wolfram), while the actual connected CodeGraph MCP call succeeded. CodeGraph logs show the same 15-minute no-MCP-traffic self-shutdown/restart pattern for many hours before this deployment. Therefore this is recorded as a pre-existing generic-probe false negative, not a failed live CodeGraph path or candidate regression. The authoritative connected path, listener, process health, index, and ten-minute PID continuity were all green.

## Ten-minute no-console/live-health observation

One bounded PowerShell monitor ran once, without duplication, from `2026-08-11T15:17:15.4516306Z` to `2026-08-11T15:28:02.3574344Z`.

Monitor contract:

- 21 samples, 30-second nominal interval; measured span `646.906` seconds.
- Each sample enumerated the installed `mcphub.exe` roots and their recursive children from CIM, captured visible top-level HWND ownership through `EnumWindows`, and classified `ConsoleWindowClass`.
- Each sample required stable critical and full running PID signatures, canonical executable paths only, no `--debug-console`, CodeGraph PID ownership of port `9303`, GUI `/api/status` HTTP `200`, API health error count `0`, and `running`/zero-restart health for CodeGraph, HFSS, and CST.
- Any deviation caused immediate `exit 9`; the terminal result was exit `0`.

| Observation | All 21 samples |
|---|---|
| API / daemon state | `api=True`; 36 rows running; API health errors `0`. |
| Installed roots | 47 PIDs; exact root PID set stable; no exits or respawns. |
| Critical PIDs | Supervisor `118144`, GUI `119604`, CodeGraph `95552`, HFSS `99888`, CST `116092`; stable throughout. |
| CodeGraph listener | One listener on port `9303`, owned by PID `95552`, throughout. |
| Visible hub/child windows | `0` in every sample. |
| Visible `ConsoleWindowClass` | `0` correlated and `0` system-wide in every sample. |
| Console hosting evidence | 21-22 correlated `conhost.exe` descendants existed for command-wrapped managed children, but none owned a visible `ConsoleWindowClass` or any visible top-level window. External `GetConsoleProcessList` membership cannot be queried read-only without attaching the probe process to the target console; no invasive attach was performed against the live fleet. |
| Recursive child count | 154-156 because short-lived managed child processes appeared and exited; installed/root and supervised running PID signatures did not change. |

Final Win32 snapshot at `2026-08-11T15:29:13.1759927Z`: 47 installed roots, 154 correlated descendants, zero correlated visible windows, zero global/correlated `ConsoleWindowClass`, 21 hidden correlated `conhost.exe` processes. GUI PID `119604` reported process DPI-awareness value `0` through `GetProcessDpiAwareness`; no visible GUI HWND existed because the admitted launch was `--no-browser`.

## Rollback, cleanup, and residual gaps

| Item | Disposition |
|---|---|
| Rollback | Not triggered: canonical upgrade, successor readiness, connected CodeGraph, and full observation passed. The owner-retained prior binary remains available at the machine-neutral path recorded above with its exact hash. |
| Temporary rollback copy | Deleted after the observation; the task-owned `.scratch/console-opt-in-r2-live` directory no longer exists. |
| Canonical retained prior | Preserved; it is owner-created rollback state, not task scratch. |
| Generic CodeGraph health probe | Residual diagnostic gap: generic initialize probe reports HTTP 502 despite successful real connected MCP calls and stable process/listener health. Pre-existing log evidence makes it non-regressive for this release, but the health-probe compatibility should be handled in a separate admitted item. |
| Native console-group membership | Windows exposes no non-invasive external `GetConsoleProcessList` query. This gate used process ancestry, hidden `conhost.exe` classification, top-level window ownership, `ConsoleWindowClass`, PE subsystem admission, and the absence of the debug flag. |
| Screenshots | None: the target is deliberately a background/no-browser process with no visible app window. Win32 enumeration was the direct negative-visibility oracle. |

## Changed-file and external-state inventory

This lane added only `work-items/active/2026-08-11-windows-console-opt-in-r2/deployment-live.md` in the repository. It did not change source, status, ledger, lifecycle, README, PR work-items, or PR worktrees; did not stage, commit, push, publish, use Sandbox, or alter PRs.

Readiness-only diff checks passed: `git diff --check -- <artifact>` exited `0`; the artifact-specific local-path/credential marker scan returned no hits. The installed publication-safety shell entrypoint exited `0` with `publication-safety: clean (tracked, examined 0 files -- nothing staged)`. Its zero-file scope is expected because this lane was forbidden to stage; it does not replace the explicit artifact scan.

Authorized live state changes were limited to the canonical install owner: the canonical binary now matches the published candidate, supervisor/managed daemon PIDs were replaced through `install --upgrade`, and the background GUI was restored through `gui --no-browser`. No scratch or test binary is running.

## Gate

**PASS** — the canonical installed binary is byte-for-byte the candidate built from published commit `b87dc8ddc30d4aba815790f6a5a8b88fb37884c1`, it is PE subsystem 2, supervisor/GUI/API/configured daemons recovered, actual CodeGraph MCP is functional, and a measured 646.906-second live window contained no visible hub/child window, no `ConsoleWindowClass`, no critical/root respawn, and no readiness loss.

## Terms and Abbreviations

- **API**: Application Programming Interface.
- **CIM**: Common Information Model, used here for Windows process enumeration.
- **DPI**: Dots per inch; Windows display scaling basis.
- **GUI**: Graphical User Interface.
- **HWND**: Windows native window handle.
- **MCP**: Model Context Protocol.
- **PE**: Portable Executable, the Windows binary format.
- **PID**: Process identifier.
- **SHA-256**: Cryptographic hash used to bind binaries and artifacts.
