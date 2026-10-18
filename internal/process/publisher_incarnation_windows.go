//go:build windows

package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
)

type publisherCaptureOperations struct {
	currentPID func() int
	hold       func(int) (*heldPIDObservation, error)
	birth      func(windows.Handle) (uint64, error)
	principal  func(*heldPIDObservation) (string, *SIDFormatObservation, error)
}

func publisherCaptureNativeOperations() publisherCaptureOperations {
	return publisherCaptureOperations{
		currentPID: os.Getpid,
		principal: func(held *heldPIDObservation) (string, *SIDFormatObservation, error) {
			value, err := held.OwnerSID()
			return value, held.ownerSIDFormatting, err
		},
		hold: func(pid int) (*heldPIDObservation, error) {
			base, err := holdPIDForJobObservationWithAccess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, windows.OpenProcess, verifyHandleIdentity, processInAnyJob, windows.CloseHandle)
			if err != nil {
				return nil, err
			}
			return &heldPIDObservation{heldPIDJobObservation: base, ownerSID: processHandleOwnerUserSID, wait: windows.WaitForSingleObject}, nil
		},
		birth: func(handle windows.Handle) (uint64, error) {
			var created, exited, kernel, user windows.Filetime
			if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
				return 0, err
			}
			return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), nil
		},
	}
}

func CapturePublisherIncarnation(ctx context.Context, binding PublisherIncarnationBinding) (*PublisherIncarnation, error) {
	return capturePublisherIncarnationWithOperations(ctx, binding, publisherCaptureNativeOperations())
}

func capturePublisherIncarnationWithOperations(ctx context.Context, binding PublisherIncarnationBinding, ops publisherCaptureOperations) (*PublisherIncarnation, error) {
	p := &PublisherIncarnation{issuer: &publisherIncarnationIssuer{marker: 1}, binding: binding}
	if !publisherBindingValid(binding) {
		p.finalized = true
		p.finishedInBound = ctx != nil && ctx.Err() == nil
		p.primaryErr = creatorClosureContext(ctx)
		if p.primaryErr == nil { p.primaryErr = errors.New("publisher source binding and native capture owner required") }
		return p, errors.Join(p.primaryErr, p.cleanupErr)
	}
	state, err := capturePublisherNativeStateWithOperations(ctx, ops)
	state.copyToIncarnation(p)
	return p, err
}

// ObserveCurrentPublisherNative declares no publisher binding. It retains only
// the same current-self query, principal conversion and original close facts.
func ObserveCurrentPublisherNative(ctx context.Context) (*PublisherNativeObservation, error) {
	return observeCurrentPublisherNativeWithOperations(ctx, publisherCaptureNativeOperations())
}

func observeCurrentPublisherNativeWithOperations(ctx context.Context, ops publisherCaptureOperations) (*PublisherNativeObservation, error) {
	state, err := capturePublisherNativeStateWithOperations(ctx, ops)
	return &PublisherNativeObservation{issuer: &publisherNativeObservationIssuer{marker: 1}, state: state}, err
}

func capturePublisherNativeStateWithOperations(ctx context.Context, ops publisherCaptureOperations) (capture *publisherNativeCaptureState, retErr error) {
	p := &publisherNativeCaptureState{originalContext: ctx}
	if ctx != nil { p.deadline, _ = ctx.Deadline() }
	capture = p
	defer func() {
		p.finalized = true
		p.finishedInBound = ctx != nil && ctx.Err() == nil
		retErr = errors.Join(p.primaryErr, p.cleanupErr)
	}()
	if err := creatorClosureContext(ctx); err != nil {
		p.primaryErr = err
		return
	}
	if ops.currentPID == nil || ops.hold == nil || ops.birth == nil || ops.principal == nil {
		p.primaryErr = errors.New("publisher source binding and native capture owner required")
		return
	}
	pid := ops.currentPID()
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) {
		p.primaryErr = errors.New("current application PID is invalid")
		return
	}
	p.native.PID = uint32(pid)
	held, err := ops.hold(pid)
	if held != nil {
		defer func() {
			closeErr := held.Close()
			p.queryClosed = closeErr == nil
			p.cleanupErr = errors.Join(p.cleanupErr, closeErr, ctx.Err())
		}()
	}
	if err != nil || held == nil {
		p.primaryErr = errors.Join(err, errors.New("current native publisher query unavailable"))
		return
	}
	_, p.primaryErr = readHeldPublisherNative(ctx, p, held, ops, true)
	return
}

// Capture, positive lookup and the paired native control read one retained
// query object through this same identity/principal/wait owner.
func readHeldPublisherIncarnation(ctx context.Context, p *PublisherIncarnation, held *heldPIDObservation, ops publisherCaptureOperations, requireLive bool) (*uint32, error) {
	state := &publisherNativeCaptureState{native: p.native, imagePath: p.imagePath, sidFormatting: p.sidFormatting, principal: p.principal, queryClosed: p.queryClosed, finishedInBound: p.finishedInBound, finalized: p.finalized, primaryErr: p.primaryErr, cleanupErr: p.cleanupErr}
	wait, err := readHeldPublisherNative(ctx, state, held, ops, requireLive)
	state.copyToIncarnation(p)
	return wait, err
}

func readHeldPublisherNative(ctx context.Context, p *publisherNativeCaptureState, held *heldPIDObservation, ops publisherCaptureOperations, requireLive bool) (*uint32, error) {
	pid := int(p.native.PID)
	identity, err := held.Identity()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(identity.ExecutablePath) || identity.StartedAt.IsZero() {
		return nil, errors.New("native publisher image/birth undefined")
	}
	p.imagePath = identity.ExecutablePath
	birth, err := ops.birth(held.handle)
	if err != nil || birth == 0 {
		return nil, errors.Join(err, errors.New("native publisher FILETIME undefined"))
	}
	created := windows.Filetime{HighDateTime: uint32(birth >> 32), LowDateTime: uint32(birth)}
	if !identity.StartedAt.Equal(time.Unix(0, created.Nanoseconds()).UTC()) {
		return nil, errors.New("same held publisher birth observations differ")
	}
	p.native.NativeBirthFILETIME = birth
	proof := PIDIdentityProof{PID: pid, ExecutablePath: identity.ExecutablePath, StartedAt: identity.StartedAt.UTC().Format(time.RFC3339Nano)}
	if err := held.VerifyIdentity(proof); err != nil {
		return nil, err
	}
	principal, formatting, err := ops.principal(held)
	p.sidFormatting = formatting
	if err != nil {
		return nil, err
	}
	if err := p.sidFormatting.Validate(principal); err != nil {
		return nil, err
	}
	p.principal = principal
	wait, err := held.Wait()
	observedWait := wait.Result
	if err != nil || (requireLive && wait.Result != uint32(windows.WAIT_TIMEOUT)) {
		return &observedWait, errors.Join(err, errors.New("publisher wait is not qualified for this source operation"))
	}
	if err := ctx.Err(); err != nil {
		return &observedWait, err
	}
	p.native.NativeImagePathSHA256 = sidFormatObservationDigest([]byte(p.imagePath))
	p.native.PrincipalSIDSHA256 = sidFormatObservationDigest([]byte(principal))
	p.native.NativeCaptureSHA256, err = publisherNativeDigest(p.native)
	return &observedWait, err
}
