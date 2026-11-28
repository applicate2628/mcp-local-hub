package api

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/scheduler"
)

// Generation is deliberately separate from definition bytes. The replacement
// runs after the snapshot bytes have been captured, without a native scheduler,
// goroutine scheduling, or sleeps. Delete records the generation it would hit.
type replacementGenerationScheduler struct {
	*weeklyAtomicScheduler
	generation         uint64
	replacementXML     []byte
	replaced           bool
	deletedGenerations []uint64
}

func (s *replacementGenerationScheduler) ExportXML(name string) ([]byte, error) {
	raw, err := s.weeklyAtomicScheduler.ExportXML(name)
	if err == nil && !s.replaced {
		s.replaced = true
		s.generation++
		s.mu.Lock()
		s.xml[name] = bytes.Clone(s.replacementXML)
		s.mu.Unlock()
	}
	return raw, err
}

func (s *replacementGenerationScheduler) Delete(name string) error {
	s.deletedGenerations = append(s.deletedGenerations, s.generation)
	return s.weeklyAtomicScheduler.Delete(name)
}

func TestSchedulerReplacementRefusalPreservesNewGeneration(t *testing.T) {
	for _, owner := range []string{"weekly", "upgrade"} {
		for _, definition := range []string{"different-xml", "identical-xml"} {
			t.Run(owner+"/"+definition, func(t *testing.T) {
				prior := []byte("<Task>observed-generation</Task>")
				replacement := []byte("<Task>external-generation</Task>")
				if definition == "identical-xml" {
					replacement = bytes.Clone(prior)
				}
				base := &weeklyAtomicScheduler{xml: map[string][]byte{WeeklyRefreshTaskName: bytes.Clone(prior)}}
				root := filepath.Dir(installWeeklyAtomicHarness(t, base))
				sch := &replacementGenerationScheduler{weeklyAtomicScheduler: base, generation: 1, replacementXML: replacement}
				spec := weeklyRefreshTaskSpec(filepath.Join(root, mcphubShortName), nil)
				var err error
				var result weeklyRefreshTransactionResult
				desiredCalled := false
				if owner == "weekly" {
					result, err = runWeeklyRefreshTaskTransaction(sch, weeklyRefreshMutation{
						desired: func([]byte, bool) (scheduler.TaskSpec, error) {
							desiredCalled = true
							return spec, nil
						},
					})
				} else {
					var snapshot []byte
					snapshot, err = snapshotTaskForSchedulerUpgrade(sch, spec.Name)
					if err != nil {
						t.Fatal(err)
					}
					err = replaceTaskForSchedulerUpgrade(sch, spec, snapshot)
				}
				// Check the concrete effect before the result vocabulary so a
				// negative control records which external generation was deleted.
				if sch.generation != 2 || !bytes.Equal(base.xml[WeeklyRefreshTaskName], replacement) ||
					len(sch.deletedGenerations) != 0 || base.deleteCalls != 0 || base.createCalls != 0 || base.restoreCalls != 0 || base.importCalls != 0 {
					t.Fatalf("external generation was mutated: generation=%d deletedGenerations=%v delete/create/restore/import=%d/%d/%d/%d current=%q err=%v", sch.generation, sch.deletedGenerations, base.deleteCalls, base.createCalls, base.restoreCalls, base.importCalls, base.xml[WeeklyRefreshTaskName], err)
				}
				if owner == "weekly" && (desiredCalled || result.state != "" || result.restoreStatus != "n/a" || len(result.finalXML) != 0) {
					t.Fatalf("existing weekly task reached preparation or reported success: called=%t result=%+v", desiredCalled, result)
				}
				var refusal *SchedulerReplacementRefusedError
				if !errors.Is(err, ErrSchedulerReplacementUnowned) || !errors.As(err, &refusal) {
					t.Fatalf("result = %v, want typed replacement refusal", err)
				}
				path := readInstallRecoverySnapshot(t, root, WeeklyRefreshTaskName, prior)
				if refusal.TaskName != WeeklyRefreshTaskName || refusal.SnapshotPath != path || refusal.Cause != nil {
					t.Fatalf("refusal metadata = %+v, want original snapshot at %q", refusal, path)
				}
			})
		}
	}
}

func TestSchedulerReplacementRefusalRetainsStorageFailure(t *testing.T) {
	root := hardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(root))
	err := refuseSchedulerTaskReplacement("existing-task", bytes.Repeat([]byte{'x'}, maxStateFileBytes))
	var refusal *SchedulerReplacementRefusedError
	if !errors.Is(err, ErrSchedulerReplacementUnowned) || !errors.As(err, &refusal) || refusal.Cause == nil {
		t.Fatalf("snapshot failure lost the refusal or storage cause: %v", err)
	}
	if !errors.Is(err, refusal.Cause) || refusal.SnapshotPath != "" || strings.Contains(err.Error(), "snapshot retained") {
		t.Fatalf("unconfirmed snapshot was reported as retained: %+v", refusal)
	}
	if paths, globErr := filepath.Glob(filepath.Join(root, "scheduler-recovery-*.json")); globErr != nil || len(paths) != 0 {
		t.Fatalf("oversized snapshot created unexpected files=%v err=%v", paths, globErr)
	}
}

func TestRetainAutostartTaskReplacementRefusal(t *testing.T) {
	root := hardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(root))
	observed := scheduler.EncodeXMLUTF16LEBOM("<Task>autostart-definition-canary</Task>")
	createErr := scheduler.ErrMutationOutcomeUnknown
	input := autostart.NewObservedTaskReplacementRefusal(autostart.WindowsTaskName, observed, createErr)

	err := RetainAutostartTaskReplacementRefusal(input)
	var refusal *SchedulerReplacementRefusedError
	if !errors.Is(err, createErr) || !errors.Is(err, scheduler.ErrTaskReplacementUnowned) || !errors.As(err, &refusal) {
		t.Fatalf("autostart refusal lost the typed causes: %v", err)
	}
	if path := readInstallRecoverySnapshot(t, root, autostart.WindowsTaskName, observed); refusal.SnapshotPath != path {
		t.Fatalf("snapshot path = %q, want %q", refusal.SnapshotPath, path)
	}
	if strings.Contains(err.Error(), "autostart-definition-canary") {
		t.Fatal("refusal exposed the observed definition in diagnostics")
	}

	ordinary := errors.New("ordinary enable failure")
	if RetainAutostartTaskReplacementRefusal(ordinary) != ordinary || RetainAutostartTaskReplacementRefusal(nil) != nil {
		t.Fatal("refusal processing changed the normal Enable result")
	}
}
