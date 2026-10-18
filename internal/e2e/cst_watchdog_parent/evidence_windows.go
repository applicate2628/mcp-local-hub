//go:build windows && cst_vptr_probe

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unsafe"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/e2e/cstwatchdogwire"
)

func f4OpenBoundLeaf(root windows.Handle, leaf string, write bool) (windows.Handle, error) {
	name, err := windows.NewNTUnicodeString(leaf)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{RootDirectory: root, ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	access := uint32(windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
	disposition := uint32(windows.FILE_OPEN)
	if write {
		access = windows.FILE_WRITE_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
		disposition = windows.FILE_OVERWRITE
	}
	var iosb windows.IO_STATUS_BLOCK
	var allocation int64
	var handle windows.Handle
	if err := windows.NtCreateFile(&handle, access, oa, &iosb, &allocation, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, disposition,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0); err != nil {
		return 0, err
	}
	if _, err := f4Identity(handle, false); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	return handle, nil
}

func f4ReadBoundLeaf(root windows.Handle, leaf string, maxBytes int) ([]byte, [32]byte, f4FileIdentity, error) {
	var digest [32]byte
	var identity f4FileIdentity
	if maxBytes <= 0 || maxBytes > 64*1024 {
		return nil, digest, identity, errors.New("invalid bounded evidence limit")
	}
	handle, err := f4OpenBoundLeaf(root, leaf, false)
	if err != nil {
		return nil, digest, identity, err
	}
	defer windows.CloseHandle(handle)
	identity, err = f4Identity(handle, false)
	if err != nil {
		return nil, digest, identity, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return nil, digest, identity, err
	}
	size := uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)
	if size > uint64(maxBytes) {
		return nil, digest, identity, fmt.Errorf("bounded evidence too large: %d", size)
	}
	buffer := make([]byte, maxBytes+1)
	var count uint32
	if err := windows.ReadFile(handle, buffer, &count, nil); err != nil && !errors.Is(err, io.EOF) {
		return nil, digest, identity, err
	}
	if count > uint32(maxBytes) || uint64(count) != size {
		return nil, digest, identity, fmt.Errorf("bounded evidence size drift: read=%d size=%d", count, size)
	}
	if checked, err := f4Identity(handle, false); err != nil || checked != identity {
		return nil, digest, identity, fmt.Errorf("bounded leaf identity drift: %w", err)
	}
	retained := append([]byte(nil), buffer[:count]...)
	digest = sha256.Sum256(retained)
	return retained, digest, identity, nil
}

func f4ReadMarker(root *f4OwnedRoot, cell string) (cstwatchdogwire.RootMarker, error) {
	var marker cstwatchdogwire.RootMarker
	data, _, _, err := f4ReadBoundLeaf(root.dir, cstwatchdogwire.MarkerName, 2048)
	if err != nil {
		return marker, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return marker, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return marker, errors.New("trailing root marker data")
	}
	if marker.Version != cstwatchdogwire.RootMarkerVersion || marker.Cell != cell || marker.Token != hex.EncodeToString(root.Token[:]) || marker.Identity != root.Identity {
		return marker, errors.New("root marker binding mismatch")
	}
	return marker, nil
}

func f4SetMarkerState(root *f4OwnedRoot, cell, state string) error {
	if state != "termination_unknown" && state != "finished_fake_cell" {
		return errors.New("invalid F4 root state")
	}
	marker, err := f4ReadMarker(root, cell)
	if err != nil {
		return err
	}
	if marker.State != "pending" {
		return errors.New("F4 marker already terminal")
	}
	marker.State = state
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	handle, err := f4OpenBoundLeaf(root.dir, cstwatchdogwire.MarkerName, true)
	if err != nil {
		return err
	}
	var written uint32
	writeErr := windows.WriteFile(handle, data, &written, nil)
	flushErr := windows.FlushFileBuffers(handle)
	closeErr := windows.CloseHandle(handle)
	if writeErr != nil || flushErr != nil || closeErr != nil || int(written) != len(data) {
		return fmt.Errorf("F4 marker update: n=%d want=%d write=%v flush=%v close=%v", written, len(data), writeErr, flushErr, closeErr)
	}
	return nil
}

func f4ReadEvidence(root *f4OwnedRoot, cell string, lastFlags uint32) (cstwatchdogwire.Evidence, [32]byte, error) {
	var evidence cstwatchdogwire.Evidence
	data, digest, identity, err := f4ReadBoundLeaf(root.dir, cstwatchdogwire.EvidenceName, 64*1024)
	if err != nil {
		return evidence, digest, err
	}
	if identity != root.EvidenceIdentity {
		return evidence, digest, errors.New("registered evidence leaf identity drift")
	}
	evidence, err = cstwatchdogwire.DecodeEvidence(data)
	if err != nil {
		return cstwatchdogwire.Evidence{}, digest, err
	}
	barrier := 0
	if lastFlags&cstwatchdogwire.BarrierSeen != 0 {
		barrier = 1
	}
	if evidence.Version != cstwatchdogwire.EvidenceVersion || evidence.Cell != cell || evidence.RootToken != hex.EncodeToString(root.Token[:]) || evidence.Attempted != 1 || evidence.Barrier != barrier || evidence.Fired != 1 || evidence.Finished != 1 || evidence.Cause == "" {
		return evidence, digest, errors.New("child evidence binding or accepted-phase mismatch")
	}
	return evidence, digest, nil
}

// f4ReadTask2Checkpoint accepts a partial host-authored observation only at
// the acknowledged F4 BARRIER candidate. Target identity and outer-Job
// membership are a separate parent observation before the actual ACK.
func f4ReadTask2Checkpoint(root *f4OwnedRoot, cell string, flags uint32) (cstwatchdogwire.Evidence, [32]byte, error) {
	var evidence cstwatchdogwire.Evidence
	data, digest, identity, err := f4ReadBoundLeaf(root.dir, cstwatchdogwire.EvidenceName, 64*1024)
	if err != nil {
		return evidence, digest, err
	}
	if identity != root.EvidenceIdentity {
		return evidence, digest, errors.New("registered checkpoint leaf identity drift")
	}
	evidence, err = cstwatchdogwire.DecodeEvidence(data)
	if err != nil {
		return cstwatchdogwire.Evidence{}, digest, err
	}
	if flags != cstwatchdogwire.Attempted|cstwatchdogwire.BarrierSeen ||
		evidence.Cell != cell || evidence.RootToken != hex.EncodeToString(root.Token[:]) ||
		evidence.Attempted != 1 || evidence.Barrier != 1 || evidence.Fired != 0 || evidence.Finished != 0 ||
		evidence.Cause != "barrier_checkpoint" {
		return evidence, digest, errors.New("Task 2 checkpoint binding or partial phase mismatch")
	}
	if cell == "posttoken_held_cancel" {
		if evidence.Task2 == nil || evidence.Task2.F3Sequence != 2 || evidence.Task2.ObserverState != "unsettled_owned" ||
			evidence.Task2.Debugger == nil || *evidence.Task2.Debugger.HeldEvents != 1 || !*evidence.Task2.Debugger.OwnerBound ||
			evidence.Task2.Debugger.Continued == "pass" || evidence.Task2.Debugger.Detached == "pass" || evidence.Task2.Debugger.StopAck == "pass" {
			return evidence, digest, errors.New("post-token checkpoint lacks live held-event attestation")
		}
	}
	if err := f4ValidateSelectedEvidence(cell, "", evidence); err != nil {
		return evidence, digest, err
	}
	return evidence, digest, nil
}

// f4ValidateSelectedEvidence is the parent's sole selected-cell semantic
// gate. Syntax remains with the shared wire leaf; no new Task 2 cell is
// runnable through f4AllowedCell until its process/Job oracle exists.
func f4ValidateSelectedEvidence(cell, fault string, evidence cstwatchdogwire.Evidence) error {
	task2 := evidence.Task2
	switch cell {
	case "b1_crash":
		if task2 == nil || task2.Barrier != "B1" || task2.ObserverState != "none" || task2.Debugger != nil ||
			!f4Task2TargetProved(task2.Target) || task2.Settlement.SettledExit == "pass" {
			return errors.New("B1 target/barrier evidence unproved")
		}
	case "b2_crash":
		if task2 == nil || task2.Barrier != "B2" || task2.ObserverState != "none" || !f4Task2TargetProved(task2.Target) ||
			task2.Debugger == nil || *task2.Debugger.HeldEvents != 1 || !*task2.Debugger.OwnerBound ||
			task2.Debugger.Continued == "pass" || task2.Debugger.Detached == "pass" || task2.Debugger.StopAck == "pass" ||
			task2.Settlement.SettledExit == "pass" {
			return errors.New("B2 held-event/target evidence unproved")
		}
	case "posttoken_held_cancel":
		if task2 == nil || task2.Barrier != "B2" || task2.ObserverState != "unsettled_owned" || !f4Task2TargetProved(task2.Target) ||
			task2.Debugger == nil || !*task2.Debugger.OwnerBound || task2.Settlement.SettledExit == "pass" {
			return errors.New("post-token custody evidence unproved")
		}
		if fault != "" {
			debugger, settlement := task2.Debugger, task2.Settlement
			if *debugger.HeldEvents != 1 || debugger.Detached != "unknown" || debugger.StopAck != "unknown" ||
				settlement.TargetJobClose != "unknown" || settlement.ChildWait != "unknown" || settlement.StdinJoin != "unknown" ||
				settlement.StdoutEOF != "unknown" || settlement.StderrEOF != "unknown" || settlement.SettledExit != "unknown" {
				return errors.New("post-token fault claimed held-event or settlement facts without proof")
			}
			switch fault {
			case "observer_stop_deadline":
				if debugger.Continued != "unknown" {
					return errors.New("post-token stop deadline continued held event")
				}
			case "continue_failed":
				if debugger.Continued != "fail" {
					return errors.New("post-token Continue failure lacks failed attempt")
				}
			default:
				return errors.New("unimplemented post-token terminal fault")
			}
		}
	case "positive_settled":
		if task2 == nil || task2.Barrier != "B2" || task2.ObserverState != "settled" || !f4Task2TargetProved(task2.Target) ||
			task2.Debugger == nil || *task2.Debugger.HeldEvents != 0 || !*task2.Debugger.OwnerBound || task2.Debugger.StopAck != "pass" ||
			task2.Settlement.TargetJobClose != "pass" || task2.Settlement.ChildWait != "pass" || task2.Settlement.StdinJoin != "pass" ||
			task2.Settlement.StdoutEOF != "pass" || task2.Settlement.StderrEOF != "pass" || task2.Settlement.SettledExit != "pass" {
			return errors.New("positive target/debugger/stream settlement unproved")
		}
	case "host_crash_b0", "host_crash_b1", "host_crash_b2", "host_crash_posttoken":
		return errors.New("host crash cannot enter normal terminal postflight")
	default:
		if !f4AllowedCell(cell) || task2 != nil {
			return errors.New("unapproved or Task 2-bearing B0 evidence")
		}
	}
	return nil
}

func f4Task2TargetProved(target *cstwatchdogwire.Task2Target) bool {
	return target != nil && target.OriginalRetained != nil && *target.OriginalRetained &&
		target.StartClosed != nil && *target.StartClosed && target.InnerMember != nil && *target.InnerMember
}
