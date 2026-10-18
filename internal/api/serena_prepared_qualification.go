package api

import (
	"context"
	"encoding/json"
	"errors"
	"mcp-local-hub/internal/process"
	"path/filepath"
	"time"
)

type PreparedSerenaQualificationRequest struct {
	QualificationID               string
	Request                       PreparedSerenaRequest
	Inputs                        SerenaPreparationInputs
	Roots                         PreparedSerenaOwnedRoots
	Installation                  PreparedSerenaInstallation
	WorkDeadline, CleanupDeadline time.Time
	Resources                     *PreparedSerenaQualificationResources
}
type PreparedSerenaQualificationPort interface {
	QualifyUnpublished(context.Context, PreparedSerenaQualificationRequest) (*PreparedSerenaQualificationObservation, error)
}
type PreparedSerenaQualificationEvidence interface {
	ValidateOriginalQualification(context.Context, PreparedSerenaQualificationRequest) error
	NativeSettlement() *process.ContainedProcessSettlement
	OriginalIO() PreparedSerenaQualificationIOEvidence
	ProtocolEvidence() PreparedSerenaProtocolEvidence
	TerminationObservation() PreparedSerenaQualificationTerminationV1
}
type PreparedSerenaNoChildQualificationEvidence interface {
	ValidateNoNativeFactoryRelease(context.Context, PreparedSerenaQualificationRequest) error
}
type PreparedSerenaQualificationIOEvidence interface {
	ValidateOriginalQualificationIO(context.Context, PreparedSerenaQualificationRequest) error
	ReceiptSHA256() string
}
type PreparedSerenaProtocolEvidence interface {
	ValidateOriginalQualificationProtocol(context.Context, PreparedSerenaQualificationRequest) error
	ReceiptSHA256() string
}
type PreparedSerenaQualificationTerminationV1 struct {
	Disposition                                                                                                 string
	NativeExitDefined                                                                                           bool
	NativeExitCode                                                                                              *int
	NativeExitError                                                                                             error
	OwnedActionReceiptSHA256                                                                                    string
	ProtocolReceivedSequence, OwnedActionStartedSequence, OwnedActionCompletedSequence, ResourceClosureSequence uint64
}
type PreparedSerenaQualificationSnapshotV1 struct {
	Schema                      string   `json:"schema"`
	Version                     int      `json:"version"`
	OperationID                 string   `json:"operation_id"`
	QualificationID             string   `json:"qualification_id"`
	GenerationID                string   `json:"generation_id"`
	RequestIdentitySHA256       string   `json:"request_identity_sha256"`
	SourceSHA256                string   `json:"source_sha256"`
	EnvironmentProjectionSHA256 string   `json:"environment_projection_sha256"`
	InstallationReceiptSHA256   string   `json:"installation_receipt_sha256"`
	EntrypointSHA256            string   `json:"entrypoint_sha256"`
	ProtocolReceiptSHA256       *string  `json:"protocol_receipt_sha256"`
	NativeReceiptSHA256         *string  `json:"native_receipt_sha256"`
	IOReceiptSHA256             *string  `json:"io_receipt_sha256"`
	TerminationDisposition      string   `json:"termination_disposition"`
	NativeExitDefined           bool     `json:"native_exit_defined"`
	NativeExitCode              *int     `json:"native_exit_code"`
	NativeExitError             *string  `json:"native_exit_error"`
	OwnedActionStarted          bool     `json:"owned_action_started"`
	OwnedActionCompleted        bool     `json:"owned_action_completed"`
	ProtocolComplete            bool     `json:"protocol_complete"`
	ResourcesComplete           bool     `json:"resources_complete"`
	Complete                    bool     `json:"complete"`
	PrimaryError                *string  `json:"primary_error"`
	CleanupErrors               []string `json:"cleanup_errors"`
}

// Only the received API factory creates this token. The DTO above cannot
// attest protocol, original listener, native or IO completion.
type PreparedSerenaQualificationObservation struct {
	request          PreparedSerenaQualificationRequest
	evidence         PreparedSerenaQualificationEvidence
	view             PreparedSerenaQualificationSnapshotV1
	primary, cleanup error
	closure          PreparedSerenaExecutionClosure
	resourceClose    *PreparedSerenaQualificationResourceCloseObservation
	noChildSource    PreparedSerenaNoChildQualificationEvidence
}
type receivedQualificationNoChildEvidence struct {
	source    PreparedSerenaNoChildQualificationEvidence
	resources *PreparedSerenaQualificationResources
}

