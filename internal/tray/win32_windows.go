//go:build windows

package tray

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file holds the raw Win32 syscall wrappers used by the tray
// child's message-pump implementation in tray_windows.go. We keep
// them in their own file so the IPC + state-update logic next door
// stays readable, and so the syscall surface is auditable in one
// place when chasing tray quirks (foreground handshake, popup
// alignment, NIM_DELETE on every exit path, etc).
//
// Calling convention notes:
//
//   - All UTF-16 strings going into Win32 use windows.UTF16PtrFromString
//     or fixed-size [N]uint16 arrays (NOTIFYICONDATAW.szTip is one).
//   - Errors are taken from the LastError slot returned by the .Call
//     wrappers; we propagate them only where the caller can act on
//     them (creation paths). Per-event paths (icon update, popup
//     positioning) log to stderr and continue — losing one update
//     should not crash the tray.
//   - syscall.LazyDLL avoids a hard import-time dependency on shell32
//     etc., matching the cross-platform build expectation: this file
//     is `//go:build windows`, but lazy-loading is still cheap and
//     keeps the surface uniform.

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW          = user32.NewProc("RegisterClassExW")
	procCreateWindowExW           = user32.NewProc("CreateWindowExW")
	procDestroyWindow             = user32.NewProc("DestroyWindow")
	procDefWindowProcW            = user32.NewProc("DefWindowProcW")
	procGetMessageW               = user32.NewProc("GetMessageW")
	procTranslateMessage          = user32.NewProc("TranslateMessage")
	procDispatchMessageW          = user32.NewProc("DispatchMessageW")
	procPostMessageW              = user32.NewProc("PostMessageW")
	procPostQuitMessage           = user32.NewProc("PostQuitMessage")
	procSendMessageW              = user32.NewProc("SendMessageW")
	procLoadCursorW               = user32.NewProc("LoadCursorW")
	procCreateIconFromResourceEx  = user32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon               = user32.NewProc("DestroyIcon")
	procCreatePopupMenu           = user32.NewProc("CreatePopupMenu")
	procAppendMenuW               = user32.NewProc("AppendMenuW")
	procDestroyMenu               = user32.NewProc("DestroyMenu")
	procTrackPopupMenu            = user32.NewProc("TrackPopupMenu")
	procSetForegroundWindow       = user32.NewProc("SetForegroundWindow")
	procGetCursorPos              = user32.NewProc("GetCursorPos")
	procGetSystemMetrics          = user32.NewProc("GetSystemMetrics")
	procMonitorFromPoint          = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW           = user32.NewProc("GetMonitorInfoW")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")

	procShellNotifyIconW       = shell32.NewProc("Shell_NotifyIconW")
	procShellNotifyIconGetRect = shell32.NewProc("Shell_NotifyIconGetRect")

	procRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")

	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
	procGetCurrentProcessId = kernel32.NewProc("GetCurrentProcessId")
)

// registerWindowMessage wraps RegisterWindowMessageW. The returned
// message ID is unique per session per name and is shared by every
// process that calls Register with the same name.
func registerWindowMessage(name string) (uint32, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	r, _, e := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(p)))
	if r == 0 {
		return 0, e
	}
	return uint32(r), nil
}

