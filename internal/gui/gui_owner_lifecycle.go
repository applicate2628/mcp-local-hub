package gui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// GUIOwnerRecord is the durable, versioned GUI ownership observation. Version
// one was the legacy two-field text file; new writers emit version two JSON.
// A record is metadata, never a replacement for the flock or OS socket-owner
// proof.
type GUIOwnerRecord struct {
	Version           int       `json:"version"`
	State             string    `json:"state"`
	PID               int       `json:"pid"`
	StartTime         time.Time `json:"start_time"`
	Port              int       `json:"port"`
	Generation        string    `json:"generation"`
	HandoffID         string    `json:"handoff_id,omitempty"`
	HandoffGeneration string    `json:"handoff_generation,omitempty"`
	HandoffTargetPID  int       `json:"handoff_target_pid,omitempty"`
	HandoffTargetPort int       `json:"handoff_target_port,omitempty"`
	Legacy            bool      `json:"-"`
}

const (
	guiOwnerRecordVersion  = 2
	guiOwnerStateActive    = "active"
	guiOwnerStateHandoff   = "handoff"
	guiOwnerStateTombstone = "tombstone"
)

// GUIOwnerLifecycle owns the durable record transitions for one process that
// already owns the GUI flock. SingleInstanceLock deliberately remains an
// unlock-only resource; cleanup changes the record before that lock releases.
type GUIOwnerLifecycle struct {
	path       string
	generation string
	pid        int
	startTime  time.Time
}

func newGUIOwnerLifecycle(path string, record GUIOwnerRecord) *GUIOwnerLifecycle {
	return &GUIOwnerLifecycle{path: path, generation: record.Generation, pid: record.PID, startTime: record.StartTime.UTC()}
}

// NewGUIOwnerLifecycle reads the record written by this process after flock
// acquisition. It is intentionally unavailable for legacy v1 records: legacy
// is a compatibility input only and cannot authorize a destructive CAS.
func NewGUIOwnerLifecycle(path string) (*GUIOwnerLifecycle, error) {
	record, err := ReadGUIOwnerRecord(path)
	if err != nil {
		return nil, err
	}
	if record.Legacy || record.Version != guiOwnerRecordVersion || record.State != guiOwnerStateActive || record.Generation == "" || record.PID <= 0 || record.StartTime.IsZero() {
		return nil, errors.New("GUI owner lifecycle requires a v2 active record")
	}
	if record.PID != os.Getpid() || record.Generation != guiOwnerGeneration(record.PID, record.StartTime) {
		return nil, errors.New("GUI owner lifecycle record does not belong to this process")
	}
	current, err := currentGUIOwnerRecord(os.Getpid(), record.Port)
	if err != nil || !current.StartTime.Equal(record.StartTime) {
		if err != nil {
			return nil, fmt.Errorf("verify GUI owner process generation: %w", err)
		}
		return nil, errors.New("GUI owner lifecycle record has a stale process generation")
	}
	return newGUIOwnerLifecycle(path, record), nil
}

// ReadGUIOwnerRecord accepts legacy v1 only so old installations remain
// reachable during upgrade. New operational writers must require v2.
func ReadGUIOwnerRecord(path string) (GUIOwnerRecord, error) {
	b, err := api.ReadStateFileInodeAnchored(path)
	if err != nil {
		return GUIOwnerRecord{}, err
	}
	trimmed := strings.TrimSpace(string(b))
	if !strings.HasPrefix(trimmed, "{") {
		parts := strings.Fields(trimmed)
		if len(parts) != 2 {
			return GUIOwnerRecord{}, fmt.Errorf("malformed pidport %q", string(b))
		}
		pid, pidErr := strconv.Atoi(parts[0])
		port, portErr := strconv.Atoi(parts[1])
		if pidErr != nil {
			return GUIOwnerRecord{}, fmt.Errorf("parse pid: %w", pidErr)
		}
		if portErr != nil {
			return GUIOwnerRecord{}, fmt.Errorf("parse port: %w", portErr)
		}
		return GUIOwnerRecord{Version: 1, State: guiOwnerStateActive, PID: pid, Port: port, Legacy: true}, nil
	}
	var record GUIOwnerRecord
	if err := json.Unmarshal(b, &record); err != nil {
		return GUIOwnerRecord{}, fmt.Errorf("parse v2 gui owner record: %w", err)
	}
	if err := validateGUIOwnerRecord(record); err != nil {
		return GUIOwnerRecord{}, err
	}
	return record, nil
}

