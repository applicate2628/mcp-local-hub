//go:build windows

package tray

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// runChildImpl is the Windows implementation of RunChild. It owns
// a hidden message-only window, an attached shell tray icon, and a
// classic Win32 message pump. Stdin (state lines) is drained on a
// separate goroutine that posts thread-safe PostMessageW pings to
// the pump; the pump owns all user32/shell32 calls because they
// require single-threaded ownership of the icon's message queue.
//
// Why this exists vs. fyne.io/systray: fyne's TrackPopupMenu uses
// GetCursorPos coordinates, which makes the menu appear at the
// mouse pointer rather than anchored to the icon. There is no
// public knob to override that. Direct Win32 lets us call
// Shell_NotifyIconGetRect and align the popup against the icon's
// own rectangle (TPM_RIGHTALIGN | TPM_BOTTOMALIGN), which is the
// expected Explorer-style UX. As a side benefit we drop a CGo
// dependency (fyne pulled in DBus on Linux even though the tray is
// Windows-only at runtime).
//
// IPC contract is unchanged: parent → child sends {"state":"…"}
// JSON lines on stdin; child → parent sends {"event":"…"} JSON
// lines on stdout. See tray.go for the canonical envelopes.
func runChildImpl(r io.Reader, w io.Writer) error {
	// All user32/shell32 calls below require ownership of the OS
	// thread that received the icon's WM_CREATE — we lock the
	// goroutine to its OS thread for the lifetime of this function.
	// cobra invokes RunE on the goroutine that ran main(), which is
	// already the process main thread, but LockOSThread is the
	// portable invariant.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Declare per-monitor DPI awareness BEFORE creating any window
	// or registering the shell icon. Without this, Win32 delivers
	// mouse-event wParam in PHYSICAL pixels while TrackPopupMenu
	// expects DPI-VIRTUALIZED pixels (because windowsgui binaries
	// are DPI_UNAWARE by default on Windows 10/11). Result: on a
	// scaled monitor, the popup gets placed at virtual coords
	// outside the virtual screen rect, and Windows clamps it to
	// the screen edge — typically the upper-left, which is the
	// "menu in upper-left while icon is lower-right" symptom we
	// captured via diagnostic logging.
	//
	// Best-effort; if SetProcessDpiAwarenessContext fails (e.g.
	// Windows < 1607, or a manifest already set awareness) we
	// log and continue.
	if !setProcessDpiAwareV2() {
		fmt.Fprintln(os.Stderr, "tray: SetProcessDpiAwarenessContext(PER_MONITOR_AWARE_V2) failed; popup placement may be off on scaled monitors")
	}

	tc, err := newTrayChild(w)
	if err != nil {
		return fmt.Errorf("tray child init: %w", err)
	}
	// Cleanup is wrapped in a function literal so a panic on the
	// pump thread still fires NIM_DELETE and frees handles. That's
	// the difference between a clean exit and a ghost icon left in
	// the system tray that only goes away when the user mouses over
	// it.
	defer func() {
		tc.shutdown()
	}()

	// Stdin reader goroutine. Posts WM_APP+N messages to the pump
	// rather than touching shell32 directly — PostMessageW is the
	// Win32-blessed cross-thread signal.
	go tc.readStdinLoop(r)

	// Blocking message pump. Returns on WM_QUIT (PostQuitMessage)
	// or GetMessage failure (-1, treated as fatal).
	tc.runMessagePump()
	return nil
}

