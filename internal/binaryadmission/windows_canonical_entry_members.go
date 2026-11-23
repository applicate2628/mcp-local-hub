package binaryadmission

import (
	"context"
	"errors"
	"slices"
	"sync"
)

const WindowsCanonicalEntryMemberDirectorySchemaV2 = "windows-canonical-entry-member-directory-v2"

type WindowsCanonicalEntryBeforeKindV2 string

const (
	WindowsCanonicalEntryBeforeAbsentV2      WindowsCanonicalEntryBeforeKindV2 = "absent"
	WindowsCanonicalEntryBeforeRootProductV2 WindowsCanonicalEntryBeforeKindV2 = "legacy-root-product-only"
	WindowsCanonicalEntryBeforeLegacyV2      WindowsCanonicalEntryBeforeKindV2 = "legacy-entry-set-v1"
	WindowsCanonicalEntryBeforeBundleV2      WindowsCanonicalEntryBeforeKindV2 = "owned-bundle-v2"
)

// These records describe received objects. They cannot issue an original owner.
type WindowsCanonicalEntryFileIdentityV2 struct {
	VolumeSerialNumber uint32 `json:"volume_serial_number"`
	FileIndexHigh      uint32 `json:"file_index_high"`
	FileIndexLow       uint32 `json:"file_index_low"`
	CreationTime       uint64 `json:"creation_time"`
}

type WindowsCanonicalEntryMemberEvidenceV2 struct {
	Member         string                                   `json:"member"`
	File           WindowsCanonicalEntryFileV1              `json:"file"`
	NativeIdentity WindowsCanonicalEntryFileIdentityV2      `json:"native_identity"`
	ParentIdentity WindowsCanonicalEntryDirectoryIdentityV1 `json:"parent_identity"`
	Size           int64                                    `json:"size"`
	Attributes     uint32                                   `json:"attributes"`
}

type WindowsCanonicalEntryMemberDirectorySnapshotV2 struct {
	Schema            string                                   `json:"schema"`
	Root              string                                   `json:"root"`
	RelativePath      string                                   `json:"relative_path"`
	Created           bool                                     `json:"created"`
	ParentIdentity    WindowsCanonicalEntryDirectoryIdentityV1 `json:"parent_identity"`
	DirectoryIdentity WindowsCanonicalEntryDirectoryIdentityV1 `json:"directory_identity"`
}

type WindowsCanonicalEntryMemberAbsenceV2 struct {
	ParentRelative  string                                   `json:"parent_relative"`
	ParentIdentity  WindowsCanonicalEntryDirectoryIdentityV1 `json:"parent_identity"`
	MissingRelative string                                   `json:"missing_relative"`
}

type WindowsCanonicalEntryPriorObservationV2 struct {
	BeforeKind      WindowsCanonicalEntryBeforeKindV2
	BundleDirectory *WindowsCanonicalEntryPayloadDirectorySnapshotV1
	Inventory       *RuntimeInventoryV1
	Members         []WindowsCanonicalEntryMemberEvidenceV2
	Absences        map[string]WindowsCanonicalEntryMemberAbsenceV2
	Directories     []WindowsCanonicalEntryMemberDirectorySnapshotV2
}

type WindowsCanonicalEntryMemberDispositionV2 struct {
	Transferred, CloseEntered, OriginalOwnerPending, DispositionPending bool
	Moves, Cleanup                                                      []WindowsCanonicalEntryMoveResultV1
	CloseError                                                          error
}

type WindowsCanonicalEntryRecoveryPurposeV2 string

const (
	WindowsCanonicalRecoveryRollbackV2         WindowsCanonicalEntryRecoveryPurposeV2 = "rollback"
	WindowsCanonicalRecoveryCommittedForwardV2 WindowsCanonicalEntryRecoveryPurposeV2 = "committed-forward"
	WindowsCanonicalRecoveryRollbackCleanupV2  WindowsCanonicalEntryRecoveryPurposeV2 = "rollback-cleanup"
)

type WindowsCanonicalEntryMemberRecoveryPlanV2 struct {
	Member                 string                                 `json:"member"`
	Prior                  *WindowsCanonicalEntryMemberEvidenceV2 `json:"prior"`
	PriorAbsence           *WindowsCanonicalEntryMemberAbsenceV2  `json:"prior_absence"`
	Staged                 *WindowsCanonicalEntryMemberEvidenceV2 `json:"staged"`
	PriorAsideRelative     *string                                `json:"prior_aside_relative"`
	SuccessorAsideRelative *string                                `json:"successor_aside_relative"`
}

