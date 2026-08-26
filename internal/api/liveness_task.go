// Package api — supervisor-liveness scheduled-task install surface
// (v0.6 redesign spec §15 P1-b / §5.x Phase 3a + 3b).
//
// The liveness task is the v0.6 owner-death recovery: it drives `mcphub
// supervise --ensure-alive` every ~1 minute; that action probes the
// flock-authoritative SupervisorRunningUnderStateDir and, when no supervisor
// holds the lock, relaunches the owner via the autostart task.
//
// Phase 3a added it ALONGSIDE the still-present v0.4.x watchdog; Phase 3b
// (spec §5 Phase C/D) then DELETED the watchdog engine, leaving the liveness
// task as the sole maintenance-task install surface. This file therefore also
// hosts the canonical-exe / current-user resolvers migrated out of the deleted
// watchdog-side files (it is their surviving consumer) plus the
// RemoveLegacyWatchdogTask cleanup for existing hosts.
package api

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os/user"
	"strings"
	"unicode/utf16"

	"mcp-local-hub/internal/scheduler"
)

// livenessWorkingDir returns the directory portion of the canonical mcphub
// executable path for the liveness task's <WorkingDirectory> element.
//
// It must NOT use filepath.Dir: path/filepath is OS-specific, so on a
// non-Windows host filepath.Dir of a Windows-shaped path
// ("C:\Users\…\bin\mcphub.exe") finds no '/' separator and returns "." —
// which makes the rendered XML differ by host OS and fails the
// cross-platform liveness test. The liveness task is a Windows-GA surface
// (scheduler.New() returns "not implemented" on POSIX), but its XML must
// render identically on any host so the test asserts one expected output.
//
// Splitting on the LAST separator of either kind ('\' or '/') yields the
// correct parent dir for both the Windows canonical path (backslash) and
// the POSIX canonical path (forward slash), independent of the host OS's
// filepath separator. A path with no separator returns itself verbatim
// (the original filepath.Dir(".")-style fallback never applied to a real
// absolute exe path).
func livenessWorkingDir(exePath string) string {
	if i := strings.LastIndexAny(exePath, `\/`); i >= 0 {
		return exePath[:i]
	}
	return exePath
}

// canonicalMcphubPathFn is the canonical-mcphub-path resolver used by the
// scheduled-task install Command field. Production: thin adapter over the
// internal canonicalMcphubPath() (install.go). Tests: inject a stub via
// SetTestCanonicalMcphubPathFn (testhooks.go).
//
// Migrated here from watchdog_xml_validator.go when the v0.6 redesign
// (spec §5 Phase D) deleted the watchdog engine — InstallLivenessTask
// (below) is the surviving consumer of both resolvers, so they live with
// the task they install rather than in deleted watchdog-side files.
var canonicalMcphubPathFn = func() (string, error) { return canonicalMcphubPath() }

// currentWindowsUserFn is the current-user resolver used by the
// scheduled-task install principal field. Production:
// defaultCurrentWindowsUser() (strips DOMAIN\\ prefix from
// user.Current().Username). Tests: inject via SetTestCurrentWindowsUserFn.
var currentWindowsUserFn = defaultCurrentWindowsUser

// defaultCurrentWindowsUser returns the bare username of the running
// process, stripping the DOMAIN\\ prefix Windows attaches to
// user.Current().Username.
func defaultCurrentWindowsUser() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("user.Current: %w", err)
	}
	name := u.Username
	if i := strings.LastIndex(name, "\\"); i >= 0 {
		name = name[i+1:]
	}
	return name, nil
}

// LivenessTaskName is the canonical scheduled-task name installed alongside
// the daemon tasks by `mcphub setup` (Phase 3a). Kept as a package-level
// constant so callers reference one literal.
const LivenessTaskName = "\\mcp-local-hub-liveness"

// LivenessTaskResult identifies the pre-state that an EnsureLivenessTask
// receipt can restore. It is an internal transaction result, not a public CLI
// state string.
type LivenessTaskResult string

const (
	LivenessTaskUnchanged LivenessTaskResult = "unchanged"
	LivenessTaskCreated   LivenessTaskResult = "created-from-absent"
	LivenessTaskReplaced  LivenessTaskResult = "replaced-with-prior-xml"
)

// LivenessTaskReceipt is valid only for the setup transaction that received
// it. The captured settled XML is a compare-before-restore fence: rollback
// refuses to delete or overwrite a liveness task changed after settlement.
type LivenessTaskReceipt struct {
	Result     LivenessTaskResult
	priorXML   []byte
	settledXML []byte
}

var ErrLivenessTaskRollbackConflict = errors.New("api: liveness task rollback conflict")

