//go:build windows && cst_vptr_probe

// Package cstwatchdogwire owns only the private tagged F4 parent/host cell.
package cstwatchdogwire

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"strings"
	"unicode/utf8"
)

const Size = 96
const Magic = "CSTF4W01"
const MarkerName = "watchdog-pending.json"
const EvidenceName = "watchdog-evidence.json"
const RootMarkerVersion = "CSTF4RootV1"
const EvidenceVersion = "CSTF4WatchdogEvidenceV1"

type FileIdentity struct {
	Volume    uint32 `json:"volume"`
	IndexHigh uint32 `json:"indexHigh"`
	IndexLow  uint32 `json:"indexLow"`
}

type RootMarker struct {
	Version  string       `json:"version"`
	Cell     string       `json:"cell"`
	Token    string       `json:"token"`
	Identity FileIdentity `json:"identity"`
	State    string       `json:"state"`
}

type Evidence struct {
	Version   string         `json:"version"`
	Cell      string         `json:"cell"`
	RootToken string         `json:"rootToken"`
	Attempted int            `json:"attempted"`
	Barrier   int            `json:"barrier"`
	Fired     int            `json:"fired"`
	Finished  int            `json:"finished"`
	Cause     string         `json:"cause"`
	Task2     *Task2Evidence `json:"task2,omitempty"`
}

// Task2Evidence is a child observation, not the parent's Job, deadline or
// settlement verdict. A nil Task2 field preserves the exact B0 JSON shape.
type Task2Evidence struct {
	Barrier       string           `json:"barrier"`
	ObserverState string           `json:"observerState"`
	F3NonceDigest string           `json:"f3NonceDigest,omitempty"`
	F3Sequence    uint64           `json:"f3Sequence,omitempty"`
	SlotSHA256    string           `json:"slotSha256,omitempty"`
	Target        *Task2Target     `json:"target"`
	Debugger      *Task2Debugger   `json:"debugger,omitempty"`
	Settlement    *Task2Settlement `json:"settlement"`
}

type Task2Target struct {
	PID              uint32 `json:"pid"`
	CreationTicks    uint64 `json:"creationTicks"`
	ImageSHA256      string `json:"imageSha256"`
	OriginalRetained *bool  `json:"originalRetained"`
	StartClosed      *bool  `json:"startClosed"`
	InnerMember      *bool  `json:"innerMember"`
}

type Task2Debugger struct {
	HeldEvents *int   `json:"heldEvents"`
	OwnerBound *bool  `json:"ownerBound"`
	Continued  string `json:"continued"`
	Detached   string `json:"detached"`
	StopAck    string `json:"stopAck"`
}

type Task2Settlement struct {
	TargetJobClose string `json:"targetJobClose"`
	ChildWait      string `json:"childWait"`
	StdinJoin      string `json:"stdinJoin"`
	StdoutEOF      string `json:"stdoutEOF"`
	StderrEOF      string `json:"stderrEOF"`
	SettledExit    string `json:"settledExit"`
}

