//go:build windows && cst_vptr_probe

package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/e2e/cstwatchdogwire"
	"mcp-local-hub/internal/process"
)

const f4Startup = 2 * time.Second
const f4D = 250 * time.Millisecond
const f4A = 500 * time.Millisecond
const f4VerdictSlack = time.Second

type f4Verdict struct {
	Version                  string `json:"version"`
	Cell                     string `json:"cell"`
	Fault                    string `json:"fault,omitempty"`
	Status                   string `json:"status"`
	RootPath                 string `json:"rootPath,omitempty"` // local raw transcript only
	RootTokenDigest          string `json:"rootTokenDigest,omitempty"`
	BinarySHA256             string `json:"binarySha256"`
	StartupMS                int64  `json:"startupMs"`
	DMS                      int64  `json:"dMs"`
	AMS                      int64  `json:"aMs"`
	T0UTC                    string `json:"t0Utc,omitempty"`
	FrozenUTC                string `json:"frozenUtc,omitempty"`
	ElapsedMS                int64  `json:"elapsedMs"`
	Attempted                int    `json:"attempted"`
	Barrier                  int    `json:"barrier"`
	Fired                    int    `json:"fired"`
	Finished                 int    `json:"finished"`
	JobCloses                int    `json:"jobCloses"`
	HostSignaled             bool   `json:"hostSignaled"`
	HelperSignaled           bool   `json:"helperSignaled"`
	CustodianSignaled        bool   `json:"custodianSignaled"`
	TargetObserved           bool   `json:"targetObserved"`
	TargetSignaled           bool   `json:"targetSignaled"`
	RetainedOriginals        int    `json:"retainedOriginals"`
	CheckpointSHA256         string `json:"checkpointSha256,omitempty"`
	EvidenceSHA256           string `json:"evidenceSha256,omitempty"`
	SourceSHA256             string `json:"sourceSha256,omitempty"`
	ProtectedSourceSHA256    string `json:"protectedSourceSha256,omitempty"`
	RootPresent              bool   `json:"rootPresent"`
	ForeignUnchanged         bool   `json:"foreignUnchanged"`
	ProtectedSourceUnchanged bool   `json:"protectedSourceUnchanged"`
	Cause                    string `json:"cause,omitempty"`
}

func f4AllowedCell(cell string) bool {
	switch cell {
	case "hang_after_fired", "watchdog_postcreate_findprocess", "crash_ready_tie", "closed_control", "silence", "bad_magic", "precreate", "postcreate_transfer", "find_process", "find_process_nil", "b1_crash", "b2_crash", "posttoken_held_cancel":
		return true
	default:
		return false
	}
}

func f4AllowedFault(cell, fault string) bool {
	switch fault {
	case "":
		return cell != "posttoken_held_cancel"
	case "observer_stop_deadline", "continue_failed":
		return cell == "posttoken_held_cancel"
	case "omit_ready", "late_started", "wrong_cell_started", "corrupt_ready", "evidence_missing", "fired_extra_barrier":
		return cell == "precreate"
	case "duplicate_fired":
		return cell == "closed_control"
	default:
		return false
	}
}

func f4NeedsBarrier(cell string) bool {
	switch cell {
	case "hang_after_fired", "crash_ready_tie", "closed_control", "silence", "bad_magic", "b1_crash", "b2_crash", "posttoken_held_cancel":
		return true
	default:
		return false
	}
}

func f4ExpectedCause(cell, fault string) (uint32, string) {
	switch cell {
	case "crash_ready_tie", "b1_crash", "b2_crash":
		return cstwatchdogwire.CauseCustodianLost, "custodian_lost"
	case "posttoken_held_cancel":
		if f4AllowedFault(cell, fault) {
			return cstwatchdogwire.CauseChildFault, fault
		}
		return cstwatchdogwire.CauseNone, ""
	case "closed_control", "bad_magic":
		return cstwatchdogwire.CauseChannelFault, "custodian_channel_fault"
	case "silence":
		return cstwatchdogwire.CauseSilent, "custodian_silent"
	case "precreate":
		return cstwatchdogwire.CauseLaunchPrecreate, "launch_precreate_failed"
	case "postcreate_transfer", "find_process", "find_process_nil":
		return cstwatchdogwire.CausePostCreateIdentity, "post_create_identity_uncertain"
	default:
		return cstwatchdogwire.CauseNone, ""
	}
}