// trayChild owns the per-instance Win32 state for the tray.
type trayChild struct {
	hInstance    uintptr
	hwnd         uintptr
	classNamePtr *uint16 // kept alive for the lifetime of hwnd

	uid uint32 // shell-icon UID; we use 1, only one icon per process

	// taskbarCreatedMsg is the registered window message ID for
	// "TaskbarCreated", broadcast by the shell when explorer.exe
	// restarts. Receiving this message means our previous
	// Shell_NotifyIcon registration is gone and we must re-add
	// the icon. Without this handling, the tray icon disappears
	// permanently after any explorer restart (Task Manager →
	// restart, shell crash + auto-restart, OS update reboot of
	// shell only, etc.) and the user has no way to recover
	// short of restarting the tray subprocess.
	taskbarCreatedMsg uint32

	// currentState tracks the most-recently applied state so we can
	// re-apply it on TaskbarCreated without going back to the parent
	// for a fresh state line. Defaults to StateHealthy.
	currentStateMu sync.Mutex
	currentState   TrayState

	// Currently-installed icon handle. Replaced via NIM_MODIFY when
	// the parent sends a new state. The previous handle is destroyed
	// AFTER the modify call returns so the shell never references a
	// freed icon.
	hIconMu sync.Mutex
	hIcon   uintptr

	// iconRegistered reflects whether NIM_ADD has succeeded and not
	// yet been undone by NIM_DELETE. Both shutdown() and the WM_CLOSE
	// path consult this flag so neither double-deletes (causing shell
	// errors in the next NIM_ADD on a re-spawned child) nor leaves a
	// ghost icon (skipping NIM_DELETE because WM_CLOSE already cleared
	// hwnd). Codex bot review on PR #24 P2 (NIM_DELETE invariant).
	iconRegistered bool

	// versionV4 reflects whether NIM_SETVERSION(NOTIFYICON_VERSION_4)
	// succeeded. Under V4, wParam in the tray callback carries screen
	// X/Y coords; in legacy mode it carries the icon UID instead and
	// must NOT be decoded as coordinates (the menu would anchor near
	// (1,0)). Codex bot review on PR #24 P2 (legacy callback fallback).
	versionV4 bool

	// lastMenuShow is when we last invoked the right-click popup
	// menu. Used to debounce the Win11 dual-fire pattern: a single
	// mouse right-click delivers BOTH WM_RBUTTONUP AND WM_CONTEXTMENU
	// through the V4 callback. Without dedup the popup shows twice.
	// 200ms is comfortably longer than the dual-fire interval
	// (empirically <10ms) but shorter than the slowest deliberate
	// double-click. Codex bot review on PR #24 P2 (keyboard menu
	// access via WM_CONTEXTMENU).
	lastMenuShow time.Time

	// JSON encoder for child→parent events. Mutex-protected because
	// menu clicks fire on the pump thread and (unlikely but cheap to
	// guard) state updates could arrive concurrently if the design
	// later changes.
	encMu sync.Mutex
	enc   *json.Encoder

}

// newTrayChild registers the window class, creates the message-only
// window, builds the initial icon, and registers it with the shell.
// It returns a fully initialized trayChild ready for the message
// pump.
func newTrayChild(w io.Writer) (*trayChild, error) {
	tc := &trayChild{
		uid:          1,
		enc:          json.NewEncoder(w),
		currentState: StateHealthy,
	}

	tc.hInstance = getModuleHandle()

	// Register for the shell's "TaskbarCreated" broadcast so we can
	// re-add our icon if explorer restarts. Best-effort: a failure
	// here just means the icon won't survive shell restart, but the
	// rest of the tray works fine on first install.
	if msg, err := registerWindowMessage("TaskbarCreated"); err == nil {
		tc.taskbarCreatedMsg = msg
	} else {
		fmt.Fprintf(os.Stderr, "tray child: RegisterWindowMessage(TaskbarCreated): %v\n", err)
	}

	// Per-process unique class name. Multiple mcphub instances
	// shouldn't normally coexist, but the parent restarts the child
	// on certain failure paths, and a stale RegisterClassExW would
	// fail with ERROR_CLASS_ALREADY_EXISTS.
	className := fmt.Sprintf("mcphub-tray-%d", getCurrentProcessId())
	clsPtr, err := windows.UTF16PtrFromString(className)
	if err != nil {
		return nil, fmt.Errorf("class name UTF-16 conv: %w", err)
	}
	tc.classNamePtr = clsPtr

	// Trampoline: package-level wndProc dispatches to the singleton
	// trayChild via the activeChild pointer. We can't pass a Go
	// closure as WNDPROC (cgo callback restriction even via syscall);
	// the singleton pointer is set right before CreateWindow and
	// cleared in shutdown.
	cls := WNDCLASSEXW{
		CbSize:        uint32(unsafe.Sizeof(WNDCLASSEXW{})),
		LpfnWndProc:   syscall.NewCallback(wndProcTrampoline),
		HInstance:     tc.hInstance,
		HCursor:       loadDefaultCursor(),
		LpszClassName: tc.classNamePtr,
	}
	if _, err := registerClassExW(&cls); err != nil {
		return nil, fmt.Errorf("RegisterClassExW: %w", err)
	}

	// Pre-CreateWindow: install the singleton so the WM_NCCREATE /
	// initial WndProc calls can find us. RegisterClassExW does not
	// invoke WndProc; CreateWindowEx does (synchronously).
	setActiveChild(tc)

	hwnd, err := createTrayHostWindow(tc.classNamePtr, tc.hInstance)
	if err != nil {
		setActiveChild(nil)
		return nil, fmt.Errorf("CreateWindowExW: %w", err)
	}
	tc.hwnd = hwnd

	// Build initial Healthy icon and register the tray icon.
	if err := tc.installIcon(StateHealthy); err != nil {
		destroyWindow(tc.hwnd)
		setActiveChild(nil)
		return nil, fmt.Errorf("install initial tray icon: %w", err)
	}

	return tc, nil
}

