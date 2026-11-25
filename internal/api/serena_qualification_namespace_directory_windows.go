//go:build windows && serena_qualification && test_state_path_env

package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type serenaQualificationDirectory struct {
	path     string
	handle   *serenaTreeHandle
	identity SerenaTreeObject
	private  bool
}

// Every ancestor remains open without FILE_SHARE_DELETE. A path cannot be
// rebound by renaming an intermediate directory while the namespace is live.
type serenaQualificationDirectories struct {
	entries               []serenaQualificationDirectory
	runRootIdentitySHA256 string
	rootIdentitySHA256    string
	closed                bool
	closeErr              error
}

func openSerenaQualificationDirectories(ctx context.Context, runRoot, root string) (chain *serenaQualificationDirectories, retErr error) {
	if _, err := serenaQualificationDeadline(ctx); err != nil {
		return nil, err
	}
	if err := validateSerenaQualificationPaths(runRoot, root); err != nil {
		return nil, err
	}
	chain = &serenaQualificationDirectories{}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, chain.close())
		}
	}()
	volumeRoot := filepath.VolumeName(root) + string(filepath.Separator)
	access := uint32(windows.FILE_TRAVERSE | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL)
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	handle, err := ntOpenStateReadRootMode(volumeRoot, access, share)
	if err != nil {
		return chain, err
	}
	chain.entries = append(chain.entries, serenaQualificationDirectory{path: volumeRoot, handle: &serenaTreeHandle{handle: handle}})
	chain.entries[0].identity, err = serenaTreeHandleIdentity(handle, nil)
	if err != nil || !chain.entries[0].identity.Directory {
		return chain, errors.Join(err, errors.New("SERENA_QUALIFICATION_VOLUME_DIRECTORY_REQUIRED"))
	}
	currentPath := volumeRoot
	private := false
	directory := true
	for _, name := range strings.Split(strings.TrimPrefix(root, volumeRoot), string(filepath.Separator)) {
		if err := ctx.Err(); err != nil {
			return chain, err
		}
		parent := chain.entries[len(chain.entries)-1].handle.handle
		handle, err := ntOpenStateReadRelativeMode(parent, name, access, &directory, windows.FILE_OPEN, share, nil)
		if err != nil {
			return chain, err
		}
		currentPath = filepath.Join(currentPath, name)
		private = private || strings.EqualFold(currentPath, runRoot)
		chain.entries = append(chain.entries, serenaQualificationDirectory{path: currentPath, handle: &serenaTreeHandle{handle: handle}, private: private})
		entry := &chain.entries[len(chain.entries)-1]
		entry.identity, err = serenaTreeHandleIdentity(handle, nil)
		if err != nil || !entry.identity.Directory {
			return chain, errors.Join(err, errors.New("SERENA_QUALIFICATION_NATIVE_DIRECTORY_REQUIRED"))
		}
		if private {
			if err := validateSerenaQualificationDirectoryPolicy(ctx, *entry); err != nil {
				return chain, err
			}
		}
		if strings.EqualFold(currentPath, runRoot) {
			chain.runRootIdentitySHA256 = serenaQualificationDirectoryIdentity(entry.identity)
		}
	}
	if !private || chain.runRootIdentitySHA256 == "" {
		return chain, errors.New("SERENA_QUALIFICATION_NATIVE_RUN_ROOT_NOT_REACHED")
	}
	chain.rootIdentitySHA256 = serenaQualificationDirectoryIdentity(chain.entries[len(chain.entries)-1].identity)
	if err := chain.validate(ctx); err != nil {
		return chain, err
	}
	return chain, nil
}

func serenaQualificationDirectoryIdentity(object SerenaTreeObject) string {
	return stateFileReadObservationDigest([]byte("serena-qualification-directory-v1|" + object.Device + "|" + object.FileID))
}

func validateSerenaQualificationDirectoryPolicy(ctx context.Context, entry serenaQualificationDirectory) error {
	// The observed descriptor parser owns its actual GetSecurityInfo output,
	// SID conversions, and LocalFree results. It does not call the resolver.
	b := newStateFileReadBuilder(ctx, entry.path, 1)
	violations, policyErr := windowsDACLAllowlistViolationsObserved(entry.handle.handle, windowsDACLSignificantBits, b, "qualification-directory")
	if len(violations) != 0 {
		policyErr = errors.Join(policyErr, errors.New("SERENA_QUALIFICATION_DIRECTORY_ACL_OUTSIDE_ALLOWLIST"))
	}
	_, observation, err := b.finish(nil, policyErr)
	if observation == nil || !observation.finalized || !observation.view.OwnedResourcesComplete || !observation.view.BorrowedResourcesComplete {
		return errors.Join(err, errors.New("SERENA_QUALIFICATION_DIRECTORY_POLICY_RESOURCES_PENDING"))
	}
	return err
}

func (chain *serenaQualificationDirectories) validate(ctx context.Context) error {
	if chain == nil || chain.closed || len(chain.entries) < 2 || chain.rootIdentitySHA256 == "" || chain.runRootIdentitySHA256 == "" {
		return errors.New("SERENA_QUALIFICATION_DIRECTORY_OWNER_CLOSED")
	}
	if _, err := serenaQualificationDeadline(ctx); err != nil {
		return err
	}
	for _, entry := range chain.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.handle == nil || entry.handle.close.Attempted {
			return errors.New("SERENA_QUALIFICATION_DIRECTORY_HANDLE_CLOSED")
		}
		current, err := serenaTreeHandleIdentity(entry.handle.handle, nil)
		if err != nil {
			return err
		}
		if !current.Directory || current.Device != entry.identity.Device || current.FileID != entry.identity.FileID {
			return errors.New("SERENA_QUALIFICATION_DIRECTORY_IDENTITY_CHANGED")
		}
		if entry.private {
			if err := validateSerenaQualificationDirectoryPolicy(ctx, entry); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (chain *serenaQualificationDirectories) close() error {
	if chain == nil {
		return nil
	}
	if chain.closed {
		return chain.closeErr
	}
	chain.closed = true
	for i := len(chain.entries) - 1; i >= 0; i-- {
		_, err := chain.entries[i].handle.closeOnce()
		chain.closeErr = errors.Join(chain.closeErr, err)
	}
	return chain.closeErr
}
