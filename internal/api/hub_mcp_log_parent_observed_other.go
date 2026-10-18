//go:build !windows

package api

import (
	"context"
	"errors"
)

func acquireHubMcpAuditParent(context.Context, *StateReadPolicyObservation) (*hubMcpAuditParent, error) {
	return nil, errors.New("observed Windows audit parent unavailable on this platform")
}
