//go:build windows && serena_qualification && test_state_path_env

package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/process"
)

const serenaQualificationRunRootEnv = "MCPHUB_SERENA_QUALIFICATION_RUN_ROOT"

var serenaQualificationNamespaces struct {
	sync.Mutex
	active     *SerenaQualificationNamespace
	cleanupErr error
}

// SerenaQualificationNamespace owns an existing private integration namespace.
// It retains native no-follow directory handles until Close. It creates no
// directories and grants no authority to an ordinary product executable.
type SerenaQualificationNamespace struct {
	issuer       *stateFileReadIssuer
	ctx          context.Context
	deadline     time.Time
	root         string
	runRoot      string
	source       *buildinfo.PublisherSourceObservation
	native       *process.PublisherNativeObservation
	image        *CurrentPublisherImageObservation
	sid          *windows.SID
	principal    string
	formatting   *process.SIDFormatObservation
	directories  *serenaQualificationDirectories
	binding      StateDirQualificationSnapshotV1
	sourceSHA256 string
	closed       bool
	closeErr     error
}

// OpenSerenaQualificationNamespace is available only in the dedicated Windows
// integration build. The caller must retain a bounded context until Close,
// bind both existing test state-root mechanisms to root, and supply the private
// run root through MCPHUB_SERENA_QUALIFICATION_RUN_ROOT. Scopes cannot overlap.
func OpenSerenaQualificationNamespace(ctx context.Context, root string) (*SerenaQualificationNamespace, error) {
	deadline, err := serenaQualificationDeadline(ctx)
	if err != nil {
		return nil, err
	}
	if !testing.Testing() {
		return nil, errors.New("SERENA_QUALIFICATION_TEST_RUNTIME_REQUIRED")
	}
	runRoot := os.Getenv(serenaQualificationRunRootEnv)
	if err := validateSerenaQualificationPaths(runRoot, root); err != nil {
		return nil, err
	}
	if err := validateSerenaQualificationStateBinding(runRoot, root); err != nil {
		return nil, err
	}

	serenaQualificationNamespaces.Lock()
	defer serenaQualificationNamespaces.Unlock()
	if serenaQualificationNamespaces.active != nil || serenaQualificationNamespaces.cleanupErr != nil {
		return nil, errors.Join(serenaQualificationNamespaces.cleanupErr, errors.New("SERENA_QUALIFICATION_NAMESPACE_ALREADY_OWNED"))
	}
	source, err := buildinfo.CurrentPublisherSource()
	if err != nil {
		return nil, err
	}
	native, err := process.ObserveCurrentPublisherNative(ctx)
	if err != nil {
		return nil, err
	}
	image, err := NewCurrentPublisherImageObservation(ctx, native)
	if err != nil {
		return nil, err
	}
	sid, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	principal, formatting, err := process.FormatSIDObserved(sid)
	if err != nil {
		return nil, err
	}
	if err := formatting.ValidateInput(sid, principal); err != nil {
		return nil, err
	}
	if principal == "" || stateFileReadObservationDigest([]byte(principal)) != native.NativeSnapshot().PrincipalSIDSHA256 || image.Snapshot().SourceEpoch != source.Epoch() {
		return nil, errors.New("SERENA_QUALIFICATION_PUBLISHER_BINDING_DIFFERS")
	}
	directories, err := openSerenaQualificationDirectories(ctx, runRoot, root)
	if err != nil {
		if directories != nil && directories.closeErr != nil {
			serenaQualificationNamespaces.cleanupErr = directories.closeErr
		}
		return nil, err
	}
	scope := &SerenaQualificationNamespace{
		issuer: &stateFileReadIssuer{marker: 1}, ctx: ctx, deadline: deadline,
		root: root, runRoot: runRoot, source: source, native: native, image: image,
		sid: sid, principal: principal, formatting: formatting, directories: directories,
		binding: StateDirQualificationSnapshotV1{
			RunRootPathSHA256:     stateFileReadObservationDigest([]byte(runRoot)),
			RunRootIdentitySHA256: directories.runRootIdentitySHA256,
			RootIdentitySHA256:    directories.rootIdentitySHA256,
			SourceEpoch:           source.Epoch(), PublisherImageSHA256: image.Snapshot().ImageSHA256,
			PrincipalSIDSHA256: native.NativeSnapshot().PrincipalSIDSHA256,
		},
	}
	raw, err := json.Marshal(scope.binding)
	if err == nil {
		scope.sourceSHA256 = stateFileReadObservationDigest(raw)
		err = scope.validateCurrent(ctx)
	}
	if err != nil {
		scope.closed = true
		scope.closeErr = directories.close()
		serenaQualificationNamespaces.cleanupErr = scope.closeErr
		return nil, errors.Join(err, scope.closeErr)
	}
	serenaQualificationNamespaces.active = scope
	return scope, nil
}

