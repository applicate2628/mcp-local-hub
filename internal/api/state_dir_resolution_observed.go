package api

import (
	"context"
	"errors"
	"time"

	"mcp-local-hub/internal/process"
)

type StateDirResolutionSnapshotV1 struct {
	Schema             string                   `json:"schema"`
	Version            uint32                   `json:"version"`
	PathSHA256         string                   `json:"path_sha256"`
	SourceSHA256       string                   `json:"source_sha256"`
	HRESULTDefined     bool                     `json:"hresult_defined"`
	HRESULT            *uint32                  `json:"hresult"`
	AllocationAcquired bool                     `json:"allocation_acquired"`
	DecodeSucceeded    bool                     `json:"decode_succeeded"`
	FreeCalled         bool                     `json:"free_called"`
	FreeReturned       bool                     `json:"free_returned"`
	ReleaseKind        string                   `json:"release_kind"`
	Complete           bool                     `json:"complete"`
	PrimaryError       *StateFileReadFailureV1  `json:"primary_error"`
	CleanupErrors      []StateFileReadFailureV1 `json:"cleanup_errors"`
}

type StateDirResolutionObservation struct {
	issuer                                      *stateFileReadIssuer
	path                                        string
	view                                        StateDirResolutionSnapshotV1
	principal                                   string
	formatting                                  *process.SIDFormatObservation
	principalCheck                              func() error
	deadline                                    time.Time
	deadlineDefined, finalized, finishedInBound bool
	primaryErr, cleanupErr                      error
}

func (o *StateDirResolutionObservation) Snapshot() StateDirResolutionSnapshotV1 {
	if o == nil {
		return StateDirResolutionSnapshotV1{}
	}
	v := o.view
	if v.HRESULT != nil {
		x := *v.HRESULT
		v.HRESULT = &x
	}
	if v.PrimaryError != nil {
		x := *v.PrimaryError
		v.PrimaryError = &x
	}
	v.CleanupErrors = append([]StateFileReadFailureV1{}, v.CleanupErrors...)
	return v
}

func (o *StateDirResolutionObservation) Validate(ctx context.Context, resolvedPath string) error {
	if o == nil || o.issuer == nil || ctx == nil || !o.finalized || !o.finishedInBound || !o.view.Complete || resolvedPath == "" || o.path != resolvedPath || o.view.PathSHA256 != stateFileReadObservationDigest([]byte(resolvedPath)) || o.view.SourceSHA256 == "" || !o.view.HRESULTDefined || o.view.HRESULT == nil || int32(*o.view.HRESULT) < 0 || !o.view.DecodeSucceeded || !o.view.AllocationAcquired || !o.view.FreeCalled || !o.view.FreeReturned || o.view.ReleaseKind != "CoTaskMemFree-void" {
		return errors.New("original closed canonical state resolver required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.deadlineDefined && !time.Now().Before(o.deadline) {
		return context.DeadlineExceeded
	}
	if o.principalCheck == nil {
		return errors.New("original resolver principal receiving required")
	}
	if err := o.principalCheck(); err != nil {
		return errors.Join(err, o.primaryErr, o.cleanupErr)
	}
	return errors.Join(o.primaryErr, o.cleanupErr)
}
