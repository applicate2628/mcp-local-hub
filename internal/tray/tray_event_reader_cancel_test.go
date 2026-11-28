package tray

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestReadAttemptEvents_CancelledWarningDeliverySettlesNormally(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// No receiver can accept a warning. Cancellation must settle the reader
	// without turning normal lifecycle shutdown into an error.
	warnings := make(chan struct{})
	err := readAttemptEvents(ctx, strings.NewReader("{\"event\":\n"), Config{}, warnings)
	if err != nil {
		t.Fatalf("readAttemptEvents after cancellation = %v, want nil", err)
	}
}

func TestReadAttemptEvents_PreservesScannerFailureDuringCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	input := strings.NewReader(strings.Repeat("x", bufio.MaxScanTokenSize+1))
	err := readAttemptEvents(ctx, input, Config{}, make(chan struct{}))
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("readAttemptEvents scanner failure = %v, want %v", err, bufio.ErrTooLong)
	}
}
