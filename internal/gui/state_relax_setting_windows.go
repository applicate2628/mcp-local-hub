//go:build windows

// internal/gui/state_relax_setting_windows.go
//
// GET + POST /api/settings/state-read-relax — operator toggle for the
// MCPHUB_ALLOW_UNHARDENED_STATE_READ env var (see
// internal/api/client_write_init.go for the env var doc + the
// regression that motivated it). The Windows path writes/reads
// HKCU\Environment\MCPHUB_ALLOW_UNHARDENED_STATE_READ directly, then
// broadcasts WM_SETTINGCHANGE so already-running shells/Explorer
// pick up the change without a logoff. Already-running mcphub
// processes (the supervisor + GUI itself) still need a restart to
// see the new value because they pulled their env block at exec
// time — that limitation is surfaced in the response.
//
// Why a registry write instead of just a gui-preferences.yaml flag:
// the supervisor process starts via Task Scheduler at logon BEFORE
// the GUI has a chance to read gui-preferences.yaml and propagate
// the flag inward. The env var is part of the supervisor's process
// env at spawn; the only place that lives across reboot AND is
// inherited by Task-Scheduler-spawned processes is HKCU\Environment.

package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows/registry"

	"mcp-local-hub/internal/api"
)

const stateReadRelaxRegName = "MCPHUB_ALLOW_UNHARDENED_STATE_READ"

func registerStateRelaxSettingRoutes(s *Server) {
	s.mux.HandleFunc("/api/settings/state-read-relax", s.requireSameOrigin(s.stateRelaxSettingHandler))
}

// stateRelaxSettingRequest is the POST body shape.
type stateRelaxSettingRequest struct {
	Enabled bool `json:"enabled"`
}

// stateRelaxSettingResponse is returned for both GET and POST.
//
//   - `enabled` reflects the on-disk HKCU value AFTER the operation
//     (so the frontend always sees ground truth, not the cached
//     pre-write state).
//   - `restart_required` is true when the operation changed state;
//     surfaced because the running mcphub processes won't pick up
//     the new env var without a restart.
type stateRelaxSettingResponse struct {
	Enabled         bool `json:"enabled"`
	RestartRequired bool `json:"restart_required"`
}

func (s *Server) stateRelaxSettingHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.stateRelaxSettingGet(w)
	case http.MethodPost:
		s.stateRelaxSettingPost(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) stateRelaxSettingGet(w http.ResponseWriter) {
	enabled, err := readStateRelaxRegistry()
	if err != nil {
		writeAPIError(w, fmt.Errorf("read registry: %w", err), http.StatusInternalServerError, "REGISTRY_READ_FAILED")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stateRelaxSettingResponse{Enabled: enabled})
}

func (s *Server) stateRelaxSettingPost(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req stateRelaxSettingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, fmt.Errorf("decode body: %w", err), http.StatusBadRequest, "BAD_REQUEST")
		return
	}

	prev, _ := readStateRelaxRegistry()
	if err := writeStateRelaxRegistry(req.Enabled); err != nil {
		writeAPIError(w, fmt.Errorf("write registry: %w", err), http.StatusInternalServerError, "REGISTRY_WRITE_FAILED")
		return
	}
	// Broadcast WM_SETTINGCHANGE so Explorer + future-spawned shells
	// see the new value without a logoff. Already-running mcphub
	// processes will NOT pick it up — restart-required flag covers
	// that case in the response.
	_ = broadcastStateRelaxEnvChange()

	_ = api.LogHubMcpEvent("info", "state-read-relax-toggled-via-gui", map[string]any{
		"enabled": req.Enabled,
		"changed": prev != req.Enabled,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stateRelaxSettingResponse{
		Enabled:         req.Enabled,
		RestartRequired: prev != req.Enabled,
	})
}

// readStateRelaxRegistry returns true iff
// HKCU\Environment\MCPHUB_ALLOW_UNHARDENED_STATE_READ is set to "1"
// or "true" (case-insensitive). A missing value returns (false, nil).
func readStateRelaxRegistry() (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE)
	if err != nil {
		return false, fmt.Errorf("open HKCU\\Environment: %w", err)
	}
	defer k.Close()
	val, _, err := k.GetStringValue(stateReadRelaxRegName)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", stateReadRelaxRegName, err)
	}
	switch val {
	case "1", "true", "True", "TRUE":
		return true, nil
	}
	return false, nil
}

// writeStateRelaxRegistry sets (enabled=true) or deletes
// (enabled=false) the HKCU\Environment\MCPHUB_ALLOW_UNHARDENED_STATE_READ
// value. Deletion (vs writing "0") is intentional so the env var
// stays unset for tools that distinguish absence from "false" — the
// Go-side `operatorAllowsUnhardenedStateRead` honors both forms but
// other inspectors may not.
func writeStateRelaxRegistry(enabled bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open HKCU\\Environment: %w", err)
	}
	defer k.Close()
	if enabled {
		if err := k.SetStringValue(stateReadRelaxRegName, "1"); err != nil {
			return fmt.Errorf("set %s: %w", stateReadRelaxRegName, err)
		}
		return nil
	}
	if err := k.DeleteValue(stateReadRelaxRegName); err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil // already absent
		}
		return fmt.Errorf("delete %s: %w", stateReadRelaxRegName, err)
	}
	return nil
}

// broadcastStateRelaxEnvChange sends WM_SETTINGCHANGE with lparam
// "Environment" to all top-level windows so Explorer and future-
// spawned shells pick up the new HKCU\Environment value without a
// logoff. Same wire pattern as setup_windows.go:broadcastEnvChange,
// duplicated here because that one is package-private to internal/cli.
func broadcastStateRelaxEnvChange() error {
	const (
		HWND_BROADCAST      = 0xFFFF
		WM_SETTINGCHANGE    = 0x001A
		SMTO_ABORTIFHUNG    = 0x0002
		smtoTimeoutMillisec = 5000
	)
	user32 := syscall.NewLazyDLL("user32.dll")
	procSendMessageTimeoutW := user32.NewProc("SendMessageTimeoutW")
	lparam, err := syscall.UTF16PtrFromString("Environment")
	if err != nil {
		return fmt.Errorf("utf16 lparam: %w", err)
	}
	var result uintptr
	r, _, callErr := procSendMessageTimeoutW.Call(
		uintptr(HWND_BROADCAST),
		uintptr(WM_SETTINGCHANGE),
		0,
		uintptr(unsafe.Pointer(lparam)),
		uintptr(SMTO_ABORTIFHUNG),
		uintptr(smtoTimeoutMillisec),
		uintptr(unsafe.Pointer(&result)),
	)
	if r == 0 {
		return fmt.Errorf("SendMessageTimeoutW failed: %v", callErr)
	}
	return nil
}