func (e *receivedQualificationNoChildEvidence) Complete() bool {
	return e != nil && e.source != nil && e.resources != nil
}
func ReceiveUnpublishedSerenaNoChildQualification(ctx context.Context, r PreparedSerenaQualificationRequest, e PreparedSerenaNoChildQualificationEvidence) (*PreparedSerenaQualificationObservation, error) {
	if err := r.validate(ctx, true); err != nil {
		return nil, err
	}
	if e == nil || r.Resources == nil {
		return nil, errors.New("SERENA_UNPUBLISHED_NO_CHILD_SOURCE_MISSING")
	}
	if err := e.ValidateNoNativeFactoryRelease(ctx, r); err != nil {
		return nil, err
	}
	digest := preparedSerenaHash([]byte(r.Request.OperationID + "/" + r.QualificationID + "/received-original-no-native-factory"))
	closure := PreparedSerenaExecutionClosure{nativeEvidence: &receivedQualificationNoChildEvidence{e, r.Resources}, OriginalOperationID: r.Request.OperationID, NoChildKnown: true, NoChildReleased: true, HandlesClosed: true, IOClosed: true, ReceiptSHA256: digest}
	close, closeErr := r.Resources.finishResources(ctx, &closure)
	key, _ := r.Request.Identity()
	o := &PreparedSerenaQualificationObservation{request: r, noChildSource: e, closure: closure, resourceClose: close, cleanup: closeErr}
	o.view = PreparedSerenaQualificationSnapshotV1{Schema: "serena-unpublished-qualification-v1", Version: 1, OperationID: r.Request.OperationID, QualificationID: r.QualificationID, GenerationID: r.Request.GenerationID, RequestIdentitySHA256: key, SourceSHA256: r.Installation.SourceSHA256, EnvironmentProjectionSHA256: r.Inputs.EnvironmentProjectionSHA256, InstallationReceiptSHA256: r.Installation.Closure.ReceiptSHA256, EntrypointSHA256: r.Installation.Entrypoint.SHA256, TerminationDisposition: "refused", ResourcesComplete: closeErr == nil && close != nil && close.Validate(ctx, r.Resources) == nil, CleanupErrors: []string{}}
	o.primary = errors.New("SERENA_UNPUBLISHED_NO_CHILD_REFUSED")
	o.view.PrimaryError = qualificationErrorPointer(o.primary)
	if closeErr != nil {
		o.view.CleanupErrors = append(o.view.CleanupErrors, closeErr.Error())
	}
	return o, errors.Join(o.primary, closeErr)
}

func qualificationErrorPointer(err error) *string {
	if err == nil {
		return nil
	}
	v := err.Error()
	return &v
}

