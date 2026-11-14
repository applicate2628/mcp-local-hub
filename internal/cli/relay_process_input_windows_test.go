//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestProcessRelayInputWindowsQualification(t *testing.T) {
	t.Run("regular_file_stays_uninterruptible", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "regular")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		owner := &relayProcessInput{file: file}
		interrupt, err := processRelayInputInterrupt(owner)
		if err != nil || interrupt != nil {
			t.Fatalf("regular file acquired unsupported stop action: %v", err)
		}
		if _, err := file.Stat(); err != nil {
			t.Fatalf("qualification closed regular input: %v", err)
		}
	})
	t.Run("closed_handle_refused", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "closed")
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		interrupt, err := processRelayInputInterrupt(&relayProcessInput{file: file})
		if err == nil || interrupt != nil {
			t.Fatal("invalid native handle qualified")
		}
	})
	t.Run("owned_pipe_has_explicit_stop", func(t *testing.T) {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		defer writer.Close()
		owner := &relayProcessInput{file: reader}
		interrupt, err := processRelayInputInterrupt(owner)
		if err != nil || interrupt == nil {
			t.Fatalf("owned pipe did not qualify: %v", err)
		}
		if err := interrupt(); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
			t.Fatalf("owner stop not normalized: %v", err)
		}
		if _, err := writer.Stat(); err != nil {
			t.Fatalf("foreign pipe end closed: %v", err)
		}
	})
}