// shutdown removes the icon, destroys the window, and frees the
// last HICON. Idempotent — callable from defer regardless of which
// failure path got us here. The WM_CLOSE handler typically issues
// NIM_DELETE first (and clears iconRegistered), so this function's
// guarded NIM_DELETE only fires on early-failure paths where the
// pump never reached WM_CLOSE.
func (tc *trayChild) shutdown() {
	if tc == nil {
		return
	}
	// NIM_DELETE invariant: drop the icon while we still hold a
	// valid hwnd. Skipping this when iconRegistered=false avoids a
	// double-delete after WM_CLOSE has already done it.
	if tc.iconRegistered && tc.hwnd != 0 {
		nid := NOTIFYICONDATAW{
			CbSize: uint32(unsafe.Sizeof(NOTIFYICONDATAW{})),
			HWnd:   tc.hwnd,
			UID:    tc.uid,
		}
		shellNotifyIcon(NIM_DELETE, &nid)
		tc.iconRegistered = false
	}
	if tc.hwnd != 0 {
		destroyWindow(tc.hwnd)
		tc.hwnd = 0
	}
	tc.hIconMu.Lock()
	if tc.hIcon != 0 {
		destroyIcon(tc.hIcon)
		tc.hIcon = 0
	}
	tc.hIconMu.Unlock()
	setActiveChild(nil)
}

// installIcon registers the initial tray icon (NIM_ADD) using the
// supplied state. Called once during init.
func (tc *trayChild) installIcon(state TrayState) error {
	hicon, err := makeHIconForState(state)
	if err != nil {
		return err
	}
	tc.hIconMu.Lock()
	tc.hIcon = hicon
	tc.hIconMu.Unlock()

	nid := tc.buildNID(state, hicon)
	if !shellNotifyIcon(NIM_ADD, &nid) {
		destroyIcon(hicon)
		tc.hIconMu.Lock()
		tc.hIcon = 0
		tc.hIconMu.Unlock()
		return fmt.Errorf("Shell_NotifyIcon NIM_ADD failed")
	}
	tc.iconRegistered = true
	// Switch to the Win7+ callback ABI so click coords arrive in
	// wParam directly. Without NIM_SETVERSION the shell uses the
	// legacy XP-era encoding (wParam = icon UID, lParam = mouse
	// event), forcing callers to fall back to icon-rect or cursor
	// heuristics for anchor coordinates.
	verNID := NOTIFYICONDATAW{
		CbSize:   uint32(unsafe.Sizeof(NOTIFYICONDATAW{})),
		HWnd:     tc.hwnd,
		UID:      tc.uid,
		UVersion: NOTIFYICON_VERSION_4,
	}
	if shellNotifyIcon(NIM_SETVERSION, &verNID) {
		tc.versionV4 = true
	} else {
		// Non-fatal: handleMessage's wmTrayCallback path detects
		// !versionV4 and avoids decoding wParam as XY (in legacy mode
		// wParam carries the icon UID, which would anchor the popup
		// near (1,0)). Codex bot review on PR #24 P2.
		fmt.Fprintf(os.Stderr, "tray child: NIM_SETVERSION(4) failed; falling back to icon-rect/cursor anchor\n")
	}
	return nil
}

// updateIcon swaps the displayed icon to one matching `state`. The
// previous HICON is destroyed only after the shell acknowledges the
// modify, so a half-failed update never leaks.
func (tc *trayChild) updateIcon(state TrayState) {
	hicon, err := makeHIconForState(state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tray child: makeHIcon(%s): %v\n", state, err)
		return
	}
	nid := tc.buildNID(state, hicon)
	if !shellNotifyIcon(NIM_MODIFY, &nid) {
		fmt.Fprintf(os.Stderr, "tray child: NIM_MODIFY(%s) failed\n", state)
		destroyIcon(hicon)
		return
	}
	tc.hIconMu.Lock()
	old := tc.hIcon
	tc.hIcon = hicon
	tc.hIconMu.Unlock()
	if old != 0 {
		destroyIcon(old)
	}
	tc.currentStateMu.Lock()
	tc.currentState = state
	tc.currentStateMu.Unlock()
}