// Receiving derives authority from the original evidence objects, never the
// public snapshot. Even a failed protocol can discharge its original resources
// when the original native/IO owner positively settles them.
func ReceiveUnpublishedSerenaQualification(ctx context.Context, r PreparedSerenaQualificationRequest, evidence PreparedSerenaQualificationEvidence) (observed *PreparedSerenaQualificationObservation, retErr error) {
	if err := r.validate(ctx, true); err != nil {
		return nil, err
	}
	if evidence == nil || r.Resources == nil || r.Resources.request != r.Request || r.Resources.qualificationID != r.QualificationID {
		return nil, errors.New("SERENA_UNPUBLISHED_EVIDENCE_MISSING")
	}
	key, _ := r.Request.Identity()
	observed = &PreparedSerenaQualificationObservation{request: r, evidence: evidence}
	observed.view = PreparedSerenaQualificationSnapshotV1{Schema: "serena-unpublished-qualification-v1", Version: 1, OperationID: r.Request.OperationID, QualificationID: r.QualificationID, GenerationID: r.Request.GenerationID, RequestIdentitySHA256: key, SourceSHA256: r.Installation.SourceSHA256, EnvironmentProjectionSHA256: r.Inputs.EnvironmentProjectionSHA256, InstallationReceiptSHA256: r.Installation.Closure.ReceiptSHA256, EntrypointSHA256: r.Installation.Entrypoint.SHA256, CleanupErrors: []string{}}
	defer func() {
		observed.primary = retErr
		observed.view.PrimaryError = qualificationErrorPointer(retErr)
		observed.view.Complete = retErr == nil && observed.view.ProtocolComplete && observed.view.ResourcesComplete
	}()
	if err := evidence.ValidateOriginalQualification(ctx, r); err != nil {
		return observed, err
	}
	native := evidence.NativeSettlement()
	ioEvidence := evidence.OriginalIO()
	if native == nil || !native.Complete() || ioEvidence == nil {
		return observed, errors.New("SERENA_UNPUBLISHED_ORIGINAL_CLOSURE_PENDING")
	}
	if err := ioEvidence.ValidateOriginalQualificationIO(ctx, r); err != nil {
		return observed, err
	}
	ioSHA := ioEvidence.ReceiptSHA256()
	if !preparedSerenaDigest(ioSHA) {
		return observed, errors.New("SERENA_UNPUBLISHED_IO_RECEIPT_INVALID")
	}
	view := native.Snapshot()
	nativeBytes, encodeErr := json.Marshal(struct {
		Identity                                      process.ContainedRootIdentity
		RootWaitKnown                                 bool
		ExitCode                                      int
		TerminalAction                                string
		MembersExited, AccountingKnown, HandlesClosed bool
		FinalActive                                   uint32
		MemberPIDs                                    []uint32
		Error                                         *string
	}{view.RootIdentity, view.RootWaitKnown, view.RootExitCode, view.TerminalAction, view.MembersExited, view.AccountingKnown, view.HandlesClosed, view.FinalActive, view.CapturedMemberPIDs, qualificationErrorPointer(view.Error)})
	if encodeErr != nil {
		return observed, encodeErr
	}
	nativeSHA := preparedSerenaHash(nativeBytes)
	observed.view.NativeReceiptSHA256 = &nativeSHA
	observed.view.IOReceiptSHA256 = &ioSHA
	receiptBytes, _ := json.Marshal(struct{ Native, IO, Operation, Qualification string }{nativeSHA, ioSHA, r.Request.OperationID, r.QualificationID})
	observed.closure = PreparedSerenaExecutionClosure{nativeEvidence: native, OriginalOperationID: r.Request.OperationID, RootExitKnown: view.RootWaitKnown, RootExitCode: view.RootExitCode, MembersExited: view.MembersExited, HandlesClosed: view.HandlesClosed, IOClosed: true, NoChildKnown: view.NoChildKnown, NoChildReleased: view.NoChildKnown && view.HandlesClosed, ReceiptSHA256: preparedSerenaHash(receiptBytes)}
	// Only a received original closure can admit owned qualification-root removal.
	observed.resourceClose, observed.cleanup = r.Resources.finishResources(ctx, &observed.closure)
	if observed.cleanup != nil {
		observed.view.CleanupErrors = append(observed.view.CleanupErrors, observed.cleanup.Error())
	}
	observed.view.ResourcesComplete = observed.cleanup == nil && observed.resourceClose != nil && observed.resourceClose.Validate(ctx, r.Resources) == nil
	termination := evidence.TerminationObservation()
	observed.view.TerminationDisposition = termination.Disposition
	observed.view.NativeExitDefined = termination.NativeExitDefined
	observed.view.NativeExitCode = termination.NativeExitCode
	observed.view.NativeExitError = qualificationErrorPointer(termination.NativeExitError)
	if !view.RootIdentity.Known || !view.RootWaitKnown || !termination.NativeExitDefined || termination.NativeExitCode == nil || *termination.NativeExitCode != view.RootExitCode {
		return observed, errors.Join(observed.cleanup, errors.New("SERENA_UNPUBLISHED_EXIT_UNDEFINED_OR_DRIFTED"))
	}
	protocol := evidence.ProtocolEvidence()
	if protocol == nil {
		return observed, errors.Join(observed.cleanup, errors.New("SERENA_UNPUBLISHED_PROTOCOL_MISSING"))
	}
	if err := protocol.ValidateOriginalQualificationProtocol(ctx, r); err != nil {
		return observed, errors.Join(observed.cleanup, err)
	}
	protocolSHA := protocol.ReceiptSHA256()
	if !preparedSerenaDigest(protocolSHA) {
		return observed, errors.Join(observed.cleanup, errors.New("SERENA_UNPUBLISHED_PROTOCOL_RECEIPT_INVALID"))
	}
	observed.view.ProtocolReceiptSHA256 = &protocolSHA
	observed.view.ProtocolComplete = true
	switch termination.Disposition {
	case "natural_success":
		if view.RootExitCode != 0 || termination.NativeExitError != nil || view.TerminalAction != "not-requested" || termination.OwnedActionStartedSequence != 0 || termination.OwnedActionCompletedSequence != 0 || termination.OwnedActionReceiptSHA256 != "" {
			return observed, errors.Join(observed.cleanup, errors.New("SERENA_UNPUBLISHED_NATURAL_OUTCOME_INVALID"))
		}
	case "protocol_qualified_owned_stop":
		if view.TerminalAction != "request-succeeded" || !preparedSerenaDigest(termination.OwnedActionReceiptSHA256) || termination.ProtocolReceivedSequence == 0 || termination.OwnedActionStartedSequence <= termination.ProtocolReceivedSequence || termination.OwnedActionCompletedSequence <= termination.OwnedActionStartedSequence || termination.ResourceClosureSequence <= termination.OwnedActionCompletedSequence {
			return observed, errors.Join(observed.cleanup, errors.New("SERENA_UNPUBLISHED_OWNED_STOP_CHRONOLOGY_INVALID"))
		}
		observed.view.OwnedActionStarted = true
		observed.view.OwnedActionCompleted = true
	default:
		return observed, errors.Join(observed.cleanup, errors.New("SERENA_UNPUBLISHED_TERMINATION_NOT_QUALIFIED"))
	}
	if termination.ProtocolReceivedSequence == 0 || termination.ResourceClosureSequence <= termination.ProtocolReceivedSequence {
		return observed, errors.Join(observed.cleanup, errors.New("SERENA_UNPUBLISHED_PROTOCOL_CLOSURE_ORDER_INVALID"))
	}
	return observed, observed.cleanup
}
func (o *PreparedSerenaQualificationObservation) Snapshot() PreparedSerenaQualificationSnapshotV1 {
	if o == nil {
		return PreparedSerenaQualificationSnapshotV1{}
	}
	raw, _ := json.Marshal(o.view)
	var v PreparedSerenaQualificationSnapshotV1
	_ = json.Unmarshal(raw, &v)
	return v
}
func (o *PreparedSerenaQualificationObservation) Validate(ctx context.Context, r PreparedSerenaQualificationRequest) error {
	if o == nil || o.evidence == nil || !o.view.Complete || o.primary != nil || o.cleanup != nil || o.request.Resources != r.Resources || o.request.Request != r.Request || o.request.QualificationID != r.QualificationID {
		return errors.New("SERENA_UNPUBLISHED_OBSERVATION_INCOMPLETE")
	}
	if err := r.validate(ctx, true); err != nil {
		return err
	}
	if err := o.resourceClose.Validate(ctx, r.Resources); err != nil {
		return err
	}
	return o.evidence.ValidateOriginalQualification(ctx, r)
}
func (o *PreparedSerenaQualificationObservation) ReceiptSHA256() string {
	if o == nil || !o.view.Complete {
		return ""
	}
	raw, _ := json.Marshal(o.view)
	return preparedSerenaHash(raw)
}

