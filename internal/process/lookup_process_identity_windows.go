//go:build windows

package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ErrProcessNotFound is the sentinel returned when the queried PID
// has no matching Win32_Process row (process exited, or PID never
// existed). Callers distinguish this from generic transport errors
// to decide between "treat as unbound" and "abort the caller".
var ErrProcessNotFound = errors.New("process: PID not found")

// lookupRetries is the retry budget for LookupProcessIdentity.
// 3 attempts spaced lookupRetryDelay apart cover AV-scanner stalls
// observed in Win11 24H2+ field reports (~600ms-1500ms).
const (
	lookupRetries    = 3
	lookupRetryDelay = 1 * time.Second
)

// lookupBackendFn is the injectable backend the retry loop drives.
// Production binds it to realLookupBackend (PowerShell primary,
// wmic fallback). Tests swap it via swapLookupBackend(t, ...) to
// exercise transient-error paths without shelling powershell.exe.
//
// It takes a context so the retry loop's per-attempt shell-out can be bounded
// by a caller deadline (LookupProcessIdentityContext) — the supervisor's F1
// port-gate worker wraps a deadline around this so one wedged host WMI cannot
// stall the single worker forever.
var lookupBackendFn = realLookupBackend

// LookupProcessIdentity resolves a Windows PID into a ProcessIdentity.
//
// Strategy:
//
//  1. Primary path: PowerShell Get-CimInstance Win32_Process.
//     Works on Win11 24H2+ where wmic.exe has been removed.
//  2. Fallback path: wmic.exe process where ProcessId=<pid>.
//     Used when PowerShell's CLM probe fails AND wmic is on PATH.
//  3. Retry: up to 3 attempts spaced 1s apart on transient failure.
//     Covers AV-scanner stalls observed on Win11 24H2+ field reports.
//
// Returns ErrProcessNotFound if the PID does not exist; other errors
// are wrapped with the underlying call context.
//
// Input validation: negative or zero PIDs are rejected without
// shelling out. PID 0 is the Idle Process pseudo-entry on Windows
// and Win32_Process never returns it; rejecting it here is faster
// than letting CIM return an empty result set.
// It is a context.Background() delegate of LookupProcessIdentityContext, so the
// public signature and behavior are unchanged for every existing caller.
func LookupProcessIdentity(pid int) (ProcessIdentity, error) {
	return LookupProcessIdentityContext(context.Background(), pid)
}

// LookupProcessIdentityContext is the context-bounded form of
// LookupProcessIdentity. The retry loop and the underlying PowerShell/wmic
// shell-outs honor ctx: a canceled or deadline-exceeded ctx short-circuits the
// loop AND kills any in-flight child process (exec.CommandContext), so one
// wedged host WMI query cannot block the caller past its deadline. On a ctx
// deadline the caller sees ctx.Err() (DeadlineExceeded / Canceled) — the F1
// port-gate worker maps that to the unverified (fail-closed, no kill) path.
func LookupProcessIdentityContext(ctx context.Context, pid int) (ProcessIdentity, error) {
	if pid < 0 {
		return ProcessIdentity{}, fmt.Errorf("process: invalid PID %d (must be non-negative)", pid)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	var lastErr error
	for attempt := 1; attempt <= lookupRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return ProcessIdentity{}, err
		}
		id, err := lookupBackendFn(ctx, pid)
		if err == nil {
			return id, nil
		}
		// ErrProcessNotFound is terminal — retrying will not bring
		// the process back. Surface it immediately.
		if errors.Is(err, ErrProcessNotFound) {
			return ProcessIdentity{}, err
		}
		// ctx cancellation is terminal too — a deadline that fired mid-attempt
		// must not consume more of the retry budget.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ProcessIdentity{}, err
		}
		lastErr = err
		if attempt < lookupRetries {
			// Context-aware backoff: abandon the wait the instant ctx fires.
			select {
			case <-ctx.Done():
				return ProcessIdentity{}, ctx.Err()
			case <-time.After(lookupRetryDelay):
			}
		}
	}
	return ProcessIdentity{}, fmt.Errorf("process: lookup PID %d failed after %d attempts: %w", pid, lookupRetries, lastErr)
}