// Win32 constants used here. Numeric values match the SDK headers.
const (
	WM_DESTROY     = 0x0002
	WM_CLOSE       = 0x0010
	WM_QUIT        = 0x0012
	WM_COMMAND     = 0x0111
	WM_CONTEXTMENU = 0x007B
	WM_NULL        = 0x0000
	WM_LBUTTONUP   = 0x0202
	WM_RBUTTONUP   = 0x0205
	WM_USER        = 0x0400
	WM_APP         = 0x8000

	// NOTIFYICON_VERSION_4 callback events. With NIM_SETVERSION(4)
	// the shell sends these *instead of* WM_LBUTTONUP/WM_RBUTTONUP
	// for tray icon clicks, and only these carry the icon-anchor
	// coordinates in wParam.
	NIN_SELECT     = WM_USER + 0 // left-click
	NIN_KEYSELECT  = WM_USER + 1 // SHIFT+F10 / Menu-key keyboard activation
	NIN_POPUPOPEN  = WM_USER + 6 // tooltip / hover popup opens — wParam = icon anchor X/Y
	NIN_POPUPCLOSE = WM_USER + 7 // tooltip / hover popup closes

	// Tray callback / cross-thread post messages — we choose values
	// well above WM_APP so they don't collide with documented
	// system or common-control messages.
	wmTrayCallback = WM_APP + 1 // shell-icon → window
	wmStateUpdate  = WM_APP + 2 // stdin reader → pump (lParam = TrayState)
	wmShutdown     = WM_APP + 3 // stdin reader → pump (parent EOF)

	// CreatePopupMenu flags
	MF_STRING    = 0x00000000
	MF_SEPARATOR = 0x00000800

	// TrackPopupMenu flags (alignment + return value)
	TPM_LEFTALIGN   = 0x0000
	TPM_CENTERALIGN = 0x0004
	TPM_RIGHTALIGN  = 0x0008
	TPM_TOPALIGN    = 0x0000
	TPM_BOTTOMALIGN = 0x0020
	TPM_RIGHTBUTTON = 0x0002
	TPM_RETURNCMD   = 0x0100
	TPM_NONOTIFY    = 0x0080

	// NOTIFYICONDATAW flags
	NIF_MESSAGE = 0x00000001
	NIF_ICON    = 0x00000002
	NIF_TIP     = 0x00000004
	// NIF_SHOWTIP forces the standard tooltip on Vista+ when V4 callback
	// ABI is in use. Without it, NOTIFYICON_VERSION_4 documentation says
	// the standard tooltip is suppressed, even when SzTip is populated.
	// Codex CLI xhigh review on PR #24 P2.
	NIF_SHOWTIP = 0x00000080

	// Shell_NotifyIcon dwMessage
	NIM_ADD        = 0x00000000
	NIM_MODIFY     = 0x00000001
	NIM_DELETE     = 0x00000002
	NIM_SETVERSION = 0x00000004

	// NOTIFYICON_VERSION_4 enables the Win7+ callback semantics where
	// the shell passes click coordinates and the source icon UID
	// directly in wParam/lParam — without this, callbacks use the
	// legacy XP-era encoding that has no click coordinates and forces
	// callers to fall back to cursor or icon-rect heuristics.
	NOTIFYICON_VERSION_4 = 4

	// Window creation
	HWND_MESSAGE = ^uintptr(2) // (HWND)-3 sentinel for message-only window
	IDC_ARROW    = 32512
)

// menu command IDs (used in WM_COMMAND.LOWORD(wParam))
const (
	cmdOpenDashboard      = 1
	cmdQuit               = 2
	cmdQuitAndStopDaemons = 3
)

// POINT matches Win32 POINT.
type POINT struct {
	X, Y int32
}

// RECT matches Win32 RECT.
type RECT struct {
	Left, Top, Right, Bottom int32
}

// MSG matches Win32 MSG.
type MSG struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      POINT
	_       uint32 // padding on 64-bit (lPrivate); harmless on 32-bit
}

// WNDCLASSEXW matches Win32 WNDCLASSEXW.
type WNDCLASSEXW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

// NOTIFYICONDATAW — pinned to the layout used since Vista. We set
// only the fields covered by NIF_ICON | NIF_MESSAGE | NIF_TIP, but
// the struct must be the full size (cbSize) or Shell_NotifyIcon
// fails on modern Windows.
//
// szTip is fixed at 128 uint16 to match NOTIFYICONDATA_V2_SIZE+;
// values longer than 127 chars get silently truncated by the shell.
type NOTIFYICONDATAW struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     uintptr
}

// NOTIFYICONIDENTIFIER for Shell_NotifyIconGetRect.
type NOTIFYICONIDENTIFIER struct {
	CbSize uint32
	HWnd   uintptr
	UID    uint32
	GuidItem windows.GUID
}