// reAddIcon re-registers the tray icon after the shell has reset.
// Triggered by the "TaskbarCreated" broadcast. Called from the
// pump thread inside handleMessage. Builds a fresh HICON for the
// last-known state so the icon comes back showing the same color
// the user was looking at before explorer crashed.
func (tc *trayChild) reAddIcon() {
	tc.currentStateMu.Lock()
	state := tc.currentState
	tc.currentStateMu.Unlock()

	// New HICON: the old one was tied to the gone-shell registration.
	hicon, err := makeHIconForState(state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tray child: TaskbarCreated re-add: makeHIcon: %v\n", err)
		return
	}
	nid := tc.buildNID(state, hicon)
	if !shellNotifyIcon(NIM_ADD, &nid) {
		destroyIcon(hicon)
		fmt.Fprintf(os.Stderr, "tray child: TaskbarCreated re-add: NIM_ADD failed\n")
		return
	}
	tc.iconRegistered = true
	// Re-establish the V4 callback ABI on the new registration. The
	// versionV4 flag MUST track the OUTCOME on this fresh registration
	// — keeping the previous true value when SETVERSION fails would
	// make handleMessage decode wParam as V4 coords against a legacy
	// callback ABI on the new shell instance, mis-anchoring the menu
	// (Codex bot review on PR #24 P2). Reset both ways: success →
	// true, failure → false.
	verNID := NOTIFYICONDATAW{
		CbSize:   uint32(unsafe.Sizeof(NOTIFYICONDATAW{})),
		HWnd:     tc.hwnd,
		UID:      tc.uid,
		UVersion: NOTIFYICON_VERSION_4,
	}
	tc.versionV4 = shellNotifyIcon(NIM_SETVERSION, &verNID)
	// Swap HICON ownership and free the old (now-orphan) handle.
	tc.hIconMu.Lock()
	old := tc.hIcon
	tc.hIcon = hicon
	tc.hIconMu.Unlock()
	if old != 0 {
		destroyIcon(old)
	}
}

// buildNID assembles the NOTIFYICONDATAW used by NIM_ADD/MODIFY.
func (tc *trayChild) buildNID(state TrayState, hicon uintptr) NOTIFYICONDATAW {
	nid := NOTIFYICONDATAW{
		CbSize:           uint32(unsafe.Sizeof(NOTIFYICONDATAW{})),
		HWnd:             tc.hwnd,
		UID:              tc.uid,
		// NIF_SHOWTIP forces the standard tooltip on V4 (the Win7+
		// callback ABI we use); without it, NOTIFYICONDATA docs say
		// the standard tooltip is suppressed and SzTip never reaches
		// the user. Codex CLI xhigh review on PR #24 P2.
		UFlags:           NIF_ICON | NIF_MESSAGE | NIF_TIP | NIF_SHOWTIP,
		UCallbackMessage: wmTrayCallback,
		HIcon:            hicon,
	}
	tip := "mcp-local-hub: " + state.String()
	tipUTF16 := utf16FromString(tip, len(nid.SzTip))
	copy(nid.SzTip[:], tipUTF16)
	return nid
}

// makeHIconForState pulls the cached ICO bytes for the given state
// and asks the shell to materialize them as an HICON.
func makeHIconForState(state TrayState) (uintptr, error) {
	bytes := IconBytes(state)
	if len(bytes) == 0 {
		return 0, fmt.Errorf("empty icon bytes for state %s", state)
	}
	return createIconFromResourceEx(bytes, 16, 16)
}

// utf16FromString converts s to a UTF-16 slice truncated to fit a
// fixed Win32 buffer of `cap` uint16 elements (including the
// trailing NUL). Bytes beyond the cap are silently dropped, matching
// the shell's own behavior for over-long tooltips.
func utf16FromString(s string, cap int) []uint16 {
	if cap <= 0 {
		return nil
	}
	out := windows.StringToUTF16(s)
	if len(out) > cap {
		out = out[:cap-1]
		out = append(out, 0)
	}
	return out
}