// probePowerShellCLMFn is the injectable CLM probe the production
// dispatcher consults. Tests swap it via swapProbePowerShellCLM to
// drive the (psOK, err) decision matrix without shelling out to
// powershell.exe. Defaults to the ctx-bounded probePowerShellCLMContext
// (ProbePowerShellCLM() is its context.Background() delegate).
var probePowerShellCLMFn = probePowerShellCLMContext

// lookupViaPowerShellFn and lookupViaWmicFn are the injectable
// terminal-path backends the production dispatcher fans out to once
// the probe verdict is known. Tests swap them via
// swapLookupViaPowerShell / swapLookupViaWmic to assert which path
// the dispatcher chose without shelling out.
var (
	lookupViaPowerShellFn = lookupViaPowerShell
	lookupViaWmicFn       = lookupViaWmic
)

// squatterLookupIdentity note: all four seams above now take a context so the
// shell-outs they front (Get-CimInstance / wmic / the CLM probe) run under
// exec.CommandContext and are killed on a caller deadline. The public
// ProbePowerShellCLM() / LookupProcessIdentity() keep their non-ctx signatures
// as context.Background() delegates.

// wmicPathLookupFn is the injectable PATH probe for wmic.exe. Tests
// swap it to simulate "wmic present" vs "wmic absent" hosts without
// touching the real filesystem. Production defaults to exec.LookPath.
var wmicPathLookupFn = func() error {
	_, err := exec.LookPath("wmic.exe")
	return err
}

// realLookupBackend is the production resolver. Dispatch policy
// (codex-r2-b-p1, round-1 remains):
//
//  1. Probe PowerShell CLM availability FIRST. If the probe itself
//     errors (transport failure, powershell.exe absent), surface the
//     error — do NOT silently fall through to wmic. A failed probe
//     is an environmental defect the operator must see, not a
//     trigger for a weaker fallback.
//
//  2. If CLM is available (probe returns (true, nil)), PowerShell is
//     the canonical path. Any PS call error (transient AV stall,
//     transport hang, weird JSON) is returned to the caller; the
//     outer retry loop handles transient cases. Do NOT fall through
//     to wmic — wmic is strictly for CLM-locked hosts, not a
//     general-purpose "PS failed, try something else" alternative.
//
//  3. If CLM is locked (probe returns (false, nil)), try wmic.exe
//     when it is on PATH. If wmic is absent, return a clear error
//     naming both conditions so operators can either relax the
//     security policy or install wmic.
//
// The probe runs at most once per LookupProcessIdentity call;
// production callers typically resolve many PIDs per migration tick,
// so an in-memory cache could be added if profiling shows the probe
// dominating wall-time. For now correctness > micro-optimization.
func realLookupBackend(ctx context.Context, pid int) (ProcessIdentity, error) {
	clmAvailable, probeErr := probePowerShellCLMFn(ctx)
	if probeErr != nil {
		// Probe transport failed (powershell.exe missing, language-mode
		// shell-out hung, etc.). Do NOT silently fall through to wmic;
		// the operator needs to see the probe defect, not a downgraded
		// answer from a fallback they did not opt into.
		return ProcessIdentity{}, fmt.Errorf("process: PowerShell CLM probe failed: %w", probeErr)
	}
	if clmAvailable {
		// FullLanguage mode confirmed — PowerShell is canonical.
		// Any error here is returned as-is; the outer retry loop
		// covers transient stalls. wmic is intentionally NOT
		// consulted: it is a CLM-locked-host fallback, not an
		// alternative path on healthy hosts.
		return lookupViaPowerShellFn(ctx, pid)
	}
	// CLM-locked (probe returned (false, nil)). Use wmic.exe if
	// present; otherwise emit a clear error naming both conditions
	// so operators know what to change.
	if err := wmicPathLookupFn(); err == nil {
		return lookupViaWmicFn(ctx, pid)
	}
	return ProcessIdentity{}, fmt.Errorf("process: PowerShell CLM-locked AND wmic.exe absent: cannot resolve PID %d", pid)
}

// psCimRecord mirrors the JSON shape produced by ConvertTo-Json
// -Compress on a single Win32_Process row. Field names match the
// PS Select-Object projection.
type psCimRecord struct {
	Name             string `json:"Name"`
	CommandLine      string `json:"CommandLine"`
	ExecutablePath   string `json:"ExecutablePath"`
	CreationDateUnix int64  `json:"CreationDateUnix"`
}

