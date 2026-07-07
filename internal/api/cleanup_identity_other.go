//go:build !windows

package api

import "mcp-local-hub/internal/process"

var orphanLookupIdentityFn = func(int) (process.ProcessIdentity, error) {
	return process.ProcessIdentity{}, process.ErrProcessIdentityUnsupported
}
