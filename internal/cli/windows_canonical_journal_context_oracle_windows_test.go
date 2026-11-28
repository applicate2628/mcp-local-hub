//go:build windows

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func TestValidateOriginalRecoveryUsesCallerContextAtProtectedRead(t *testing.T) {
	outer := t.Context()
	child, cancel := context.WithCancel(outer)
	defer cancel()

	raw := []byte("synthetic Product journal read fixture\n")
	dir := t.TempDir()
	if err := apitest.HardenedDirForTestMain(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, WindowsProductRecoveryFileLeafV3)
	if err := api.SecureWriteClientConfig(path, raw); err != nil {
		t.Fatal(err)
	}
	allowance, err := NewWindowsProductJournalAllowance(int64(len(raw)))
	if err != nil {
		t.Fatal(err)
	}
	control, original, err := readWindowsProductRecoveryJournal(outer, path, allowance)
	if err != nil || original == nil || !bytes.Equal(control, raw) {
		t.Fatalf("live outer context could not read the protected fixture: observation=%t bytes_match=%t error=%v", original != nil, bytes.Equal(control, raw), err)
	}

	const phase = WindowsProductRecoverySuccessorStarting
	evidence := &windowsProductJournalReadEvidence{}
	phaseCalls := 0
	binding := windowsProductJournalBinding{
		ctx:       outer,
		allowance: allowance,
		evidence:  evidence,
		phase: func() string {
			phaseCalls++
			cancel()
			return string(phase)
		},
	}
	source := &windowsCanonicalJournalRecoverySource{
		purpose: canonicalRecoveryRollback,
		req: windowsProductRecoveryRequest{
			Context: outer, JournalAllowance: allowance, journalReadEvidence: evidence,
		},
		deps: withWindowsProductRecoveryDefaults(windowsProductRecoveryDeps{journal: binding}),
		journal: windowsProductRecoveryJournalV3{
			EntryBundle: &windowsCanonicalEntrySetJournalV2{},
			Phase:       phase,
		},
		raw:    raw,
		path:   path,
		active: true,
	}
	source.self = source

	err = source.ValidateOriginalRecovery(child)
	if phaseCalls != 1 || outer.Err() != nil || child.Err() != context.Canceled {
		t.Errorf("phase did not cancel only the caller: calls=%d outer=%v child=%v", phaseCalls, outer.Err(), child.Err())
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("ValidateOriginalRecovery error=%v, want caller cancellation", err)
	}
	if len(evidence.outcomes) != 1 {
		t.Errorf("retained journal outcomes=%d, want one terminal cancellation", len(evidence.outcomes))
	} else {
		outcome := evidence.outcomes[0]
		if outcome.path != path || outcome.phase != string(phase) || outcome.allowance != allowance {
			t.Error("terminal journal outcome lost the selected path, phase, or allowance")
		}
		// Cancellation here precedes API entry, so there is no usable file observation.
		if !errors.Is(outcome.readError, context.Canceled) || outcome.original != nil {
			t.Errorf("retained journal error=%v, original observation=%t; want cancellation before protected read", outcome.readError, outcome.original != nil)
		}
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, raw) {
		t.Fatalf("journal fixture changed: bytes_match=%t error=%v", bytes.Equal(current, raw), err)
	}
}