// readStdinLoop drains parent→child JSON state lines and converts
// each into a wmStateUpdate post to the pump. On EOF or read error
// (parent gone), it posts a single wmShutdown so the pump tears
// down cleanly.
func (tc *trayChild) readStdinLoop(r io.Reader) {
	// Snapshot hwnd at goroutine start. tc.hwnd is set once before
	// this goroutine launches (in newTrayChild) and zeroed by
	// shutdown()/WM_CLOSE on another thread; reading the field
	// concurrently would be a Go memory-model race AND would let
	// PostMessageW(NULL, ...) broadcast wmShutdown to all top-level
	// windows in the process. Sonnet review on PR #24 P2.
	hwnd := tc.hwnd
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		var msg stateMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			fmt.Fprintf(os.Stderr, "tray child: bad state line %q: %v\n", scanner.Bytes(), err)
			continue
		}
		state, ok := parseStateLabel(msg.State)
		if !ok {
			fmt.Fprintf(os.Stderr, "tray child: unknown state %q\n", msg.State)
			continue
		}
		// PostMessageW is thread-safe; the pump pulls the value out
		// of lParam and runs the actual NIM_MODIFY on its own thread.
		if hwnd != 0 {
			postMessageW(hwnd, wmStateUpdate, uintptr(state), 0)
		}
	}
	// Stdin EOF (parent gone) — wake the pump so its WM_CLOSE handler
	// runs NIM_DELETE before the child exits. Skip if hwnd was never
	// initialized (we'd broadcast wmShutdown to all top-levels).
	if hwnd != 0 {
		postMessageW(hwnd, wmShutdown, 0, 0)
	}
}

// runMessagePump is the classic Win32 GetMessage loop. Returns on
// WM_QUIT (rc=0) or GetMessage error (rc<0).
func (tc *trayChild) runMessagePump() {
	var msg MSG
	for {
		rc := getMessageW(&msg, 0)
		if rc == 0 {
			return // WM_QUIT
		}
		if rc < 0 {
			fmt.Fprintf(os.Stderr, "tray child: GetMessage failed; exiting pump\n")
			return
		}
		translateMessage(&msg)
		dispatchMessageW(&msg)
	}
}

// emitEvent writes a JSON event line to the parent. Mutex-protected.
func (tc *trayChild) emitEvent(ev string) {
	tc.encMu.Lock()
	defer tc.encMu.Unlock()
	_ = tc.enc.Encode(eventMessage{Event: ev})
}