func validateGUIOwnerRecord(record GUIOwnerRecord) error {
	if record.Version != guiOwnerRecordVersion {
		return fmt.Errorf("unsupported gui owner record version %d", record.Version)
	}
	if record.PID <= 0 || record.StartTime.IsZero() || record.Generation == "" {
		return errors.New("v2 gui owner record is missing process generation")
	}
	if record.Port < 0 || record.Port > 65535 {
		return fmt.Errorf("v2 gui owner record has invalid port %d", record.Port)
	}
	switch record.State {
	case guiOwnerStateActive, guiOwnerStateTombstone:
		if record.HandoffID != "" || record.HandoffGeneration != "" || record.HandoffTargetPID != 0 || record.HandoffTargetPort != 0 {
			return errors.New("non-handoff gui owner record contains handoff fields")
		}
	case guiOwnerStateHandoff:
		if record.HandoffID == "" || record.HandoffGeneration == "" || record.HandoffTargetPID <= 0 || record.HandoffTargetPort <= 0 || record.HandoffTargetPort > 65535 {
			return errors.New("handoff gui owner record is incomplete")
		}
	default:
		return fmt.Errorf("unknown gui owner record state %q", record.State)
	}
	return nil
}

func currentGUIOwnerRecord(pid, port int) (GUIOwnerRecord, error) {
	if pid <= 0 {
		return GUIOwnerRecord{}, fmt.Errorf("invalid GUI owner PID %d", pid)
	}
	// Publishing this process's durable generation must not inherit a test
	// override intended to model an incumbent/kill target. The platform probe is
	// the existing exact-start primitive; no new process-discovery path is
	// introduced here.
	identityFn := processID
	if pid == os.Getpid() {
		identityFn = processIDImpl
	}
	identity, err := identityFn(pid)
	if err != nil || !identity.Alive || identity.Denied || identity.StartTime.IsZero() {
		if err != nil {
			return GUIOwnerRecord{}, fmt.Errorf("read GUI owner process generation: %w", err)
		}
		return GUIOwnerRecord{}, fmt.Errorf("read GUI owner process generation for PID %d", pid)
	}
	start := identity.StartTime.UTC()
	return GUIOwnerRecord{
		Version: guiOwnerRecordVersion, State: guiOwnerStateActive, PID: pid, StartTime: start, Port: port,
		Generation: guiOwnerGeneration(pid, start),
	}, nil
}

func guiOwnerGeneration(pid int, start time.Time) string {
	sum := sha256.Sum256([]byte(strconv.Itoa(pid) + ":" + strconv.FormatInt(start.UTC().UnixNano(), 10)))
	return hex.EncodeToString(sum[:])
}

func marshalGUIOwnerRecord(record GUIOwnerRecord) ([]byte, error) {
	if err := validateGUIOwnerRecord(record); err != nil {
		return nil, err
	}
	return json.Marshal(record)
}

func writeGUIOwnerRecord(path string, record GUIOwnerRecord) error {
	b, err := marshalGUIOwnerRecord(record)
	if err != nil {
		return err
	}
	return api.WriteStateFileBytesLockHeld(path, append(b, '\n'))
}

func writeCurrentGUIOwnerRecord(path string, pid, port int) (GUIOwnerRecord, error) {
	record, err := currentGUIOwnerRecord(pid, port)
	if err != nil {
		return GUIOwnerRecord{}, err
	}
	if err := writeGUIOwnerRecord(path, record); err != nil {
		return GUIOwnerRecord{}, err
	}
	return record, nil
}

func (l *GUIOwnerLifecycle) matches(record GUIOwnerRecord) bool {
	return l != nil && !record.Legacy && record.Version == guiOwnerRecordVersion && record.PID == l.pid && record.Generation == l.generation && record.StartTime.Equal(l.startTime)
}

// UpdatePort records the listener's actual bound port while the caller holds
// the flock. A successor record is never overwritten.
func (l *GUIOwnerLifecycle) UpdatePort(port int) error {
	if l == nil {
		return errors.New("GUI owner lifecycle is nil")
	}
	record, err := ReadGUIOwnerRecord(l.path)
	if err != nil {
		return err
	}
	if !l.matches(record) || record.State != guiOwnerStateActive {
		return errors.New("GUI owner record changed before port update")
	}
	record.Port = port
	return writeGUIOwnerRecord(l.path, record)
}

