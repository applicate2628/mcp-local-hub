//go:build windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

type windowsCanonicalEntryFixtureInput struct {
	ProductRoot          string `json:"product_root"`
	SourceEpoch          string `json:"source_epoch"`
	ProductVersion       string `json:"product_version"`
	BuildID              string `json:"build_id"`
	ReverseProjection    string `json:"reverse_projection"`
	CanonicalEntrySHA256 string `json:"canonical_entry_sha256"`
	ProductSHA256        string `json:"product_sha256"`
	IdentitySHA256       string `json:"identity_sha256"`
}

type windowsCanonicalEntryFixture struct {
	Input       windowsCanonicalEntryFixtureInput
	Binding     binaryadmission.WindowsCanonicalEntryBuildBindingV1
	Targets     binaryadmission.WindowsCanonicalEntrySetTargetsV1
	Observation binaryadmission.WindowsCanonicalEntryBuildObservationV1
	Original    *binaryadmission.WindowsCanonicalEntrySetV1
}

// The supplied build data must independently agree with the public build
// observer and a native acquisition. The explicit test-Product mode permits
// absent publisher-source metadata; it never relaxes file or binding admission.
func loadWindowsCanonicalEntryFixture(t *testing.T, requiredEnv string) *windowsCanonicalEntryFixture {
	t.Helper()
	required, requireSet := os.LookupEnv(requiredEnv)
	if requireSet && required != "1" {
		t.Fatalf("%s must be 1 when supplied", requiredEnv)
	}
	inputPath, inputSet := os.LookupEnv("MCPHUB_CANONICAL_ENTRY_FIXTURE")
	if !inputSet && !requireSet {
		t.Skip("requires an explicitly supplied qualified Windows canonical entry fixture")
	}
	if !filepath.IsAbs(inputPath) {
		t.Fatal("MCPHUB_CANONICAL_ENTRY_FIXTURE requires an absolute fixture input file")
	}
	raw, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	f := &windowsCanonicalEntryFixture{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&f.Input); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		t.Fatalf("fixture input must be one JSON object: %v", err)
	}
	input := f.Input
	testProduct, testProductSet := os.LookupEnv("MCPHUB_CANONICAL_FIXTURE_TEST_PRODUCT")
	if testProductSet && testProduct != "1" {
		t.Fatal("MCPHUB_CANONICAL_FIXTURE_TEST_PRODUCT must be 1 when supplied")
	}
	if !cleanAbsolutePath(input.ProductRoot) || input.ProductVersion == "" ||
		(input.SourceEpoch == "" && !testProductSet) || (input.SourceEpoch != "" && !validLowerHexSHA256(input.SourceEpoch)) ||
		!validLowerHexSHA256(input.ProductSHA256) || !validLowerHexSHA256(input.IdentitySHA256) {
		t.Fatal("fixture requires an absolute package root, version, file hashes, and source epoch or explicit test-Product mode")
	}
	f.Binding, err = binaryadmission.NewWindowsCanonicalEntryBuildBindingV1(input.BuildID, input.CanonicalEntrySHA256, input.ReverseProjection)
	if err != nil {
		t.Fatal(err)
	}
	f.Targets, err = binaryadmission.CanonicalEntryTargetsV1(filepath.Join(input.ProductRoot, binaryadmission.WindowsCanonicalEntryRelativePathV1))
	if err != nil {
		t.Fatal(err)
	}
	f.Observation, err = binaryadmission.CheckWindowsCanonicalEntryBuildV1(t.Context(), f.Targets.CanonicalEntryPath, f.Binding)
	if err != nil {
		t.Fatal(err)
	}
	expected := f.Observation.EntrySet
	if f.Observation.SourceEpoch != input.SourceEpoch || expected.ProductArtifact.Version != input.ProductVersion ||
		expected.CanonicalEntry.SHA256 != input.CanonicalEntrySHA256 || expected.Product.SHA256 != input.ProductSHA256 || expected.Identity.SHA256 != input.IdentitySHA256 {
		t.Fatal("qualified build differs from supplied epoch, version, or artifact hashes")
	}
	f.Original, err = binaryadmission.AcquireWindowsCanonicalEntrySetV1(t.Context(), f.Targets, expected)
	if f.Original != nil {
		t.Cleanup(func() {
			if err := f.Original.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil || f.Original == nil {
		t.Fatalf("acquire qualified original canonical entry set: %v", err)
	}
	return f
}

type qualifiedUpgradeGUICommandFixture struct {
	*upgradeGUICommandC3Fixture
	input                           windowsCanonicalEntryCommandInput
	successorStarts, successorReady int
}

func newQualifiedUpgradeGUICommandFixture(t *testing.T) *qualifiedUpgradeGUICommandFixture {
	t.Helper()
	built := loadWindowsCanonicalEntryFixture(t, "MCPHUB_CANONICAL_COMMAND_FIXTURE_REQUIRED")
	executable, err := os.Executable()
	if err != nil || !samePath(executable, built.Targets.ProductPath) {
		t.Fatalf("command fixture must execute its qualified Product: executable=%q Product=%q error=%v", executable, built.Targets.ProductPath, err)
	}
	executingInfo, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	productInfo, err := os.Stat(built.Targets.ProductPath)
	if err != nil || !os.SameFile(executingInfo, productInfo) {
		t.Fatalf("command fixture executable is not the supplied Product object: %v", err)
	}
	withNoopSchedulerEnv(t)
	t.Setenv("LOCALAPPDATA", apitest.HardenedTempDir(t))
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", "")
	f := &qualifiedUpgradeGUICommandFixture{
		upgradeGUICommandC3Fixture: &upgradeGUICommandC3Fixture{
			stateDir: withTempStateDir(t), candidate: executable,
			target: filepath.Join(apitest.HardenedTempDir(t), "mcphub.exe"), order: new([]string),
		},
		input: windowsCanonicalEntryCommandInput{binding: built.Binding},
	}
	// A historical root Product must be executable: native held-process checks
	// cannot be exercised by the old synthetic parser-only PE fixture.
	f.priorBytes, err = os.ReadFile(executable)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(f.priorBytes)) != built.Input.ProductSHA256 {
		t.Fatalf("read original test Product bytes: %v", err)
	}
	if err := os.WriteFile(f.target, f.priorBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStateSidecarBytes(t, filepath.Join(f.stateDir, "supervisor-intent.json"), []byte(`{"version":1,"updated_at":"2026-09-28T00:00:00Z","daemons":[],"strict_mode":false}`))
	targets, err := binaryadmission.CanonicalEntryTargetsV1(f.target)
	if err != nil {
		t.Fatal(err)
	}
	priorFactory := newWindowsProductTxnFn
	t.Cleanup(func() { newWindowsProductTxnFn = priorFactory })
	newWindowsProductTxnFn = func(req windowsProductTxnRequest) (*WindowsProductTxn, error) {
		f.factoryCalls++
		if req.Context == nil || req.StateDir != f.stateDir || req.Mode != WindowsProductPairModeUpgrade ||
			req.CanonicalEntryPath != targets.CanonicalEntryPath || req.ProductPath != targets.ProductPath || req.IdentityPath != targets.IdentityPath ||
			req.EntrySet == nil || req.EntrySet.original == nil || req.EntrySet.prior == nil {
			t.Fatal("command did not pass its original complete-set owners and explicit target paths")
		}
		f.tasks = &productPairTaskFake{order: f.order}
		deps := WindowsProductTxnDeps{
			StartSupervisor: req.StartSupervisor, WaitSupervisorReady: req.WaitSupervisorReady,
			RestartPrior: req.RestartPrior, SettleSuccessor: req.SettleSuccessor,
			PublishCommitted: windowsProductEventPublisher(req.StateDir),
		}
		if f.fault != nil {
			deps.Fault = func(stage WindowsProductStage) error {
				if stage == WindowsProductStageProductPromoted {
					return f.fault
				}
				return nil
			}
		}
		f.txn = &WindowsProductTxn{Opts: WindowsProductTxnOpts{
			Context: req.Context, StateDir: req.StateDir, ProductPath: req.ProductPath,
			CanonicalEntryPath: req.CanonicalEntryPath, IdentityPath: req.IdentityPath, EntrySet: req.EntrySet,
			LegacyWindowlessPath: req.LegacyWindowlessPath, StagedProduct: req.StagedProduct,
			Mode: req.Mode, ReplacementAuthority: req.ReplacementAuthority, Tasks: f.tasks, Deps: deps,
		}}
		return f.txn, nil
	}
	return f
}

// These command tests exercise native file/previous-process admission and the
// caller's ordering. Successor creation/control is an explicit test backend;
// its native implementation has separate process-owner integration tests.
func useQualifiedUpgradeCommandSuccessorBackend(t *testing.T, f *qualifiedUpgradeGUICommandFixture) {
	t.Helper()
	priorRunner := runInstallUpgradeWindowsFn
	t.Cleanup(func() { runInstallUpgradeWindowsFn = priorRunner })
	runInstallUpgradeWindowsFn = func(ctx context.Context, opts UpgradeOpts) error {
		if opts.WindowsProduct == nil || opts.WindowsProduct != f.txn || opts.priorRuntime == nil {
			t.Fatal("command runner lost the native transaction or prior runtime")
		}
		start := func(path string) (SupervisorLaunch, error) {
			if path != f.txn.Opts.ProductPath {
				t.Fatal("successor backend received a path other than the canonical Product")
			}
			f.successorStarts++
			return &fixtureSupervisorLaunch{order: f.order}, nil
		}
		ready := func(_ context.Context, path string, product binaryadmission.WindowsArtifact) error {
			if path != f.txn.Opts.ProductPath || product.SHA256 != fmt.Sprintf("%x", sha256.Sum256(f.priorBytes)) {
				t.Fatal("successor readiness backend received a different Product")
			}
			f.successorReady++
			return nil
		}
		opts.WindowsProduct.Opts.Deps.StartSupervisor = start
		opts.WindowsProduct.Opts.Deps.WaitSupervisorReady = ready
		if prepared := opts.WindowsProduct.prepared; prepared != nil {
			prepared.deps.StartSupervisor = start
			prepared.deps.WaitSupervisorReady = ready
		}
		return RunInstallUpgrade(ctx, opts)
	}
}

const qualifiedUpgradeGUIActorEnv = "MCPHUB_CANONICAL_COMMAND_ACTOR"

type qualifiedUpgradeGUIActorInput struct {
	Image    string    `json:"image"`
	Deadline time.Time `json:"deadline"`
}

type qualifiedUpgradeGUIActorPIDs struct {
	GUI, Tray, Supervisor int
}

// A Go test executable has no Cobra main. This test-only dispatch gives the
// controlled actors their actual gui/tray/supervise argv, before TestMain can
// interpret them as a test-suite invocation. It is absent from production.
func init() {
	if input, present := os.LookupEnv(qualifiedUpgradeGUIActorEnv); present {
		if err := runQualifiedUpgradeGUIActor(input); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "qualified command actor:", err)
			os.Exit(3)
		}
		os.Exit(0)
	}
}

