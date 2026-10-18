package process

import (
	"context"
	"errors"
)

type OriginalJobMemberSource interface{ originalMemberSourceBinding() *originalMemberSource }
type originalMemberSource struct {
	gate       *originalMemberCustody
	available  func() error
	acquire    func(context.Context, *OriginalJobMemberObservation) error
	revalidate func(context.Context, *OriginalJobMemberObservation, uint32) error
}

func (s *originalMemberSource) originalMemberSourceBinding() *originalMemberSource { return s }

type OriginalJobMemberRowV1 struct {
	PID                 uint32 `json:"pid"`
	NativeBirthFILETIME uint64 `json:"native_birth_filetime"`
	ImagePathSHA256     string `json:"image_path_sha256"`
	PrincipalSIDSHA256  string `json:"principal_sid_sha256"`
	MembershipDefined   bool   `json:"membership_defined"`
	IsMember            bool   `json:"is_member"`
	LiveDefined         bool   `json:"live_defined"`
	Live                bool   `json:"live"`
}

type OriginalJobMemberResourceV1 struct {
	PID              uint32              `json:"pid"`
	BorrowedOriginal bool                `json:"borrowed_original"`
	Acquired         bool                `json:"acquired"`
	CloseAttempted   bool                `json:"close_attempted"`
	Closed           bool                `json:"closed"`
	CloseError       *SIDFormatFailureV1 `json:"close_error"`
}

type OriginalJobMemberSnapshotV1 struct {
	Schema                 string                        `json:"schema"`
	Version                uint32                        `json:"version"`
	OriginalCreationSHA256 string                        `json:"original_creation_sha256"`
	OriginalJobSHA256      string                        `json:"original_job_sha256"`
	MembershipSHA256       string                        `json:"membership_sha256"`
	PreTotal               uint32                        `json:"pre_total"`
	PreActive              uint32                        `json:"pre_active"`
	PostTotal              uint32                        `json:"post_total"`
	PostActive             uint32                        `json:"post_active"`
	Rows                   []OriginalJobMemberRowV1      `json:"rows"`
	Resources              []OriginalJobMemberResourceV1 `json:"resources"`
	Complete               bool                          `json:"complete"`
	PrimaryError           *SIDFormatFailureV1           `json:"primary_error"`
	CleanupErrors          []SIDFormatFailureV1          `json:"cleanup_errors"`
}

type originalMemberResource struct {
	view     OriginalJobMemberResourceV1
	close    func() error
	closeErr error
	native   any
}
type OriginalJobMemberObservation struct {
	source                               *originalMemberSource
	view                                 OriginalJobMemberSnapshotV1
	resources                            []*originalMemberResource
	closing, finalizerStarted, finalized bool
	calls                                int
	primaryErr, cleanupErr               error
	closeObservation                     *OriginalJobMemberCloseObservation
}

type OriginalJobMemberCloseSnapshotV1 struct {
	Schema        string                        `json:"schema"`
	Version       uint32                        `json:"version"`
	Resources     []OriginalJobMemberResourceV1 `json:"resources"`
	Pending       bool                          `json:"pending"`
	Complete      bool                          `json:"complete"`
	CleanupErrors []SIDFormatFailureV1          `json:"cleanup_errors"`
}
type OriginalJobMemberCloseObservation struct {
	owner *OriginalJobMemberObservation
	view  OriginalJobMemberCloseSnapshotV1
}

func (o *OriginalJobMemberObservation) Snapshot() OriginalJobMemberSnapshotV1 {
	if o == nil || o.source == nil {
		return OriginalJobMemberSnapshotV1{}
	}
	o.source.gate.mu.Lock()
	defer o.source.gate.mu.Unlock()
	v := o.view
	v.Rows = append([]OriginalJobMemberRowV1{}, v.Rows...)
	v.Resources = append([]OriginalJobMemberResourceV1{}, v.Resources...)
	v.CleanupErrors = append([]SIDFormatFailureV1{}, v.CleanupErrors...)
	if v.PrimaryError != nil {
		x := *v.PrimaryError
		v.PrimaryError = &x
	}
	for i := range v.Resources {
		if v.Resources[i].CloseError != nil {
			x := *v.Resources[i].CloseError
			v.Resources[i].CloseError = &x
		}
	}
	return v
}

func observeOriginalJobMembers(ctx context.Context, source *originalMemberSource) (*OriginalJobMemberObservation, error) {
	if err := creatorClosureContext(ctx); err != nil {
		return nil, err
	}
	if source == nil || source.gate == nil || source.acquire == nil || source.revalidate == nil {
		return nil, errors.New("original member source unavailable")
	}
	o := &OriginalJobMemberObservation{source: source, view: OriginalJobMemberSnapshotV1{Schema: "original-job-members-v1", Version: 1, Rows: []OriginalJobMemberRowV1{}, Resources: []OriginalJobMemberResourceV1{}, CleanupErrors: []SIDFormatFailureV1{}}}
	if err := source.gate.reserve(o); err != nil {
		return nil, err
	}
	exit, err := source.gate.enterQuery(o)
	if err != nil {
		_, closeErr := o.CloseObserved()
		return o, errors.Join(err, closeErr)
	}
	err = source.acquire(ctx, o)
	if err != nil {
		o.primaryErr = err
		o.view.PrimaryError = &SIDFormatFailureV1{Code: "ORIGINAL_MEMBER_ACQUISITION_FAILED"}
	} else {
		o.view.Complete = true
	}
	exit()
	if err != nil {
		_, closeErr := o.CloseObserved()
		return o, errors.Join(err, closeErr)
	}
	return o, nil
}

