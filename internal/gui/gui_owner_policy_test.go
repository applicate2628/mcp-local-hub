package gui

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// The fixture models settings-operation return boundaries. It owns no native
// settings lock; the tests use real GUI flocks only inside hardened temp dirs.
type guiOwnerPolicyFixture struct {
	control          api.GUIOwnerControl
	resumeReleaseErr error
	readErr          error
	policyReleaseErr error
	beforeReturn     func()
	resumes          int
	reads            int
}

func (p *guiOwnerPolicyFixture) WithGUIOwnerControl(fn func(api.GUIOwnerControl) error) error {
	p.reads++
	if p.beforeReturn != nil {
		defer p.beforeReturn()
	}
	if p.readErr != nil {
		return p.readErr
	}
	return errors.Join(fn(p.control), p.policyReleaseErr)
}

func (p *guiOwnerPolicyFixture) ResumeGUIOwner(expected api.GUIOwnerControl) (api.GUIOwnerControlResult, error) {
	p.resumes++
	if p.beforeReturn != nil {
		defer p.beforeReturn()
	}
	if !p.control.Equal(expected) {
		return api.GUIOwnerControlResult{}, api.ErrGUIControlConflict
	}
	p.control.RunDesired = api.GUIRunRunning
	p.control.StopCause, p.control.StopGeneration = "", ""
	return api.GUIOwnerControlResult{Control: p.control, Applied: true}, p.resumeReleaseErr
}

func guiOwnerPolicyAcquireOptions(restart bool, policy GUIOwnerPolicy) SingleInstanceAcquireOptions {
	options := SingleInstanceAcquireOptions{OwnerPolicy: policy, RestartV3Enabled: restart}
	if restart {
		now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
		options.MarkerStore = phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) { return nil, nil })
		options.Deadlines = restartTestDeadlines(&now)
	}
	return options
}

// Non-fatal assertions let the acquisition return and release its tentative
// lease even when a policy callback observes an ordering regression.
func checkGUIOwnerRecordBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if want == nil {
		if !os.IsNotExist(err) {
			t.Errorf("owner record appeared before policy return: bytes=%q err=%v", got, err)
		}
		return
	}
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("owner record changed: got=%q want=%q err=%v", got, want, err)
	}
}

func checkGUIOwnerLeaseHeld(t *testing.T, path string) {
	t.Helper()
	if unheld, err := ProbeSingleInstanceLockUnheld(path); err != nil || unheld {
		t.Errorf("policy operation lost its original GUI lease: unheld=%t err=%v", unheld, err)
	}
}

func keepGUIOwnerPolicyLease(t *testing.T, lease *SingleInstanceLock) {
	t.Helper()
	if lease == nil {
		return
	}
	// Registered after HardenedTempDir, so settlement and release happen before
	// fixture removal, including when an assertion fails after acquisition.
	t.Cleanup(func() {
		if owner := lease.OwnerLifecycle(); owner != nil {
			if err := owner.TerminalSettle(); err != nil {
				t.Errorf("settle fixture owner: %v", err)
			}
		}
		if err := lease.ReleaseErr(); err != nil {
			t.Errorf("release fixture owner: %v", err)
		}
	})
}