// InstallLivenessTask is the idempotent install of the supervisor-liveness
// scheduled task: resolve the canonical mcphub.exe path + the current Windows
// user, render the liveness XML via scheduler.BuildLivenessXML, encode it
// for Task Scheduler's /XML parser, and ImportXML it under LivenessTaskName.
//
// CLI-level concerns (admin-elevation refusal, audit entry, interactive
// confirm) live in the CLI layer that calls this (runSetupWatchdog rides the
// same state-dir-sanity + elevation gates); this API method is the
// unconditional execution path.
//
// On Linux/macOS scheduler.New() returns "not implemented" so this fails
// loud at the ImportXML call — the liveness task is a Windows-GA capability
// in v0.6.
func (a *API) InstallLivenessTask() error {
	_, err := a.EnsureLivenessTask()
	return err
}

// EnsureLivenessTask settles the API-owned liveness task and returns an exact
// rollback receipt. The caller is responsible for proving the supervisor
// target first; this owner never creates or mutates that separate resource.
func (a *API) EnsureLivenessTask() (LivenessTaskReceipt, error) {
	canonicalExe, err := canonicalMcphubPathFn()
	if err != nil {
		return LivenessTaskReceipt{}, err
	}
	userName, err := currentWindowsUserFn()
	if err != nil {
		return LivenessTaskReceipt{}, err
	}
	workingDir := livenessWorkingDir(canonicalExe)
	sch, err := newScheduler()
	if err != nil {
		return LivenessTaskReceipt{}, err
	}
	prior, exportErr := sch.ExportXML(LivenessTaskName)
	receipt := LivenessTaskReceipt{}
	if exportErr != nil {
		if !errors.Is(exportErr, scheduler.ErrTaskNotFound) {
			return receipt, fmt.Errorf("snapshot liveness task: %w", exportErr)
		}
		receipt.Result = LivenessTaskCreated
	} else {
		receipt.Result = LivenessTaskReplaced
		receipt.priorXML = append([]byte(nil), prior...)
		if !livenessTaskReadable(prior) {
			return receipt, fmt.Errorf("liveness task definition is corrupt")
		}
		if livenessTaskMatches(prior, canonicalExe, workingDir, userName) {
			receipt.Result = LivenessTaskUnchanged
			receipt.settledXML = append([]byte(nil), prior...)
			return receipt, nil
		}
	}
	xmlBytes := scheduler.EncodeXMLUTF16LEBOM(scheduler.BuildLivenessXML(canonicalExe, workingDir, userName))
	if err := sch.ImportXML(LivenessTaskName, xmlBytes); err != nil {
		return receipt, fmt.Errorf("import liveness task: %w", err)
	}
	settled, err := sch.ExportXML(LivenessTaskName)
	if err != nil {
		return receipt, fmt.Errorf("read back liveness task: %w", err)
	}
	receipt.settledXML = append([]byte(nil), settled...)
	if !livenessTaskMatches(settled, canonicalExe, workingDir, userName) {
		return receipt, fmt.Errorf("liveness task definition drifted after import")
	}
	return receipt, nil
}

// RestoreLivenessTask restores only the exact pre-state captured in receipt.
func (a *API) RestoreLivenessTask(receipt LivenessTaskReceipt) error {
	if receipt.Result == LivenessTaskUnchanged {
		return nil
	}
	if len(receipt.settledXML) == 0 {
		return fmt.Errorf("%w: receipt has no settled liveness XML", ErrLivenessTaskRollbackConflict)
	}
	sch, err := newScheduler()
	if err != nil {
		return err
	}
	current, err := sch.ExportXML(LivenessTaskName)
	if err != nil || !bytes.Equal(current, receipt.settledXML) {
		return fmt.Errorf("%w: liveness task changed after settlement", ErrLivenessTaskRollbackConflict)
	}
	if receipt.Result == LivenessTaskCreated {
		if err := sch.Delete(LivenessTaskName); err != nil {
			return fmt.Errorf("delete created liveness task: %w", err)
		}
		return nil
	}
	if err := sch.ImportXML(LivenessTaskName, receipt.priorXML); err != nil {
		return fmt.Errorf("restore liveness task XML: %w", err)
	}
	restored, err := sch.ExportXML(LivenessTaskName)
	if err != nil || !bytes.Equal(restored, receipt.priorXML) {
		return fmt.Errorf("%w: liveness XML readback differs from captured pre-state", ErrLivenessTaskRollbackConflict)
	}
	return nil
}

