//go:build !windows

package process

import (
	"context"
	"errors"
)

func CapturePublisherIncarnation(ctx context.Context, binding PublisherIncarnationBinding) (*PublisherIncarnation, error) {
	return nil, errors.New("publisher native capture unsupported on this platform")
}

func ObserveCurrentPublisherNative(ctx context.Context) (*PublisherNativeObservation, error) {
	return nil, errors.New("publisher native capture unsupported on this platform")
}