// Close invalidates all borrowed observations before closing the native chain.
// A failed native close is reported and never retried; the process cannot open
// another qualification scope after uncertain cleanup.
func (s *SerenaQualificationNamespace) Close() error {
	if s == nil {
		return nil
	}
	serenaQualificationNamespaces.Lock()
	defer serenaQualificationNamespaces.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if serenaQualificationNamespaces.active == s {
		serenaQualificationNamespaces.active = nil
	}
	s.closeErr = s.directories.close()
	if s.closeErr != nil {
		serenaQualificationNamespaces.cleanupErr = errors.Join(serenaQualificationNamespaces.cleanupErr, s.closeErr)
	}
	return s.closeErr
}

func serenaQualificationDeadline(ctx context.Context) (time.Time, error) {
	if ctx == nil {
		return time.Time{}, errors.New("SERENA_QUALIFICATION_CONTEXT_REQUIRED")
	}
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	deadline, finite := ctx.Deadline()
	if !finite || !time.Now().Before(deadline) {
		return time.Time{}, errors.New("SERENA_QUALIFICATION_DEADLINE_REQUIRED")
	}
	return deadline, nil
}

func validateSerenaQualificationPaths(runRoot, root string) error {
	for _, path := range []string{runRoot, root} {
		volume := filepath.VolumeName(path)
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(volume) != 2 || volume[1] != ':' || path == volume+string(filepath.Separator) {
			return errors.New("SERENA_QUALIFICATION_ABSOLUTE_PRIVATE_ROOT_REQUIRED")
		}
		for _, part := range strings.Split(strings.TrimPrefix(path[len(volume):], string(filepath.Separator)), string(filepath.Separator)) {
			if !serenaTreeName(part) || !filepath.IsLocal(part) || strings.TrimRight(part, " .") != part {
				return errors.New("SERENA_QUALIFICATION_PATH_COMPONENT_INVALID")
			}
		}
	}
	relative, err := filepath.Rel(runRoot, root)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("SERENA_QUALIFICATION_ROOT_OUTSIDE_RUN")
	}
	return nil
}

func validateSerenaQualificationStateBinding(runRoot, root string) error {
	if root == "" || daemonStateRootOverride != root || os.Getenv("MCPHUB_STATE_DIR_OVERRIDE") != root || os.Getenv(serenaQualificationRunRootEnv) != runRoot {
		return errors.New("SERENA_QUALIFICATION_STATE_ROOT_BINDING_DIFFERS")
	}
	resolved, err := DaemonStateDirReadOnly()
	if err != nil || resolved != root {
		return errors.Join(err, errors.New("SERENA_QUALIFICATION_ORDINARY_RESOLVER_DIFFERS"))
	}
	return nil
}

// The registry mutex serializes this check with Close and scope replacement.
func (s *SerenaQualificationNamespace) validateCurrent(ctx context.Context) error {
	deadline, err := serenaQualificationDeadline(ctx)
	if err != nil {
		return err
	}
	if s == nil || s.closed || s.issuer == nil || s.ctx == nil || s.deadline.IsZero() || deadline.After(s.deadline) || !time.Now().Before(s.deadline) || s.directories == nil || s.source == nil || s.native == nil || s.image == nil || s.sid == nil {
		return errors.New("SERENA_QUALIFICATION_NAMESPACE_UNAVAILABLE")
	}
	if err := errors.Join(s.ctx.Err(), validateSerenaQualificationStateBinding(s.runRoot, s.root)); err != nil {
		return err
	}
	if err := s.source.Validate(s.binding.SourceEpoch); err != nil {
		return err
	}
	if err := s.native.Validate(ctx); err != nil {
		return err
	}
	if err := s.image.Validate(ctx, s.native); err != nil {
		return err
	}
	currentSID, err := currentUserSID()
	if err != nil {
		return err
	}
	if !s.sid.Equals(currentSID) || stateFileReadObservationDigest([]byte(s.principal)) != s.binding.PrincipalSIDSHA256 {
		return errors.New("SERENA_QUALIFICATION_PRINCIPAL_CHANGED")
	}
	if err := s.formatting.ValidateInput(s.sid, s.principal); err != nil {
		return err
	}
	return s.directories.validate(ctx)
}