func TestGUIOwnerPolicyAcquisitionKeepsIndependentOwners(t *testing.T) {
	for _, route := range []struct {
		name    string
		restart bool
	}{{"legacy", false}, {"reservation", true}} {
		t.Run(route.name, func(t *testing.T) {
			// The canonical fallback is sealed too, so restoring an original
			// NewAPI call for the negative control cannot reach installed state.
			t.Setenv("LOCALAPPDATA", apitest.HardenedTempDir(t))
			aPath := filepath.Join(apitest.HardenedTempDir(t), "gui.pidport")
			bPath := filepath.Join(apitest.HardenedTempDir(t), "gui.pidport")
			autorun := false
			stopped := api.GUIOwnerControl{
				RunDesired: api.GUIRunStopped, AutorunEnabled: &autorun,
				StopCause: "tray-quit", StopGeneration: strings.Repeat("a", 64),
			}
			aPolicy := &guiOwnerPolicyFixture{control: stopped}
			bPolicy := &guiOwnerPolicyFixture{control: api.GUIOwnerControl{RunDesired: api.GUIRunRunning}}
			var aBefore, bBefore []byte
			aPolicy.beforeReturn = func() {
				checkGUIOwnerRecordBytes(t, aPath, aBefore)
				checkGUIOwnerLeaseHeld(t, aPath)
			}
			bPolicy.beforeReturn = func() {
				checkGUIOwnerRecordBytes(t, bPath, bBefore)
				checkGUIOwnerLeaseHeld(t, bPath)
			}
			aOptions := guiOwnerPolicyAcquireOptions(route.restart, aPolicy)
			aOptions.GUIResumeControl = &stopped
			aLease, err := AcquireSingleInstanceAt(aPath, 9125, aOptions)
			keepGUIOwnerPolicyLease(t, aLease)
			if err != nil {
				t.Fatalf("resume first owner: %v", err)
			}
			bLease, err := AcquireSingleInstanceAt(bPath, 9126, guiOwnerPolicyAcquireOptions(route.restart, bPolicy))
			keepGUIOwnerPolicyLease(t, bLease)
			if err != nil {
				t.Fatalf("acquire second owner: %v", err)
			}
			if aPolicy.resumes != 1 || bPolicy.resumes != 0 || aPolicy.control.RunDesired != api.GUIRunRunning || aPolicy.control.StopCause != "" || aPolicy.control.StopGeneration != "" || aPolicy.control.AutorunEnabled == nil || *aPolicy.control.AutorunEnabled {
				t.Fatalf("resume did not preserve the first owner's policy: first=%#v second=%#v", aPolicy, bPolicy)
			}
			assertV2GUIOwnerRecord(t, aPath, 9125)
			assertV2GUIOwnerRecord(t, bPath, 9126)
			aBefore, err = os.ReadFile(aPath)
			if err != nil {
				t.Fatal(err)
			}
			bBefore, err = os.ReadFile(bPath)
			if err != nil {
				t.Fatal(err)
			}
			originalB, err := ReadGUIOwnerRecord(bPath)
			if err != nil {
				t.Fatal(err)
			}
			// Changing only A's dependency must refuse A while B still uses
			// its own running policy. The production default remains one
			// canonical settings store, not separate settings per GUI owner.
			aPolicy.control = stopped
			if err := aLease.OwnerLifecycle().BeginHandoff("a-handoff", "a-child", 12345, 9127); !errors.Is(err, api.ErrGUIStoppedByUser) {
				t.Errorf("first owner ignored its stopped policy: %v", err)
			}
			checkGUIOwnerRecordBytes(t, aPath, aBefore)
			if err := bLease.OwnerLifecycle().BeginHandoff("b-handoff", "b-child", 12346, 9128); err != nil {
				t.Fatalf("independent second owner refused handoff: %v", err)
			}
			handoffB, err := ReadGUIOwnerRecord(bPath)
			if err != nil {
				t.Fatal(err)
			}
			wantB := originalB
			wantB.State = guiOwnerStateHandoff
			wantB.HandoffID, wantB.HandoffGeneration = "b-handoff", "b-child"
			wantB.HandoffTargetPID, wantB.HandoffTargetPort = 12346, 9128
			if handoffB != wantB {
				t.Errorf("handoff changed the bound owner generation: got=%#v want=%#v", handoffB, wantB)
			}
			if originalB.Generation != guiOwnerGeneration(originalB.PID, originalB.StartTime) {
				t.Error("acquisition did not publish the exact process generation")
			}
			if aPolicy.reads != 2 || bPolicy.reads != 2 {
				t.Errorf("policy was not retained through handoff: first reads=%d second reads=%d", aPolicy.reads, bPolicy.reads)
			}
		})
	}
}