func (s *f4RunState) closeOuterJob(v *f4Verdict) {
	if s.job == nil || s.jobCloseCount != 0 {
		return
	}
	s.jobCloseCount++
	v.JobCloses = s.jobCloseCount
	if err := s.job.Close(); err != nil {
		v.Status = "UNVERIFIED:watchdog_job_close_failed"
		v.Cause = err.Error()
	}
}

func f4WaitOriginal(handle windows.Handle, deadline time.Time) (bool, error) {
	if handle == 0 {
		return false, nil
	}
	result, err := windows.WaitForSingleObject(handle, f4WaitMilliseconds(deadline))
	if err != nil {
		return false, err
	}
	if result != windows.WAIT_OBJECT_0 {
		return false, fmt.Errorf("original wait not signaled: %d", result)
	}
	return true, nil
}

func f4Fail(v *f4Verdict, stage string, err error) {
	v.Status = "UNVERIFIED:" + stage
	v.FrozenUTC = time.Now().UTC().Format(time.RFC3339Nano)
	if err != nil {
		v.Cause = err.Error()
	}
}

func (s *f4RunState) waitNormal(deadline time.Time, v *f4Verdict) error {
	if err := windows.SetEvent(s.control.helperRelease); err != nil {
		return err
	}
	for _, row := range []struct {
		handle windows.Handle
		flag   *bool
	}{
		{s.hostOriginal, &v.HostSignaled},
		{s.helperOriginal, &v.HelperSignaled},
		{s.custodianOriginal, &v.CustodianSignaled},
	} {
		if row.handle == 0 {
			continue
		}
		signaled, err := f4WaitOriginal(row.handle, deadline)
		*row.flag = signaled
		if err != nil {
			return err
		}
	}
	for _, original := range []windows.Handle{s.hostOriginal, s.helperOriginal} {
		if original == 0 {
			return errors.New("normal cell lacks original host/helper")
		}
		var exitCode uint32
		if err := windows.GetExitCodeProcess(original, &exitCode); err != nil || exitCode != 0 {
			return fmt.Errorf("normal fake child exit=%d err=%v", exitCode, err)
		}
	}
	for {
		active, pids, err := process.CSTObserverJobCensusForTest(s.job)
		if err != nil {
			return err
		}
		if active == 0 && len(pids) == 0 {
			break
		}
		if time.Until(deadline) <= 0 {
			return fmt.Errorf("outer Job members not settled: active=%d listed=%d", active, len(pids))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if result, err := windows.WaitForSingleObject(s.control.ready, 0); err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		return fmt.Errorf("extra/late F4 ready event: result=%d err=%v", result, err)
	}
	return nil
}

func (s *f4RunState) normalPostflight(v *f4Verdict, expectedCause string) error {
	if marker, err := f4ReadMarker(s.root, s.config.Cell); err != nil || marker.State != "pending" {
		return fmt.Errorf("root marker drift: state=%q err=%v", marker.State, err)
	}
	evidence, hash, err := f4ReadEvidence(s.root, s.config.Cell, s.control.lastFlags)
	if err != nil {
		return fmt.Errorf("evidence readback: %w", err)
	}
	if err := f4ValidateSelectedEvidence(s.config.Cell, s.config.Fault, evidence); err != nil {
		return err
	}
	if evidence.Cause != expectedCause {
		return fmt.Errorf("evidence cause mismatch: %q", evidence.Cause)
	}
	v.EvidenceSHA256 = hex.EncodeToString(hash[:])
	data, sourceHash, _, err := f4ReadBoundLeaf(s.root.dir, "synthetic-source.bin", 64)
	if err != nil || string(data) != "CST observer fake source; no native project\n" {
		return fmt.Errorf("synthetic source drift: %w", err)
	}
	v.SourceSHA256 = hex.EncodeToString(sourceHash[:])
	opened, err := f4OpenRootNoFollow(s.root.Path)
	if err != nil {
		return err
	}
	identity, identityErr := f4Identity(opened, true)
	closeErr := windows.CloseHandle(opened)
	if identityErr != nil || closeErr != nil || identity != s.root.Identity {
		return fmt.Errorf("exact root identity drift: identity=%v close=%v", identityErr, closeErr)
	}
	return nil
}

func (s *f4RunState) finish(v *f4Verdict, started, t0 time.Time) {
	if v.Status == "" {
		v.Status = "UNVERIFIED:watchdog_child_lost"
	}
	if v.FrozenUTC == "" {
		v.FrozenUTC = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if s.root != nil {
		v.RootPath = s.root.Path
	}
	if s.foreignOriginal != 0 && s.source != nil {
		foreign, source, err := s.checkForeignSource(s.job != nil && s.job.Handle() != 0)
		v.ForeignUnchanged, v.ProtectedSourceUnchanged = foreign, source
		if err != nil {
			stage := "watchdog_foreign_drift"
			if foreign && !source {
				stage = "watchdog_source_drift"
			}
			v.Cause = fmt.Sprintf("%s; foreign/source pre-close: %v", v.Cause, err)
			v.Status = "UNVERIFIED:" + stage
		}
	}
	if v.Status != "FAKE_CELL_READY" {
		s.closeOuterJob(v)
	}
	deadline := time.Now().Add(f4A)
	if !t0.IsZero() {
		bounded := t0.Add(f4D + f4A)
		if bounded.Before(deadline) {
			deadline = bounded
		}
	}
	for _, row := range []struct {
		handle  windows.Handle
		flag    *bool
		cmdWait func() error
	}{
		{s.hostOriginal, &v.HostSignaled, func() error {
			if s.hostCmd != nil && s.hostCmd.Process != nil {
				return s.hostCmd.Wait()
			}
			return nil
		}},
		{s.helperOriginal, &v.HelperSignaled, func() error {
			if s.helperCmd != nil && s.helperCmd.Process != nil {
				return s.helperCmd.Wait()
			}
			return nil
		}},
		{s.custodianOriginal, &v.CustodianSignaled, nil},
	} {
		if row.handle == 0 {
			continue
		}
		signaled, waitErr := f4WaitOriginal(row.handle, deadline)
		*row.flag = signaled
		if waitErr != nil {
			v.RetainedOriginals++
			if v.Status == "FAKE_CELL_READY" {
				v.Status = "UNVERIFIED:watchdog_original_wait_unknown"
			}
			v.Cause = fmt.Sprintf("%s; original wait: %v", v.Cause, waitErr)
			continue // keep the raw handle until this one-cell parent exits
		}
		if row.cmdWait != nil {
			if err := row.cmdWait(); err != nil && v.Status == "FAKE_CELL_READY" {
				v.Status = "UNVERIFIED:watchdog_child_lost"
				v.Cause = err.Error()
			}
		}
		if err := windows.CloseHandle(row.handle); err != nil {
			v.RetainedOriginals++
			if v.Status == "FAKE_CELL_READY" {
				v.Status = "UNVERIFIED:watchdog_original_wait_unknown"
			}
			v.Cause = fmt.Sprintf("%s; original handle close: %v", v.Cause, err)
		}
	}
	if s.targetObservation != 0 {
		v.TargetObserved = true
		signaled, waitErr := f4WaitOriginal(s.targetObservation, deadline)
		v.TargetSignaled = signaled
		closeErr := windows.CloseHandle(s.targetObservation)
		s.targetObservation = 0
		if waitErr != nil || closeErr != nil {
			if v.Status == "FAKE_CELL_READY" {
				v.Status = "UNVERIFIED:watchdog_target_identity_unproved"
			}
			v.Cause = fmt.Sprintf("%s; target observation wait=%v close=%v", v.Cause, waitErr, closeErr)
		}
	}
	if v.Status == "FAKE_CELL_READY" {
		s.closeOuterJob(v)
	} else if s.jobCloseCount == 0 {
		s.closeOuterJob(v)
	}
	if s.foreignOriginal != 0 && s.source != nil {
		foreign, source, err := s.checkForeignSource(false)
		v.ForeignUnchanged = v.ForeignUnchanged && foreign
		v.ProtectedSourceUnchanged = v.ProtectedSourceUnchanged && source
		if err != nil {
			stage := "watchdog_foreign_drift"
			if foreign && !source {
				stage = "watchdog_source_drift"
			}
			v.Cause = fmt.Sprintf("%s; foreign/source post-close: %v", v.Cause, err)
			v.Status = "UNVERIFIED:" + stage
		}
	}
	if s.control != nil {
		if err := s.control.close(); err != nil {
			if v.Status == "FAKE_CELL_READY" {
				v.Status = "UNVERIFIED:watchdog_handle_transfer_failed"
			}
			v.Cause = fmt.Sprintf("%s; control close: %v", v.Cause, err)
		}
	}
	if !t0.IsZero() && time.Since(t0) > f4D+f4A+f4VerdictSlack && v.Status == "FAKE_CELL_READY" {
		v.Status = "UNVERIFIED:watchdog_deadline"
	}
	if s.root != nil {
		opened, err := f4OpenRootNoFollow(s.root.Path)
		if err == nil {
			var actual f4FileIdentity
			actual, err = f4Identity(opened, true)
			closeErr := windows.CloseHandle(opened)
			if err == nil {
				err = closeErr
			}
			v.RootPresent = err == nil && actual == s.root.Identity
		}
		if !v.RootPresent {
			v.Status = "UNVERIFIED:watchdog_root_unbound"
			v.Cause = fmt.Sprintf("%s; root postflight: %v", v.Cause, err)
		}
		state := "termination_unknown"
		if v.Status == "FAKE_CELL_READY" {
			state = "finished_fake_cell"
		}
		if err := f4SetMarkerState(s.root, s.config.Cell, state); err != nil {
			v.Status = "UNVERIFIED:watchdog_root_unbound"
			v.Cause = fmt.Sprintf("%s; root terminal marker: %v", v.Cause, err)
		}
	}
	if s.root != nil {
		if err := s.root.Close(); err != nil {
			if v.Status == "FAKE_CELL_READY" {
				v.Status = "UNVERIFIED:watchdog_root_unbound"
			}
			v.Cause = fmt.Sprintf("%s; root handle close: %v", v.Cause, err)
		}
	}
	if s.source != nil && s.source.original != 0 {
		if err := windows.CloseHandle(s.source.original); err != nil {
			v.Status = "UNVERIFIED:watchdog_source_drift"
			v.Cause = fmt.Sprintf("%s; source original close: %v", v.Cause, err)
		}
		s.source.original = 0
	}
	if s.foreignOriginal != 0 {
		if err := windows.CloseHandle(s.foreignOriginal); err != nil {
			v.Status = "UNVERIFIED:watchdog_foreign_drift"
			v.Cause = fmt.Sprintf("%s; foreign original close: %v", v.Cause, err)
		}
		s.foreignOriginal = 0
	}
	if v.Status == "FAKE_CELL_READY" {
		v.Status = "FAKE_CELL_PASS"
	}
	if !t0.IsZero() {
		v.ElapsedMS = time.Since(t0).Milliseconds()
	} else {
		v.ElapsedMS = time.Since(started).Milliseconds()
	}
}

func f4RunCell(config f4RunConfig) (verdict f4Verdict) {
	started := time.Now()
	verdict = f4Verdict{Version: "CSTF4WatchdogRunV1", Cell: config.Cell, Fault: config.Fault, BinarySHA256: config.BinarySHA256,
		StartupMS: f4Startup.Milliseconds(), DMS: f4D.Milliseconds(), AMS: f4A.Milliseconds()}
	if !f4AllowedCell(config.Cell) || !f4AllowedFault(config.Cell, config.Fault) {
		f4Fail(&verdict, "watchdog_root_unbound", errors.New("unapproved exact cell/fault pair"))
		return verdict
	}
	if err := f4ValidateBinary(config.Binary, config.BinarySHA256); err != nil {
		f4Fail(&verdict, "watchdog_root_unbound", err)
		return verdict
	}
	state := &f4RunState{config: config}
	var t0 time.Time
	defer func() { state.finish(&verdict, started, t0) }()
	if err := state.admitForeignAndSource(); err != nil {
		f4Fail(&verdict, "watchdog_foreign_or_source_unadmitted", err)
		return verdict
	}
	verdict.ProtectedSourceSHA256 = hex.EncodeToString(state.source.hash[:])
	root, err := f4CreateBoundRoot(config.RootParent, config.Cell)
	state.root = root
	if err != nil {
		f4Fail(&verdict, "watchdog_root_unbound", err)
		return verdict
	}
	verdict.RootPath = root.Path
	rootDigest := sha256.Sum256(root.Token[:])
	verdict.RootTokenDigest = hex.EncodeToString(rootDigest[:16])
	state.job, err = f4NewOuterJob()
	if err != nil {
		f4Fail(&verdict, "watchdog_member_unproved", err)
		return verdict
	}
	if _, err := rand.Read(state.nonce[:]); err != nil {
		f4Fail(&verdict, "watchdog_root_unbound", err)
		return verdict
	}
	binding := cstwatchdogwire.NewBinding(state.nonce, config.Cell, root.Token)
	state.control, err = f4NewControl(binding)
	if err != nil {
		f4Fail(&verdict, "watchdog_handle_transfer_failed", err)
		return verdict
	}
	startupStart := time.Now()
	startupDeadline := startupStart.Add(f4Startup)
	if err := state.launchHelper(); err != nil {
		f4Fail(&verdict, "watchdog_startup_unready", err)
		return verdict
	}
	if time.Until(startupDeadline) <= 0 {
		f4Fail(&verdict, "watchdog_startup_unready", errors.New("helper exceeded startup admission bound"))
		return verdict
	}
	hostErr := state.launchHost()
	if config.Cell == "watchdog_postcreate_findprocess" {
		if !errors.Is(hostErr, process.ErrSpawnPostCreate) || state.hostOriginal == 0 || state.job.Handle() == 0 {
			f4Fail(&verdict, "watchdog_handle_transfer_failed", fmt.Errorf("second-launch fault was not post-create with a live parent Job: %w", hostErr))
			return verdict
		}
		if err := f4OriginalAlive(state.helperOriginal); err != nil {
			f4Fail(&verdict, "watchdog_member_unproved", err)
			return verdict
		}
		member, _, pids, err := process.CSTObserverOriginalMemberForTest(state.job, state.helperOriginal)
		if err != nil || !member {
			f4Fail(&verdict, "watchdog_member_unproved", fmt.Errorf("helper no longer in valid parent Job: members=%d err=%v", len(pids), err))
			return verdict
		}
		f4Fail(&verdict, "watchdog_postcreate_findprocess_failed", hostErr)
		return verdict
	}
	if hostErr != nil {
		f4Fail(&verdict, "watchdog_startup_unready", hostErr)
		return verdict
	}
	if time.Until(startupDeadline) <= 0 {
		f4Fail(&verdict, "watchdog_startup_unready", errors.New("host exceeded startup admission bound"))
		return verdict
	}
	if err := state.classifyMembers(); err != nil {
		f4Fail(&verdict, "watchdog_member_unproved", err)
		return verdict
	}
	if _, _, err := state.checkForeignSource(true); err != nil {
		f4Fail(&verdict, "watchdog_foreign_or_source_unproved", err)
		return verdict
	}
	frame, err := state.control.poll(state.hostOriginal, startupDeadline)
	if err != nil {
		f4Fail(&verdict, "watchdog_startup_unready", err)
		return verdict
	}
	if err := state.control.validateNext(frame, f4NeedsBarrier(config.Cell)); err != nil || frame.Phase != cstwatchdogwire.Ready {
		f4Fail(&verdict, "watchdog_receipt_invalid", errors.Join(err, errors.New("first F4 phase was not READY")))
		return verdict
	}
	if marker, err := f4ReadMarker(root, config.Cell); err != nil || marker.State != "pending" {
		f4Fail(&verdict, "watchdog_root_unbound", fmt.Errorf("root marker state=%q err=%v", marker.State, err))
		return verdict
	}
	if err := f4OriginalAlive(state.hostOriginal); err != nil {
		f4Fail(&verdict, "watchdog_child_lost", err)
		return verdict
	}
	t0 = time.Now() // monotonic D anchor, immediately before START acknowledgment
	verdict.T0UTC = t0.UTC().Format(time.RFC3339Nano)
	if err := state.control.accept(frame); err != nil {
		f4Fail(&verdict, "watchdog_control_wait_failed", err)
		return verdict
	}
	for state.control.lastPhase != cstwatchdogwire.Finished {
		if time.Since(t0) > f4D+f4A+f4VerdictSlack {
			f4Fail(&verdict, "watchdog_deadline", errors.New("external F4 verdict cutoff exceeded"))
			return verdict
		}
		phaseDeadline := t0.Add(f4D)
		if state.control.lastPhase == cstwatchdogwire.Fired && config.Cell != "hang_after_fired" {
			phaseDeadline = t0.Add(f4D + f4A)
		}
		frame, err = state.control.poll(state.hostOriginal, phaseDeadline)
		if err != nil {
			stage := "watchdog_receipt_invalid"
			if errors.Is(err, errF4WaitDeadline) {
				stage = "watchdog_deadline"
			} else if errors.Is(err, errF4HostExited) {
				stage = "watchdog_child_lost"
			}
			f4Fail(&verdict, stage, err)
			return verdict
		}
		if err := state.control.validateNext(frame, f4NeedsBarrier(config.Cell)); err != nil {
			f4Fail(&verdict, "watchdog_receipt_invalid", err)
			return verdict
		}
		if frame.Phase == cstwatchdogwire.Barrier {
			if err := state.captureCustodian(frame.CustodianHandle); err != nil {
				f4Fail(&verdict, "watchdog_handle_transfer_failed", err)
				return verdict
			}
			if config.Cell == "b1_crash" || config.Cell == "b2_crash" || config.Cell == "posttoken_held_cancel" {
				if marker, err := f4ReadMarker(state.root, config.Cell); err != nil || marker.State != "pending" {
					f4Fail(&verdict, "watchdog_precrash_observation_unproved", fmt.Errorf("checkpoint marker drift: state=%q err=%v", marker.State, err))
					return verdict
				}
				checkpoint, hash, err := f4ReadTask2Checkpoint(state.root, config.Cell, frame.Flags)
				if err != nil {
					f4Fail(&verdict, "watchdog_precrash_observation_unproved", err)
					return verdict
				}
				verdict.CheckpointSHA256 = hex.EncodeToString(hash[:])
				if err := state.captureTargetObservation(checkpoint.Task2.Target); err != nil {
					f4Fail(&verdict, "watchdog_target_identity_unproved", err)
					return verdict
				}
			}
		}
		if frame.Phase == cstwatchdogwire.Finished {
			expected, _ := f4ExpectedCause(config.Cell, config.Fault)
			if config.Cell == "hang_after_fired" || frame.Cause != expected {
				f4Fail(&verdict, "watchdog_receipt_invalid", errors.New("unexpected finished cause or hang completion"))
				return verdict
			}
		}
		if err := state.control.accept(frame); err != nil {
			f4Fail(&verdict, "watchdog_control_wait_failed", err)
			return verdict
		}
		if frame.Flags&cstwatchdogwire.Attempted != 0 {
			verdict.Attempted = 1
		}
		if frame.Flags&cstwatchdogwire.BarrierSeen != 0 {
			verdict.Barrier = 1
		}
		if frame.Flags&cstwatchdogwire.InjectionFired != 0 {
			verdict.Fired = 1
		}
		if frame.Flags&cstwatchdogwire.FinishedSeen != 0 {
			verdict.Finished = 1
		}
		if frame.Phase == cstwatchdogwire.Fault {
			f4Fail(&verdict, "watchdog_child_lost", errors.New("child published FAULT phase"))
			return verdict
		}
	}
	if verdict.Attempted != 1 || verdict.Fired != 1 || verdict.Finished != 1 || (f4NeedsBarrier(config.Cell) && verdict.Barrier != 1) {
		f4Fail(&verdict, "watchdog_receipt_invalid", errors.New("required accepted phase counter absent"))
		return verdict
	}
	if err := state.waitNormal(t0.Add(f4D+f4A), &verdict); err != nil {
		f4Fail(&verdict, "watchdog_original_wait_unknown", err)
		return verdict
	}
	_, expectedCause := f4ExpectedCause(config.Cell, config.Fault)
	if err := state.normalPostflight(&verdict, expectedCause); err != nil {
		f4Fail(&verdict, "watchdog_evidence_unknown", err)
		return verdict
	}
	verdict.Status = "FAKE_CELL_READY" // final Job close still decides PASS
	verdict.FrozenUTC = time.Now().UTC().Format(time.RFC3339Nano)
	return verdict
}

func main() {
	binary := flag.String("binary", "", "absolute hash-checked tagged e2e test binary")
	binaryHash := flag.String("binary-sha256", "", "SHA-256 of the exact binary")
	cell := flag.String("cell", "", "one exact Task 1 B0 fake cell")
	fault := flag.String("fault", "", "one allowlisted tagged receipt/evidence fault")
	rootParent := flag.String("root-parent", "", "absolute task-owned ignored root parent")
	foreignPID := flag.Uint("foreign-pid", 0, "exact live unrelated process PID for read-only original-handle census")
	sourcePath := flag.String("source-path", "", "absolute read-only protected source path")
	sourceHash := flag.String("source-sha256", "", "SHA-256 of the protected source bytes")
	flag.Parse()
	verdict := f4RunCell(f4RunConfig{Binary: *binary, BinarySHA256: *binaryHash, Cell: *cell, Fault: *fault, RootParent: *rootParent,
		ForeignPID: uint32(*foreignPID), SourcePath: *sourcePath, SourceSHA256: *sourceHash})
	encoded, err := json.Marshal(verdict)
	if err != nil {
		fmt.Fprintln(os.Stderr, "F4 parent verdict encoding failed")
		os.Exit(3)
	}
	fmt.Println(string(encoded))
	if verdict.Status != "FAKE_CELL_PASS" {
		os.Exit(2)
	}
}