// --- syscall wrappers ---

func getModuleHandle() uintptr {
	r, _, _ := procGetModuleHandleW.Call(0)
	return r
}

func getCurrentProcessId() uint32 {
	r, _, _ := procGetCurrentProcessId.Call()
	return uint32(r)
}

func loadDefaultCursor() uintptr {
	r, _, _ := procLoadCursorW.Call(0, IDC_ARROW)
	return r
}

func registerClassExW(cls *WNDCLASSEXW) (uint16, error) {
	r, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(cls)))
	if r == 0 {
		return 0, e
	}
	return uint16(r), nil
}

func createTrayHostWindow(className *uint16, hInstance uintptr) (uintptr, error) {
	// CreateWindowExW(WS_EX_TOOLWINDOW, className, "", WS_POPUP,
	//                 0, 0, 0, 0, 0 (no parent), 0 (no menu),
	//                 hInstance, NULL).
	//
	// Why a top-level invisible window instead of HWND_MESSAGE:
	//   1. HWND_MESSAGE is a message-only window. The shell broadcasts
	//      "TaskbarCreated" only to TOP-LEVEL windows. Without a
	//      top-level host, our tray icon never re-registers after
	//      explorer.exe restart (Codex bot review on PR #24 P2).
	//   2. SetForegroundWindow's foreground-window requirement for
	//      TrackPopupMenu only honors top-level activatable windows.
	//      Message-only windows can't satisfy it, so the popup
	//      sometimes fails to appear when another app owns focus
	//      (Codex bot review on PR #24 P1).
	//
	// The window is created with style WS_POPUP and WS_EX_TOOLWINDOW:
	//   - WS_POPUP: top-level, no caption, no border. The window is
	//     hidden (we never call ShowWindow with SW_SHOW), so the user
	//     never sees it; size 0x0 keeps the back buffer trivial.
	//   - WS_EX_TOOLWINDOW: keeps the (always-hidden) window out of
	//     Alt+Tab and the taskbar. Required so a stray repaint can't
	//     accidentally surface a 0x0 ghost.
	const (
		WS_POPUP        = uintptr(0x80000000)
		WS_EX_TOOLWINDOW = uintptr(0x00000080)
	)
	r, _, e := procCreateWindowExW.Call(
		WS_EX_TOOLWINDOW,
		uintptr(unsafe.Pointer(className)),
		0,        // window name (NULL)
		WS_POPUP, // style
		0, 0, 0, 0,
		0, // no parent — top-level
		0, // menu
		hInstance,
		0, // lpParam
	)
	if r == 0 {
		return 0, e
	}
	return r, nil
}

func destroyWindow(hwnd uintptr) {
	procDestroyWindow.Call(hwnd)
}

func defWindowProcW(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
	return r
}

func getMessageW(msg *MSG, hwnd uintptr) int32 {
	// HWND filter = 0 → all messages for the calling thread.
	// wMsgFilterMin/Max = 0 → no message-range filter.
	r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(msg)), hwnd, 0, 0)
	return int32(r)
}

func translateMessage(msg *MSG) {
	procTranslateMessage.Call(uintptr(unsafe.Pointer(msg)))
}

func dispatchMessageW(msg *MSG) {
	procDispatchMessageW.Call(uintptr(unsafe.Pointer(msg)))
}

func postMessageW(hwnd uintptr, msg uint32, wparam, lparam uintptr) bool {
	r, _, _ := procPostMessageW.Call(hwnd, uintptr(msg), wparam, lparam)
	return r != 0
}

func postQuitMessage(exitCode int32) {
	procPostQuitMessage.Call(uintptr(exitCode))
}

func setForegroundWindow(hwnd uintptr) {
	procSetForegroundWindow.Call(hwnd)
}

func getCursorPos(pt *POINT) bool {
	r, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(pt)))
	return r != 0
}

