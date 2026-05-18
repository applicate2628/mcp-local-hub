//go:build linux

package process

import (
	"testing"
	"time"
)

func TestProcessStartTimeFromBootAndJiffiesUsesRuntimeHz(t *testing.T) {
	got, ok := processStartTimeFromBootAndJiffies(1_700_000_000, 375, 250)
	if !ok {
		t.Fatal("processStartTimeFromBootAndJiffies returned !ok")
	}
	want := time.Unix(1_700_000_001, 500_000_000).UTC()
	if !got.Equal(want) {
		t.Fatalf("processStartTimeFromBootAndJiffies = %s, want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestProcessStartTimeFromBootAndJiffiesRejectsInvalidHz(t *testing.T) {
	if got, ok := processStartTimeFromBootAndJiffies(1_700_000_000, 375, 0); ok {
		t.Fatalf("processStartTimeFromBootAndJiffies invalid hz = %s, true; want !ok", got.Format(time.RFC3339Nano))
	}
}
