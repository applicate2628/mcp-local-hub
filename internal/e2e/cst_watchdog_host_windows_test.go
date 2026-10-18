//go:build windows && cst_vptr_probe

package e2e

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/e2e/cstwatchdogwire"
)

type f4TestHost struct {
	cell       string
	fault      string
	root       string
	token      [16]byte
	binding    cstwatchdogwire.Binding
	rootHandle windows.Handle
	mapping    windows.Handle
	view       uintptr
	ready      windows.Handle
	ack        windows.Handle
	sequence   uint64
}

func f4Parse16(value string) ([16]byte, error) {
	var result [16]byte
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(result) {
		return result, errors.New("invalid 128-bit F4 input")
	}
	copy(result[:], decoded)
	return result, nil
}

// f4BindTestHost verifies the parent's exact no-follow root before any cell
// work. The inherited handles are only the separate 96-byte status channel.
func f4BindTestHost(cell string) (*f4TestHost, error) {
	root := os.Getenv("MCPHUB_CST_F4_ROOT")
	if root == "" {
		return nil, errors.New("F4 external parent root/clock absent; no watchdog or ownership proof")
	}
	if os.Getenv("MCPHUB_CST_F4_D_MS") != strconv.Itoa(int(cstFakeObservationDeadline/time.Millisecond)) || os.Getenv("MCPHUB_CST_F4_A_MS") != strconv.Itoa(int(cstFakeCloseAllowance/time.Millisecond)) {
		return nil, errors.New("F4 parent/child observation bounds disagree")
	}
	token, err := f4Parse16(os.Getenv("MCPHUB_CST_F4_ROOT_TOKEN"))
	if err != nil {
		return nil, err
	}
	nonce, err := f4Parse16(os.Getenv("MCPHUB_CST_F4_NONCE"))
	if err != nil {
		return nil, err
	}
	rootHandle, err := cstFingerprintOpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open bound F4 root: %w", err)
	}
	closeRoot := true
	defer func() {
		if closeRoot {
			_ = windows.CloseHandle(rootHandle)
		}
	}()
	info, err := cstFingerprintHandleInfo(rootHandle, true)
	if err != nil {
		return nil, err
	}
	identity := cstwatchdogwire.FileIdentity{Volume: info.VolumeSerialNumber, IndexHigh: info.FileIndexHigh, IndexLow: info.FileIndexLow}
	markerHandle, err := cstFingerprintOpenRelative(rootHandle, cstwatchdogwire.MarkerName, false)
	if err != nil {
		return nil, fmt.Errorf("open bound F4 marker: %w", err)
	}
	if _, err := cstFingerprintHandleInfo(markerHandle, false); err != nil {
		_ = windows.CloseHandle(markerHandle)
		return nil, err
	}
	var markerBytes [2049]byte
	var read uint32
	readErr := windows.ReadFile(markerHandle, markerBytes[:], &read, nil)
	closeErr := windows.CloseHandle(markerHandle)
	if readErr != nil || closeErr != nil || read == 0 || read > 2048 {
		return nil, fmt.Errorf("F4 marker read/close/size: read=%d readErr=%v closeErr=%v", read, readErr, closeErr)
	}
	var marker cstwatchdogwire.RootMarker
	if err := json.Unmarshal(markerBytes[:read], &marker); err != nil {
		return nil, err
	}
	if marker.Version != cstwatchdogwire.RootMarkerVersion || marker.State != "pending" || marker.Cell != cell || marker.Token != hex.EncodeToString(token[:]) || marker.Identity != identity {
		return nil, errors.New("F4 root marker/token/file identity mismatch")
	}
	parts := strings.Split(os.Getenv("MCPHUB_CST_F4_HANDLES"), ",")
	if len(parts) != 3 {
		return nil, errors.New("F4 handle allowlist missing")
	}
	var handles [3]windows.Handle
	for i, part := range parts {
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil || n == 0 {
			return nil, errors.New("invalid F4 inherited handle")
		}
		handles[i] = windows.Handle(n)
	}
	view, err := windows.MapViewOfFile(handles[0], windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, cstwatchdogwire.Size)
	if err != nil {
		return nil, err
	}
	closeRoot = false
	return &f4TestHost{cell: cell, fault: os.Getenv("MCPHUB_CST_F4_FAULT"), root: root, token: token, binding: cstwatchdogwire.NewBinding(nonce, cell, token), rootHandle: rootHandle,
		mapping: handles[0], view: view, ready: handles[1], ack: handles[2]}, nil
}