func ResolveDaemonStateDirObserved(ctx context.Context) (string, *StateDirResolutionObservation, error) {
	serenaQualificationNamespaces.Lock()
	defer serenaQualificationNamespaces.Unlock()
	s := serenaQualificationNamespaces.active
	if s == nil {
		return "", nil, errors.New("SERENA_QUALIFICATION_NAMESPACE_NOT_OPEN")
	}
	if err := s.validateCurrent(ctx); err != nil {
		return "", nil, err
	}
	binding := s.binding
	deadline, _ := ctx.Deadline()
	o := &StateDirResolutionObservation{
		qualification: s, issuer: s.issuer, path: s.root,
		principal: s.principal, formatting: s.formatting,
		deadline: deadline, deadlineDefined: true, finalized: true, finishedInBound: true,
		view: StateDirResolutionSnapshotV1{
			Schema: serenaQualificationResolutionSchema, Version: 1,
			PathSHA256: stateFileReadObservationDigest([]byte(s.root)), SourceSHA256: s.sourceSHA256,
			Qualification: &binding, ReleaseKind: "borrowed-retained-directory",
			Complete: true, CleanupErrors: []StateFileReadFailureV1{},
		},
	}
	return s.root, o, nil
}

func validateSerenaQualificationResolution(ctx context.Context, o *StateDirResolutionObservation, path string) error {
	if o == nil {
		return errors.New("SERENA_QUALIFICATION_RESOLUTION_UNAVAILABLE")
	}
	scope, ok := o.qualification.(*SerenaQualificationNamespace)
	if !ok || scope == nil {
		return errors.New("SERENA_QUALIFICATION_RESOLUTION_UNAVAILABLE")
	}
	return scope.validateStateDirResolution(ctx, o, path)
}

func (s *SerenaQualificationNamespace) validateStateDirResolution(ctx context.Context, o *StateDirResolutionObservation, path string) error {
	serenaQualificationNamespaces.Lock()
	defer serenaQualificationNamespaces.Unlock()
	if s == nil || serenaQualificationNamespaces.active != s || o == nil || o.qualification != s || o.issuer != s.issuer || !o.finalized || !o.finishedInBound || !o.deadlineDefined || o.path != s.root || path != s.root || o.principal != s.principal || o.formatting != s.formatting || o.primaryErr != nil || o.cleanupErr != nil {
		return errors.New("SERENA_QUALIFICATION_RESOLUTION_BINDING_DIFFERS")
	}
	v := o.view
	if v.Schema != serenaQualificationResolutionSchema || v.Version != 1 || !v.Complete || v.Qualification == nil || *v.Qualification != s.binding || v.SourceSHA256 != s.sourceSHA256 || v.PathSHA256 != stateFileReadObservationDigest([]byte(path)) || v.PrimaryError != nil || len(v.CleanupErrors) != 0 || v.ReleaseKind != "borrowed-retained-directory" || v.HRESULTDefined || v.HRESULT != nil || v.AllocationAcquired || v.DecodeSucceeded || v.FreeCalled || v.FreeReturned {
		return errors.New("SERENA_QUALIFICATION_RESOLUTION_FACTS_DIFFER")
	}
	deadline, err := serenaQualificationDeadline(ctx)
	if err != nil {
		return err
	}
	if deadline.After(o.deadline) || !time.Now().Before(o.deadline) {
		return errors.New("SERENA_QUALIFICATION_RESOLUTION_DEADLINE_RENEWED")
	}
	return s.validateCurrent(ctx)
}
