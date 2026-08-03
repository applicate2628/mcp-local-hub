//go:build !windows

package api

import (
	"context"

	"golang.org/x/sys/unix"
)

// ReadStateFileBeneathRootNoFollow reads one bounded regular state file through
// a no-follow descriptor chain rooted at root. The returned bytes are exactly
// the bytes whose digest was verified; no pathname is reopened after loading.
func ReadStateFileBeneathRootNoFollow(ctx context.Context, root string, relativeComponents []string, expectedSHA256 string) ([]byte, error) {
	return readStateFileBeneathRootNoFollow(ctx, root, relativeComponents, expectedSHA256, nil)
}

// ReadClientConfigBackupBeneathRootNoFollow reads one bounded client-config
// backup through the same retained no-follow descriptor chain as state files.
// expectedSHA256 is empty only while capturing a fresh forward backup; callers
// restoring a persisted pin must pass its persisted SHA-256.
func ReadClientConfigBackupBeneathRootNoFollow(ctx context.Context, root string, relativeComponents []string, expectedSHA256 string) ([]byte, error) {
	return readStateFileBeneathRootNoFollowWithPolicy(ctx, root, relativeComponents, stateReadBeneathRootPolicy{
		maxBytes: MaxClientConfigBackupBytes, expectedSHA256: expectedSHA256, allowEmptyExpectedSHA256: true,
	}, nil)
}

// readStateFileBeneathRootNoFollow accepts a per-call test step between
// retained-handle operations. The exported reader always passes nil; no
// process-global hook is retained.
func readStateFileBeneathRootNoFollow(
	ctx context.Context,
	root string,
	relativeComponents []string,
	expectedSHA256 string,
	step stateReadBeneathRootStepFunc,
) (result []byte, retErr error) {
	return readStateFileBeneathRootNoFollowWithPolicy(ctx, root, relativeComponents, stateReadBeneathRootPolicy{
		maxBytes: maxStateFileBytes, expectedSHA256: expectedSHA256,
	}, step)
}

func readStateFileBeneathRootNoFollowWithPolicy(
	ctx context.Context,
	root string,
	relativeComponents []string,
	policy stateReadBeneathRootPolicy,
	step stateReadBeneathRootStepFunc,
) (result []byte, retErr error) {
	if err := validateStateReadBeneathRootInputWithPolicy(root, relativeComponents, policy); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, newStateFileReadError(StateFileReadErrorCanceled, "before-root-open", "", err)
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "open-root", "", err)
	}
	currentFD := rootFD
	defer func() {
		if closeErr := unix.Close(currentFD); closeErr != nil && retErr == nil {
			zeroStateSecretBytes(result)
			result = nil
			retErr = newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "close-parent", "", closeErr)
		}
	}()

	for i, component := range relativeComponents[:len(relativeComponents)-1] {
		if err := ctx.Err(); err != nil {
			return nil, newStateFileReadError(StateFileReadErrorCanceled, "before-component-open", component, err)
		}
		if err := invokeStateReadBeneathRootStep(step, stateReadBeneathRootStep{
			Event: stateReadBeneathRootBeforeComponentOpen, ComponentIndex: i, Component: component,
		}); err != nil {
			return nil, err
		}
		nextFD, openErr := unix.Openat(currentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "open-component", component, openErr)
		}
		if closeErr := unix.Close(currentFD); closeErr != nil {
			_ = unix.Close(nextFD)
			return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "close-parent", component, closeErr)
		}
		currentFD = nextFD
	}
	finalComponent := relativeComponents[len(relativeComponents)-1]
	if err := ctx.Err(); err != nil {
		return nil, newStateFileReadError(StateFileReadErrorCanceled, "before-final-open", finalComponent, err)
	}
	if err := invokeStateReadBeneathRootStep(step, stateReadBeneathRootStep{
		Event: stateReadBeneathRootBeforeComponentOpen, ComponentIndex: len(relativeComponents) - 1, Component: finalComponent,
	}); err != nil {
		return nil, err
	}
	finalFD, err := unix.Openat(currentFD, finalComponent, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "open-final", finalComponent, err)
	}
	defer func() {
		if closeErr := unix.Close(finalFD); closeErr != nil && retErr == nil {
			zeroStateSecretBytes(result)
			result = nil
			retErr = newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "close-final", finalComponent, closeErr)
		}
	}()
	var st unix.Stat_t
	if err := unix.Fstat(finalFD, &st); err != nil {
		return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "stat-final", finalComponent, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "stat-final", finalComponent, nil)
	}
	if st.Size > int64(policy.maxBytes) {
		return nil, newStateFileReadError(StateFileReadErrorTooLarge, "stat-final", finalComponent, nil)
	}
	buf := make([]byte, 0, minStateReadCapacity(st.Size, policy.maxBytes))
	chunk := make([]byte, stateReadChunkSize)
	defer zeroStateSecretBytes(chunk)
	for {
		if err := ctx.Err(); err != nil {
			zeroStateSecretBytes(buf)
			return nil, newStateFileReadError(StateFileReadErrorCanceled, "before-read", finalComponent, err)
		}
		requested := stateReadRequestLimitWithCap(len(buf), len(chunk), policy.maxBytes)
		if requested == 0 {
			zeroStateSecretBytes(buf)
			return nil, newStateFileReadError(StateFileReadErrorTooLarge, "before-read", finalComponent, nil)
		}
		if err := invokeStateReadBeneathRootStep(step, stateReadBeneathRootStep{
			Event: stateReadBeneathRootBeforeRead, ComponentIndex: len(relativeComponents) - 1,
			Component: finalComponent, Requested: requested,
		}); err != nil {
			zeroStateSecretBytes(buf)
			return nil, err
		}
		n, readErr := unix.Read(finalFD, chunk[:requested])
		remaining := policy.maxBytes - len(buf)
		if n > remaining {
			zeroStateSecretBytes(buf)
			return nil, newStateFileReadError(StateFileReadErrorTooLarge, "read-final", finalComponent, nil)
		}
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if readErr != nil {
			zeroStateSecretBytes(buf)
			return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "read-final", finalComponent, readErr)
		}
		if n == 0 {
			break
		}
	}
	if buf == nil {
		buf = []byte{}
	}
	if policy.expectedSHA256 != "" && !stateReadChecksumMatches(buf, policy.expectedSHA256) {
		zeroStateSecretBytes(buf)
		return nil, newStateFileReadError(StateFileReadErrorChecksumMismatch, "verify-checksum", finalComponent, nil)
	}
	return buf, nil
}