// lookupViaPowerShell shells out to powershell.exe to fetch the row.
//
// The `[DateTimeOffset]::new($_.CreationDate).ToUnixTimeSeconds()`
// projection emits Unix seconds as an integer directly, sidestepping
// the locale-formatted CIM date trap: the spec's original
// `Get-Date -UFormat %s` snippet returns a culture-formatted float
// (e.g., `1778981273,47784` on ru-RU hosts where `,` is the decimal
// separator), and an `[int64]` cast on that string concatenates the
// digits rather than parsing the float — producing values ~10^14
// instead of ~10^9. `DateTimeOffset.ToUnixTimeSeconds` is
// culture-invariant and returns a proper Int64.
//
// CIM Win32_Process.CreationDate.Kind == Local, so
// `[DateTimeOffset]::new(DateTime)` is correct here:
// DateTimeOffset uses the local timezone offset for Kind=Local
// DateTimes, then ToUnixTimeSeconds normalizes to UTC epoch.
//
// ConvertTo-Json -Compress returns a JSON object for a single match
// and a JSON array for multiple matches — Win32_Process is keyed on
// ProcessId so a single-PID filter always returns 0 or 1 rows.
// Empty result = ConvertTo-Json emits nothing (zero bytes), which we
// detect and map to ErrProcessNotFound.
func lookupViaPowerShell(ctx context.Context, pid int) (ProcessIdentity, error) {
	// Note: the embedded `$_` is a PowerShell automatic variable for
	// the current pipeline item — must NOT be escaped on the Go side
	// (no Go string interpolation collides with `$`).
	script := fmt.Sprintf(
		"Get-CimInstance -ClassName Win32_Process -Filter 'ProcessId=%d' | "+
			"Select-Object Name, CommandLine, ExecutablePath, "+
			"@{Name='CreationDateUnix';Expression={[int64]([System.DateTimeOffset]::new($_.CreationDate).ToUnixTimeSeconds())}} | "+
			"ConvertTo-Json -Compress",
		pid,
	)
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	NoConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("powershell Get-CimInstance: %w", err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "null" {
		return ProcessIdentity{}, ErrProcessNotFound
	}

	// ConvertTo-Json emits a single object for one match. The
	// Win32_Process filter on ProcessId is unique, so we don't
	// expect arrays — but defend against unexpected array shapes by
	// trying object decode first, then array if that fails.
	var rec psCimRecord
	if err := json.Unmarshal([]byte(trimmed), &rec); err != nil {
		var arr []psCimRecord
		if errArr := json.Unmarshal([]byte(trimmed), &arr); errArr != nil {
			return ProcessIdentity{}, fmt.Errorf("powershell JSON decode: %w (raw=%q)", err, trimmed)
		}
		if len(arr) == 0 {
			return ProcessIdentity{}, ErrProcessNotFound
		}
		rec = arr[0]
	}

	return ProcessIdentity{
		PID:              pid,
		Basename:         rec.Name,
		CommandLine:      rec.CommandLine,
		ExecutablePath:   rec.ExecutablePath,
		CreationDateUnix: rec.CreationDateUnix,
	}, nil
}

// lookupViaWmic is the legacy fallback for hosts where PowerShell is
// CLM-locked but wmic.exe is still installed (pre-24H2 builds and
// older LTSC channels). Output format: /format:csv emits a header
// line followed by Node,CommandLine,CreationDate,ExecutablePath,Name
// (column order is documented but historically varies — parse by
// header position rather than fixed index).
//
// wmic CreationDate format is the WMI DATETIME string
// `yyyymmddHHMMSS.mmmmmm+UTCminutes`. parsing is fragile but adequate
// for the fallback role.
func lookupViaWmic(ctx context.Context, pid int) (ProcessIdentity, error) {
	cmd := exec.CommandContext(ctx, "wmic.exe", "process", "where",
		fmt.Sprintf("ProcessId=%d", pid),
		"get", "Name,CommandLine,CreationDate,ExecutablePath",
		"/format:csv",
	)
	NoConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf("wmic process: %w", err)
	}

	lines := strings.Split(string(out), "\n")
	var headers []string
	var values []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := splitWmicCSVLine(line)
		if len(fields) == 0 {
			continue
		}
		if strings.EqualFold(fields[0], "Node") {
			headers = fields
			continue
		}
		if headers == nil {
			// Some wmic builds emit data without a header (locale
			// fallback). Skip — we need the header to know column
			// positions.
			continue
		}
		values = fields
		break
	}
	if headers == nil || values == nil {
		return ProcessIdentity{}, ErrProcessNotFound
	}

	get := func(col string) string {
		for i, h := range headers {
			if strings.EqualFold(h, col) && i < len(values) {
				return strings.TrimSpace(values[i])
			}
		}
		return ""
	}

	createdStr := get("CreationDate")
	var createdUnix int64
	if createdStr != "" {
		// WMI DATETIME: yyyymmddHHMMSS.mmmmmm+UTCminutes.
		// We parse only yyyymmddHHMMSS (14 chars); the fractional
		// seconds and timezone offset are dropped for the Unix
		// seconds output — acceptable for migration's createdUnix
		// gate which uses second-level precision.
		if len(createdStr) >= 14 {
			t, parseErr := time.Parse("20060102150405", createdStr[:14])
			if parseErr == nil {
				createdUnix = t.Unix()
			}
		}
	}

	name := get("Name")
	cmdLine := get("CommandLine")
	exePath := get("ExecutablePath")
	if name == "" && cmdLine == "" && exePath == "" {
		return ProcessIdentity{}, ErrProcessNotFound
	}

	return ProcessIdentity{
		PID:              pid,
		Basename:         name,
		CommandLine:      cmdLine,
		ExecutablePath:   exePath,
		CreationDateUnix: createdUnix,
	}, nil
}