// DecodeEvidence owns only strict V1 syntax and field presence. The parent
// alone decides which observations a selected scenario requires.
func DecodeEvidence(data []byte) (Evidence, error) {
	var evidence Evidence
	if len(data) == 0 || len(data) > 64*1024 || !utf8.Valid(data) {
		return evidence, errors.New("invalid evidence size or UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&evidence); err != nil {
		return Evidence{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return Evidence{}, errors.New("trailing child evidence data")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Evidence{}, err
	}
	for _, name := range []string{"version", "cell", "rootToken", "attempted", "barrier", "fired", "finished", "cause"} {
		value, present := fields[name]
		if !present || bytes.Equal(value, []byte("null")) {
			return Evidence{}, fmt.Errorf("missing evidence field %s", name)
		}
	}
	if evidence.Version != EvidenceVersion || evidence.Cell == "" || !canonicalHex(evidence.RootToken, 32) || evidence.Cause == "" ||
		(evidence.Attempted != 0 && evidence.Attempted != 1) || (evidence.Barrier != 0 && evidence.Barrier != 1) ||
		(evidence.Fired != 0 && evidence.Fired != 1) || (evidence.Finished != 0 && evidence.Finished != 1) {
		return Evidence{}, errors.New("invalid evidence envelope")
	}
	if raw, present := fields["task2"]; present {
		if bytes.Equal(raw, []byte("null")) || evidence.Task2 == nil {
			return Evidence{}, errors.New("null Task 2 evidence")
		}
		parsed, err := decodeTask2Value(raw, true)
		if err != nil {
			return Evidence{}, err
		}
		evidence.Task2 = &parsed
	}
	return evidence, nil
}

func canonicalHex(value string, digits int) bool {
	if len(value) != digits || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func task2Outcome(value string) bool {
	switch value {
	case "pass", "fail", "unknown", "not-applicable":
		return true
	default:
		return false
	}
}

func validTask2(value *Task2Evidence, requireCorrelation bool) error {
	if value == nil || (value.Barrier != "B1" && value.Barrier != "B2") ||
		(value.ObserverState != "none" && value.ObserverState != "unsettled_owned" && value.ObserverState != "settled" && value.ObserverState != "unknown") ||
		value.Target == nil || value.Settlement == nil {
		return errors.New("invalid Task 2 observation envelope")
	}
	if requireCorrelation {
		if !canonicalHex(value.F3NonceDigest, 64) || value.F3Sequence == 0 || !canonicalHex(value.SlotSHA256, 64) {
			return errors.New("missing Task 2 host correlation")
		}
	} else if value.F3NonceDigest != "" || value.F3Sequence != 0 || value.SlotSHA256 != "" {
		return errors.New("host correlation cannot be child-authored in slot")
	}
	target := value.Target
	if target.PID == 0 || target.CreationTicks == 0 || !canonicalHex(target.ImageSHA256, 64) ||
		target.OriginalRetained == nil || target.StartClosed == nil || target.InnerMember == nil {
		return errors.New("missing Task 2 target identity or state")
	}
	if value.Debugger != nil {
		debugger := value.Debugger
		if debugger.HeldEvents == nil || *debugger.HeldEvents < 0 || *debugger.HeldEvents > 1 || debugger.OwnerBound == nil ||
			!task2Outcome(debugger.Continued) || !task2Outcome(debugger.Detached) || !task2Outcome(debugger.StopAck) {
			return errors.New("invalid Task 2 debugger observation")
		}
	}
	settlement := value.Settlement
	for _, outcome := range []string{settlement.TargetJobClose, settlement.ChildWait, settlement.StdinJoin,
		settlement.StdoutEOF, settlement.StderrEOF, settlement.SettledExit} {
		if !task2Outcome(outcome) {
			return errors.New("invalid Task 2 settlement observation")
		}
	}
	return nil
}

const (
	Ready uint32 = 1 + iota
	Started
	Barrier
	Fired
	Finished
	Fault
)

const (
	Attempted uint32 = 1 << iota
	BarrierSeen
	InjectionFired
	FinishedSeen
)

const (
	CauseNone uint32 = iota
	CauseCustodianLost
	CauseChannelFault
	CauseSilent
	CauseLaunchPrecreate
	CausePostCreateIdentity
	CauseRootUnbound
	CauseChildFault
)

var checksumTable = crc32.MakeTable(crc32.Castagnoli)

type Binding struct {
	Nonce      [16]byte
	CellDigest [16]byte
	RootDigest [16]byte
}

type Frame struct {
	Binding
	Sequence        uint64
	Phase           uint32
	Flags           uint32
	CustodianHandle uint64 // ephemeral; never persist this value
	Cause           uint32
}

func NewBinding(nonce [16]byte, cell string, rootToken [16]byte) Binding {
	cellHash := sha256.Sum256([]byte(cell))
	rootHash := sha256.Sum256(rootToken[:])
	var binding Binding
	binding.Nonce = nonce
	copy(binding.CellDigest[:], cellHash[:16])
	copy(binding.RootDigest[:], rootHash[:16])
	return binding
}

func validShape(frame Frame) error {
	if frame.Sequence == 0 || frame.Phase < Ready || frame.Phase > Fault || frame.Flags & ^uint32(Attempted|BarrierSeen|InjectionFired|FinishedSeen) != 0 || frame.Cause > CauseChildFault {
		return errors.New("invalid F4 sequence, phase, flags or cause")
	}
	if frame.Flags&(BarrierSeen|InjectionFired|FinishedSeen) != 0 && frame.Flags&Attempted == 0 {
		return errors.New("F4 cumulative flags lack attempted")
	}
	if frame.CustodianHandle != 0 && frame.Phase != Barrier {
		return errors.New("custodian handle outside barrier")
	}
	switch frame.Phase {
	case Ready:
		if frame.Flags != 0 || frame.Cause != CauseNone || frame.CustodianHandle != 0 {
			return errors.New("invalid ready frame")
		}
	case Started:
		if frame.Flags != Attempted || frame.Cause != CauseNone || frame.CustodianHandle != 0 {
			return errors.New("invalid started frame")
		}
	case Barrier:
		if frame.Flags != Attempted|BarrierSeen || frame.Cause != CauseNone || frame.CustodianHandle == 0 {
			return errors.New("invalid barrier frame")
		}
	case Fired:
		if frame.Flags != Attempted|InjectionFired && frame.Flags != Attempted|BarrierSeen|InjectionFired {
			return errors.New("invalid fired flags")
		}
		if frame.Cause != CauseNone || frame.CustodianHandle != 0 {
			return errors.New("invalid fired payload")
		}
	case Finished:
		if frame.Flags != Attempted|InjectionFired|FinishedSeen && frame.Flags != Attempted|BarrierSeen|InjectionFired|FinishedSeen {
			return errors.New("invalid finished flags")
		}
		if frame.CustodianHandle != 0 {
			return errors.New("finished carries handle")
		}
	case Fault:
		if frame.Cause == CauseNone || frame.CustodianHandle != 0 || frame.Flags&FinishedSeen != 0 {
			return errors.New("invalid fault frame")
		}
	}
	return nil
}

func Encode(frame Frame) ([Size]byte, error) {
	var raw [Size]byte
	if err := validShape(frame); err != nil {
		return raw, err
	}
	copy(raw[0:8], Magic)
	copy(raw[8:24], frame.Nonce[:])
	copy(raw[24:40], frame.CellDigest[:])
	copy(raw[40:56], frame.RootDigest[:])
	binary.LittleEndian.PutUint64(raw[56:64], frame.Sequence)
	binary.LittleEndian.PutUint32(raw[64:68], frame.Phase)
	binary.LittleEndian.PutUint32(raw[68:72], frame.Flags)
	binary.LittleEndian.PutUint64(raw[72:80], frame.CustodianHandle)
	binary.LittleEndian.PutUint32(raw[80:84], frame.Cause)
	binary.LittleEndian.PutUint32(raw[92:96], crc32.Checksum(raw[:92], checksumTable))
	return raw, nil
}

// Decode requires two stable complete copies and the parent's exact binding.
// Sequence and transition ordering remain the receiving parent's state.
func Decode(first, second []byte, binding Binding) (Frame, error) {
	var frame Frame
	if len(first) != Size || len(second) != Size || !bytes.Equal(first, second) {
		return frame, errors.New("torn or wrong-sized F4 cell")
	}
	if string(first[0:8]) != Magic || !bytes.Equal(first[8:24], binding.Nonce[:]) || !bytes.Equal(first[24:40], binding.CellDigest[:]) || !bytes.Equal(first[40:56], binding.RootDigest[:]) {
		return frame, errors.New("F4 magic, nonce, cell or root mismatch")
	}
	for _, value := range first[84:92] {
		if value != 0 {
			return frame, errors.New("F4 reserved byte nonzero")
		}
	}
	if binary.LittleEndian.Uint32(first[92:96]) != crc32.Checksum(first[:92], checksumTable) {
		return frame, errors.New("F4 CRC mismatch")
	}
	frame = Frame{
		Binding:         binding,
		Sequence:        binary.LittleEndian.Uint64(first[56:64]),
		Phase:           binary.LittleEndian.Uint32(first[64:68]),
		Flags:           binary.LittleEndian.Uint32(first[68:72]),
		CustodianHandle: binary.LittleEndian.Uint64(first[72:80]),
		Cause:           binary.LittleEndian.Uint32(first[80:84]),
	}
	if err := validShape(frame); err != nil {
		return Frame{}, fmt.Errorf("F4 shape: %w", err)
	}
	return frame, nil
}