// showPopupMenu builds the menu, anchors it to the icon's screen
// rectangle (Shell_NotifyIconGetRect), and dispatches the user's
// choice. Anchoring rule:
//
//   - Menus open from the icon's top-left corner; with
//     TPM_RIGHTALIGN | TPM_BOTTOMALIGN, the menu's *bottom-right*
//     corner is placed at the supplied (x,y), which puts it above
//     and to the left of the icon. That matches what Windows does
//     for native shell menus when the tray sits at the screen's
//     bottom-right (the default).
//
// On Windows where the icon is hidden in the overflow flyout (or
// pre-Win7 hosts where Shell_NotifyIconGetRect doesn't exist), we
// fall back to GetCursorPos so the menu still appears next to the
// click — same behavior as the legacy fyne path.
// showPopupMenuAt anchors the popup at the supplied screen
// coordinates (from the V4 click event's wParam).
func (tc *trayChild) showPopupMenuAt(x, y int32) {

	hmenu, err := createPopupMenu()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tray child: CreatePopupMenu: %v\n", err)
		return
	}
	defer destroyMenu(hmenu)

	if err := appendMenuStringW(hmenu, cmdOpenDashboard, "Open dashboard"); err != nil {
		fmt.Fprintf(os.Stderr, "tray child: AppendMenu(open): %v\n", err)
		return
	}
	if err := appendMenuSeparator(hmenu); err != nil {
		fmt.Fprintf(os.Stderr, "tray child: AppendMenu(sep1): %v\n", err)
		return
	}
	if err := appendMenuStringW(hmenu, cmdRunAllDaemons, "Run all daemons"); err != nil {
		fmt.Fprintf(os.Stderr, "tray child: AppendMenu(run-all): %v\n", err)
		return
	}
	if err := appendMenuStringW(hmenu, cmdStopAllDaemons, "Stop all daemons"); err != nil {
		fmt.Fprintf(os.Stderr, "tray child: AppendMenu(stop-all): %v\n", err)
		return
	}
	if err := appendMenuSeparator(hmenu); err != nil {
		fmt.Fprintf(os.Stderr, "tray child: AppendMenu(sep2): %v\n", err)
		return
	}
	if err := appendMenuStringW(hmenu, cmdQuit, "Quit (keep daemons)"); err != nil {
		fmt.Fprintf(os.Stderr, "tray child: AppendMenu(quit): %v\n", err)
		return
	}
	if err := appendMenuStringW(hmenu, cmdQuitAndStopDaemons, "Quit and stop all daemons"); err != nil {
		fmt.Fprintf(os.Stderr, "tray child: AppendMenu(quit-stop): %v\n", err)
		return
	}

	// Required Win32 dance: the calling thread must be the
	// foreground thread for TrackPopupMenu to display reliably.
	// Without this, switching to e.g. a Chrome window between the
	// click that started the popup and TrackPopupMenu can prevent
	// the menu from appearing at all.
	setForegroundWindow(tc.hwnd)

	// Adaptive alignment based on the work area of the monitor
	// containing (or nearest to) the anchor. Picking the quadrant
	// from primary-monitor SM_C[XY]SCREEN failed on multi-monitor
	// setups (negative-coord secondary monitor on the left, or
	// taskbar on a non-primary monitor). MonitorFromPoint returns
	// the right monitor regardless of layout; rcWork excludes the
	// taskbar so the menu opens into usable screen real estate
	// rather than overlapping the taskbar.
	// Forced CENTERALIGN per user preference: menu is horizontally
	// centered on the anchor X. Adaptive vertical alignment
	// (top-half = grow down, bottom-half = grow up) still applies
	// via monitorWorkArea so menus from a top-of-screen tray don't
	// extend off the top.
	//
	// TPM_NONOTIFY suppresses the WM_COMMAND notification path —
	// without it, the shell sends BOTH the synchronous return value
	// (TPM_RETURNCMD) AND a WM_COMMAND message, making each menu
	// action fire twice. Codex bot review on PR #24 P1.
	flags := uint32(TPM_RIGHTBUTTON | TPM_RETURNCMD | TPM_CENTERALIGN | TPM_NONOTIFY)
	if work, ok := monitorWorkArea(x, y); ok {
		midY := (work.Top + work.Bottom) / 2
		if y > midY {
			flags |= TPM_BOTTOMALIGN // menu grows up (icon at bottom)
		} else {
			flags |= TPM_TOPALIGN // menu grows down (icon at top)
		}
	} else {
		// Fallback if MonitorFromPoint somehow fails: assume bottom
		// taskbar (the historical Windows default).
		flags |= TPM_BOTTOMALIGN
	}
	cmd := trackPopupMenu(hmenu, flags, x, y, tc.hwnd)

	// Documented Win32 quirk: posting a no-op message to the same
	// window after TrackPopupMenu clears the menu loop's lingering
	// state. Without it the next click sometimes opens the menu
	// once and then becomes a no-op until focus changes. Cheap and
	// safe to always send.
	postMessageW(tc.hwnd, WM_NULL, 0, 0)

	switch cmd {
	case cmdOpenDashboard:
		tc.emitEvent("open-dashboard")
	case cmdQuit:
		// Emit BEFORE PostQuitMessage so the parent gets the event
		// before the pipe closes on shutdown.
		tc.emitEvent("quit")
		postQuitMessage(0)
	case cmdQuitAndStopDaemons:
		// Same ordering as cmdQuit: emit first, post-quit second so the
		// parent has time to act on the event before our pipe closes.
		// The parent reacts to "quit-and-stop-all" by calling api.StopAll
		// and only then triggering its own shutdown.
		tc.emitEvent("quit-and-stop-all")
		postQuitMessage(0)
	case cmdRunAllDaemons:
		// Fire-and-forget: parent calls api.RestartAll. Restart of a
		// stopped daemon is functionally a start, so this serves as
		// "Run all" for the user. No PostQuitMessage — GUI stays open.
		tc.emitEvent("run-all")
	case cmdStopAllDaemons:
		// Fire-and-forget: parent calls api.StopAll. GUI stays open.
		tc.emitEvent("stop-all")
	}
}

// --- WndProc plumbing ---

// activeChild is the singleton we route WndProc calls to. The Win32
// callback ABI doesn't carry a user data pointer naturally; we set
// this once before CreateWindow and clear it on shutdown. Concurrent
// access is read-only after init, so a plain pointer is enough — but
// we use atomics anyway to make the lifetime obvious.
var activeChildMu sync.RWMutex
var activeChild *trayChild

func setActiveChild(tc *trayChild) {
	activeChildMu.Lock()
	defer activeChildMu.Unlock()
	activeChild = tc
}

func getActiveChild() *trayChild {
	activeChildMu.RLock()
	defer activeChildMu.RUnlock()
	return activeChild
}

// wndProcTrampoline is the WNDPROC fed to RegisterClassExW. It must
// match the LRESULT (HWND, UINT, WPARAM, LPARAM) signature exactly.
func wndProcTrampoline(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	tc := getActiveChild()
	if tc == nil {
		return defWindowProcW(hwnd, msg, wparam, lparam)
	}
	return tc.handleMessage(hwnd, msg, wparam, lparam)
}

