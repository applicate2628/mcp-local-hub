//go:build windows

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/scheduler"
)

func bootstrapProductToTarget(w io.Writer, curExe, target string) error {
	windowlessSource := scheduler.WindowsOwnedEntrypointPath(curExe)
	windowlessTarget := scheduler.WindowsOwnedEntrypointPath(target)
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return fmt.Errorf("resolve state-dir for Windows setup product pair: %w", err)
	}
	if reconciled, err := reconcileWindowsProductPairReceipt(stateDir, curExe, windowlessSource, target, windowlessTarget, WindowsProductPairModeSetup); err != nil {
		return err
	} else if reconciled {
		fmt.Fprintf(w, "\u2713 mcphub Windows product pair already committed at %s; committed event reconciled\n", filepath.Dir(target))
		return nil
	}
	stagedCLI, err := stageWindowsProductCandidate(curExe, target)
	if err != nil {
		return fmt.Errorf("stage setup canonical CLI: %w", err)
	}
	defer os.Remove(stagedCLI)
	stagedWindowless, err := stageWindowsProductCandidate(windowlessSource, windowlessTarget)
	if err != nil {
		return fmt.Errorf("stage setup windowless adapter: %w", err)
	}
	defer os.Remove(stagedWindowless)
	txn, err := newWindowsProductPairTxnFn(windowsProductPairTxnRequest{
		Context: context.Background(), StateDir: stateDir, CLIPath: target, WindowlessPath: windowlessTarget,
		StagedCLI: stagedCLI, StagedWindowless: stagedWindowless, Mode: WindowsProductPairModeSetup,
	})
	if err != nil {
		return fmt.Errorf("construct Windows setup product pair transaction: %w", err)
	}
	result, err := txn.Run(context.Background())
	if err != nil {
		return fmt.Errorf("install Windows setup product pair: %w", err)
	}
	if result.Outcome != WindowsProductPairCommittedOutcome {
		return fmt.Errorf("install Windows setup product pair: unexpected terminal outcome %q", result.Outcome)
	}
	fmt.Fprintf(w, "\u2713 mcphub Windows product pair installed at %s\n", filepath.Dir(target))
	return nil
}

// Windows SendMessageTimeout constants. Kept local so we don't pull in the
// whole golang.org/x/sys/windows package just for two numbers.
const (
	hwndBroadcast    uintptr = 0xFFFF
	wmSettingChange  uintptr = 0x001A
	smtoAbortIfHung  uintptr = 0x0002
	broadcastTimeout uintptr = 1000 // ms
)

// ensureOnPath makes sure dir is on HKCU\Environment\Path and broadcasts
// WM_SETTINGCHANGE so already-running Explorer and shells pick it up.
//
// We treat an unexpanded spelling like %USERPROFILE%\.local\bin as equivalent
// to the expanded form so we don't duplicate the entry when a user already
// registered it that way. The existing value's type (REG_SZ vs REG_EXPAND_SZ)
// is preserved on write; we do not silently change users from one to the other.
func ensureOnPath(w io.Writer, dir string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open HKCU\\Environment: %w", err)
	}
	defer k.Close()

	existing, valtype, err := k.GetStringValue("Path")
	if err != nil {
		if !errors.Is(err, registry.ErrNotExist) {
			return fmt.Errorf("read HKCU\\Environment\\Path: %w", err)
		}
		// No user PATH yet — default to REG_EXPAND_SZ so future %VAR% entries
		// expand correctly; Windows itself uses EXPAND_SZ for fresh values.
		valtype = registry.EXPAND_SZ
	}

	if pathContainsDir(existing, dir) {
		fmt.Fprintf(w, "\u2713 %s already on user PATH\n", dir)
		return nil
	}

	newValue := existing
	if newValue != "" && !strings.HasSuffix(newValue, ";") {
		newValue += ";"
	}
	newValue += dir

	// Preserve the original value type. SetExpandStringValue writes REG_EXPAND_SZ,
	// SetStringValue writes REG_SZ. Silently flipping the type would be a behavior
	// change for tools that introspect the registry type.
	if valtype == registry.EXPAND_SZ {
		if err := k.SetExpandStringValue("Path", newValue); err != nil {
			return fmt.Errorf("write HKCU\\Environment\\Path: %w", err)
		}
	} else {
		if err := k.SetStringValue("Path", newValue); err != nil {
			return fmt.Errorf("write HKCU\\Environment\\Path: %w", err)
		}
	}

	if err := broadcastEnvChange(); err != nil {
		// The value is written; notifying running processes is best-effort.
		fmt.Fprintf(w, "\u26a0 PATH updated but WM_SETTINGCHANGE broadcast failed: %v\n", err)
		fmt.Fprintf(w, "\u2713 %s added to user PATH \u2014 restart your shell to pick it up\n", dir)
		return nil
	}
	fmt.Fprintf(w, "\u2713 %s added to user PATH \u2014 restart your shell to pick it up\n", dir)
	return nil
}

