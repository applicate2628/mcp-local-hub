//go:build windows

package api

import (
	"context"
	"errors"
)

// The caller has already resolved policy. This SAME lower descent emits no
// audit and does not call either top-level policy or parent admission again.
func acquireHubMcpAuditParent(ctx context.Context, policy *StateReadPolicyObservation) (parent *hubMcpAuditParent, retErr error) {
	parent = &hubMcpAuditParent{}
	if policy == nil {
		return parent, errors.New("completed original audit policy required")
	}
	b := newStateFileReadBuilder(ctx, policy.stateDir, 0)
	parent.resources = b
	defer func() {
		parent.closureErr = closeConfigParentResources(b)
		parent.finalized = true
		retErr = errors.Join(retErr, parent.closureErr)
	}()
	if err := policy.Validate(ctx, policy.view.EnvIdentitySHA256); err != nil {
		return parent, err
	}
	outcome := &configParentDescentOutcome{}
	exists, err := secureParentDirAnywhereModeObserved(ctx, policy.stateDir, false, true, b, outcome)
	if err != nil {
		refuse, relaxEligible := clientConfigParentGateRefusalOrRelax(err)
		if !relaxEligible || policy.view.EffectiveStrict {
			return parent, refuse
		}
		if closeErr := closeConfigParentResources(b); closeErr != nil {
			return parent, errors.Join(err, closeErr)
		}
		exists, err = secureParentDirAnywhereModeObserved(ctx, policy.stateDir, true, true, b, outcome)
	}
	parent.identity = outcome.identity
	if err == nil && !exists {
		return parent, errors.New("original audit root was not acquired")
	}
	return parent, err
}
