//go:build windows

package api

import (
	"context"

	"golang.org/x/sys/windows"
)

func readStateBeneathRootWithNativeFaults(ctx context.Context, root string, components []string, policy stateReadBeneathRootPolicy, step stateReadBeneathRootStepFunc, faults *stateReadNativeFaults) ([]byte, error) {
	return readStateFileBeneathRootNoFollowWithNative(ctx, root, components, policy, step, stateReadBeneathRootNative{
		statFinal: func(handle windows.Handle, info *windows.ByHandleFileInformation) error {
			faults.statCalls++
			if err := windows.GetFileInformationByHandle(handle, info); err != nil {
				return err
			}
			return faults.statErr
		},
		close: func(handle windows.Handle) error {
			return faults.close(uintptr(handle), func() error { return windows.CloseHandle(handle) })
		},
	})
}
