package api

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"mcp-local-hub/internal/clients"
)

const codexProjectConfigLockPrefix = "codex-project-config-"

var _ clients.CodexProjectConfigLockRunnerFactory = NewCodexProjectConfigMutationOwner

// CodexProjectConfigMutationOwner owns one full-project-identity state lock.
// The caller supplies no path; the lock leaf is derived once from the validated
// identity and the API package's existing daemon-state directory owner.
type CodexProjectConfigMutationOwner struct {
	lockPath   string
	acquire    func(string) (func() error, error)
	tryAcquire func(string) (func() error, bool, error)
}

// NewCodexProjectConfigMutationOwner is the factory accepted directly by
// clients.NewCodexCLIForProject.
func NewCodexProjectConfigMutationOwner(identity clients.CodexProjectIdentity) (clients.CodexProjectConfigLockRunner, error) {
	fingerprint := identity.ProjectIdentitySHA256()
	decoded, decodeErr := hex.DecodeString(fingerprint)
	if len(fingerprint) != 64 || fingerprint != strings.ToLower(fingerprint) || decodeErr != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("%w: invalid project identity fingerprint", clients.ErrCodexProjectConfigLockUnavailable)
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return nil, fmt.Errorf("%w: resolve daemon state directory: %w", clients.ErrCodexProjectConfigLockUnavailable, err)
	}
	return &CodexProjectConfigMutationOwner{
		lockPath:   filepath.Join(stateDir, codexProjectConfigLockPrefix+fingerprint+".lock"),
		acquire:    lockLeafLedgered,
		tryAcquire: tryLockLeafLedgered,
	}, nil
}

func (o *CodexProjectConfigMutationOwner) RunRead(fn func() error) error {
	return o.run(false, fn)
}

func (o *CodexProjectConfigMutationOwner) RunMutation(fn func() error) error {
	return o.run(true, fn)
}

func (o *CodexProjectConfigMutationOwner) run(mutation bool, fn func() error) (err error) {
	_, err = o.runObserved(nil, mutation, fn)
	return err
}

func (o *CodexProjectConfigMutationOwner) RunMutationContext(ctx context.Context, fn func() error) (clients.ProjectConfigLockExecution, error) {
	if ctx == nil {
		return clients.ProjectConfigLockExecution{PrimaryError: context.Canceled}, context.Canceled
	}
	return o.runObserved(ctx, true, fn)
}

func (o *CodexProjectConfigMutationOwner) RunReadContext(ctx context.Context, fn func() error) (clients.ProjectConfigLockExecution, error) {
	if ctx == nil {
		return clients.ProjectConfigLockExecution{PrimaryError: context.Canceled}, context.Canceled
	}
	return o.runObserved(ctx, false, fn)
}

func (o *CodexProjectConfigMutationOwner) runObserved(ctx context.Context, mutation bool, fn func() error) (observed clients.ProjectConfigLockExecution, err error) {
	defer func() {
		if observed.PrimaryError == nil && observed.CleanupError == nil {
			observed.PrimaryError = err
		}
	}()
	if o == nil || o.lockPath == "" || o.acquire == nil {
		return observed, fmt.Errorf("%w: project config lock owner is unavailable", clients.ErrCodexProjectConfigLockUnavailable)
	}
	if fn == nil {
		return observed, errors.New("Codex project config lock body is nil")
	}
	var release func() error
	var acquireErr error
	if ctx == nil {
		release, acquireErr = o.acquire(o.lockPath)
	} else {
		if o.tryAcquire == nil {
			return observed, clients.ErrCodexProjectConfigLockUnavailable
		}
		for {
			if ctx.Err() != nil {
				return observed, ctx.Err()
			}
			var acquired bool
			release, acquired, acquireErr = o.tryAcquire(o.lockPath)
			if acquireErr != nil || acquired {
				break
			}
			select {
			case <-ctx.Done():
				return observed, ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	if acquireErr != nil {
		return observed, fmt.Errorf("%w: acquire project config lock: %w", clients.ErrCodexProjectConfigLockUnavailable, acquireErr)
	}
	if release == nil {
		return observed, fmt.Errorf("%w: project config lock acquisition returned no release handle", clients.ErrCodexProjectConfigLockUnavailable)
	}
	bodyApplied := false
	var releaseErr error
	observed.ReleaseDefined = true
	defer func() {
		releaseAndJoinApplied(&err, func() error {
			releaseErr = release()
			return releaseErr
		}, "release Codex project config lock", bodyApplied)
		succeeded := releaseErr == nil
		observed.ReleaseSucceeded = &succeeded
		observed.CleanupError = releaseErr
		if errors.Is(releaseErr, ErrLockReleaseUnconfirmed) {
			err = fmt.Errorf("%w: %w", clients.ErrCodexProjectConfigLockReleaseUnconfirmed, err)
		}
	}()
	if ctx != nil && ctx.Err() != nil {
		observed.PrimaryError = ctx.Err()
		return observed, ctx.Err()
	}
	observed.BodyEntered = true
	err = fn()
	observed.PrimaryError = err
	bodyApplied = mutation && err == nil
	return observed, err
}
