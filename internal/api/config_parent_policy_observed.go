package api

import (
	"context"
	"errors"
	"time"
)

type ConfigParentDirectoryIdentityV1 struct {
	VolumeSerialNumber uint64   `json:"volume_serial_number"`
	FileID             [16]byte `json:"file_id"`
}

type ConfigParentPolicySnapshotV1 struct {
	Schema            string                            `json:"schema"`
	Version           uint32                            `json:"version"`
	PathSHA256        string                            `json:"path_sha256"`
	CreateMissing     bool                              `json:"create_missing"`
	Exists            bool                              `json:"exists"`
	CreatedComponents []string                          `json:"created_components"`
	FinalIdentity     *ConfigParentDirectoryIdentityV1  `json:"final_identity"`
	GateDisposition   string                            `json:"gate_disposition"`
	Resources         []StateFileReadResourceSnapshotV1 `json:"resources"`
	ResourcesComplete bool                              `json:"resources_complete"`
	Complete          bool                              `json:"complete"`
	PrimaryError      *StateFileReadFailureV1           `json:"primary_error"`
	CleanupErrors     []StateFileReadFailureV1          `json:"cleanup_errors"`
	AuditEventError   *StateFileReadFailureV1           `json:"audit_event_error"`
}

type ConfigParentPolicyObservation struct {
	issuer                                      *stateFileReadIssuer
	path                                        string
	createMissing                               bool
	view                                        ConfigParentPolicySnapshotV1
	policy                                      *StateReadPolicyObservation
	resources                                   *stateFileReadBuilder
	deadline                                    time.Time
	deadlineDefined, finalized, finishedInBound bool
	primaryErr, cleanupErr                      error
	auditObservation                            *HubMcpLogObservation
	auditEventErr                               error
}

type configParentDescentOutcome struct {
	created  []string
	identity *ConfigParentDirectoryIdentityV1
}

func closeConfigParentResources(b *stateFileReadBuilder) error {
	if b == nil {
		return errors.New("original parent resource builder required")
	}
	err := b.borrowedErr
	for _, r := range b.resources {
		if r.view.Acquired {
			err = errors.Join(err, b.closeResource(r.view.Slot))
		}
	}
	return err
}

func (o *ConfigParentPolicyObservation) Snapshot() ConfigParentPolicySnapshotV1 {
	if o == nil {
		return ConfigParentPolicySnapshotV1{}
	}
	v := o.view
	if v.AuditEventError != nil {
		x := *v.AuditEventError
		v.AuditEventError = &x
	}
	v.CreatedComponents = append([]string{}, v.CreatedComponents...)
	if v.FinalIdentity != nil {
		x := *v.FinalIdentity
		v.FinalIdentity = &x
	}
	v.Resources = append([]StateFileReadResourceSnapshotV1{}, v.Resources...)
	for i := range v.Resources {
		r := &v.Resources[i]
		if r.IdentitySHA256 != nil {
			x := *r.IdentitySHA256
			r.IdentitySHA256 = &x
		}
		if r.NativeErrorKind != nil {
			x := *r.NativeErrorKind
			r.NativeErrorKind = &x
		}
		if r.NativeErrorCode != nil {
			x := *r.NativeErrorCode
			r.NativeErrorCode = &x
		}
	}
	if v.PrimaryError != nil {
		x := *v.PrimaryError
		v.PrimaryError = &x
	}
	v.CleanupErrors = append([]StateFileReadFailureV1{}, v.CleanupErrors...)
	return v
}

func (o *ConfigParentPolicyObservation) Validate(ctx context.Context, dir string, createMissing bool) error {
	if o == nil || o.issuer == nil || ctx == nil || !o.finalized || !o.finishedInBound || !o.view.Complete || !o.view.ResourcesComplete || o.path != dir || o.createMissing != createMissing || o.view.PathSHA256 != stateFileReadObservationDigest([]byte(dir)) || (o.view.Exists && o.view.FinalIdentity == nil) || (!createMissing && len(o.view.CreatedComponents) != 0) {
		return errors.New("original closed parent preflight required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.deadlineDefined && !time.Now().Before(o.deadline) {
		return context.DeadlineExceeded
	}
	if err := o.policy.Validate(ctx, StateReadPolicyEnvironmentIdentity()); err != nil {
		return err
	}
	if o.view.GateDisposition == "default-relax" {
		if err := o.auditObservation.ValidateResourceClosure(ctx, o.policy, "warn", "client-write-unhardened-fallback"); err != nil {
			return err
		}
	}
	if o.resources == nil || o.resources.borrowedErr != nil {
		return errors.Join(errors.New("original parent resources unknown"), o.primaryErr, o.cleanupErr)
	}
	for _, r := range o.resources.resources {
		if r.view.Acquired && (!r.view.CloseAttempted || !r.view.CloseResultDefined || !r.view.Closed || r.closeErr != nil) {
			return errors.Join(errors.New("original parent close incomplete"), r.closeErr)
		}
	}
	return errors.Join(o.primaryErr, o.cleanupErr)
}
