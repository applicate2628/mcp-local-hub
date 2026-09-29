//go:build windows && cst_vptr_probe

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/e2e/cstwatchdogwire"
)

// A B0-only receipt is not evidence that a target, debugger event or stream
// was ever owned in a Task 2 cell, even when its root and source still match.
func TestF4ParentRejectsB0OnlyPostflightForTask2Cells(t *testing.T) {
	for _, cell := range []string{
		"b1_crash",
		"b2_crash",
		"posttoken_held_cancel",
		"positive_settled",
	} {
		t.Run(cell, func(t *testing.T) {
			root := f4Task2EvidenceFixture(t, cell)
			data, err := json.Marshal(cstwatchdogwire.Evidence{
				Version: cstwatchdogwire.EvidenceVersion, Cell: cell,
				RootToken: hex.EncodeToString(root.Token[:]),
				Attempted: 1, Barrier: 1, Fired: 1, Finished: 1,
				Cause: "synthetic_b0_only",
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root.Path, f4EvidenceName), data, 0600); err != nil {
				t.Fatal(err)
			}
			state := f4RunState{root: root, config: f4RunConfig{Cell: cell}, control: &f4Control{
				lastFlags: cstwatchdogwire.Attempted | cstwatchdogwire.BarrierSeen | cstwatchdogwire.InjectionFired | cstwatchdogwire.FinishedSeen,
			}}
			var verdict f4Verdict
			err = state.normalPostflight(&verdict, "synthetic_b0_only")
			if err == nil {
				t.Fatal("B0-only parent postflight accepted without Task 2 target/debugger/stream proof")
			}
		})
	}
}

func TestF4ParentTargetObservationRejectsForeignOutsideJob(t *testing.T) {
	job, err := f4NewOuterJob()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := job.Close(); err != nil {
			t.Errorf("empty test Job close: %v", err)
		}
	})
	pid := uint32(os.Getpid())
	self, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		t.Fatal(err)
	}
	created, err := f4ProcessCreated(self)
	closeErr := windows.CloseHandle(self)
	if err != nil || closeErr != nil || created <= 0 {
		t.Fatalf("self creation identity: created=%d err=%v close=%v", created, err, closeErr)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	state := f4RunState{job: job, config: f4RunConfig{Binary: binary, BinarySHA256: hash}}
	target := &cstwatchdogwire.Task2Target{PID: pid, CreationTicks: uint64(created), ImageSHA256: hash}
	if err := state.captureTargetObservation(target); err == nil {
		t.Fatal("foreign current process was accepted as a member of the empty owned Job")
	}
	if state.targetObservation != 0 {
		t.Fatal("rejected observation leaked a parent process handle")
	}
}

func TestF4ParentAcceptsCompleteB1Evidence(t *testing.T) {
	root := f4Task2EvidenceFixture(t, "b1_crash")
	fixture := fmt.Sprintf(`{"version":"CSTF4WatchdogEvidenceV1","cell":"b1_crash","rootToken":%q,"attempted":1,"barrier":1,"fired":1,"finished":1,"cause":"custodian_lost","task2":{"barrier":"B1","observerState":"none","f3NonceDigest":%q,"f3Sequence":2,"slotSha256":%q,"target":{"pid":101,"creationTicks":1,"imageSha256":%q,"originalRetained":true,"startClosed":true,"innerMember":true},"settlement":{"targetJobClose":"unknown","childWait":"unknown","stdinJoin":"unknown","stdoutEOF":"unknown","stderrEOF":"unknown","settledExit":"unknown"}}}`,
		hex.EncodeToString(root.Token[:]), strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64))
	if err := os.WriteFile(filepath.Join(root.Path, f4EvidenceName), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	state := f4RunState{root: root, config: f4RunConfig{Cell: "b1_crash"}, control: &f4Control{
		lastFlags: cstwatchdogwire.Attempted | cstwatchdogwire.BarrierSeen | cstwatchdogwire.InjectionFired | cstwatchdogwire.FinishedSeen,
	}}
	var verdict f4Verdict
	if err := state.normalPostflight(&verdict, "custodian_lost"); err != nil {
		t.Fatalf("complete B1 evidence rejected: %v", err)
	}
}

