package process

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// CreatorIdentity is a read-only capability minted by the original retained
// creator. It transfers neither native handles nor permission to reconstruct
// process ownership from a PID or a tracker timestamp.
type CreatorIdentity struct {
	owner         *ProvisionalProcess
	observation   CreatorIdentitySnapshot
	sidFormatting *SIDFormatObservation
}

type CreatorIdentitySnapshot struct {
	Version                   uint32 `json:"version"`
	OriginalPID               int    `json:"original_pid"`
	OriginalStartedAt         string `json:"original_started_at"`
	ExecutablePath            string `json:"executable_path"`
	OwnerSID                  string `json:"owner_sid"`
	OriginalJobBound          bool   `json:"original_job_bound"`
	CurrentJobMembershipKnown bool   `json:"current_job_membership_known"`
	CurrentJobMember          bool   `json:"current_job_member"`
	RootWaitKnown             bool   `json:"root_wait_known"`
	RootWaitResult            uint32 `json:"root_wait_result"`
	RootAlive                 bool   `json:"root_alive"`
	EvidenceSHA256            string `json:"-"`
}

func (identity *CreatorIdentity) Snapshot() CreatorIdentitySnapshot {
	if identity == nil || identity.owner == nil || identity.sidFormatting == nil || identity.sidFormatting.Validate(identity.observation.OwnerSID) != nil {
		return CreatorIdentitySnapshot{}
	}
	return identity.observation
}

type provisionalCreatorIdentityObserver interface {
	observeCreatorIdentity(context.Context) (CreatorIdentitySnapshot, error)
}

func (p *ProvisionalProcess) ObserveCreatorIdentity(ctx context.Context) (*CreatorIdentity, error) {
	if err := creatorClosureContext(ctx); err != nil {
		return nil, err
	}
	if p == nil || p.owner == nil || p.cmd == nil || p.job == nil {
		return nil, creatorClosureUnavailable(errors.New("original creator identity unavailable"))
	}
	if err := p.lockCreatorContext(ctx); err != nil {
		return nil, err
	}
	held := true
	defer func() {
		if held {
			p.mu.Unlock()
		}
	}()
	if p.closeAttempted || p.terminalStarted {
		return nil, creatorClosureUnavailable(errors.New("original creator custody already released or terminal action entered"))
	}
	observer, ok := p.owner.(provisionalCreatorIdentityObserver)
	if !ok {
		return nil, creatorClosureUnavailable(errors.New("original held identity unsupported"))
	}
	if p.identityInFlight {
		return nil, ErrOriginalMemberObservationBusy
	}
	p.identityInFlight = true
	p.mu.Unlock()
	held = false
	defer func() { p.mu.Lock(); p.identityInFlight = false; p.mu.Unlock() }()
	view, err := observer.observeCreatorIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if view.Version != 1 || view.OriginalPID <= 0 || view.OriginalStartedAt == "" || view.ExecutablePath == "" || view.OwnerSID == "" || !view.OriginalJobBound || !view.CurrentJobMembershipKnown || !view.RootWaitKnown || (view.RootWaitResult != 0 && view.RootWaitResult != 258) || view.RootAlive != (view.RootWaitResult == 258) || (view.RootAlive && !view.CurrentJobMember) {
		return nil, creatorClosureUnavailable(errors.New("original creator identity incomplete"))
	}
	if err := creatorClosureContext(ctx); err != nil {
		return nil, err
	}
	sidObserver, ok := p.owner.(interface{ creatorSIDFormatObservation() *SIDFormatObservation })
	if !ok {
		return nil, creatorClosureUnavailable(errors.New("original SID formatting slot unavailable"))
	}
	sidFormatting := sidObserver.creatorSIDFormatObservation()
	if err := sidFormatting.Validate(view.OwnerSID); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	view.EvidenceSHA256 = hex.EncodeToString(digest[:])
	return &CreatorIdentity{owner: p, observation: view, sidFormatting: sidFormatting}, nil
}