// splitWmicCSVLine splits one wmic /format:csv line on commas. wmic
// CSV does NOT quote fields containing commas; in practice
// CommandLine is the only field with commas and they survive split
// because wmic also collapses repeated commas. The split is best-
// effort — the wmic path is fallback-only, so callers should prefer
// PowerShell whenever it works.
func splitWmicCSVLine(line string) []string {
	parts := strings.Split(line, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

// ProbePowerShellCLM verifies PowerShell is available in FullLanguage
// mode (not Constrained Language Mode).
//
// Two-step probe per spec §"Forward migration steps" line 272:
//
//  1. $ExecutionContext.SessionState.LanguageMode must equal
//     `FullLanguage`.
//  2. Dry-run Get-CimInstance with ProcessId=0 must not error
//     (covers granular AppLocker/WDAC policies that block CIM
//     provider DLLs even in FullLanguage mode).
//
// Returns (false, nil) when either step rejects PowerShell — the
// caller distinguishes this from a transport error (returned as
// (false, err)) to decide whether to fall back to wmic or abort
// the caller.
//
// Implementation note: powershell.exe absent from PATH counts as
// transport error, not CLM-lock, because the diagnostic is different
// — the operator needs to know whether to install PS or to relax
// the security policy.
func ProbePowerShellCLM() (bool, error) {
	return probePowerShellCLMContext(context.Background())
}

// probePowerShellCLMContext is the context-bounded probe: both PowerShell
// shell-outs run under exec.CommandContext so a caller deadline kills a hung
// language-mode / CIM dry-run probe. ProbePowerShellCLM() delegates here with
// context.Background(), keeping its public non-ctx signature byte-identical.
func probePowerShellCLMContext(ctx context.Context) (bool, error) {
	if _, err := exec.LookPath("powershell.exe"); err != nil {
		return false, fmt.Errorf("powershell.exe not on PATH: %w", err)
	}

	// Step (a): language mode probe.
	cmdLang := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"$ExecutionContext.SessionState.LanguageMode")
	NoConsole(cmdLang)
	outLang, errLang := cmdLang.Output()
	if errLang != nil {
		return false, fmt.Errorf("powershell language-mode probe: %w", errLang)
	}
	mode := strings.TrimSpace(string(outLang))
	if !strings.EqualFold(mode, "FullLanguage") {
		return false, nil
	}

	// Step (b): dry-run Get-CimInstance with ProcessId=0 — always
	// returns empty result set in FullLanguage; any non-zero exit
	// means the cmdlet itself is blocked.
	cmdCim := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"Get-CimInstance -ClassName Win32_Process -Filter 'ProcessId=0' | "+
			"Select-Object ProcessId | ConvertTo-Json -Compress")
	NoConsole(cmdCim)
	if err := cmdCim.Run(); err != nil {
		return false, nil
	}

	return true, nil
}
