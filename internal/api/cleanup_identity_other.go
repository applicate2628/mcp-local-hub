//go:build !windows

package api

import (
	"context"

	"mcp-local-hub/internal/process"
)

var orphanLookupIdentityFn = func(context.Context, int) (process.ProcessIdentity, error) {
	return process.ProcessIdentity{}, process.ErrProcessIdentityUnsupported
}