func (r PreparedSerenaQualificationRequest) validate(ctx context.Context, receiving bool) error {
	if ctx == nil || !preparedSerenaID(r.QualificationID) || r.Inputs.Request != r.Request || r.WorkDeadline.IsZero() || r.CleanupDeadline.IsZero() || !r.WorkDeadline.Equal(r.Request.WorkDeadline) || !r.CleanupDeadline.Equal(r.Request.AbsoluteCleanupDeadline) || !preparedSerenaDigest(r.Inputs.EnvironmentProjectionSHA256) {
		return errors.New("SERENA_UNPUBLISHED_QUALIFICATION_INPUT_INVALID")
	}
	key, err := r.Request.Identity()
	if err != nil {
		return err
	}
	if !preparedSerenaFile(r.Roots.Generation) || !preparedSerenaFile(r.Roots.Attempt) || r.Roots.Generation.Path != filepath.Join(r.Request.Namespace, key, "generations", r.Request.GenerationID) || r.Roots.Attempt.Path != filepath.Join(r.Request.Namespace, key, "attempts", r.Request.OperationID) || !r.Installation.Closure.complete(r.Request.OperationID) || !r.Installation.Closure.RootExitKnown || r.Installation.Closure.RootExitCode != 0 || !preparedSerenaFile(r.Installation.Entrypoint) || !preparedSerenaDigest(r.Installation.InventorySHA256) {
		return errors.New("SERENA_UNPUBLISHED_INSTALLATION_UNQUALIFIED")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	deadline, defined := ctx.Deadline()
	bound := r.WorkDeadline
	if receiving {
		bound = r.CleanupDeadline
	}
	if !defined || deadline.After(bound) {
		return errors.New("SERENA_UNPUBLISHED_QUALIFICATION_DEADLINE_RENEWED")
	}
	return nil
}
func ValidateUnpublishedSerenaQualificationRequest(ctx context.Context, r PreparedSerenaQualificationRequest) error {
	if err := r.validate(ctx, false); err != nil {
		return err
	}
	return revalidatePreparedSerenaFile(ctx, r.Installation.Entrypoint)
}
