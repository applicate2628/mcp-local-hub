package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
)

type SupervisorStateLeafRole string

const (
	SupervisorStateLeaf  SupervisorStateLeafRole = "supervisor-state"
	SupervisorIntentLeaf SupervisorStateLeafRole = "supervisor-intent"
)

type SupervisorStateNamespaceRequest struct {
	SourceOperationID, RequestSHA256, SourceEpoch string
	LeafRoles                                     []SupervisorStateLeafRole
	CreateMissing                                 bool
}
type SupervisorStateNamespaceObservation struct {
	request         SupervisorStateNamespaceRequest
	root            string
	platform        serenaRecordNamespacePlatform
	namespaceSHA256 string
	policy          *StateReadPolicyObservation
	resolution      *StateDirResolutionObservation
}

func supervisorNamespaceLeaf(role SupervisorStateLeafRole) (string, error) {
	switch role {
	case SupervisorStateLeaf:
		return "supervisor-state.json", nil
	case SupervisorIntentLeaf:
		return "supervisor-intent.json", nil
	default:
		return "", errors.New("SUPERVISOR_NAMESPACE_ROLE_INVALID")
	}
}
func AcquireSupervisorStateNamespaceObserved(ctx context.Context, request SupervisorStateNamespaceRequest) (observation *SupervisorStateNamespaceObservation, retErr error) {
	if ctx == nil || !preparedSerenaID(request.SourceOperationID) || !preparedSerenaDigest(request.RequestSHA256) || !preparedSerenaID(request.SourceEpoch) || len(request.LeafRoles) == 0 {
		return nil, errors.New("SUPERVISOR_NAMESPACE_REQUEST_INVALID")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("SUPERVISOR_NAMESPACE_DEADLINE_REQUIRED")
	}
	roles := map[SupervisorStateLeafRole]bool{}
	for _, role := range request.LeafRoles {
		if _, err := supervisorNamespaceLeaf(role); err != nil {
			return nil, err
		}
		if roles[role] {
			return nil, errors.New("SUPERVISOR_NAMESPACE_ROLE_DUPLICATE")
		}
		roles[role] = true
	}
	root, resolution, err := ResolveDaemonStateDirObserved(ctx)
	if err != nil {
		return nil, err
	}
	if resolution == nil || resolution.Validate(ctx, root) != nil {
		return nil, errors.New("SUPERVISOR_NAMESPACE_RESOLVER_UNQUALIFIED")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("SUPERVISOR_NAMESPACE_ROOT_INVALID")
	}
	request.LeafRoles = append([]SupervisorStateLeafRole(nil), request.LeafRoles...)
	observation = &SupervisorStateNamespaceObservation{request: request, root: root, resolution: resolution}
	defer func() {
		if retErr != nil {
			_, closeErr := observation.CloseObserved()
			retErr = errors.Join(retErr, closeErr)
		}
	}()
	observation.policy, err = ResolveStateReadPolicyObserved(ctx)
	if err != nil {
		return observation, err
	}
	if err = observation.policy.Validate(ctx, StateReadPolicyEnvironmentIdentity()); err != nil {
		return observation, err
	}
	observation.platform, retErr = acquireSupervisorStateNamespacePlatform(ctx, observation)
	return observation, retErr
}
func (o *SupervisorStateNamespaceObservation) NamespaceSHA256() string {
	if o == nil || o.platform == nil || o.namespaceSHA256 == "" {
		return ""
	}
	return o.namespaceSHA256
}

func (o *SupervisorStateNamespaceObservation) ValidateOperation(ctx context.Context, sourceOperationID, requestSHA256 string) error {
	if o == nil || ctx == nil || sourceOperationID != o.request.SourceOperationID || requestSHA256 != o.request.RequestSHA256 || !preparedSerenaID(sourceOperationID) || !preparedSerenaDigest(requestSHA256) {
		return errors.New("SUPERVISOR_NAMESPACE_OPERATION_MISMATCH")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := o.resolution.Validate(ctx, o.root); err != nil {
		return err
	}
	if o.platform == nil {
		return errors.New("SUPERVISOR_NAMESPACE_SOURCE_UNAVAILABLE")
	}
	if err := o.policy.Validate(ctx, StateReadPolicyEnvironmentIdentity()); err != nil {
		return err
	}
	return o.platform.validate(ctx, filepath.Join(o.root, "supervisor-state.json"))
}
func (o *SupervisorStateNamespaceObservation) validateStateFileFreshNamespaceClose(proof *StateFileFreshNamespaceCloseObservation) error {
	if o == nil || o.platform == nil {
		return errors.New("SUPERVISOR_NAMESPACE_SOURCE_UNAVAILABLE")
	}
	return o.platform.validateClose(proof)
}
func (o *SupervisorStateNamespaceObservation) ValidateNamespace(ctx context.Context, path string, role SupervisorStateLeafRole, expectedSHA string) error {
	if o == nil || o.platform == nil || !preparedSerenaDigest(expectedSHA) || expectedSHA != o.namespaceSHA256 {
		return errors.New("SUPERVISOR_NAMESPACE_SOURCE_UNAVAILABLE")
	}
	leaf, err := supervisorNamespaceLeaf(role)
	if err != nil {
		return err
	}
	allowed := false
	for _, current := range o.request.LeafRoles {
		if current == role {
			allowed = true
		}
	}
	if !allowed || path != filepath.Join(o.root, leaf) {
		return errors.New("SUPERVISOR_NAMESPACE_LEAF_UNDECLARED")
	}
	if err = o.policy.Validate(ctx, StateReadPolicyEnvironmentIdentity()); err != nil {
		return err
	}
	return o.platform.validate(ctx, path)
}
func (o *SupervisorStateNamespaceObservation) validateStateFileFreshNamespace(ctx context.Context, path, role string) error {
	return o.ValidateNamespace(ctx, path, SupervisorStateLeafRole(role), o.NamespaceSHA256())
}
func (o *SupervisorStateNamespaceObservation) borrowStateFileFreshNamespace(ctx context.Context, body func() error) error {
	if o == nil || o.platform == nil {
		return errors.New("SUPERVISOR_NAMESPACE_SOURCE_UNAVAILABLE")
	}
	return o.platform.borrow(ctx, body)
}
func (o *SupervisorStateNamespaceObservation) CloseObserved() (*StateFileFreshNamespaceCloseObservation, error) {
	if o == nil || o.platform == nil {
		return nil, errors.New("SUPERVISOR_NAMESPACE_ACQUISITION_UNAVAILABLE")
	}
	return o.platform.close()
}
func supervisorNamespaceDigest(root, epoch string, identity SerenaTreeObject) (string, error) {
	if !preparedSerenaID(epoch) || identity.Device == "" || identity.FileID == "" {
		return "", errors.New("SUPERVISOR_NAMESPACE_IDENTITY_UNDEFINED")
	}
	raw, err := json.Marshal(struct {
		Schema         string `json:"schema"`
		Owner          string `json:"owner"`
		SourceEpoch    string `json:"source_epoch"`
		RootPathSHA256 string `json:"root_path_sha256"`
		VolumeID       string `json:"volume_id"`
		FileID128      string `json:"file_id128"`
	}{"supervisor-state-namespace-v1", "canonical-supervisor-state", epoch, preparedSerenaHash([]byte(root)), strings.ToLower(identity.Device), strings.ToLower(identity.FileID)})
	if err != nil {
		return "", err
	}
	return preparedSerenaHash(raw), nil
}