// getSystemMetrics wraps GetSystemMetrics.
func getSystemMetrics(index int32) int32 {
	r, _, _ := procGetSystemMetrics.Call(uintptr(index))
	return int32(r)
}

// MONITORINFO matches Win32 MONITORINFO. cbSize is set by caller.
type MONITORINFO struct {
	CbSize    uint32
	RcMonitor RECT
	RcWork    RECT
	DwFlags   uint32
}

// MONITOR_DEFAULTTONEAREST = 2: return the monitor closest to the
// supplied point even if the point is outside any monitor's bounds.
const MONITOR_DEFAULTTONEAREST = 2

// DPI_AWARENESS_CONTEXT values (Win10 1607+) passed to
// SetProcessDpiAwarenessContext. -4 (PER_MONITOR_AWARE_V2) is the
// modern default for desktop apps that handle their own scaling.
// We use it so that mouse-event coords AND TrackPopupMenu coords
// share the same physical-pixel space; DPI_UNAWARE virtualizes
// only one side which causes off-screen popup placement on
// scaled monitors.
const DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = ^uintptr(3) // -4 cast as uintptr

// setProcessDpiAwareV2 declares the calling process per-monitor DPI
// aware (V2). Returns false if the call failed (Windows < 1607,
// already-set, or syscall error). Best-effort; we log but continue
// even on failure since the existing behaviour pre-fix was unaware,
// so worst case is the prior (broken) behaviour.
//
// SetProcessDpiAwarenessContext was added in Windows 10 1607. On
// older builds (Win7/8.1/early 10) the user32 export does not exist,
// and LazyProc.Call would panic when Find() fails internally.
// procSetProcessDpiAwarenessContext.Find() is the lazy resolver — we
// call it explicitly first so the missing-API path returns false
// instead of crashing the tray child at startup. Codex bot review
// on PR #24 P2 (guard optional DPI API).
func setProcessDpiAwareV2() bool {
	if err := procSetProcessDpiAwarenessContext.Find(); err != nil {
		return false
	}
	r, _, _ := procSetProcessDpiAwarenessContext.Call(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2)
	return r != 0
}

// monitorWorkArea returns the work-area rect (excludes taskbar) of
// the monitor containing or nearest to the supplied screen-coord
// point. Used by the popup-menu placer so that the menu opens into
// usable screen space regardless of which monitor the icon's
// anchor lands on (multi-monitor with negative-coord secondaries
// is the common case where SM_C[XY]SCREEN fail).
func monitorWorkArea(x, y int32) (RECT, bool) {
	pt := POINT{X: x, Y: y}
	hMon, _, _ := procMonitorFromPoint.Call(
		// POINT is two int32 packed; on x64 we pass it as a single
		// 64-bit register per Win32 ABI for "POINT" by-value calls.
		uintptr(uint64(uint32(pt.X))|uint64(uint32(pt.Y))<<32),
		uintptr(MONITOR_DEFAULTTONEAREST),
	)
	if hMon == 0 {
		return RECT{}, false
	}
	mi := MONITORINFO{CbSize: uint32(unsafe.Sizeof(MONITORINFO{}))}
	r, _, _ := procGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi)))
	if r == 0 {
		return RECT{}, false
	}
	return mi.RcWork, true
}

