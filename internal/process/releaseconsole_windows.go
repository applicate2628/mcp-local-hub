//go:build windows

package process

import (
	"syscall"
	"unsafe"
)

// FreeConsole is not exposed by golang.org/x/sys/windows (v0.46.0), so it
// is resolved directly — the same LazyDLL idiom cmd/mcphub's
// console_windows.go uses for its AttachConsole counterpart.
var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procFreeConsole           = kernel32.NewProc("FreeConsole")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
)

// HasConsole reports whether this process is currently a CLIENT of a
// console — i.e. whether a closing terminal can deliver CTRL_CLOSE_EVENT
// to it. It is the probe half of the pair whose mutate half is
// ReleaseParentConsole.
//
// GetConsoleProcessList is the correct primitive and the obvious-looking
// alternatives are not:
//
//   - GetConsoleWindow() != 0 is WRONG under a pseudoconsole (ConPTY, what
//     Windows Terminal uses): an attached process has no console WINDOW, so
//     the probe reports "no console" while the process is very much a
//     client and very much killable by the closing terminal.
//   - "did AttachConsole succeed" is WRONG as a proxy because it cannot see
//     a console the process already had, and it conflates "I attached one"
//     with "I am attached".
//
// GetConsoleProcessList returns 0 (with ERROR_INVALID_HANDLE) when the
// caller has no console, and a nonzero client count otherwise. The count
// itself is not interesting here — only zero vs nonzero — so the small
// stack buffer is sufficient even on a console with many clients (a
// too-small buffer still returns the required size, which is nonzero).
func HasConsole() bool {
	var buf [4]uint32
	n, _, _ := procGetConsoleProcessList.Call(
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return n != 0
}

// ReleaseParentConsole detaches this process from the console it is
// attached to, if any.
//
// It is the runtime counterpart to cmd/mcphub's
// attachParentConsoleIfAvailable: mcphub.exe ships as a Windows-SUBSYSTEM
// binary (so an Explorer double-click flashes no black window) and calls
// AttachConsole(ATTACH_PARENT_PROCESS) at startup so CLI output is visible
// when the operator runs it from a terminal.
//
// Attaching makes the process a CLIENT of that console. When the terminal
// closes, the console delivers CTRL_CLOSE_EVENT to every attached client
// and then terminates them — which is why a terminal-launched `mcphub gui`
// used to die (tray icon and all) the moment its terminal was closed. Note
// that a console control handler CANNOT save it: per the Win32 contract a
// handler for CTRL_CLOSE_EVENT gets only a short cleanup window before the
// system terminates the process regardless of the value it returns.
// Releasing the console is therefore the mechanism, not merely one option.
//
// POSTCONDITION (the load-bearing property): on return this process is no
// longer a console client, so no CTRL_CLOSE_EVENT can reach it. That is
// what callers rely on, and what the test asserts via
// GetConsoleProcessList. Note that GetConsoleWindow is NOT a valid probe
// for this: under a pseudoconsole (ConPTY, as used by Windows Terminal) an
// attached process has no console WINDOW and GetConsoleWindow returns NULL
// while the process is still very much a console client.
//
// There is deliberately NO return value. FreeConsole reports success (a
// nonzero BOOL) even when the process had no console to begin with —
// verified empirically on Windows 11, where a call with nothing attached
// still returns 1 — so its result cannot distinguish "released a console"
// from "there was none". A bool return here would be a false affordance.
// Calling with no console attached (Explorer double-click, detached spawn,
// Task Scheduler action) is a safe no-op, and the call is idempotent.
//
// Callers must treat this as one-way and final for stdio: once the console
// is released, os.Stdout / os.Stderr refer to closed console handles and
// subsequent writes are discarded. Call it only after all operator-facing
// startup output has been written, and BEFORE spawning any child that
// would otherwise inherit or attach to the same console.
func ReleaseParentConsole() {
	_, _, _ = procFreeConsole.Call()
}