func TestGUIOwnerPolicyAcquisitionRefusalPreservesRecord(t *testing.T) {
	for _, route := range []struct {
		name    string
		restart bool
	}{{"legacy", false}, {"reservation", true}} {
		t.Run(route.name, func(t *testing.T) {
			for _, failure := range []string{"resume_conflict", "resume_release", "policy_read", "stopped", "policy_release"} {
				t.Run(failure, func(t *testing.T) {
					dir := apitest.HardenedTempDir(t)
					t.Setenv("LOCALAPPDATA", dir)
					path := filepath.Join(dir, "gui.pidport")
					start := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
					prior := GUIOwnerRecord{Version: 2, State: guiOwnerStateTombstone, PID: 42, StartTime: start, Generation: guiOwnerGeneration(42, start)}
					if err := writeGUIOwnerRecord(path, prior); err != nil {
						t.Fatal(err)
					}
					before, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					policy := &guiOwnerPolicyFixture{control: api.GUIOwnerControl{RunDesired: api.GUIRunRunning}}
					policy.beforeReturn = func() {
						checkGUIOwnerRecordBytes(t, path, before)
						checkGUIOwnerLeaseHeld(t, path)
					}
					options := guiOwnerPolicyAcquireOptions(route.restart, policy)
					wantErr := errors.New("synthetic settings failure")
					switch failure {
					case "resume_conflict":
						options.GUIResumeControl = &api.GUIOwnerControl{RunDesired: api.GUIRunStopped, StopCause: "tray-quit"}
						wantErr = api.ErrGUIControlConflict
					case "resume_release":
						policy.control = api.GUIOwnerControl{RunDesired: api.GUIRunStopped, StopCause: "tray-quit"}
						expected := policy.control
						options.GUIResumeControl = &expected
						policy.resumeReleaseErr = wantErr
					case "policy_read":
						policy.readErr = wantErr
					case "stopped":
						policy.control = api.GUIOwnerControl{RunDesired: api.GUIRunStopped, StopCause: "tray-quit"}
						wantErr = api.ErrGUIStoppedByUser
					case "policy_release":
						policy.policyReleaseErr = wantErr
					}
					lease, err := AcquireSingleInstanceAt(path, 9125, options)
					keepGUIOwnerPolicyLease(t, lease)
					if lease != nil || !errors.Is(err, wantErr) {
						t.Fatalf("failed settings operation published an owner: lease=%v err=%v want=%v", lease != nil, err, wantErr)
					}
					checkGUIOwnerRecordBytes(t, path, before)
					if unheld, err := ProbeSingleInstanceLockUnheld(path); err != nil || !unheld {
						t.Errorf("refused acquisition retained its tentative lease: unheld=%t err=%v", unheld, err)
					}
					if options.GUIResumeControl != nil && policy.reads != 0 {
						t.Error("resume failure continued into automatic-start admission")
					}
					if failure == "resume_release" && policy.control.RunDesired != api.GUIRunRunning {
						t.Error("resume release fixture did not model an applied policy followed by release failure")
					}
				})
			}
		})
	}
}

func TestGUIOwnerPolicyHandoffRefusalPreservesRecord(t *testing.T) {
	for _, failure := range []string{"policy_read", "policy_release"} {
		t.Run(failure, func(t *testing.T) {
			dir := apitest.HardenedTempDir(t)
			t.Setenv("LOCALAPPDATA", dir)
			path := filepath.Join(dir, "gui.pidport")
			policy := &guiOwnerPolicyFixture{control: api.GUIOwnerControl{RunDesired: api.GUIRunRunning}}
			lease, err := AcquireSingleInstanceAt(path, 9125, guiOwnerPolicyAcquireOptions(false, policy))
			keepGUIOwnerPolicyLease(t, lease)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			policy.beforeReturn = func() {
				checkGUIOwnerRecordBytes(t, path, before)
				checkGUIOwnerLeaseHeld(t, path)
			}
			wantErr := errors.New("synthetic settings failure")
			if failure == "policy_read" {
				policy.readErr = wantErr
			} else {
				policy.policyReleaseErr = wantErr
			}
			if err := lease.OwnerLifecycle().BeginHandoff("handoff", "child", 12345, 9126); !errors.Is(err, wantErr) {
				t.Errorf("handoff ignored settings failure: got=%v want=%v", err, wantErr)
			}
			checkGUIOwnerRecordBytes(t, path, before)
			checkGUIOwnerLeaseHeld(t, path)
		})
	}
}