func (tc *trayChild) handleMessage(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	// TaskbarCreated is a registered message ID — its numeric value
	// is computed at runtime, so it can't appear as a switch case.
	// Check it before the switch on well-known constants.
	if tc.taskbarCreatedMsg != 0 && msg == tc.taskbarCreatedMsg {
		tc.reAddIcon()
		return 0
	}
	switch msg {
	case wmTrayCallback:
		// NOTIFYICON_VERSION_4 callback semantics (per Microsoft
		// NOTIFYICONDATAW docs, callback section):
		//   - lParam.LOWORD = notification event
		//   - lParam.HIWORD = icon UID
		//   - wParam carries SCREEN-COORDINATE X/Y *only* for:
		//     mouse messages between WM_MOUSEFIRST..WM_MOUSELAST
		//     (i.e. WM_LBUTTONUP/WM_RBUTTONUP/etc), plus the
		//     synthetic notify events NIN_POPUPOPEN, NIN_SELECT,
		//     NIN_KEYSELECT.
		//   - For OTHER notify events such as WM_CONTEXTMENU and
		//     NIN_BALLOONTIMEOUT, wParam is undefined — decoding
		//     it as X/Y yields garbage screen coordinates (this is
		//     the "menu near the clock" symptom we hit earlier).
		//
		// We therefore listen ONLY for the coord-bearing events
		// and decode wParam.LO/HI as signed int16 (signed for
		// negative coords on multi-monitor setups where the primary
		// is not at top-left). WM_CONTEXTMENU is intentionally NOT
		// in the list — under V4 the shell sends both WM_RBUTTONUP
		// AND WM_CONTEXTMENU for one right-click, but only the
		// former carries usable coordinates. (Codex consult 2026-04-30.)
		// V4 callback decoding — see win32_windows.go NOTIFYICON_VERSION_4
		// constants. wParam X/Y are defined ONLY for mouse messages
		// (WM_RBUTTONUP/WM_LBUTTONUP/etc) and NIN_*SELECT.
		// WM_CONTEXTMENU has UNDEFINED wParam and Win11 emits both
		// WM_RBUTTONUP and WM_CONTEXTMENU for one right-click, so we
		// only handle the coord-bearing message.
		//
		// Caching NIN_POPUPOPEN coords as a "true icon anchor" was
		// tried and rejected: NIN_POPUPOPEN is the tooltip-popup-open
		// event, not a click event, and on Win11 it doesn't fire
		// reliably for pinned icons. A stale cache from an earlier
		// flyout hover would then override the click's own valid
		// wParam and aim the menu at the wrong screen quadrant.
		// In legacy callback mode (V4 setup failed), wParam carries
		// the icon UID, NOT screen coordinates. Decoding it as XY in
		// that case anchors the popup near (1,0). Codex bot review
		// on PR #24 P2 (legacy callback fallback). Under V4, the
		// shell-encoded message lives in the low word of lParam;
		// under legacy, it lives in the entire lParam (mouse-message
		// constants fit in 16 bits, so masking is a no-op for the
		// values we care about — WM_RBUTTONUP=0x0205, etc).
		event := uint32(lparam) & 0xFFFF
		switch event {
		case WM_LBUTTONUP, NIN_SELECT, NIN_KEYSELECT:
			// Mouse left-click (WM_LBUTTONUP / NIN_SELECT) and keyboard
			// activation via Enter/Space (NIN_KEYSELECT) bring the
			// dashboard to the front, matching the
			// Config.ActivateWindow contract documented in tray.go.
			// Per MS Shell_NotifyIcon docs, NIN_KEYSELECT is the
			// keyboard equivalent of NIN_SELECT (single primary
			// action), NOT the menu invocation. The dedicated
			// WM_CONTEXTMENU branch below handles keyboard menu
			// invocation (Shift+F10 / Apps key). Codex bot review
			// on PR #24 P2.
			tc.emitEvent("open-dashboard")
		case WM_RBUTTONUP, WM_CONTEXTMENU:
			// Win11 quirk: a single mouse right-click delivers BOTH
			// WM_RBUTTONUP AND WM_CONTEXTMENU through the V4 callback
			// in quick succession (<10ms apart, empirically). Without
			// dedup the popup menu would flash open, close as the user
			// dismisses it, then re-open from the second event. Keyboard
			// Menu key / Shift+F10 invocation arrives ONLY as
			// WM_CONTEXTMENU, so adding it to the case is required for
			// keyboard accessibility (Codex bot review on PR #24 P2).
			now := time.Now()
			if now.Sub(tc.lastMenuShow) < 200*time.Millisecond {
				return 0
			}
			tc.lastMenuShow = now

			// Anchor at the icon's deterministic screen rect.
			// Even under V4 the wParam X/Y tracks the cursor pixel
			// at click time — not the icon's stable center — so two
			// right-clicks on the same pinned icon produce different
			// wParams (empirical: 5 clicks → 5 distinct coords
			// spanning ~30x25 pixels). Shell_NotifyIconGetRect
			// returns the icon's own rect; we use its top-edge
			// horizontal center as anchor. Combined with the forced
			// TPM_CENTERALIGN + adaptive TPM_BOTTOMALIGN below, the
			// menu's bottom edge sits flush against the icon's top
			// edge, horizontally centered on the icon.
			var x, y int32
			if tc.versionV4 && event == WM_RBUTTONUP {
				// WM_CONTEXTMENU's wParam in the V4 callback is
				// undefined for keyboard invocation — don't trust it
				// for coords. WM_RBUTTONUP carries valid mouse X/Y.
				x = int32(int16(uint16(wparam & 0xFFFF)))
				y = int32(int16(uint16((wparam >> 16) & 0xFFFF)))
			}
			id := NOTIFYICONIDENTIFIER{
				CbSize: uint32(unsafe.Sizeof(NOTIFYICONIDENTIFIER{})),
				HWnd:   tc.hwnd,
				UID:    tc.uid,
			}
			var rect RECT
			if shellNotifyIconGetRect(&id, &rect) {
				x = (rect.Left + rect.Right) / 2
				y = rect.Top
			} else if event == WM_CONTEXTMENU || !tc.versionV4 {
				// Cursor fallback when icon-rect API failed AND
				// either:
				//   - we're handling WM_CONTEXTMENU under V4 (wParam
				//     is undefined for keyboard Apps-key/Shift+F10
				//     invocation; without this branch x/y stay (0,0)
				//     and the menu opens in the screen corner), or
				//   - we're in legacy-callback mode (wParam carries
				//     the icon UID, NOT screen coords).
				// Codex bot review on PR #24 P2 (V4 WM_CONTEXTMENU
				// anchor fallback).
				var pt POINT
				if getCursorPos(&pt) {
					x, y = pt.X, pt.Y
				}
			}
			tc.showPopupMenuAt(x, y)
		}
		return 0

	case wmStateUpdate:
		// wParam is the TrayState value posted by the stdin reader.
		state := TrayState(wparam)
		tc.updateIcon(state)
		return 0

	case wmShutdown:
		// Parent EOF — initiate clean teardown. We send WM_CLOSE so
		// the standard close path (DestroyWindow → WM_DESTROY →
		// PostQuitMessage) runs instead of bypassing it.
		postMessageW(tc.hwnd, WM_CLOSE, 0, 0)
		return 0

	case WM_CLOSE:
		// Codex bot review on PR #24 P2: NIM_DELETE BEFORE
		// destroyWindow + zero-hwnd. Otherwise the close path
		// drops the icon registration without telling the shell,
		// leaving a ghost icon visible until the user mouses over
		// it. iconRegistered prevents the deferred shutdown() from
		// double-deleting (which would error and could disturb a
		// future child re-spawn's NIM_ADD on the same UID).
		if tc.iconRegistered {
			nid := NOTIFYICONDATAW{
				CbSize: uint32(unsafe.Sizeof(NOTIFYICONDATAW{})),
				HWnd:   tc.hwnd,
				UID:    tc.uid,
			}
			shellNotifyIcon(NIM_DELETE, &nid)
			tc.iconRegistered = false
		}
		destroyWindow(tc.hwnd)
		// hwnd will be invalidated post-destroy; clear so shutdown
		// doesn't try to delete twice.
		tc.hwnd = 0
		return 0

	case WM_DESTROY:
		// Last user32 message before the pump exits. Posting WM_QUIT
		// makes GetMessage return 0 in runMessagePump.
		postQuitMessage(0)
		return 0

	// WM_COMMAND case removed: TrackPopupMenu now uses TPM_NONOTIFY
	// (see showPopupMenuAt), so menu selections come back ONLY via
	// TPM_RETURNCMD. Without TPM_NONOTIFY, the shell sent BOTH
	// channels for one click, firing each menu action twice
	// (Codex bot review on PR #24 P1).
	}
	return defWindowProcW(hwnd, msg, wparam, lparam)
}
