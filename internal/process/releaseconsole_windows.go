//go:build windows

package process

import "syscall"

// FreeConsole is not exposed by golang.org/x/sys/windows (v0.46.0), so it
// is resolved directly — the same LazyDLL idiom cmd/mcphub's
// console_windows.go uses for its AttachConsole counterpart.
var procFreeConsole = syscall.NewLazyDLL("kernel32.dll").NewProc("FreeConsole")

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