type WindowsCanonicalEntryMemberMoveRecordV2 struct {
	Member               string  `json:"member"`
	Prior                bool    `json:"prior"`
	SourceRelative       string  `json:"source_relative"`
	DestinationRelative  string  `json:"destination_relative"`
	Applied              bool    `json:"applied"`
	Readback             bool    `json:"readback"`
	NativeStatusKnown    bool    `json:"native_status_known"`
	NativeStatus         uint32  `json:"native_status"`
	Attempted            bool    `json:"attempted"`
	Error                *string `json:"error"`
	CloseError           *string `json:"close_error"`
	OriginalOwnerPending bool    `json:"original_owner_pending"`
}

type WindowsCanonicalEntryNativeResultRecordV2 struct {
	Member               string  `json:"member"`
	SourceRelative       string  `json:"source_relative"`
	DestinationRelative  string  `json:"destination_relative"`
	Attempted            bool    `json:"attempted"`
	NativeStatusKnown    bool    `json:"native_status_known"`
	NativeStatus         uint32  `json:"native_status"`
	Applied              bool    `json:"applied"`
	Readback             bool    `json:"readback"`
	Error                *string `json:"error"`
	CloseError           *string `json:"close_error"`
	OriginalOwnerPending bool    `json:"original_owner_pending"`
}

type WindowsCanonicalEntryMemberCleanupRecordV2 struct {
	TransactionID  string                                 `json:"transaction_id"`
	Side           string                                 `json:"side"`
	Member         string                                 `json:"member"`
	SourceRelative string                                 `json:"source_relative"`
	ExpectedSHA256 string                                 `json:"expected_sha256"`
	State          string                                 `json:"state"`
	Purpose        WindowsCanonicalEntryRecoveryPurposeV2 `json:"purpose"`
}

type WindowsCanonicalEntryMemberDirectoryRecordV2 struct {
	Original    WindowsCanonicalEntryMemberDirectorySnapshotV2 `json:"original"`
	State       string                                         `json:"state"`
	Disposition *WindowsCanonicalEntryNativeResultRecordV2     `json:"disposition"`
}

type WindowsCanonicalEntryMembersRecoveryInputV2 struct {
	Targets       WindowsCanonicalEntrySetTargetsV2
	BeforeKind    WindowsCanonicalEntryBeforeKindV2
	Expected      WindowsCanonicalEntrySetSnapshotV2
	TransactionID string
	Purpose       WindowsCanonicalEntryRecoveryPurposeV2
	Members       []WindowsCanonicalEntryMemberRecoveryPlanV2
	Directories   []WindowsCanonicalEntryMemberDirectoryRecordV2
	Moves         []WindowsCanonicalEntryMemberMoveRecordV2
	Cleanup       []WindowsCanonicalEntryMemberCleanupRecordV2
}

// Implemented by the transaction's original fenced journal receiver. Its
// callback is synchronous and unique; a decoded snapshot is not this source.
type WindowsCanonicalRecoverySourceV2 interface {
	WithOriginalMemberRecoveryInput(context.Context, WindowsCanonicalRecoverySideV1, func(WindowsCanonicalEntryMembersRecoveryInputV2) error) error
	ValidateOriginalRecovery(context.Context) error
	OriginalMemberDirectories(context.Context) ([]*WindowsCanonicalEntryPayloadDirectoryV1, error)
}

// A single directory owner is shared by all roots borrowing its exact object.
// This index carries no independent filesystem or recovery authority.
type canonicalEntryMemberDirectoriesV2 struct {
	mu     sync.Mutex
	owners map[string]*WindowsCanonicalEntryPayloadDirectoryV1
}

type canonicalEntryOwnedMemberV2 struct {
	original *canonicalEntryNativeFile
	evidence WindowsCanonicalEntryMemberEvidenceV2
	cleanup  *WindowsCanonicalEntryMoveResultV1
}

type canonicalEntryMembersV2 struct {
	mu                         sync.Mutex
	root                       *WindowsCanonicalEntrySetV1
	parent                     *canonicalEntryNativeFile
	targets                    WindowsCanonicalEntrySetTargetsV2
	snapshot                   WindowsCanonicalEntrySetSnapshotV2
	members                    map[string]*canonicalEntryOwnedMemberV2
	keys                       []string
	directories                *canonicalEntryMemberDirectoriesV2
	source                     WindowsCanonicalRecoverySourceV2
	ready, transferred, closed bool
	closeError                 error
	acquisitionError           error
	probeCloseError            error
	recoveryInput              *WindowsCanonicalEntryMembersRecoveryInputV2
	recoveryAbsences           map[string]WindowsCanonicalEntryMemberCleanupRecordV2
	recoverySide               WindowsCanonicalRecoverySideV1
	captures                   []WindowsCanonicalEntryMemberEvidenceV2
	moves, cleanup             []WindowsCanonicalEntryMoveResultV1
}

