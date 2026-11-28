//go:build !windows

package api

import (
	"context"

	"golang.org/x/sys/unix"
)

func readStateBeneathRootWithNativeFaults(ctx context.Context, root string, components []string, policy stateReadBeneathRootPolicy, step stateReadBeneathRootStepFunc, faults *stateReadNativeFaults) ([]byte, error) {
	return readStateFileBeneathRootNoFollowWithNative(ctx, root, components, policy, step, stateReadBeneathRootNative{
		statFinal: func(fd int, info *unix.Stat_t) error {
			faults.statCalls++
			if err := unix.Fstat(fd, info); err != nil {
				return err
			}
			return faults.statErr
		},
		close: func(fd int) error {
			return faults.close(uintptr(fd), func() error { return unix.Close(fd) })
		},
	})
}
