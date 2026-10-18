//go:build windows

package api

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func parentDescentSlot(b *stateFileReadBuilder, kind string) string {
	if b == nil {
		return ""
	}
	return fmt.Sprintf("parent-%s-%d", kind, len(b.resources))
}
func registerParentDirectory(b *stateFileReadBuilder, slot string, h windows.Handle) {
	if b == nil {
		return
	}
	if b.resource(slot) == nil {
		b.prearmResource(slot)
	}
	b.acquire(slot, func() error { return windows.CloseHandle(h) })
}
func closeParentDirectory(b *stateFileReadBuilder, slot string, h windows.Handle) error {
	if b == nil {
		return windows.CloseHandle(h)
	}
	return b.closeResource(slot)
}
func parentObservedError(b *stateFileReadBuilder, err error) error {
	if b == nil {
		return nil
	}
	return err
}
func joinParentObserved(b *stateFileReadBuilder, primary, secondary error) error {
	if b == nil {
		return primary
	}
	return errors.Join(primary, secondary)
}

func queryConfigParentDirectoryIdentity(h windows.Handle) (ConfigParentDirectoryIdentityV1, error) {
	var identity ConfigParentDirectoryIdentityV1
	// Native FILE_ID_INFO contains a uint64 volume and the complete 128-bit ID.
	if unsafe.Sizeof(identity) != 24 {
		return identity, errors.New("FILE_ID_INFO layout differs")
	}
	if err := windows.GetFileInformationByHandleEx(h, windows.FileIdInfo, (*byte)(unsafe.Pointer(&identity)), 24); err != nil {
		return identity, err
	}
	if identity.FileID == ([16]byte{}) {
		return identity, errors.New("original parent full identity is undefined")
	}
	return identity, nil
}

func SecureConfigLockParentPolicyObserved(ctx context.Context, dir string, createMissing bool) (exists bool, observation *ConfigParentPolicyObservation, retErr error) {
	o := &ConfigParentPolicyObservation{issuer: &stateFileReadIssuer{marker: 1}, path: dir, createMissing: createMissing, view: ConfigParentPolicySnapshotV1{Schema: "config-parent-policy-observation-v1", Version: 1, PathSHA256: stateFileReadObservationDigest([]byte(dir)), CreateMissing: createMissing, CreatedComponents: []string{}, Resources: []StateFileReadResourceSnapshotV1{}, CleanupErrors: []StateFileReadFailureV1{}, GateDisposition: "unavailable"}}
	observation = o
	b := newStateFileReadBuilder(ctx, dir, 0)
	o.resources = b
	outcome := &configParentDescentOutcome{}
	defer func() {
		for _, r := range b.resources {
			if r.view.Acquired {
				o.cleanupErr = errors.Join(o.cleanupErr, b.closeResource(r.view.Slot))
			}
			o.view.Resources = append(o.view.Resources, r.view)
		}
		o.cleanupErr = errors.Join(o.cleanupErr, b.borrowedErr)
		if ctx != nil {
			o.cleanupErr = errors.Join(o.cleanupErr, ctx.Err())
		}
		o.view.CreatedComponents = append(o.view.CreatedComponents, outcome.created...)
		o.view.FinalIdentity = outcome.identity
		o.view.Exists = exists
		o.finalized = true
		o.finishedInBound = ctx != nil && ctx.Err() == nil
		o.view.ResourcesComplete = o.cleanupErr == nil
		o.view.Complete = o.finishedInBound && o.primaryErr == nil && o.cleanupErr == nil && o.policy != nil && (!exists || outcome.identity != nil)
		if o.primaryErr != nil {
			o.view.PrimaryError = &StateFileReadFailureV1{Code: "CONFIG_PARENT_PREFLIGHT_FAILED", Stage: "preflight", Slot: "parent"}
		}
		if o.cleanupErr != nil {
			o.view.CleanupErrors = append(o.view.CleanupErrors, StateFileReadFailureV1{Code: "CONFIG_PARENT_RESOURCES_UNKNOWN", Stage: "close", Slot: "parent"})
		}
		retErr = errors.Join(o.primaryErr, o.cleanupErr)
	}()
	if ctx == nil {
		o.primaryErr = errors.New("parent preflight context required")
		return
	}
	o.deadline, o.deadlineDefined = ctx.Deadline()
	if err := ctx.Err(); err != nil {
		o.primaryErr = err
		return
	}
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		o.primaryErr = errors.New("canonical absolute parent path required")
		return
	}
	policy, err := ResolveStateReadPolicyObserved(ctx)
	o.policy = policy
	if err != nil {
		o.primaryErr = err
		return
	}
	if err := policy.Validate(ctx, StateReadPolicyEnvironmentIdentity()); err != nil {
		o.primaryErr = err
		return
	}
	exists, err = secureParentDirAnywhereModeObserved(ctx, dir, false, createMissing, b, outcome)
	if err == nil {
		o.view.GateDisposition = "strict-gate"
		if !exists {
			o.view.GateDisposition = "missing"
		}
		return
	}
	refuse, relaxEligible := clientConfigParentGateRefusalOrRelax(err)
	if !relaxEligible || policy.Snapshot().EffectiveStrict {
		o.primaryErr = refuse
		return
	}
	if closeErr := closeConfigParentResources(b); closeErr != nil {
		o.primaryErr = errors.Join(err, closeErr)
		return
	}
	audit, auditErr := LogHubMcpEventObserved(ctx, policy, "warn", "client-write-unhardened-fallback", map[string]any{"path": dir, "reason": "default-relax-on-solo-host (config-lock-parent-dir)", "origin": "SecureCreateParentDirForConfigLock", "err": err.Error()})
	o.auditObservation = audit
	o.auditEventErr = auditErr
	o.view.AuditEventError = hubMcpLogFailure(auditErr, "EVENT")
	if closureErr := audit.ValidateResourceClosure(ctx, policy, "warn", "client-write-unhardened-fallback"); closureErr != nil {
		o.primaryErr = errors.Join(err, closureErr)
		return
	}
	exists, o.primaryErr = secureParentDirAnywhereModeObserved(ctx, dir, true, createMissing, b, outcome)
	o.view.GateDisposition = "default-relax"
	return
}
