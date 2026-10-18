//go:build !windows

package api

import (
	"errors"
	"mcp-local-hub/internal/clients"
)

func WriteStateFileBytesAtomicObserved(clients.ConfigWriteRequest) (*StateFilePublicationObservation, error) {
	return nil, errors.New("STATE_OBSERVED_WRITER_PLATFORM_UNQUALIFIED")
}
func WriteStateFileBytesLockHeldObserved(clients.ConfigWriteRequest, *StateFileObservedLease) (*StateFilePublicationObservation, error) {
	return nil, errors.New("STATE_OBSERVED_WRITER_PLATFORM_UNQUALIFIED")
}
