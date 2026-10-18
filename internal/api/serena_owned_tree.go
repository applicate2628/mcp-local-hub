package api

import (
	"context"
	"encoding/json"
	"errors"
)

type SerenaTreeLimits struct {
	Entries          int   `json:"entries"`
	Depth            int   `json:"depth"`
	RecordBytes      int64 `json:"record_bytes"`
	EncodedPathBytes int64 `json:"encoded_path_bytes"`
	LogicalBytes     int64 `json:"logical_bytes"`
	OpenHandles      int   `json:"open_handles"`
}

// One package-owned finite policy. Actual cold-package fit remains unverified.
func DefaultSerenaTreeLimits() SerenaTreeLimits {
	return SerenaTreeLimits{Entries: 131072, Depth: 64, RecordBytes: 64 << 20, EncodedPathBytes: 64 << 20, LogicalBytes: 32 << 30, OpenHandles: 256}
}
func (l SerenaTreeLimits) valid() bool {
	return l.Entries > 0 && l.Depth > 0 && l.RecordBytes > 0 && l.EncodedPathBytes > 0 && l.LogicalBytes > 0 && l.OpenHandles > 0
}

type SerenaOwnedTreeRequest struct {
	Namespace                       string
	PackageIdentitySHA256           string
	OperationID, GenerationID, Kind string
	QualificationID                 string
	Limits                          SerenaTreeLimits
	// Only same-attempt opaque generation capabilities qualify surviving links.
	survivors []*SerenaOwnedTree
}
type SerenaTreeObject struct {
	Relative       []string `json:"relative"`
	Device, FileID string
	Directory      bool
	Bytes          int64
	Links          uint32
	Modified       uint64
	ContentSHA256  string             `json:"content_sha256,omitempty"`
	Streams        []SerenaTreeStream `json:"streams"`
}

type SerenaTreeStream struct {
	Name                  string `json:"name"`
	Bytes, AllocatedBytes int64
}
type SerenaTreeInventory struct {
	Version      int                `json:"version"`
	OperationID  string             `json:"operation_id"`
	RootIdentity SerenaTreeObject   `json:"root_identity"`
	Objects      []SerenaTreeObject `json:"objects"`
	SHA256       string             `json:"-"`
}
type SerenaTreeClose struct {
	Attempted, Closed bool
	Error             error
}
type SerenaTreeCleanupReceipt struct {
	Before          SerenaTreeInventory
	Removed, Absent bool
	DeleteAttempts  int
	Closes          []SerenaTreeClose
	Pending         []string
	Error           error
}

func (r SerenaTreeCleanupReceipt) Complete() bool {
	if r.Error != nil || !r.Removed || !r.Absent || len(r.Pending) != 0 {
		return false
	}
	for _, c := range r.Closes {
		if !c.Attempted || !c.Closed || c.Error != nil {
			return false
		}
	}
	return len(r.Closes) > 0
}

type serenaTreePlatform interface {
	inventory(context.Context) (SerenaTreeInventory, error)
	remove(context.Context, PreparedSerenaExecutionClosure) (SerenaTreeCleanupReceipt, error)
	close() (SerenaTreeClose, error)
	workingDirectory(context.Context) (string, error)
}

func (t *SerenaOwnedTree) WorkingDirectory(ctx context.Context) (string, error) {
	if t == nil || t.platform == nil {
		return "", errors.New("SERENA_TREE_CAPABILITY_MISSING")
	}
	return t.platform.workingDirectory(ctx)
}

// There is no public constructor from pathname/serialized identity/handle.
type SerenaOwnedTree struct {
	request  SerenaOwnedTreeRequest
	platform serenaTreePlatform
}

func AcquireSerenaOwnedTree(ctx context.Context, r SerenaOwnedTreeRequest) (*SerenaOwnedTree, error) {
	if ctx == nil || !preparedSerenaDigest(r.PackageIdentitySHA256) || !preparedSerenaID(r.OperationID) || !preparedSerenaID(r.GenerationID) || !r.Limits.valid() || (r.Kind != "generation" && r.Kind != "attempt" && r.Kind != "qualification") || (r.Kind == "qualification" && !preparedSerenaID(r.QualificationID)) || (r.Kind != "qualification" && r.QualificationID != "") {
		return nil, errors.New("SERENA_TREE_REQUEST_INVALID")
	}
	return acquireSerenaTreePlatform(ctx, r)
}
func (t *SerenaOwnedTree) Inventory(ctx context.Context) (SerenaTreeInventory, error) {
	if t == nil || t.platform == nil {
		return SerenaTreeInventory{}, errors.New("SERENA_TREE_CAPABILITY_MISSING")
	}
	return t.platform.inventory(ctx)
}
func (t *SerenaOwnedTree) RemoveOwned(ctx context.Context, apply bool, closure PreparedSerenaExecutionClosure) (SerenaTreeCleanupReceipt, error) {
	if t == nil || t.platform == nil || !apply || !closure.complete(t.request.OperationID) {
		return SerenaTreeCleanupReceipt{}, errors.New("SERENA_TREE_DELETE_ADMISSION_REFUSED")
	}
	return t.platform.remove(ctx, closure)
}
func (t *SerenaOwnedTree) Close() (SerenaTreeClose, error) {
	if t == nil || t.platform == nil {
		return SerenaTreeClose{}, errors.New("SERENA_TREE_CAPABILITY_MISSING")
	}
	return t.platform.close()
}
func serenaTreeInventoryHash(v SerenaTreeInventory) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return preparedSerenaHash(raw), nil
}
