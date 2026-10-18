//go:build windows

package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/process"
)

var observedKnownFolderPathProc = windows.NewLazySystemDLL("shell32.dll").NewProc("SHGetKnownFolderPath")

type stateDirResolverOperations struct {
	resolve           func() (*uint16, uint32, bool, error)
	decode            func(*uint16) (string, error)
	free              func(*uint16)
	principal         func() (string, *process.SIDFormatObservation, error)
	validatePrincipal func(string, *process.SIDFormatObservation) error
}

func stateDirResolverNativeOperations() stateDirResolverOperations {
	return stateDirResolverOperations{
		validatePrincipal: func(value string, proof *process.SIDFormatObservation) error { return proof.Validate(value) },
		resolve: func() (*uint16, uint32, bool, error) {
			if err := observedKnownFolderPathProc.Find(); err != nil {
				return nil, 0, false, err
			}
			var path *uint16
			hr, _, _ := observedKnownFolderPathProc.Call(uintptr(unsafe.Pointer(windows.FOLDERID_LocalAppData)), 0, 0, uintptr(unsafe.Pointer(&path)))
			return path, uint32(hr), true, nil
		},
		decode: func(path *uint16) (string, error) {
			if path == nil {
				return "", errors.New("successful known-folder result has no buffer")
			}
			// Preserve the SDK's UTF16PtrToString decoder for this allocation.
			return windows.UTF16PtrToString(path), nil
		},
		free: func(path *uint16) { windows.CoTaskMemFree(unsafe.Pointer(path)) },
		principal: func() (string, *process.SIDFormatObservation, error) {
			sid, err := currentUserSID()
			if err != nil {
				return "", nil, err
			}
			value, observation, err := process.FormatSIDObserved(sid)
			if err == nil {
				err = observation.ValidateInput(sid, value)
			}
			return value, observation, err
		},
	}
}

func ResolveDaemonStateDirObserved(ctx context.Context) (string, *StateDirResolutionObservation, error) {
	return resolveDaemonStateDirWithOperations(ctx, stateDirResolverNativeOperations())
}

func resolveDaemonStateDirWithOperations(ctx context.Context, ops stateDirResolverOperations) (resolved string, observation *StateDirResolutionObservation, retErr error) {
	o := &StateDirResolutionObservation{issuer: &stateFileReadIssuer{marker: 1}, view: StateDirResolutionSnapshotV1{Schema: "state-dir-resolution-observation-v1", Version: 1, SourceSHA256: stateFileReadObservationDigest([]byte("canonical-state-dir-v1|shell32!SHGetKnownFolderPath|FOLDERID_LocalAppData|flags=0|token=current-null|" + stateDirName)), ReleaseKind: "CoTaskMemFree-void", CleanupErrors: []StateFileReadFailureV1{}}}
	observation = o
	defer func() {
		o.finalized = true
		o.finishedInBound = ctx != nil && ctx.Err() == nil
		o.view.Complete = o.finishedInBound && o.primaryErr == nil && o.cleanupErr == nil && o.view.HRESULTDefined && o.view.HRESULT != nil && int32(*o.view.HRESULT) >= 0 && o.view.AllocationAcquired && o.view.DecodeSucceeded && o.view.FreeCalled && o.view.FreeReturned && o.path != ""
		if o.primaryErr != nil {
			o.view.PrimaryError = &StateFileReadFailureV1{Code: "STATE_ROOT_RESOLUTION_FAILED", Stage: "resolve", Slot: "known-folder"}
		}
		if o.cleanupErr != nil {
			o.view.CleanupErrors = append(o.view.CleanupErrors, StateFileReadFailureV1{Code: "STATE_ROOT_RELEASE_FAILED", Stage: "close", Slot: "known-folder"})
		}
		retErr = errors.Join(o.primaryErr, o.cleanupErr)
		if retErr != nil {
			resolved = ""
		}
	}()
	if ctx == nil {
		o.primaryErr = errors.New("state resolver context required")
		return
	}
	o.deadline, o.deadlineDefined = ctx.Deadline()
	if err := ctx.Err(); err != nil {
		o.primaryErr = err
		return
	}
	if ops.resolve == nil || ops.decode == nil || ops.free == nil || ops.principal == nil || ops.validatePrincipal == nil {
		o.primaryErr = errors.New("original known-folder resolver operations required")
		return
	}
	principal, formatting, err := ops.principal()
	o.principal = principal
	o.formatting = formatting
	o.principalCheck = func() error { return ops.validatePrincipal(principal, formatting) }
	if err != nil {
		o.primaryErr = err
		return
	}
	if principal == "" {
		o.primaryErr = errors.New("original resolver principal undefined")
		return
	}
	if err := o.principalCheck(); err != nil {
		o.primaryErr = err
		return
	}
	if err := ctx.Err(); err != nil {
		o.primaryErr = err
		return
	}
	path, hr, defined, err := ops.resolve()
	o.view.HRESULTDefined = defined
	if defined {
		x := hr
		o.view.HRESULT = &x
	}
	// The documented Known Folder contract transfers every nonnull output,
	// even on a failing HRESULT. CoTaskMemFree is VOID: called/returned facts
	// are recorded without inventing a native error or success return value.
	if path != nil {
		o.view.AllocationAcquired = true
		defer func() {
			o.view.FreeCalled = true
			ops.free(path)
			o.view.FreeReturned = true
			o.cleanupErr = errors.Join(o.cleanupErr, ctx.Err())
		}()
	}
	if err != nil || !defined || int32(hr) < 0 {
		o.primaryErr = errors.Join(err, fmt.Errorf("canonical known-folder HRESULT failed or undefined: %08x", hr))
		return
	}
	if path == nil {
		o.primaryErr = errors.New("successful known-folder result has no acquired allocation")
		return
	}
	root, err := ops.decode(path)
	if err != nil || root == "" || !filepath.IsAbs(root) {
		o.primaryErr = errors.Join(err, errors.New("canonical known-folder path is undefined or not absolute"))
		return
	}
	o.view.DecodeSucceeded = true
	if err := ctx.Err(); err != nil {
		o.primaryErr = err
		return
	}
	o.path = joinStateRoot(root)
	o.view.PathSHA256 = stateFileReadObservationDigest([]byte(o.path))
	resolved = o.path
	return
}