func (h *f4TestHost) publish(phase, flags uint32, original windows.Handle, cause uint32) error {
	if h == nil {
		return errors.New("missing F4 host channel")
	}
	h.sequence++
	frame := cstwatchdogwire.Frame{Binding: h.binding, Sequence: h.sequence, Phase: phase, Flags: flags, CustodianHandle: uint64(original), Cause: cause}
	if phase == cstwatchdogwire.Started && h.fault == "wrong_cell_started" {
		frame.Binding = cstwatchdogwire.NewBinding(h.binding.Nonce, "wrong-cell", h.token)
	}
	raw, err := cstwatchdogwire.Encode(frame)
	if err != nil {
		return err
	}
	if phase == cstwatchdogwire.Ready && h.fault == "corrupt_ready" {
		raw[92] ^= 1 // malformed CRC; the wrong-cell case remains CRC-valid
	}
	if phase == cstwatchdogwire.Ready && h.fault == "omit_ready" {
		_, err := windows.WaitForSingleObject(h.ack, windows.INFINITE)
		return fmt.Errorf("omitted READY unexpectedly released: %v", err)
	}
	var written uintptr
	if err := windows.WriteProcessMemory(windows.CurrentProcess(), h.view, &raw[0], uintptr(len(raw)), &written); err != nil || written != uintptr(len(raw)) {
		return fmt.Errorf("F4 cell write: n=%d: %w", written, err)
	}
	if err := windows.SetEvent(h.ready); err != nil {
		return err
	}
	result, err := windows.WaitForSingleObject(h.ack, 2000)
	if err != nil || result != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("F4 parent acknowledgment absent: result=%d err=%v", result, err)
	}
	return nil
}

func (h *f4TestHost) fired(barrier bool) error {
	flags := cstwatchdogwire.Attempted | cstwatchdogwire.InjectionFired
	if barrier || h.fault == "fired_extra_barrier" {
		flags |= cstwatchdogwire.BarrierSeen
	}
	if err := h.publish(cstwatchdogwire.Fired, flags, 0, cstwatchdogwire.CauseNone); err != nil {
		return err
	}
	if h.fault == "duplicate_fired" {
		return h.publish(cstwatchdogwire.Fired, flags, 0, cstwatchdogwire.CauseNone)
	}
	return nil
}

func f4CauseForB0(cause string) uint32 {
	switch cause {
	case "custodian_lost":
		return cstwatchdogwire.CauseCustodianLost
	case "custodian_channel_fault":
		return cstwatchdogwire.CauseChannelFault
	case "custodian_silent":
		return cstwatchdogwire.CauseSilent
	case "launch_precreate_failed":
		return cstwatchdogwire.CauseLaunchPrecreate
	case "post_create_identity_uncertain":
		return cstwatchdogwire.CausePostCreateIdentity
	default:
		return cstwatchdogwire.CauseChildFault
	}
}

func (h *f4TestHost) close() error {
	if h == nil {
		return nil
	}
	var problems []error
	if h.view != 0 {
		problems = append(problems, windows.UnmapViewOfFile(h.view))
		h.view = 0
	}
	for _, handle := range []windows.Handle{h.mapping, h.ready, h.ack, h.rootHandle} {
		if handle != 0 {
			problems = append(problems, windows.CloseHandle(handle))
		}
	}
	h.mapping, h.ready, h.ack, h.rootHandle = 0, 0, 0, 0
	return errors.Join(problems...)
}

type f4ChildEvidence = cstwatchdogwire.Evidence

func (h *f4TestHost) writeEvidence(observation f4ChildEvidence) error {
	observation.Version = cstwatchdogwire.EvidenceVersion
	observation.Cell = h.cell
	observation.RootToken = hex.EncodeToString(h.token[:])
	bytes, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	if len(bytes) > 64*1024 {
		return errors.New("F4 child evidence exceeds 64 KiB")
	}
	name, err := windows.NewNTUnicodeString(cstwatchdogwire.EvidenceName)
	if err != nil {
		return err
	}
	oa := &windows.OBJECT_ATTRIBUTES{RootDirectory: h.rootHandle, ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	var iosb windows.IO_STATUS_BLOCK
	var allocation int64
	var handle windows.Handle
	if err := windows.NtCreateFile(&handle, windows.FILE_WRITE_DATA|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, oa, &iosb, &allocation,
		windows.FILE_ATTRIBUTE_NORMAL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OVERWRITE,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0); err != nil {
		return err
	}
	if _, err := cstFingerprintHandleInfo(handle, false); err != nil {
		_ = windows.CloseHandle(handle)
		return err
	}
	var written uint32
	writeErr := windows.WriteFile(handle, bytes, &written, nil)
	flushErr := windows.FlushFileBuffers(handle)
	closeErr := windows.CloseHandle(handle)
	if writeErr != nil || flushErr != nil || closeErr != nil || int(written) != len(bytes) {
		return fmt.Errorf("F4 child evidence write/flush/close: n=%d want=%d write=%v flush=%v close=%v", written, len(bytes), writeErr, flushErr, closeErr)
	}
	return nil
}