func runQualifiedUpgradeGUIActor(inputPath string) error {
	if !cleanAbsolutePath(inputPath) || len(os.Args) != 2 || (os.Args[1] != "gui" && os.Args[1] != "tray" && os.Args[1] != "supervise") {
		return errors.New("invalid explicit test actor input or argv")
	}
	raw, err := os.ReadFile(inputPath)
	if err != nil {
		return err
	}
	var input qualifiedUpgradeGUIActorInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil || !cleanAbsolutePath(input.Image) || !samePath(executable, input.Image) || time.Until(input.Deadline) <= 0 || time.Until(input.Deadline) > 2*time.Minute {
		return errors.New("test actor image or bounded lifetime differs")
	}
	ctx, cancel := context.WithDeadline(context.Background(), input.Deadline)
	defer cancel()
	if os.Args[1] != "gui" {
		<-ctx.Done()
		return nil
	}
	job, err := process.NewKillOnCloseJob()
	if err != nil {
		return err
	}
	var children []*exec.Cmd
	defer func() {
		_ = job.Close()
		for _, child := range children {
			_, _ = child.Process.Wait()
		}
	}()
	pids := qualifiedUpgradeGUIActorPIDs{GUI: os.Getpid()}
	for _, role := range []string{"tray", "supervise"} {
		child := exec.Command(executable, role)
		child.Env, child.Dir = os.Environ(), filepath.Dir(inputPath)
		pid, err := process.StartWithJob(job, child)
		if err != nil {
			return err
		}
		children = append(children, child)
		if role == "tray" {
			pids.Tray = pid
		} else {
			pids.Supervisor = pid
		}
	}
	ready, err := json.Marshal(pids)
	if err != nil {
		return err
	}
	readyPath := filepath.Join(filepath.Dir(inputPath), "ready.json")
	if err := os.WriteFile(readyPath+".tmp", ready, 0o600); err != nil {
		return err
	}
	if err := os.Rename(readyPath+".tmp", readyPath); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

type qualifiedUpgradeGUIActors struct {
	held       *upgradeGUIHeldTopology
	supervisor process.HeldPIDObservation
}

func startQualifiedUpgradeGUIActors(t *testing.T, f *qualifiedUpgradeGUICommandFixture) *qualifiedUpgradeGUIActors {
	t.Helper()
	root := t.TempDir()
	inputPath := filepath.Join(root, "actor.json")
	raw, err := json.Marshal(qualifiedUpgradeGUIActorInput{Image: f.target, Deadline: time.Now().Add(90 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	job, err := process.NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(f.target, "gui")
	child.Env = append(os.Environ(), qualifiedUpgradeGUIActorEnv+"="+inputPath)
	child.Dir = root
	if _, err := process.StartWithJob(job, child); err != nil {
		_ = job.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	var state *os.ProcessState
	var waitErr error
	go func() {
		state, waitErr = child.Process.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		if err := job.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
			if waitErr != nil {
				t.Error(waitErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("contained GUI actor did not exit after its owned Job closed")
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var pids qualifiedUpgradeGUIActorPIDs
	for {
		raw, err := os.ReadFile(filepath.Join(root, "ready.json"))
		if err == nil {
			if err := json.Unmarshal(raw, &pids); err != nil {
				t.Fatal(err)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-done:
			t.Fatalf("GUI actor exited before admission: state=%v error=%v", state, waitErr)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
	if pids.GUI != child.Process.Pid || pids.Tray <= 0 || pids.Supervisor <= 0 {
		t.Fatal("actor ready record differs from its creator-owned GUI process")
	}
	a := &qualifiedUpgradeGUIActors{held: &upgradeGUIHeldTopology{StateDir: f.stateDir}}
	t.Cleanup(func() {
		if err := a.held.Close(); err != nil {
			t.Error(err)
		}
	})
	a.held.GUI, err = process.HoldPIDForTermination(pids.GUI)
	if err != nil {
		t.Fatal(err)
	}
	a.held.Tray, err = process.HoldPIDForTermination(pids.Tray)
	if err != nil {
		t.Fatal(err)
	}
	a.supervisor, err = process.HoldPIDForObservation(pids.Supervisor)
	if err != nil {
		t.Fatal(err)
	}
	var ok bool
	a.held.Supervisor, ok = a.supervisor.(process.HeldPIDJobObservation)
	if !ok {
		_ = a.supervisor.Close()
		t.Fatal("native supervisor observation lacks Job membership evidence")
	}
	a.held.Identity = upgradeGUIIdentitySnapshot{
		Target:     f.target,
		GUI:        qualifiedUpgradeGUIActorIdentity(t, ctx, a.held.GUI, pids.GUI, f.target, "gui"),
		Tray:       qualifiedUpgradeGUIActorIdentity(t, ctx, a.held.Tray, pids.Tray, f.target, "tray"),
		Supervisor: qualifiedUpgradeGUIActorIdentity(t, ctx, a.held.Supervisor, pids.Supervisor, f.target, "supervise"),
	}
	a.held.Identity.SupervisorInAnyJob, err = a.held.Supervisor.InAnyJob()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateUpgradeGUIOwnedPairIdentitySnapshot(a.held.Identity); err != nil {
		t.Fatal(err)
	}
	a.held.SupervisorOwner = api.SupervisorLockOwner{PID: pids.Supervisor, StartedAt: a.held.Identity.Supervisor.StartedAt.Format(time.RFC3339Nano)}
	return a
}

func qualifiedUpgradeGUIActorIdentity(t *testing.T, ctx context.Context, held interface {
	VerifyIdentity(process.PIDIdentityProof) error
}, pid int, target, role string) upgradeGUIProcessIdentity {
	t.Helper()
	id, err := heldUpgradeGUIIdentity(held, pid, target, time.Time{}, process.LookupPIDHandleIdentity)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := process.LookupProcessIdentityContext(ctx, pid)
	if err != nil || !samePath(detail.ExecutablePath, target) || detail.CreationDateUnix != id.StartedAt.Unix() || supervisorCommandLineSubcommand(detail.CommandLine) != role {
		t.Fatalf("native %s command generation differs: %v", role, err)
	}
	sameUser, err := process.ProcessOwnerSIDMatchesCurrent(pid)
	if err != nil || !sameUser {
		t.Fatalf("native %s user differs: %v", role, err)
	}
	parent, err := retainedGenerationParentPID(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := held.VerifyIdentity(process.PIDIdentityProof{PID: pid, ExecutablePath: id.ExecutablePath, CommandLine: detail.CommandLine, StartedAt: id.StartedAt.Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	return upgradeGUIProcessIdentity{PID: pid, Role: supervisorCommandLineSubcommand(detail.CommandLine), Image: id.ExecutablePath, StartedAt: id.StartedAt, ParentPID: parent, SameUser: sameUser}
}

func (a *qualifiedUpgradeGUIActors) stop(t *testing.T) {
	t.Helper()
	committed, err := a.held.GUI.Terminate()
	if err != nil || !committed {
		t.Fatalf("terminate exact owned GUI generation: committed=%v error=%v", committed, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tray, ok := a.held.Tray.(process.HeldPIDExitWaiter)
	if !ok {
		t.Fatal("native tray hold lacks original exit observation")
	}
	if result, err := tray.WaitExit(ctx); err != nil || !result.Exited {
		t.Fatalf("GUI Job did not settle its original tray: %+v %v", result, err)
	}
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		result, err := a.supervisor.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if result.Result == windows.WAIT_OBJECT_0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("GUI Job did not settle its original supervisor", ctx.Err())
		case <-tick.C:
		}
	}
	if err := a.held.Close(); err != nil {
		t.Fatal(err)
	}
}
