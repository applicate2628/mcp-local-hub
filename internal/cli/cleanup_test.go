package cli

import (
	"strings"
	"testing"
)

func TestCleanupRejectsDryRunFalseWithoutConfirm(t *testing.T) {
	cmd := newCleanupCmdReal()
	cmd.SetArgs([]string{"--dry-run=false"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when kill mode is requested without --confirm")
	}
	if !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("expected error to mention --confirm, got: %v", err)
	}
}
