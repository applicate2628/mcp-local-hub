//go:build !windows

package process

import (
	"context"
	"errors"
)

func LookupPublisherIncarnation(ctx context.Context, record PublisherIncarnationRecordV1, binding PublisherIncarnationBinding) (*PublisherQuiescenceObservation, error) {
	return nil, errors.New("native publisher lookup is unavailable on this platform")
}
