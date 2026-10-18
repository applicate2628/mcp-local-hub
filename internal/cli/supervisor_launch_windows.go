//go:build windows

package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

// supervisorLaunchDependencies captures the original launch's namespace and
// control ports without retaining the transaction or GUI that composed them.
type supervisorLaunchDependencies struct {
	exePath, supervisorLockPath, pipePath string
	readOwner                             func(string) (api.SupervisorLockOwner, error)
	waitLockReleased                      func(context.Context, time.Duration) error
	sendIPCPeer                           supervisorPeerIPCSender
}

func (d supervisorLaunchDependencies) validate() error {
	if d.sendIPCPeer == nil {
		return errSupervisorPeerIPCSenderRequired
	}
	if !cleanAbsolutePath(d.exePath) || !cleanAbsolutePath(d.supervisorLockPath) ||
		!samePath(d.supervisorLockPath, filepath.Join(filepath.Dir(d.supervisorLockPath), "supervisor.lock")) ||
		d.pipePath != api.SupervisorIPCAddress(filepath.Dir(d.supervisorLockPath)) ||
		d.readOwner == nil || d.waitLockReleased == nil {
		return errors.New("created supervisor launch dependencies are incomplete or foreign")
	}
	return nil
}

type ownedSupervisorLaunch struct {
	mu                           sync.Mutex
	cmd                          *exec.Cmd
	deps                         supervisorLaunchDependencies
	original                     process.PIDHandleIdentity
	peer                         api.SupervisorProcessIdentityV1
	ports                        []int
	proof                        *supervisorControlProof
	handedOff, settling, settled bool
	releaseErr, settleErr        error
	waitOnce                     sync.Once
	waitDone                     chan struct{}
	waitErr                      error
}

func validateSupervisorImageIdentity(binaryPath, expectedSHA256 string, owner api.SupervisorLockOwner) error {
	if owner.PID <= 0 {
		return fmt.Errorf("successor supervisor lock owner has invalid PID %d", owner.PID)
	}
	identity, err := processLookupIdentityFn(owner.PID)
	if err != nil {
		return fmt.Errorf("probe successor process identity: %w", err)
	}
	if supervisorCommandLineSubcommand(identity.CommandLine) != "supervise" {
		return fmt.Errorf("successor PID %d is not a supervise process", owner.PID)
	}
	ownerStartedUnix, ok := parseSidecarStartedAtUnix(owner.StartedAt)
	if !ok || identity.CreationDateUnix == 0 || identity.CreationDateUnix > ownerStartedUnix {
		return fmt.Errorf("successor PID %d creation identity does not match lock generation", owner.PID)
	}
	if !samePath(identity.ExecutablePath, binaryPath) {
		return fmt.Errorf("successor PID %d executable %s does not match canonical %s", owner.PID, identity.ExecutablePath, binaryPath)
	}
	if expectedSHA256 != "" {
		hash, err := hashFile(identity.ExecutablePath)
		if err != nil {
			return fmt.Errorf("hash successor executable identity: %w", err)
		}
		if got := hex.EncodeToString(hash); got != expectedSHA256 {
			return fmt.Errorf("successor executable SHA-256 mismatch: got %s want %s", got, expectedSHA256)
		}
	}
	return nil
}

func newOwnedSupervisorLaunch(cmd *exec.Cmd, deps supervisorLaunchDependencies, ports []int) (*ownedSupervisorLaunch, error) {
	if cmd == nil || cmd.Process == nil {
		return nil, errors.New("started supervisor creator is absent")
	}
	l := &ownedSupervisorLaunch{cmd: cmd, deps: deps, ports: append([]int(nil), ports...), waitDone: make(chan struct{})}
	if err := deps.validate(); err != nil {
		return l, err // The already-created original remains with this owner.
	}
	identity, err := process.LookupPIDHandleIdentity(cmd.Process.Pid)
	if err != nil {
		return l, fmt.Errorf("capture created supervisor generation: %w", err)
	}
	l.original = identity
	l.peer, err = captureCreatedSupervisorPeer(cmd.Process, api.SupervisorLockOwner{PID: identity.PID, StartedAt: identity.StartedAt.UTC().Format(time.RFC3339Nano)})
	if err != nil {
		return l, fmt.Errorf("capture created supervisor native peer expectation: %w", err)
	}
	return l, nil
}

func (l *ownedSupervisorLaunch) validateControlExpectation(expected SupervisorControlExpectation) error {
	if l.deps.validate() != nil || !cleanAbsolutePath(expected.StateDir) || !cleanAbsolutePath(expected.ProductPath) || !validLowerHexSHA256(expected.ProductSHA256) ||
		!samePath(expected.StateDir, filepath.Dir(l.deps.supervisorLockPath)) || !samePath(expected.ProductPath, l.deps.exePath) ||
		l.deps.pipePath != api.SupervisorIPCAddress(expected.StateDir) || l.cmd == nil || l.cmd.Process == nil || l.original.PID <= 0 || l.original.PID != l.cmd.Process.Pid || l.original.StartedAt.IsZero() {
		return errors.New("created supervisor control expectation is incomplete or foreign")
	}
	return nil
}