// pathContainsDir reports whether pathValue (a raw HKCU PATH string with
// ';'-separated entries) already contains dir. Comparison is case-insensitive,
// normalizes forward/backslashes, trims trailing separators, and treats
// %USERPROFILE%\.local\bin and the expanded spelling as equivalent.
func pathContainsDir(pathValue, dir string) bool {
	if pathValue == "" {
		return false
	}
	target := normalizeWinPath(dir)
	for entry := range strings.SplitSeq(pathValue, ";") {
		if strings.EqualFold(normalizeWinPath(entry), target) {
			return true
		}
	}
	return false
}

// normalizeWinPath expands %VAR% references, converts forward slashes to
// backslashes, and trims whitespace plus trailing separators so user-entered
// spellings like "%USERPROFILE%/.local/bin" and "%USERPROFILE%\.local\bin\"
// compare equal.
func normalizeWinPath(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if expanded, err := expandEnvStrings(s); err == nil {
		s = expanded
	}
	s = strings.ReplaceAll(s, "/", "\\")
	s = strings.TrimRight(s, "\\")
	return s
}

// expandEnvStrings resolves %VAR% references the way Windows itself does
// when it reads a REG_EXPAND_SZ value.
func expandEnvStrings(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return s, err
	}
	// First call with size=0 returns the required buffer length including
	// the trailing null terminator.
	size, err := windows.ExpandEnvironmentStrings(p, nil, 0)
	if err != nil {
		return s, err
	}
	buf := make([]uint16, size)
	if _, err := windows.ExpandEnvironmentStrings(p, &buf[0], size); err != nil {
		return s, err
	}
	return syscall.UTF16ToString(buf), nil
}

// broadcastEnvChange sends WM_SETTINGCHANGE with lparam "Environment" to all
// top-level windows so Explorer, already-running shells, etc. re-read the
// environment. SMTO_ABORTIFHUNG with a 1s timeout keeps us from hanging on
// an unresponsive top-level window.
func broadcastEnvChange() error {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("SendMessageTimeoutW")

	lparam, err := syscall.UTF16PtrFromString("Environment")
	if err != nil {
		return err
	}
	var result uintptr
	ret, _, callErr := proc.Call(
		hwndBroadcast,
		wmSettingChange,
		0, // wParam
		uintptr(unsafe.Pointer(lparam)),
		smtoAbortIfHung,
		broadcastTimeout,
		uintptr(unsafe.Pointer(&result)),
	)
	if ret == 0 {
		// SendMessageTimeout returns 0 on failure or timeout; callErr is set
		// even on success for LazyProc.Call, so only trust it here.
		if callErr != nil && !isZeroSyscallError(callErr) {
			return callErr
		}
		return fmt.Errorf("SendMessageTimeout returned 0")
	}
	return nil
}

// isZeroSyscallError returns true for the benign "operation completed
// successfully" errno that LazyProc.Call surfaces on Windows regardless of
// success/failure. Checked by string because Errno(0) wraps differently
// across Go versions.
func isZeroSyscallError(err error) bool {
	if errno, ok := err.(syscall.Errno); ok {
		return errno == 0
	}
	// Fallback: some Go versions wrap it.
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == 0
	}
	return false
}