type StagedWindowsCanonicalEntrySetV2 struct {
	original       canonicalEntryMembersV2
	prior          *WindowsCanonicalEntryPriorSetV2
	priorAttempted bool
	priorError     error
}

type WindowsCanonicalEntryPriorSetV2 struct {
	original      canonicalEntryMembersV2
	observation   WindowsCanonicalEntryPriorObservationV2
	artifact      WindowsArtifact
	runtimeMember string
}

type WindowsCanonicalEntryRecoverySetV2 struct {
	original *WindowsCanonicalEntryPriorSetV2
	source   WindowsCanonicalRecoverySourceV2
	side     WindowsCanonicalRecoverySideV1
	lenders  []*WindowsCanonicalEntryPayloadDirectoryV1
}

func cloneCanonicalEntryRecoveryTextV2(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func cloneCanonicalEntryMembersRecoveryInputV2(v WindowsCanonicalEntryMembersRecoveryInputV2) WindowsCanonicalEntryMembersRecoveryInputV2 {
	v.Expected = cloneCanonicalEntrySnapshotV2(v.Expected)
	v.Members = slices.Clone(v.Members)
	for i := range v.Members {
		p := &v.Members[i]
		if p.Prior != nil {
			copy := *p.Prior
			p.Prior = &copy
		}
		if p.Staged != nil {
			copy := *p.Staged
			p.Staged = &copy
		}
		if p.PriorAbsence != nil {
			copy := *p.PriorAbsence
			p.PriorAbsence = &copy
		}
		p.PriorAsideRelative = cloneCanonicalEntryRecoveryTextV2(p.PriorAsideRelative)
		p.SuccessorAsideRelative = cloneCanonicalEntryRecoveryTextV2(p.SuccessorAsideRelative)
	}
	v.Directories = slices.Clone(v.Directories)
	for i := range v.Directories {
		if v.Directories[i].Disposition != nil {
			copy := *v.Directories[i].Disposition
			copy.Error = cloneCanonicalEntryRecoveryTextV2(copy.Error)
			copy.CloseError = cloneCanonicalEntryRecoveryTextV2(copy.CloseError)
			v.Directories[i].Disposition = &copy
		}
	}
	v.Moves = slices.Clone(v.Moves)
	for i := range v.Moves {
		v.Moves[i].Error = cloneCanonicalEntryRecoveryTextV2(v.Moves[i].Error)
		v.Moves[i].CloseError = cloneCanonicalEntryRecoveryTextV2(v.Moves[i].CloseError)
	}
	v.Cleanup = slices.Clone(v.Cleanup)
	return v
}

// Receive data before acquiring a native object. A late or duplicate provider
// callback can never begin native acquisition after this function returns.
func receiveCanonicalEntryMembersRecoveryInputV2(ctx context.Context, source WindowsCanonicalRecoverySourceV2, side WindowsCanonicalRecoverySideV1) (WindowsCanonicalEntryMembersRecoveryInputV2, error) {
	return receiveCanonicalEntryMembersRecoveryInputReadV2(ctx, source, side, nil)
}

func receiveCanonicalEntryMembersRecoveryInputReadV2(ctx context.Context, source WindowsCanonicalRecoverySourceV2, side WindowsCanonicalRecoverySideV1, check func(context.Context) error) (WindowsCanonicalEntryMembersRecoveryInputV2, error) {
	var frozen WindowsCanonicalEntryMembersRecoveryInputV2
	if ctx == nil || source == nil {
		return frozen, errors.New("original current recovery source/context required")
	}
	validate := source.ValidateOriginalRecovery
	if check != nil {
		validate = check
	}
	if err := validate(ctx); err != nil {
		return frozen, err
	}
	var mu sync.Mutex
	active := true
	calls := 0
	var callbackErr error
	providerErr := source.WithOriginalMemberRecoveryInput(ctx, side, func(input WindowsCanonicalEntryMembersRecoveryInputV2) error {
		mu.Lock()
		defer mu.Unlock()
		if !active {
			return errors.New("late current recovery callback refused")
		}
		calls++
		if calls != 1 {
			callbackErr = errors.Join(callbackErr, errors.New("current recovery callback must be unique"))
			return callbackErr
		}
		frozen = cloneCanonicalEntryMembersRecoveryInputV2(input)
		return nil
	})
	mu.Lock()
	active = false
	count, receivedErr := calls, callbackErr
	mu.Unlock()
	if count != 1 {
		return frozen, errors.Join(providerErr, receivedErr, errors.New("current recovery input not uniquely received"))
	}
	if err := errors.Join(providerErr, receivedErr, ctx.Err()); err != nil {
		return frozen, err
	}
	return frozen, validate(ctx)
}