// BeginHandoff preserves a durable handoff record until the authenticated
// replacement overwrites it with its own active generation.
func (l *GUIOwnerLifecycle) BeginHandoff(id, generation string, targetPID, targetPort int) error {
	if l == nil {
		return errors.New("GUI owner lifecycle is nil")
	}
	record, err := ReadGUIOwnerRecord(l.path)
	if err != nil {
		return err
	}
	if !l.matches(record) || record.State != guiOwnerStateActive {
		return errors.New("GUI owner record changed before handoff")
	}
	record.State = guiOwnerStateHandoff
	record.HandoffID, record.HandoffGeneration = id, generation
	record.HandoffTargetPID, record.HandoffTargetPort = targetPID, targetPort
	return writeGUIOwnerRecord(l.path, record)
}

func (l *GUIOwnerLifecycle) RestoreActive(handoffID, handoffGeneration string) error {
	if l == nil {
		return errors.New("GUI owner lifecycle is nil")
	}
	record, err := ReadGUIOwnerRecord(l.path)
	if err != nil {
		return err
	}
	if !l.matches(record) || record.State != guiOwnerStateHandoff || record.HandoffID != handoffID || record.HandoffGeneration != handoffGeneration {
		return errors.New("GUI owner record changed before handoff rollback")
	}
	record.State = guiOwnerStateActive
	record.HandoffID, record.HandoffGeneration = "", ""
	record.HandoffTargetPID, record.HandoffTargetPort = 0, 0
	return writeGUIOwnerRecord(l.path, record)
}

// TerminalSettle tombstones exactly this owner's record before flock release.
// A successor's generation is never removed; retaining a tombstone gives
// diagnostics a causal terminal record without claiming a live listener.
func (l *GUIOwnerLifecycle) TerminalSettle() error {
	if l == nil {
		return nil
	}
	record, err := ReadGUIOwnerRecord(l.path)
	if err != nil {
		return err
	}
	if !l.matches(record) {
		return nil
	}
	if record.State == guiOwnerStateHandoff {
		return nil
	}
	record.State = guiOwnerStateTombstone
	record.Port = 0
	record.HandoffID, record.HandoffGeneration = "", ""
	record.HandoffTargetPID, record.HandoffTargetPort = 0, 0
	return writeGUIOwnerRecord(l.path, record)
}

// AcquireSingleInstanceLockOnlyAt acquires the flock without publishing GUI
// ownership metadata. It is for non-GUI maintenance commands that must prove
// the GUI is stopped; using AcquireSingleInstanceAt there would create a stale
// active record with no listener.
func AcquireSingleInstanceLockOnlyAt(pidportPath string) (*SingleInstanceLock, error) {
	return tryAcquireSingleInstanceLockAt(pidportPath)
}

// VerifyGUIOwnerListener is the composition-side proof for operations that
// will repoint durable client configuration at a GUI listener. It binds a v2
// record to the OS socket owner and exact process creation time, then re-reads
// the generation after the OS probe. Legacy records remain usable only for
// compatibility handshakes, never for a durable routing mutation.
func VerifyGUIOwnerListener(ctx context.Context, path string, pid, port int) error {
	if ctx == nil {
		return errors.New("GUI owner listener verification has nil context")
	}
	first, err := ReadGUIOwnerRecord(path)
	if err != nil {
		return fmt.Errorf("read GUI owner record: %w", err)
	}
	if first.Legacy || first.State != guiOwnerStateActive || first.PID != pid || first.Port != port {
		return errors.New("GUI owner record is not an active matching v2 generation")
	}
	ownerPID, found, err := api.LoopbackPortOwnerPIDContext(ctx, port)
	if err != nil || !found || ownerPID != pid {
		if err != nil {
			return fmt.Errorf("resolve GUI listener owner: %w", err)
		}
		return errors.New("GUI listener owner does not match owner record")
	}
	identity, err := processID(pid)
	if err != nil || !identity.Alive || identity.Denied || identity.StartTime.IsZero() {
		if err != nil {
			return fmt.Errorf("read GUI owner process: %w", err)
		}
		return errors.New("GUI owner process is unavailable")
	}
	if !identity.StartTime.UTC().Equal(first.StartTime.UTC()) || !clients.IsMcphubBinary(identity.ImagePath) || !cmdlineIsGui(identity.Cmdline) {
		return errors.New("GUI listener process does not match the recorded generation")
	}
	second, err := ReadGUIOwnerRecord(path)
	if err != nil {
		return fmt.Errorf("re-read GUI owner record: %w", err)
	}
	if second.Legacy || second != first {
		return errors.New("GUI owner record changed during listener verification")
	}
	return nil
}