func livenessTaskMatches(xmlBlob []byte, canonicalExe, workingDir, userName string) bool {
	task, ok := parseLivenessTaskXML(xmlBlob)
	return ok && task.UserID == userName && task.Exec.Command == canonicalExe &&
		task.Exec.WorkingDirectory == workingDir && task.Exec.Arguments == "supervise --ensure-alive" &&
		strings.EqualFold(task.Enabled, "true")
}

type livenessTaskXML struct {
	UserID string
	Exec   struct {
		Command          string `xml:"Command"`
		Arguments        string `xml:"Arguments"`
		WorkingDirectory string `xml:"WorkingDirectory"`
	}
	Enabled string
}

func livenessTaskReadable(xmlBlob []byte) bool {
	_, ok := parseLivenessTaskXML(xmlBlob)
	return ok
}

func parseLivenessTaskXML(xmlBlob []byte) (livenessTaskXML, bool) {
	type execNode struct {
		Command          string `xml:"Command"`
		Arguments        string `xml:"Arguments"`
		WorkingDirectory string `xml:"WorkingDirectory"`
	}
	type taskRoot struct {
		UserID  string   `xml:"Principals>Principal>UserId"`
		Exec    execNode `xml:"Actions>Exec"`
		Enabled string   `xml:"Settings>Enabled"`
	}
	if len(xmlBlob) >= 2 && xmlBlob[0] == 0xff && xmlBlob[1] == 0xfe && (len(xmlBlob)-2)%2 == 0 {
		units := make([]uint16, 0, (len(xmlBlob)-2)/2)
		for i := 2; i < len(xmlBlob); i += 2 {
			units = append(units, binary.LittleEndian.Uint16(xmlBlob[i:i+2]))
		}
		xmlBlob = []byte(string(utf16.Decode(units)))
	}
	decoder := xml.NewDecoder(bytes.NewReader(xmlBlob))
	decoder.CharsetReader = func(_ string, reader io.Reader) (io.Reader, error) { return reader, nil }
	var task taskRoot
	if err := decoder.Decode(&task); err != nil {
		return livenessTaskXML{}, false
	}
	if task.Exec.Command == "" || task.Exec.Arguments == "" || task.UserID == "" {
		return livenessTaskXML{}, false
	}
	return livenessTaskXML{
		UserID: task.UserID,
		Exec: struct {
			Command          string `xml:"Command"`
			Arguments        string `xml:"Arguments"`
			WorkingDirectory string `xml:"WorkingDirectory"`
		}{
			Command: task.Exec.Command, Arguments: task.Exec.Arguments, WorkingDirectory: task.Exec.WorkingDirectory,
		},
		Enabled: task.Enabled,
	}, true
}

// UninstallLivenessTask is the idempotent removal of the supervisor-liveness
// scheduled task.
//
// Wired into the CLI uninstall path at internal/cli/setup.go's
// runUninstallWatchdog, INSIDE the same last-server partial-uninstall gate
// (shouldRemoveGlobalWatchdog) that authorizes the legacy-watchdog teardown:
// the liveness task is a hub-wide shared maintenance job, so it is removed only
// when the last managed server is uninstalled, never while peer servers
// remain. The call is non-fatal / idempotent there — scheduler.Delete
// returns nil for an absent task.
func (a *API) UninstallLivenessTask() error {
	sch, err := newScheduler()
	if err != nil {
		return err
	}
	return sch.Delete(LivenessTaskName)
}

// LegacyWatchdogTaskName is the canonical name of the v0.4.x watchdog
// scheduled task. The watchdog ENGINE was deleted in the v0.6 redesign
// (spec §5 Phase D); this literal survives ONLY so existing hosts can
// have the leftover task removed by `mcphub setup` / `mcphub uninstall`.
// Kept here (not in a deleted watchdog-side file) so the one place that
// still needs the literal owns it.
const LegacyWatchdogTaskName = "\\mcp-local-hub-watchdog"

// RemoveLegacyWatchdogTask deletes the leftover v0.4.x
// `\mcp-local-hub-watchdog` scheduled task on existing hosts. The v0.6
// supervisor owns daemon revival via its Job-Object reaper + reconcile
// loop, and owner-death recovery is the liveness task — so the legacy
// watchdog is a no-op vestige that actively fights the supervisor every
// 5 min (it writes "suspicious-xml" warnings against the v0.5.0 task XML
// its v0.4.x validator can no longer parse).
//
// Idempotent + best-effort by contract: scheduler.Delete returns nil for
// an absent task (clean hosts), so callers treat this as non-fatal. Routes
// through the same newScheduler() factory seam as UninstallLivenessTask so
// tests can drive it with a recording fake.
func (a *API) RemoveLegacyWatchdogTask() error {
	sch, err := newScheduler()
	if err != nil {
		return err
	}
	return sch.Delete(LegacyWatchdogTaskName)
}
