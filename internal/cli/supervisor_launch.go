package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/api"
)

// SupervisorLaunch is custody of one actually created supervisor. Observing an
// existing actor never creates this destructive capability.
type SupervisorLaunch interface {
	BindControl(context.Context, SupervisorControlExpectation) (SupervisorControlBinding, error)
	Abort(context.Context) error
	Handoff(context.Context, SupervisorControlBinding) error
}

type SupervisorControlExpectation struct {
	StateDir      string
	ProductPath   string
	ProductSHA256 string
}

// SupervisorControlBinding can only be issued by the launch owner. Its proof is
// private and is not serialized or reconstructed from a sidecar by callers.
type SupervisorControlBinding struct{ proof *supervisorControlProof }
type supervisorControlProof struct {
	issuer          SupervisorLaunch
	expected        SupervisorControlExpectation
	owner           api.SupervisorLockOwner
	peer            api.SupervisorProcessIdentityV1
	creator         *os.Process
	validateCreator func() error
	originalOutcome func() (bool, error)
}

func abortStartedSupervisor(launch SupervisorLaunch) error {
	if launch == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultSupervisorLockReleaseTimeout)
	defer cancel()
	if err := launch.Abort(ctx); err != nil {
		return fmt.Errorf("%w: %w", errSupervisorLaunchSettlement, err)
	}
	return nil
}

// receiveStartedSupervisor composes startup receiving for callers with no
// durable decision boundary between control binding and their existing oracle.
func receiveStartedSupervisor(ctx context.Context, launch SupervisorLaunch, expected SupervisorControlExpectation, ready func() error) (retErr error) {
	if launch == nil {
		return errors.New("supervisor start returned no creator capability")
	}
	handedOff := false
	defer func() {
		if !handedOff {
			retErr = errors.Join(retErr, abortStartedSupervisor(launch))
		}
	}()
	binding, err := launch.BindControl(ctx, expected)
	if err != nil {
		return fmt.Errorf("bind created supervisor control: %w", err)
	}
	if ready != nil {
		if err := ready(); err != nil {
			return err
		}
	}
	if err := launch.Handoff(ctx, binding); err != nil {
		return fmt.Errorf("handoff created supervisor control: %w", err)
	}
	handedOff = true
	return nil
}

func supervisorExpectation(stateDir, path, digest string) SupervisorControlExpectation {
	return SupervisorControlExpectation{StateDir: stateDir, ProductPath: path, ProductSHA256: digest}
}

func missingSupervisorLaunch(launch SupervisorLaunch) error {
	if launch == nil {
		return errors.New("supervisor start returned no creator capability")
	}
	return nil
}

var errSupervisorLaunchSettlement = errors.New("created supervisor settlement unconfirmed")

var errSupervisorControlGenerationChanged = errors.New("created supervisor generation changed")
var errSupervisorControlBinding = errors.New("original supervisor control binding unavailable")

// ExitGraceful consumes the existing private creator proof without reacquiring
// control from the current sidecar. It neither owns Wait nor transfers custody.
func (binding SupervisorControlBinding) ExitGraceful(ctx context.Context, timeoutMs int) error {
	proof := binding.proof
	if proof == nil || proof.creator == nil || proof.validateCreator == nil {
		return errSupervisorControlBinding
	}
	if err := proof.validateCreator(); err != nil {
		return errors.Join(errSupervisorControlBinding, err)
	}
	if proof.originalOutcome == nil {
		return errSupervisorControlBinding
	}
	if exited, err := proof.originalOutcome(); exited {
		return err
	}
	return api.DialSupervisorIPCExitForPeer(ctx, proof.expected.StateDir, proof.owner, proof.peer, timeoutMs)
}

// Capture only from the originally retained creator, before observing any pipe.
// This observation grants no custody, Wait, Handoff or destructive authority.
func captureCreatedSupervisorPeer(proc *os.Process, original api.SupervisorLockOwner) (api.SupervisorProcessIdentityV1, error) {
	if proc == nil || proc.Pid != original.PID {
		return api.SupervisorProcessIdentityV1{}, errSupervisorControlBinding
	}
	peer, err := api.ObserveSupervisorProcessIdentityV1(proc.Pid)
	if err != nil {
		return api.SupervisorProcessIdentityV1{}, err
	}
	if err := api.ValidateSupervisorStatusServerIdentityV1(original, peer, peer.UserSID, peer.SessionID, peer.ImagePath); err != nil {
		return api.SupervisorProcessIdentityV1{}, errors.Join(errSupervisorControlBinding, err)
	}
	return peer, nil
}

// observeCreatedSupervisorControl is the common creator-to-control proof used by
// Product receiving and the GUI's original native creator. Native identity is
// supplied by that creator; this observer owns no child, Job, Wait or handoff.
func observeCreatedSupervisorControl(ctx context.Context, expected SupervisorControlExpectation, original api.SupervisorLockOwner, peer api.SupervisorProcessIdentityV1, validateNative func(api.SupervisorLockOwner) error, readOwner func(string) (api.SupervisorLockOwner, error)) (api.SupervisorLockOwner, error) {
	if ctx == nil {
		return api.SupervisorLockOwner{}, errors.New("supervisor control requires context")
	}
	if err := ctx.Err(); err != nil {
		return api.SupervisorLockOwner{}, err
	}
	if !cleanAbsolutePath(expected.StateDir) || !cleanAbsolutePath(expected.ProductPath) || !validLowerHexSHA256(expected.ProductSHA256) || original.PID <= 0 || original.StartedAt == "" || validateNative == nil || readOwner == nil {
		return api.SupervisorLockOwner{}, errSupervisorControlBinding
	}
	for _, path := range []string{expected.StateDir, expected.ProductPath} {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !samePath(resolved, path) {
			return api.SupervisorLockOwner{}, errors.Join(errors.New("created supervisor canonical namespace changed"), err)
		}
	}
	lockPath := filepath.Join(expected.StateDir, "supervisor.lock")
	owner, err := readOwner(lockPath)
	if err != nil {
		return api.SupervisorLockOwner{}, err
	}
	if owner != original {
		return api.SupervisorLockOwner{}, errSupervisorControlGenerationChanged
	}
	if err := validateNative(owner); err != nil {
		return api.SupervisorLockOwner{}, err
	}
	if err := api.ValidateSupervisorStatusServerIdentityV1(original, peer, peer.UserSID, peer.SessionID, expected.ProductPath); err != nil {
		return api.SupervisorLockOwner{}, errors.Join(errSupervisorControlBinding, err)
	}
	if err := api.VerifySupervisorControlGenerationForPeerFromStateDir(ctx, expected.StateDir, owner, peer); err != nil {
		return api.SupervisorLockOwner{}, err
	}
	current, err := readOwner(lockPath)
	if err != nil || current != owner {
		return api.SupervisorLockOwner{}, errors.Join(errors.New("created supervisor generation changed after control proof"), err)
	}
	if err := ctx.Err(); err != nil {
		return api.SupervisorLockOwner{}, err
	}
	return owner, nil
}

func startedSupervisorDigest(path string) (string, error) {
	raw, err := hashFile(path)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