func (l *ownedSupervisorLaunch) observeControl(ctx context.Context, expected SupervisorControlExpectation) (api.SupervisorLockOwner, error) {
	if err := l.validateControlExpectation(expected); err != nil {
		return api.SupervisorLockOwner{}, err
	}
	select {
	case <-l.waitDone:
		return api.SupervisorLockOwner{}, errors.Join(errors.New("original created supervisor already exited"), l.waitErr)
	default:
	}
	original := api.SupervisorLockOwner{PID: l.original.PID, StartedAt: l.original.StartedAt.UTC().Format(time.RFC3339Nano)}
	return observeCreatedSupervisorControl(ctx, expected, original, l.peer, func(owner api.SupervisorLockOwner) error {
		return validateCreatedSupervisorNativeControl(expected, owner)
	}, l.deps.readOwner)
}

func (l *ownedSupervisorLaunch) BindControl(ctx context.Context, expected SupervisorControlExpectation) (SupervisorControlBinding, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.handedOff || l.settling || l.settled {
		return SupervisorControlBinding{}, errors.New("created supervisor custody is no longer bindable")
	}
	if ctx == nil {
		return SupervisorControlBinding{}, errors.New("supervisor binding requires context")
	}
	if err := ctx.Err(); err != nil {
		return SupervisorControlBinding{}, err
	}
	limit, cancel := context.WithTimeout(ctx, defaultSupervisorLockReleaseTimeout)
	defer cancel()
	for {
		owner, err := l.observeControl(limit, expected)
		if err == nil {
			if l.proof != nil && (l.proof.expected != expected || l.proof.owner != owner) {
				return SupervisorControlBinding{}, errors.New("created supervisor binding changed")
			}
			if l.proof == nil {
				l.proof = &supervisorControlProof{issuer: l, expected: expected, owner: owner, peer: l.peer, creator: l.cmd.Process}
				proof := l.proof
				proof.originalOutcome = l.originalChildOutcome
				proof.validateCreator = func() error {
					l.mu.Lock()
					defer l.mu.Unlock()
					if l.proof != proof || l.cmd == nil || l.cmd.Process != proof.creator || l.handedOff || l.settling || l.settled {
						return errSupervisorControlBinding
					}
					return l.validateControlExpectation(proof.expected)
				}
			}
			return SupervisorControlBinding{proof: l.proof}, nil
		}
		select {
		case <-limit.Done():
			return SupervisorControlBinding{}, errors.Join(err, limit.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (l *ownedSupervisorLaunch) Handoff(ctx context.Context, binding SupervisorControlBinding) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if binding.proof == nil || binding.proof != l.proof || binding.proof.issuer != l {
		return errors.New("created supervisor binding is missing or foreign")
	}
	if l.handedOff {
		return l.releaseErr
	}
	if l.settling || l.settled {
		return errors.New("created supervisor already settled")
	}
	if l.cmd == nil || l.cmd.Process == nil || l.proof.owner.PID != l.original.PID || l.proof.owner.StartedAt != l.original.StartedAt.UTC().Format(time.RFC3339Nano) {
		return errors.New("created supervisor binding has no accepted original generation")
	}
	// BindControl already accepted the physical supervisor-owned lock/listener.
	// Current health is diagnostic; it cannot strand custody after that receiver
	// was accepted and the caller crossed its decision boundary.
	var diagnostic error
	if ctx == nil {
		diagnostic = errors.New("supervisor handoff health context is absent")
	} else if err := ctx.Err(); err != nil {
		diagnostic = err
	} else {
		limit, cancel := context.WithTimeout(ctx, defaultSupervisorLockReleaseTimeout)
		owner, err := l.observeControl(limit, l.proof.expected)
		cancel()
		if err != nil || owner != l.proof.owner {
			diagnostic = errors.Join(errors.New("accepted supervisor current health changed"), err)
		}
	}
	state, stateErr := process.QueryPIDState(l.original.PID)
	if stateErr == nil && state == process.PIDStateDead {
		l.beginWait()
		reapCtx, cancel := context.WithTimeout(context.Background(), defaultSupervisorLockReleaseTimeout)
		defer cancel()
		select {
		case <-l.waitDone:
			l.settled = true
			l.settleErr = errors.Join(errors.New("accepted original supervisor exited before handoff"), diagnostic, l.waitErr)
			return l.settleErr
		case <-reapCtx.Done():
			return errors.Join(diagnostic, reapCtx.Err())
		}
	}
	l.handedOff = true
	l.releaseErr = errors.Join(diagnostic, stateErr, l.cmd.Process.Release())
	return l.releaseErr
}

func (l *ownedSupervisorLaunch) beginWait() {
	l.waitOnce.Do(func() { go func() { l.waitErr = l.cmd.Wait(); close(l.waitDone) }() })
}

// handoffDisposition observes the actual transfer and Release result. It does
// not select cancellation policy or grant authority to abort a transferred actor.
func (l *ownedSupervisorLaunch) handoffDisposition() (transferred bool, releaseErr error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.handedOff, l.releaseErr
}

// originalChildOutcome reads only the original Wait result. Closing waitDone
// publishes waitErr; aggregate namespace settlement may still be unsuccessful.
func (l *ownedSupervisorLaunch) originalChildOutcome() (waitReceived bool, waitErr error) {
	select {
	case <-l.waitDone:
		return true, l.waitErr
	default:
		return false, nil
	}
}

func (l *ownedSupervisorLaunch) Abort(ctx context.Context) error {
	if ctx == nil {
		return errors.New("supervisor abort requires cleanup context")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.handedOff {
		return errors.New("supervisor creator authority was transferred")
	}
	if l.settled {
		return l.settleErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var errs []error
	if !l.settling {
		l.settling = true
		l.beginWait()
		graceful := false
		select {
		case <-l.waitDone:
			graceful = true // Receive the already-retained original outcome without IPC.
		default:
		}
		if !graceful && l.proof != nil && l.deps.sendIPCPeer == nil {
			errs = append(errs, errSupervisorPeerIPCSenderRequired)
		} else if !graceful && l.proof != nil {
			limit, cancel := context.WithTimeout(ctx, time.Duration(defaultExitTimeoutMs)*time.Millisecond)
			owner, err := l.observeControl(limit, l.proof.expected)
			if err == nil && owner == l.proof.owner {
				select {
				case <-l.waitDone:
					graceful = true // Retained original outcome: do not contact a replacement.
				default:
					resp, exitErr := l.deps.sendIPCPeer(limit, l.deps.pipePath, owner, l.proof.peer, "exit", map[string]any{"graceful": true, "timeout_ms": defaultExitTimeoutMs}, time.Duration(defaultExitTimeoutMs)*time.Millisecond)
					if exitErr == nil && resp.OK && resp.Final && resp.Error == nil {
						select {
						case <-l.waitDone:
							graceful = true
						case <-limit.Done():
							exitErr = limit.Err()
						}
					}
					if exitErr != nil {
						errs = append(errs, fmt.Errorf("created supervisor graceful exit: %w", exitErr))
					}
				}
			} else if err != nil {
				errs = append(errs, err)
			}
			cancel()
		}
		if !graceful {
			if err := l.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				errs = append(errs, fmt.Errorf("terminate original created supervisor: %w", err))
			}
		}
		l.settleErr = errors.Join(errs...)
	}
	select {
	case <-l.waitDone:
	case <-ctx.Done():
		return errors.Join(l.settleErr, fmt.Errorf("reap original created supervisor: %w", ctx.Err()))
	}
	if _, expectedExit := l.waitErr.(*exec.ExitError); l.waitErr != nil && !expectedExit {
		errs = append(errs, l.waitErr)
	}
	if l.deps.waitLockReleased != nil {
		if err := l.deps.waitLockReleased(ctx, defaultSupervisorLockReleaseTimeout); err != nil {
			errs = append(errs, err)
		}
		for _, port := range l.ports {
			if err := ctx.Err(); err != nil {
				errs = append(errs, err)
				break
			}
			budget := defaultPostForceKillPortVerifyTimeout
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < budget {
				budget = time.Until(deadline)
			}
			if err := verifyPortsUnboundForUpgrade([]int{port}, budget); err != nil {
				errs = append(errs, err)
				break
			}
		}
	} else {
		errs = append(errs, errors.New("created supervisor settlement namespace is absent"))
	}
	l.settled = true
	l.settleErr = errors.Join(l.settleErr, errors.Join(errs...))
	return l.settleErr
}

func validateCreatedSupervisorNativeControl(expected SupervisorControlExpectation, owner api.SupervisorLockOwner) error {
	if err := validateSupervisorImageIdentity(expected.ProductPath, expected.ProductSHA256, owner); err != nil {
		return err
	}
	sameUser, err := process.ProcessOwnerSIDMatchesCurrent(owner.PID)
	if err != nil {
		return fmt.Errorf("created supervisor current user unconfirmed: %w", err)
	}
	if !sameUser {
		return errors.New("created supervisor current user mismatch")
	}
	return nil
}
