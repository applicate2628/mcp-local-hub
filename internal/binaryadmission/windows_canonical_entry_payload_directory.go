package binaryadmission

import (
	"context"
	"sync"
)

const WindowsCanonicalEntryPayloadDirectorySchemaV1 = "windows-canonical-entry-payload-directory-v1"

// CreationTime is the received Windows FILETIME value, in 100 ns units since
// 1601 UTC. It is object evidence, not a wall-clock admission heuristic.
type WindowsCanonicalEntryDirectoryIdentityV1 struct {
	VolumeSerialNumber uint32 `json:"volume_serial_number"`
	FileIndexHigh      uint32 `json:"file_index_high"`
	FileIndexLow       uint32 `json:"file_index_low"`
	CreationTime       uint64 `json:"creation_time"`
}

type WindowsCanonicalEntryPayloadDirectorySnapshotV1 struct {
	Schema            string                                   `json:"schema"`
	Root              string                                   `json:"root"`
	RelativePath      string                                   `json:"relative_path"`
	Created           bool                                     `json:"created"`
	ParentIdentity    WindowsCanonicalEntryDirectoryIdentityV1 `json:"parent_identity"`
	DirectoryIdentity WindowsCanonicalEntryDirectoryIdentityV1 `json:"directory_identity"`
}

// Production Source is the same private verified journal/fence receiver. A
// pathname, Created boolean or caller-authored tuple cannot issue this owner.
type WindowsCanonicalEntryPayloadDirectoryRecoverySourceV1 interface {
	WithOriginalPayloadDirectoryRecoveryInput(context.Context, func(WindowsCanonicalEntryPayloadDirectorySnapshotV1) error) error
	ValidateOriginalRecovery(context.Context) error
}

// This separate Source purpose requires durable created identity + disposal
// intent, rollback requiring absence, and the original actor/journal/fence. It
// must never qualify a committed required directory or generic missing path.
type WindowsCanonicalEntryPayloadDirectoryAbsenceSourceV1 interface {
	WithOriginalPayloadDirectoryAbsenceInput(context.Context, func(WindowsCanonicalEntryPayloadDirectorySnapshotV1) error) error
	ValidateOriginalRecovery(context.Context) error
}

type WindowsCanonicalEntryPayloadDirectoryAbsenceObservationV1 struct {
	Root           string
	RelativePath   string
	ParentIdentity WindowsCanonicalEntryDirectoryIdentityV1
	Absent         bool
}

// Existing private recovery receivers may lend an already acquired opaque
// directory owner. This interface is private; its exported method permits the
// existing Core receiver to implement it without another public constructor.
type canonicalEntryPayloadDirectorySourceV1 interface {
	OriginalPayloadDirectory(context.Context) (*WindowsCanonicalEntryPayloadDirectoryV1, error)
}

// One shared reference propagates the same directory owner to prior roots that
// were acquired before Ensure. It owns no native resource or recovery state.
type canonicalEntryPayloadDirectoryLinkV1 struct {
	mu    sync.Mutex
	owner *WindowsCanonicalEntryPayloadDirectoryV1
	prior *WindowsCanonicalEntryPriorSetV1
}

func (o *WindowsCanonicalEntrySetV1) payloadDirectoryOwner() *WindowsCanonicalEntryPayloadDirectoryV1 {
	if o == nil || o.payloadDirectoryLink == nil {
		return nil
	}
	o.payloadDirectoryLink.mu.Lock()
	defer o.payloadDirectoryLink.mu.Unlock()
	return o.payloadDirectoryLink.owner
}

type WindowsCanonicalEntryPayloadDirectoryV1 struct {
	mu                sync.Mutex
	guard             *WindowsCanonicalEntrySetV1
	parent, directory *canonicalEntryNativeFile
	snapshot          WindowsCanonicalEntryPayloadDirectorySnapshotV1
	source            WindowsCanonicalEntryPayloadDirectoryRecoverySourceV1
	borrowers         uint64
	ready             bool
	closed            bool
	acquisitionError  error
	closeError        error
	disposition       *WindowsCanonicalEntryMoveResultV1
}
