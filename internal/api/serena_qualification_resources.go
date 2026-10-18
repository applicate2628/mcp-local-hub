package api

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strconv"
	"sync"
)

type PreparedSerenaQualificationResources struct {
	mu                          sync.Mutex
	request                     PreparedSerenaRequest
	qualificationID, key        string
	installation                PreparedSerenaInstallation
	tree                        *SerenaOwnedTree
	listener                    net.Listener
	port                        int
	releaseAttempted            bool
	release                     *PreparedSerenaReservationCloseObservation
	handedOff, cleanupAttempted bool
	cleanup                     *PreparedSerenaQualificationResourceCloseObservation
}
type PreparedSerenaQualificationResourceCloseObservation struct {
	owner           *PreparedSerenaQualificationResources
	tree            SerenaTreeCleanupReceipt
	closed, inBound bool
	err             error
}

func (o *PreparedSerenaQualificationResourceCloseObservation) Validate(ctx context.Context, r *PreparedSerenaQualificationResources) error {
	if ctx == nil || ctx.Err() != nil || o == nil || r == nil || o.owner != r || !o.closed || !o.inBound || o.err != nil {
		return errors.New("SERENA_QUALIFICATION_RESOURCES_UNSETTLED")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cleanup != o {
		return errors.New("SERENA_QUALIFICATION_RESOURCE_CLOSE_FOREIGN")
	}
	return nil
}

type PreparedSerenaReservationCloseObservation struct {
	owner           *PreparedSerenaQualificationResources
	closed, inBound bool
	err             error
}

func (o *PreparedSerenaReservationCloseObservation) Validate(resources *PreparedSerenaQualificationResources) error {
	if o == nil || resources == nil || o.owner != resources || !o.closed || !o.inBound || o.err != nil {
		return errors.New("SERENA_QUALIFICATION_RESERVATION_UNSETTLED")
	}
	resources.mu.Lock()
	defer resources.mu.Unlock()
	if resources.release != o {
		return errors.New("SERENA_QUALIFICATION_RESERVATION_FOREIGN")
	}
	return nil
}

type PreparedSerenaQualificationHostInputs struct {
	WorkingDirectory, SerenaHome, TemporaryDirectory                  string
	Entrypoint                                                        PreparedSerenaFile
	Args, Environment                                                 []string
	Host, Transport                                                   string
	Port                                                              int
	OperationID, QualificationID, GenerationID, RequestIdentitySHA256 string
}

func AcquireUnpublishedSerenaQualificationResources(ctx context.Context, r PreparedSerenaRequest, installed PreparedSerenaInstallation, qualificationID string) (resource *PreparedSerenaQualificationResources, retErr error) {
	key, err := r.Identity()
	if err != nil {
		return nil, err
	}
	if ctx == nil || !preparedSerenaID(qualificationID) || !installed.Closure.complete(r.OperationID) || !installed.Closure.RootExitKnown || installed.Closure.RootExitCode != 0 || !preparedSerenaFile(installed.Entrypoint) {
		return nil, errors.New("SERENA_QUALIFICATION_RESOURCE_INPUT_INVALID")
	}
	deadline, ok := ctx.Deadline()
	if !ok || deadline.After(r.WorkDeadline) || ctx.Err() != nil {
		return nil, errors.New("SERENA_QUALIFICATION_RESOURCE_DEADLINE_INVALID")
	}
	resource = &PreparedSerenaQualificationResources{request: r, installation: installed, qualificationID: qualificationID, key: key}
	resource.tree, err = AcquireSerenaOwnedTree(ctx, SerenaOwnedTreeRequest{Namespace: r.Namespace, PackageIdentitySHA256: key, OperationID: r.OperationID, GenerationID: r.GenerationID, Kind: "qualification", QualificationID: qualificationID, Limits: r.OwnedTreeLimits})
	if err != nil {
		return resource, err
	}
	resource.listener, err = NewListenerWithSOExclusiveContext(ctx, "127.0.0.1:0")
	if err != nil {
		return resource, err
	}
	address, ok := resource.listener.Addr().(*net.TCPAddr)
	if !ok || address.IP.To4() == nil || !address.IP.Equal(net.IPv4(127, 0, 0, 1)) || address.Port <= 0 || address.Port > 65535 {
		return resource, errors.New("SERENA_QUALIFICATION_RESERVATION_ADDRESS_INVALID")
	}
	resource.port = address.Port
	return resource, ctx.Err()
}
func (r *PreparedSerenaQualificationResources) ReleaseReservationObserved(ctx context.Context) (*PreparedSerenaReservationCloseObservation, error) {
	return r.releaseReservation(ctx, false)
}
func (r *PreparedSerenaQualificationResources) releaseReservation(ctx context.Context, cleanup bool) (*PreparedSerenaReservationCloseObservation, error) {
	if r == nil || ctx == nil {
		return nil, errors.New("SERENA_QUALIFICATION_RESERVATION_UNAVAILABLE")
	}
	r.mu.Lock()
	if r.release != nil {
		proof := r.release
		r.mu.Unlock()
		return proof, proof.err
	}
	if r.releaseAttempted {
		r.mu.Unlock()
		return nil, errors.New("SERENA_QUALIFICATION_RESERVATION_CLOSE_PENDING")
	}
	r.releaseAttempted = true
	listener := r.listener
	r.mu.Unlock()
	var err error
	if listener != nil {
		err = listener.Close()
	}
	deadline, ok := ctx.Deadline()
	bound := r.request.WorkDeadline
	if cleanup {
		bound = r.request.AbsoluteCleanupDeadline
	}
	inBound := ok && !deadline.After(bound) && ctx.Err() == nil
	proof := &PreparedSerenaReservationCloseObservation{owner: r, closed: err == nil, inBound: inBound, err: errors.Join(err, ctx.Err())}
	r.mu.Lock()
	r.release = proof
	r.mu.Unlock()
	return proof, proof.err
}
func (r *PreparedSerenaQualificationResources) HostInputs(ctx context.Context, request PreparedSerenaQualificationRequest) (PreparedSerenaQualificationHostInputs, error) {
	if err := request.validate(ctx, false); err != nil {
		return PreparedSerenaQualificationHostInputs{}, err
	}
	if r == nil || r.tree == nil || request.Resources != r || request.Request != r.request || request.QualificationID != r.qualificationID || request.Installation.Entrypoint != r.installation.Entrypoint || ctx == nil || ctx.Err() != nil {
		return PreparedSerenaQualificationHostInputs{}, errors.New("SERENA_QUALIFICATION_RESOURCE_BINDING_MISMATCH")
	}
	r.mu.Lock()
	reservation := r.release
	port := r.port
	r.mu.Unlock()
	if port <= 0 || port > 65535 || reservation == nil || reservation.Validate(r) != nil {
		return PreparedSerenaQualificationHostInputs{}, errors.New("SERENA_QUALIFICATION_ORIGINAL_RESERVATION_NOT_RELEASED")
	}
	root := filepath.Join(r.request.Namespace, r.key, "qualifications", r.qualificationID)
	return PreparedSerenaQualificationHostInputs{WorkingDirectory: root, SerenaHome: root, TemporaryDirectory: root, Entrypoint: r.installation.Entrypoint, Args: []string{"start-mcp-server", "--transport", "streamable-http", "--host", "127.0.0.1", "--port", strconv.Itoa(r.port), "--enable-web-dashboard", "false", "--open-web-dashboard", "false", "--enable-gui-log-window", "false"}, Environment: preparedSerenaEnvironmentWith(request.Inputs.Environment, map[string]string{"SERENA_HOME": root, "TMP": root, "TEMP": root, "PYTHONDONTWRITEBYTECODE": "1"}), Host: "127.0.0.1", Transport: "streamable-http", Port: r.port, OperationID: r.request.OperationID, QualificationID: r.qualificationID, GenerationID: r.request.GenerationID, RequestIdentitySHA256: r.key}, nil
}

// Called by the configured original-host before-create admission, after its
// exact executable/args/env comparison. Constructor-only failures retain the
// positively observed unstarted resource branch.
func (r *PreparedSerenaQualificationResources) BeginOriginalHostFactory(ctx context.Context, request PreparedSerenaQualificationRequest) error {
	if _, err := r.HostInputs(ctx, request); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handedOff || r.cleanupAttempted {
		return errors.New("SERENA_QUALIFICATION_RESOURCE_HANDOFF_REPLAY")
	}
	r.handedOff = true
	return nil
}

// Constructor failure is finalized only before host handoff. After handoff,
// cleanup requires the SAME original host's native and IO evidence.
func (r *PreparedSerenaQualificationResources) CloseUnstartedObserved(ctx context.Context) (*PreparedSerenaQualificationResourceCloseObservation, error) {
	if r == nil {
		return nil, errors.New("SERENA_QUALIFICATION_RESOURCES_MISSING")
	}
	r.mu.Lock()
	handedOff := r.handedOff
	r.mu.Unlock()
	if handedOff {
		return nil, errors.New("SERENA_QUALIFICATION_HOST_HANDOFF_REQUIRES_ORIGINAL_RECEIPT")
	}
	return r.finishResources(ctx, nil)
}

type qualificationUnstartedNativeEvidence struct {
	owner *PreparedSerenaQualificationResources
}

func (e *qualificationUnstartedNativeEvidence) Complete() bool {
	if e == nil || e.owner == nil {
		return false
	}
	e.owner.mu.Lock()
	defer e.owner.mu.Unlock()
	return !e.owner.handedOff
}
func (r *PreparedSerenaQualificationResources) finishResources(ctx context.Context, closure *PreparedSerenaExecutionClosure) (*PreparedSerenaQualificationResourceCloseObservation, error) {
	if r == nil || ctx == nil {
		return nil, errors.New("SERENA_QUALIFICATION_RESOURCES_MISSING")
	}
	deadline, defined := ctx.Deadline()
	if !defined || deadline.After(r.request.AbsoluteCleanupDeadline) || ctx.Err() != nil {
		return nil, errors.New("SERENA_QUALIFICATION_ORIGINAL_CLEANUP_WINDOW_UNAVAILABLE")
	}
	r.mu.Lock()
	if r.cleanup != nil {
		o := r.cleanup
		r.mu.Unlock()
		return o, o.err
	}
	if r.cleanupAttempted {
		r.mu.Unlock()
		return nil, errors.New("SERENA_QUALIFICATION_RESOURCE_CLOSE_PENDING")
	}
	if r.handedOff && (closure == nil || !closure.complete(r.request.OperationID)) {
		r.mu.Unlock()
		return nil, errors.New("SERENA_QUALIFICATION_ORIGINAL_HOST_UNSETTLED")
	}
	r.cleanupAttempted = true
	r.mu.Unlock()
	if closure == nil {
		digest := preparedSerenaHash([]byte(r.request.OperationID + "/" + r.qualificationID + "/no-host-handoff"))
		closure = &PreparedSerenaExecutionClosure{nativeEvidence: &qualificationUnstartedNativeEvidence{r}, OriginalOperationID: r.request.OperationID, NoChildKnown: true, NoChildReleased: true, HandlesClosed: true, IOClosed: true, ReceiptSHA256: digest}
	}
	reservation, err := r.releaseReservation(ctx, true)
	if reservation != nil {
		err = errors.Join(err, reservation.Validate(r))
	}
	var tree SerenaTreeCleanupReceipt
	if r.tree != nil {
		var treeErr error
		tree, treeErr = r.tree.RemoveOwned(ctx, true, *closure)
		err = errors.Join(err, treeErr)
		if !tree.Complete() {
			err = errors.Join(err, errors.New("SERENA_QUALIFICATION_TREE_CLEANUP_PENDING"))
		}
	}
	deadline, defined = ctx.Deadline()
	inBound := defined && !deadline.After(r.request.AbsoluteCleanupDeadline) && ctx.Err() == nil
	err = errors.Join(err, ctx.Err())
	if !inBound {
		err = errors.Join(err, errors.New("SERENA_QUALIFICATION_CLEANUP_DEADLINE_INVALID"))
	}
	o := &PreparedSerenaQualificationResourceCloseObservation{owner: r, tree: tree, closed: err == nil, inBound: inBound, err: err}
	r.mu.Lock()
	r.cleanup = o
	r.mu.Unlock()
	return o, err
}