func TestF4ParentB2RequiresHeldUncontinuedEvent(t *testing.T) {
	owned, one, zero := true, 1, 0
	target := &cstwatchdogwire.Task2Target{PID: 101, CreationTicks: 1, ImageSHA256: strings.Repeat("c", 64),
		OriginalRetained: &owned, StartClosed: &owned, InnerMember: &owned}
	settlement := &cstwatchdogwire.Task2Settlement{TargetJobClose: "unknown", ChildWait: "unknown", StdinJoin: "unknown",
		StdoutEOF: "unknown", StderrEOF: "unknown", SettledExit: "unknown"}
	base := cstwatchdogwire.Task2Evidence{Barrier: "B2", ObserverState: "none", Target: target, Settlement: settlement,
		Debugger: &cstwatchdogwire.Task2Debugger{HeldEvents: &one, OwnerBound: &owned, Continued: "unknown", Detached: "unknown", StopAck: "unknown"}}
	if err := f4ValidateSelectedEvidence("b2_crash", "", cstwatchdogwire.Evidence{Task2: &base}); err != nil {
		t.Fatalf("held B2 control rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*cstwatchdogwire.Task2Evidence)
	}{
		{"missing_debugger", func(v *cstwatchdogwire.Task2Evidence) { v.Debugger = nil }},
		{"zero_held_events", func(v *cstwatchdogwire.Task2Evidence) {
			debugger := *v.Debugger
			debugger.HeldEvents = &zero
			v.Debugger = &debugger
		}},
		{"continued_before_barrier", func(v *cstwatchdogwire.Task2Evidence) {
			debugger := *v.Debugger
			debugger.Continued = "pass"
			v.Debugger = &debugger
		}},
		{"detached_before_barrier", func(v *cstwatchdogwire.Task2Evidence) {
			debugger := *v.Debugger
			debugger.Detached = "pass"
			v.Debugger = &debugger
		}},
		{"stop_ack_before_barrier", func(v *cstwatchdogwire.Task2Evidence) {
			debugger := *v.Debugger
			debugger.StopAck = "pass"
			v.Debugger = &debugger
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrong := base
			tc.change(&wrong)
			if err := f4ValidateSelectedEvidence("b2_crash", "", cstwatchdogwire.Evidence{Task2: &wrong}); err == nil {
				t.Fatal("B2 parent accepted missing, zero, continued, detached or stopped debugger event")
			}
		})
	}
}

func TestF4ParentPostTokenRejectsUnheldOwnedToken(t *testing.T) {
	root := f4Task2EvidenceFixture(t, "posttoken_held_cancel")
	owned, one, zero := true, 1, 0
	checkpoint := cstwatchdogwire.Evidence{Version: cstwatchdogwire.EvidenceVersion, Cell: "posttoken_held_cancel",
		RootToken: hex.EncodeToString(root.Token[:]), Attempted: 1, Barrier: 1, Cause: "barrier_checkpoint",
		Task2: &cstwatchdogwire.Task2Evidence{Barrier: "B2", ObserverState: "unsettled_owned",
			F3NonceDigest: strings.Repeat("a", 64), F3Sequence: 2, SlotSHA256: strings.Repeat("b", 64),
			Target: &cstwatchdogwire.Task2Target{PID: 101, CreationTicks: 1, ImageSHA256: strings.Repeat("c", 64),
				OriginalRetained: &owned, StartClosed: &owned, InnerMember: &owned},
			Debugger: &cstwatchdogwire.Task2Debugger{HeldEvents: &one, OwnerBound: &owned,
				Continued: "unknown", Detached: "unknown", StopAck: "unknown"},
			Settlement: &cstwatchdogwire.Task2Settlement{TargetJobClose: "unknown", ChildWait: "unknown", StdinJoin: "unknown",
				StdoutEOF: "unknown", StderrEOF: "unknown", SettledExit: "unknown"}}}
	write := func() {
		t.Helper()
		data, err := json.Marshal(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root.Path, f4EvidenceName), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, _, err := f4ReadTask2Checkpoint(root, "posttoken_held_cancel", cstwatchdogwire.Attempted|cstwatchdogwire.BarrierSeen); err != nil {
		t.Fatalf("valid post-token held checkpoint rejected: %v", err)
	}
	checkpoint.Task2.Debugger.HeldEvents = &zero
	write()
	if _, _, err := f4ReadTask2Checkpoint(root, "posttoken_held_cancel", cstwatchdogwire.Attempted|cstwatchdogwire.BarrierSeen); err == nil {
		t.Fatal("post-token checkpoint accepted unsettled_owned without a held debugger event")
	}
}

func TestF4ParentPostTokenContinueFailureRequiresFailedHeldAttempt(t *testing.T) {
	owned, one, zero := true, 1, 0
	value := cstwatchdogwire.Task2Evidence{Barrier: "B2", ObserverState: "unsettled_owned",
		Target: &cstwatchdogwire.Task2Target{PID: 101, CreationTicks: 1, ImageSHA256: strings.Repeat("c", 64),
			OriginalRetained: &owned, StartClosed: &owned, InnerMember: &owned},
		Debugger: &cstwatchdogwire.Task2Debugger{HeldEvents: &one, OwnerBound: &owned,
			Continued: "fail", Detached: "unknown", StopAck: "unknown"},
		Settlement: &cstwatchdogwire.Task2Settlement{TargetJobClose: "unknown", ChildWait: "unknown", StdinJoin: "unknown",
			StdoutEOF: "unknown", StderrEOF: "unknown", SettledExit: "unknown"}}
	if err := f4ValidateSelectedEvidence("posttoken_held_cancel", "continue_failed", cstwatchdogwire.Evidence{Task2: &value}); err != nil {
		t.Fatalf("typed held Continue failure rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*cstwatchdogwire.Task2Evidence)
	}{
		{"no_failed_continue", func(v *cstwatchdogwire.Task2Evidence) {
			debugger := *v.Debugger
			debugger.Continued = "unknown"
			v.Debugger = &debugger
		}},
		{"event_not_held", func(v *cstwatchdogwire.Task2Evidence) {
			debugger := *v.Debugger
			debugger.HeldEvents = &zero
			v.Debugger = &debugger
		}},
		{"false_settled_exit", func(v *cstwatchdogwire.Task2Evidence) {
			settlement := *v.Settlement
			settlement.SettledExit = "pass"
			v.Settlement = &settlement
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrong := value
			tc.change(&wrong)
			if err := f4ValidateSelectedEvidence("posttoken_held_cancel", "continue_failed", cstwatchdogwire.Evidence{Task2: &wrong}); err == nil {
				t.Fatal("post-token Continue failure accepted without its exact negative facts")
			}
		})
	}
}

func TestF4ParentB1CheckpointRequiresRegisteredLeaf(t *testing.T) {
	root := f4Task2EvidenceFixture(t, "b1_crash")
	fixture := fmt.Sprintf(`{"version":"CSTF4WatchdogEvidenceV1","cell":"b1_crash","rootToken":%q,"attempted":1,"barrier":1,"fired":0,"finished":0,"cause":"barrier_checkpoint","task2":{"barrier":"B1","observerState":"none","f3NonceDigest":%q,"f3Sequence":2,"slotSha256":%q,"target":{"pid":101,"creationTicks":1,"imageSha256":%q,"originalRetained":true,"startClosed":true,"innerMember":true},"settlement":{"targetJobClose":"unknown","childWait":"unknown","stdinJoin":"unknown","stdoutEOF":"unknown","stderrEOF":"unknown","settledExit":"unknown"}}}`,
		hex.EncodeToString(root.Token[:]), strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64))
	path := filepath.Join(root.Path, f4EvidenceName)
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f4ReadTask2Checkpoint(root, "b1_crash", cstwatchdogwire.Attempted|cstwatchdogwire.BarrierSeen); err != nil {
		t.Fatalf("registered B1 partial checkpoint rejected: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f4ReadTask2Checkpoint(root, "b1_crash", cstwatchdogwire.Attempted|cstwatchdogwire.BarrierSeen); err == nil {
		t.Fatal("replaced evidence leaf accepted despite registered identity drift")
	}
}

func f4Task2EvidenceFixture(t *testing.T, cell string) *f4OwnedRoot {
	t.Helper()
	parent, err := os.MkdirTemp("", "cst-task2-evidence-")
	if err != nil {
		t.Fatal(err)
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		t.Fatal(err)
	}
	var root *f4OwnedRoot
	t.Cleanup(func() {
		if root != nil {
			if err := root.Close(); err != nil {
				t.Errorf("root handle close: %v; preserving %s", err, root.Path)
				return
			}
			entries, err := os.ReadDir(root.Path)
			if err != nil || len(entries) != 3 {
				t.Errorf("unexpected test root leaves: count=%d err=%v; preserving %s", len(entries), err, root.Path)
				return
			}
			for _, name := range []string{f4EvidenceName, f4MarkerName, "synthetic-source.bin"} {
				info, err := os.Lstat(filepath.Join(root.Path, name))
				if err != nil || !info.Mode().IsRegular() {
					t.Errorf("test leaf changed %s: info=%v err=%v; preserving %s", name, info, err, root.Path)
					return
				}
			}
			for _, name := range []string{f4EvidenceName, f4MarkerName, "synthetic-source.bin"} {
				if err := os.Remove(filepath.Join(root.Path, name)); err != nil {
					t.Errorf("remove exact test leaf %s: %v; preserving %s", name, err, root.Path)
					return
				}
			}
			if err := os.Remove(root.Path); err != nil {
				t.Errorf("remove empty exact test root: %v; preserving %s", err, root.Path)
				return
			}
		}
		if err := os.Remove(parent); err != nil {
			t.Errorf("remove empty exact test parent: %v; preserving %s", err, parent)
		}
	})
	root, err = f4CreateBoundRoot(parent, cell)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root.Path, "synthetic-source.bin"), []byte("CST observer fake source; no native project\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}
