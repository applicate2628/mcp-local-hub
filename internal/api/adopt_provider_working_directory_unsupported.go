//go:build !windows

package api

import (
	"context"
	"fmt"
)

func providerWorkingDirectoryObjectIdentityFromPath(string) (providerDirectoryObjectIdentityV1, error) {
	return providerDirectoryObjectIdentityV1{}, fmt.Errorf("provider working-directory identity is unsupported")
}

func probeProviderProcessWorkingDirectory(context.Context, providerProcessWorkingDirectoryCandidateV1, providerDirectoryObjectIdentityV1) providerProcessWorkingDirectoryResultV1 {
	return providerWorkingDirectoryUnknownUnsupported()
}

func providerWorkingDirectoryUnknownUnsupported() providerProcessWorkingDirectoryResultV1 {
	return providerProcessWorkingDirectoryResultV1{
		State:     providerProcessWorkingDirectoryUnknown,
		FailureID: "provider-working-directory-layout-unsupported",
		Err:       fmt.Errorf("provider working-directory identity is unsupported"),
	}
}
