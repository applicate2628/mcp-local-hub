package api

import (
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/clients"
)

const codexProjectConfigLockPrefix = "codex-project-config-"

var _ clients.CodexProjectConfigLockRunnerFactory = NewCodexProjectConfigMutationOwner

// CodexProjectConfigMutationOwner owns one full-project-identity state lock.
// The caller supplies no path; the lock leaf is derived once from the validated
// identity and the API package's existing daemon-state directory owner.
type CodexProjectConfigMutationOwner struct {
	lockPath string
	acquire  func(string) (func() error, error)
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
		lockPath: filepath.Join(stateDir, codexProjectConfigLockPrefix+fingerprint+".lock"),
		acquire:  lockLeafLedgered,
	}, nil
}

func (o *CodexProjectConfigMutationOwner) RunRead(fn func() error) error {
	return o.run(false, fn)
}

func (o *CodexProjectConfigMutationOwner) RunMutation(fn func() error) error {
	return o.run(true, fn)
}

func (o *CodexProjectConfigMutationOwner) run(mutation bool, fn func() error) (err error) {
	if o == nil || o.lockPath == "" || o.acquire == nil {
		return fmt.Errorf("%w: project config lock owner is unavailable", clients.ErrCodexProjectConfigLockUnavailable)
	}
	if fn == nil {
		return errors.New("Codex project config lock body is nil")
	}
	release, acquireErr := o.acquire(o.lockPath)
	if acquireErr != nil {
		return fmt.Errorf("%w: acquire project config lock: %w", clients.ErrCodexProjectConfigLockUnavailable, acquireErr)
	}
	if release == nil {
		return fmt.Errorf("%w: project config lock acquisition returned no release handle", clients.ErrCodexProjectConfigLockUnavailable)
	}
	bodyApplied := false
	var releaseErr error
	defer func() {
		releaseAndJoinApplied(&err, func() error {
			releaseErr = release()
			return releaseErr
		}, "release Codex project config lock", bodyApplied)
		if errors.Is(releaseErr, ErrLockReleaseUnconfirmed) {
			err = fmt.Errorf("%w: %w", clients.ErrCodexProjectConfigLockReleaseUnconfirmed, err)
		}
	}()
	err = fn()
	bodyApplied = mutation && err == nil
	return err
}