// createIconFromResourceEx parses the bytes as an icon resource
// (ICO container in our case) and returns an HICON. The fIcon=1,
// version=0x00030000 combination matches the LookupIconIdFromDirectoryEx
// fallback path documented for ICO files.
func createIconFromResourceEx(data []byte, w, h int32) (uintptr, error) {
	if len(data) == 0 {
		return 0, syscall.EINVAL
	}
	// CreateIconFromResourceEx expects the icon RESOURCE BITS — i.e.
	// just the inner image data (PNG payload OR DIB) — NOT the full
	// ICO container with its 22-byte ICONDIR + ICONDIRENTRY header.
	// IconBytes() on Windows wraps the PNG in an ICO container so
	// the same byte slice could be passed to LoadImage (which DOES
	// expect an ICO file). For CreateIconFromResourceEx we strip
	// the wrapper and pass only the PNG bytes inside.
	//
	// The 22-byte offset is hard-coded because wrapPngInIco builds
	// a single-image ICO with a fixed header layout (see icons.go
	// `wrapPngInIco`). If a different IconBytes layout ships, this
	// must be adjusted to read ICONDIRENTRY.imageOffset.
	const icoHeaderBytes = 22
	src := data
	if len(data) > icoHeaderBytes {
		src = data[icoHeaderBytes:]
	}
	// CreateIconFromResourceEx requires a DWORD-aligned (4-byte
	// aligned) presbits buffer. data[icoHeaderBytes:] starts at
	// offset 22 inside whatever Go heap allocation backed the
	// caller's slice — typically 2-byte misaligned because Go
	// allocations are 8-byte aligned at the slice header but the
	// 22-byte offset breaks that. Copy into a fresh allocation so
	// the start is guaranteed 4-byte aligned (Go's allocator
	// returns at least 8-byte alignment for fresh slices).
	// Codex bot review on PR #24 P1.
	payload := make([]byte, len(src))
	copy(payload, src)
	r, _, e := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&payload[0])),
		uintptr(len(payload)),
		1,           // fIcon (TRUE = icon, not cursor)
		0x00030000,  // dwVersion
		uintptr(w),
		uintptr(h),
		0, // LR_DEFAULTCOLOR
	)
	if r == 0 {
		return 0, e
	}
	return r, nil
}

func destroyIcon(hicon uintptr) {
	if hicon != 0 {
		procDestroyIcon.Call(hicon)
	}
}

func createPopupMenu() (uintptr, error) {
	r, _, e := procCreatePopupMenu.Call()
	if r == 0 {
		return 0, e
	}
	return r, nil
}

func appendMenuStringW(hmenu uintptr, id uintptr, text string) error {
	p, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return err
	}
	r, _, e := procAppendMenuW.Call(
		hmenu,
		MF_STRING,
		id,
		uintptr(unsafe.Pointer(p)),
	)
	if r == 0 {
		return e
	}
	return nil
}

func appendMenuSeparator(hmenu uintptr) error {
	r, _, e := procAppendMenuW.Call(hmenu, MF_SEPARATOR, 0, 0)
	if r == 0 {
		return e
	}
	return nil
}

func destroyMenu(hmenu uintptr) {
	if hmenu != 0 {
		procDestroyMenu.Call(hmenu)
	}
}

// trackPopupMenu shows the menu at (x,y) anchored per flags. With
// TPM_RETURNCMD we get the chosen command ID back as the return
// value (0 = nothing chosen / dismissed).
func trackPopupMenu(hmenu uintptr, flags uint32, x, y int32, hwnd uintptr) int32 {
	r, _, _ := procTrackPopupMenu.Call(
		hmenu,
		uintptr(flags),
		uintptr(x),
		uintptr(y),
		0, // nReserved
		hwnd,
		0, // prcRect (NULL)
	)
	return int32(r)
}

// shellNotifyIcon wraps Shell_NotifyIconW.
func shellNotifyIcon(message uint32, data *NOTIFYICONDATAW) bool {
	r, _, _ := procShellNotifyIconW.Call(
		uintptr(message),
		uintptr(unsafe.Pointer(data)),
	)
	return r != 0
}

// shellNotifyIconGetRect asks the shell for the icon's screen
// rectangle. Returns ok=false on Windows < 7 or if the icon isn't
// currently visible (e.g. it's hidden in the overflow flyout); the
// caller falls back to cursor coordinates in that case.
func shellNotifyIconGetRect(id *NOTIFYICONIDENTIFIER, rect *RECT) bool {
	// HRESULT return; S_OK = 0.
	r, _, _ := procShellNotifyIconGetRect.Call(
		uintptr(unsafe.Pointer(id)),
		uintptr(unsafe.Pointer(rect)),
	)
	return r == 0
}