func (o *OriginalJobMemberObservation) ValidateSource(ctx context.Context, source OriginalJobMemberSource) error {
	if o == nil || source == nil || o.source == nil || source.originalMemberSourceBinding() != o.source {
		return errors.New("same original creator member source required")
	}
	return o.Revalidate(ctx)
}

func (o *OriginalJobMemberObservation) Revalidate(ctx context.Context) error { return o.match(ctx, 0) }
func (o *OriginalJobMemberObservation) MatchOwnerPID(ctx context.Context, pid uint32) error {
	if pid == 0 {
		return errors.New("positive original member PID required")
	}
	return o.match(ctx, pid)
}
func (o *OriginalJobMemberObservation) match(ctx context.Context, pid uint32) error {
	if err := creatorClosureContext(ctx); err != nil {
		return err
	}
	if o == nil || o.source == nil {
		return errors.New("original member observation unavailable")
	}
	exit, err := o.source.gate.enterQuery(o)
	if err != nil {
		return err
	}
	defer exit()
	if !o.view.Complete || o.primaryErr != nil {
		return errors.Join(errors.New("original member acquisition incomplete"), o.primaryErr)
	}
	return o.source.revalidate(ctx, o, pid)
}

func (o *OriginalJobMemberObservation) CloseObserved() (*OriginalJobMemberCloseObservation, error) {
	if o == nil || o.source == nil || o.source.gate == nil {
		return nil, errors.New("original member observation required")
	}
	g := o.source.gate
	g.mu.Lock()
	o.closing = true
	if o.finalized {
		result, err := o.closeObservation, o.cleanupErr
		g.mu.Unlock()
		return result, err
	}
	run := o.calls == 0 && !o.finalizerStarted
	if run {
		o.finalizerStarted = true
	}
	g.mu.Unlock()
	if run {
		o.finalizeClose()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !o.finalized {
		return &OriginalJobMemberCloseObservation{owner: o, view: OriginalJobMemberCloseSnapshotV1{Schema: "original-job-member-close-v1", Version: 1, Pending: true, Resources: []OriginalJobMemberResourceV1{}, CleanupErrors: []SIDFormatFailureV1{}}}, &OriginalMemberBorrowPendingError{Stage: "member-finalizer"}
	}
	return o.closeObservation, o.cleanupErr
}

func (o *OriginalJobMemberObservation) finalizeClose() {
	var cleanup error
	rows := make([]OriginalJobMemberResourceV1, 0, len(o.resources))
	failures := []SIDFormatFailureV1{}
	for _, r := range o.resources {
		if r.view.Acquired && !r.view.BorrowedOriginal && !r.view.CloseAttempted {
			r.view.CloseAttempted = true
			if r.close == nil {
				r.closeErr = errors.New("original member closer missing")
			} else {
				r.closeErr = r.close()
			}
			r.view.Closed = r.closeErr == nil
			if r.closeErr != nil {
				r.view.CloseError = &SIDFormatFailureV1{Code: "ORIGINAL_MEMBER_HANDLE_CLOSE_FAILED"}
				failures = append(failures, *r.view.CloseError)
			}
		}
		cleanup = errors.Join(cleanup, r.closeErr)
		rows = append(rows, r.view)
	}
	g := o.source.gate
	g.mu.Lock()
	o.cleanupErr = cleanup
	o.view.Resources = rows
	o.view.CleanupErrors = failures
	o.finalized = true
	o.closeObservation = &OriginalJobMemberCloseObservation{owner: o, view: OriginalJobMemberCloseSnapshotV1{Schema: "original-job-member-close-v1", Version: 1, Resources: rows, Complete: cleanup == nil, CleanupErrors: failures}}
	if g.active == o {
		g.active = nil
	}
	g.updateDrainLocked()
	g.mu.Unlock()
}

func (o *OriginalJobMemberCloseObservation) Snapshot() OriginalJobMemberCloseSnapshotV1 {
	if o == nil {
		return OriginalJobMemberCloseSnapshotV1{}
	}
	v := o.view
	v.Resources = append([]OriginalJobMemberResourceV1{}, v.Resources...)
	v.CleanupErrors = append([]SIDFormatFailureV1{}, v.CleanupErrors...)
	for i := range v.Resources {
		if v.Resources[i].CloseError != nil {
			x := *v.Resources[i].CloseError
			v.Resources[i].CloseError = &x
		}
	}
	return v
}
func (o *OriginalJobMemberCloseObservation) Validate(source OriginalJobMemberSource) error {
	if o == nil || o.owner == nil || source == nil || o.owner.source != source.originalMemberSourceBinding() || o.owner.closeObservation != o || !o.view.Complete || o.view.Pending {
		return errors.New("same original finalized member close required")
	}
	return o.owner.cleanupErr
}
