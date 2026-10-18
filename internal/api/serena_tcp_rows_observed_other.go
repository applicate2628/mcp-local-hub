//go:build !windows

package api

import (
	"context"
	"errors"
)

func ObserveOriginalLoopbackTCPRowsObserved(context.Context) (*LoopbackTCPRowsObservation, error) {
	return nil, errors.New("ORIGINAL_TCP_QUERY_PLATFORM_UNQUALIFIED")
}
