//go:build windows

package api

import "mcp-local-hub/internal/process"

var orphanLookupIdentityFn = process.LookupProcessIdentityContext
