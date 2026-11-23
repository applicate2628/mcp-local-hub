//go:build windows

package cli

import (
	"context"
	gobuildinfo "debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/api/binary_discovery"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/process"
)

type privateSerenaMigrationPin struct {
	Artifact privateSerenaMigrationArtifact
	Info     os.FileInfo
	Limit    int64
}

type privateSerenaMigrationLoaded struct {
	Input    privateSerenaMigrationInput
	Manifest privateSerenaMigrationArtifact
	Pins     []privateSerenaMigrationPin
	Catalog  []byte
	GoImage  *debug.BuildInfo
}

type privateSerenaMigrationCaseConfig struct {
	Schema      string                         `json:"schema"`
	Root        string                         `json:"root"`
	Input       privateSerenaMigrationArtifact `json:"input"`
	Directory   string                         `json:"directory"`
	ParentEnd   time.Time                      `json:"parent_end"`
	WorkEnd     time.Time                      `json:"work_end"`
	CleanupEnd  time.Time                      `json:"cleanup_end"`
	Args        []string                       `json:"args"`
	ParentBuild *debug.BuildInfo               `json:"parent_build"`
}

type privateSerenaMigrationOperation struct {
	Test              string `json:"test"`
	PrepareCalls      int    `json:"prepare_calls"`
	NonemptyCalls     int    `json:"nonempty_prepare_calls"`
	InstallCalls      int    `json:"install_calls"`
	IntentPath        string `json:"intent_path,omitempty"`
	ConstructorError  string `json:"constructor_error,omitempty"`
	PrepareError      string `json:"prepare_error,omitempty"`
	InstallError      string `json:"install_error,omitempty"`
	CleanupError      string `json:"cleanup_error,omitempty"`
	DriverError       string `json:"driver_error,omitempty"`
	OperationComplete bool   `json:"original_operation_complete"`
}

type privateSerenaMigrationChildFrame struct {
	Schema      string                               `json:"schema"`
	Root        string                               `json:"root"`
	InputSHA256 string                               `json:"input_sha256"`
	WorkEnd     time.Time                            `json:"work_end"`
	Native      process.PublisherNativeIncarnationV1 `json:"native"`
	Image       api.PublisherImageSnapshotV1         `json:"image"`
	Operations  []privateSerenaMigrationOperation    `json:"operations"`
	Seeds       []privateSerenaMigrationArtifact     `json:"seeds"`
	Complete    bool                                 `json:"complete"`
}

type privateSerenaMigrationCase struct {
	config     privateSerenaMigrationCaseConfig
	loaded     *privateSerenaMigrationLoaded
	root       privateSerenaMigrationRoot
	ctx        context.Context
	cancel     context.CancelFunc
	product    *windowsCanonicalNativeActorFixture
	frame      privateSerenaMigrationChildFrame
	apis       []*api.API
	operations []migrateSerenaInstallOperation
	blocked    error
}

// Published records are immutable. Every uncertain original remains reachable
// until this test process exits; its original outer owner receives the exit.
type privateSerenaMigrationRetainedCustody struct {
	Original any
	Cause    error
	Previous *privateSerenaMigrationRetainedCustody
}

var retainedPrivateSerenaMigrationCustody atomic.Pointer[privateSerenaMigrationRetainedCustody]

func privateSerenaMigrationFailStop(custody any, stage string, cause error) {
	retained := &privateSerenaMigrationRetainedCustody{Original: custody, Cause: cause}
	for {
		previous := retainedPrivateSerenaMigrationCustody.Load()
		retained.Previous = previous
		if retainedPrivateSerenaMigrationCustody.CompareAndSwap(previous, retained) {
			break
		}
	}
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	if len(message) > 4096 {
		message = message[:4096]
	}
	raw, _ := json.Marshal(struct {
		Schema, Stage, Error string
		CustodyRetained      bool
		Diagnostics          []api.PreparedSerenaFailureDiagnostic `json:"diagnostics,omitempty"`
	}{"private-serena-migration-abort-v1", stage, message, true, api.PreparedSerenaFailureDiagnostics(cause)})
	// Memory-derived diagnostics only: do not read/close an unjoined sink or
	// run testing cleanup while native/preparation ownership is unknown.
	_, _ = fmt.Fprintf(os.Stderr, "PRIVATE_SERENA_MIGRATION_ABORT_JSON=%s\n", raw)
	os.Exit(86)
}

func privateSerenaMigrationOrdinaryPath(path string, directory bool) error {
	if !cleanAbsolutePath(path) {
		return errors.New("private migration requires a canonical absolute path")
	}
	for current := path; ; current = filepath.Dir(current) {
		name, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return err
		}
		attributes, err := windows.GetFileAttributes(name)
		if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return errors.Join(err, fmt.Errorf("private migration refuses a reparse component: %s", current))
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if (current != path || directory) && !info.IsDir() {
			return errors.New("private migration requires an ordinary directory")
		}
		if current == path && !directory && !info.Mode().IsRegular() {
			return errors.New("private migration requires an ordinary file")
		}
		next := filepath.Dir(current)
		if next == current {
			break
		}
	}
	return nil
}

func (loaded *privateSerenaMigrationLoaded) read(t *testing.T, artifact privateSerenaMigrationArtifact, limit int64) []byte {
	t.Helper()
	if err := privateSerenaMigrationOrdinaryPath(artifact.Path, false); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	raw := readCanonicalNativeActorBytes(t, artifact.Path, artifact.SHA256, limit)
	after, err := os.Lstat(artifact.Path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("private migration original artifact changed identity: %s: %v", artifact.Path, err)
	}
	loaded.Pins = append(loaded.Pins, privateSerenaMigrationPin{Artifact: artifact, Info: before, Limit: limit})
	return raw
}

func (loaded *privateSerenaMigrationLoaded) verify(t *testing.T) {
	t.Helper()
	for _, pin := range loaded.Pins {
		if err := privateSerenaMigrationOrdinaryPath(pin.Artifact.Path, false); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(pin.Artifact.Path)
		if err != nil || !os.SameFile(pin.Info, info) {
			t.Fatalf("private migration pinned object changed: %s: %v", pin.Artifact.Path, err)
		}
		readCanonicalNativeActorBytes(t, pin.Artifact.Path, pin.Artifact.SHA256, pin.Limit)
		after, err := os.Lstat(pin.Artifact.Path)
		if err != nil || !os.SameFile(pin.Info, after) {
			t.Fatalf("private migration pinned object changed during readback: %s: %v", pin.Artifact.Path, err)
		}
	}
}

func loadPrivateSerenaMigrationInput(t *testing.T, artifact privateSerenaMigrationArtifact) *privateSerenaMigrationLoaded {
	t.Helper()
	if !privateSerenaMigrationDPath(artifact.Path) || !validLowerHexSHA256(artifact.SHA256) {
		t.Fatal("private migration requires the exact retained input manifest")
	}
	loaded := &privateSerenaMigrationLoaded{Manifest: artifact}
	raw := loaded.read(t, artifact, 1<<20)
	var err error
	loaded.Input, err = privateSerenaMigrationDecodeInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	input := loaded.Input
	_, currentCommit, _ := buildinfo.Get()
	if currentCommit != input.SourceCommit {
		t.Fatalf("private migration source commit differs from actual current image: %q != %q", currentCommit, input.SourceCommit)
	}
	for _, dir := range []string{input.SourceRoot, filepath.Join(input.SourceRoot, "internal", "cli"), input.EvidenceParent} {
		if err := privateSerenaMigrationOrdinaryPath(dir, true); err != nil {
			t.Fatal(err)
		}
	}
	var imageBytes, receiptBytes []byte
	for _, item := range []struct {
		artifact privateSerenaMigrationArtifact
		limit    int64
		dst      *[]byte
	}{
		{input.Image, 64 << 20, &imageBytes},
		{input.ProductFixture, 16 << 10, nil},
		{input.Catalog, 1 << 20, &loaded.Catalog},
		{input.UV, 64 << 20, nil}, {input.UVX, 64 << 20, nil}, {input.Python, 64 << 20, nil},
		{input.IssuanceReceipt, 1 << 20, &receiptBytes},
	} {
		value := loaded.read(t, item.artifact, item.limit)
		if item.dst != nil {
			*item.dst = value
		}
	}
	if input.CacheSeed != nil {
		loaded.read(t, *input.CacheSeed, 64<<10)
	}
	loaded.GoImage, err = gobuildinfo.ReadFile(input.Image.Path)
	if err != nil {
		t.Fatal(err)
	}
	currentGo, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("actual current Go build information is absent")
	}
	if err := privateSerenaMigrationMatchBuild(currentGo, loaded.GoImage, input.TestStatePathEnv); err != nil {
		t.Fatal(err)
	}
	identity, err := binaryadmission.ParseWindowsPublisherSourceIdentity(imageBytes)
	if err != nil || identity.Role != binaryadmission.WindowsArtifactRolePublisherTest ||
		identity.SourceEpoch != input.SourceEpoch || identity.Commit != input.SourceCommit ||
		identity.Machine != 0x8664 || identity.Subsystem != binaryadmission.WindowsGUISubsystem {
		t.Fatalf("actual private image Source/role/machine differs: %+v: %v", identity, err)
	}
	var issued struct {
		Schema, Purpose                string
		Prepared                       bool
		SourceRoot, SourceSnapshotPath string
		PublisherSource                struct {
			Epoch     string `json:"source_epoch"`
			Snapshot  string `json:"source_snapshot_sha256"`
			FileCount int    `json:"file_count"`
		}
		Package, GoEntrypoint, Role               string
		Subsystem, Machine                        uint16
		WindowsTargetArch, Version, Commit        string
		BuildDate                                 string
		BuildTags                                 []string
		Ldflags                                   string
		StagedResource, ResourcePath              string
		ResourceSHA256, ResourceConfigurationPath string
		VersionInfo                               json.RawMessage
	}
	// These are original compiler DATA, not an image/namespace authority.
	if err := privateSerenaMigrationJSON(receiptBytes, &issued); err != nil {
		t.Fatal(err)
	}
	tags := []string{}
	if input.TestStatePathEnv {
		tags = []string{"test_state_path_env"}
	}
	settings, err := privateSerenaMigrationBuildSettings(loaded.GoImage, input.TestStatePathEnv)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Schema != "windows-cli-publisher-test-inputs-v1" || issued.Purpose != privateSerenaMigrationPurpose || !issued.Prepared ||
		issued.SourceRoot != input.SourceRoot || issued.PublisherSource.Epoch != input.SourceEpoch ||
		!validLowerHexSHA256(issued.PublisherSource.Snapshot) || issued.PublisherSource.FileCount <= 0 ||
		issued.Package != "./internal/cli" || issued.GoEntrypoint != loaded.GoImage.Path || issued.Role != "publisher-test" ||
		issued.Subsystem != identity.Subsystem || issued.Machine != identity.Machine || issued.WindowsTargetArch != "amd64" ||
		issued.Version != identity.Version || issued.Commit != identity.Commit || issued.BuildDate != identity.BuildDate ||
		!reflect.DeepEqual(issued.BuildTags, tags) || issued.Ldflags == "" || settings["-ldflags"] != issued.Ldflags ||
		issued.StagedResource != "internal/cli/zz_publisher_test_versioninfo.syso" || !validLowerHexSHA256(issued.ResourceSHA256) {
		t.Fatal("private migration original compiler inputs differ from the actual private image")
	}
	if !privateSerenaMigrationDPath(issued.SourceSnapshotPath) {
		t.Fatal("private migration source snapshot is not the retained D input")
	}
	loaded.read(t, privateSerenaMigrationArtifact{Path: issued.SourceSnapshotPath, SHA256: issued.PublisherSource.Snapshot}, 64<<20)
	return loaded
}

func privateSerenaMigrationPrivateParent(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	// The original verifier may log a default-relax refusal. TestMain must
	// already have installed its D-only state and client environment before
	// this call; rejecting the relaxed observation does not undo that log.
	state, stateErr := api.DaemonStateDirReadOnly()
	for _, owned := range []string{state, os.Getenv("LOCALAPPDATA")} {
		relative, relErr := filepath.Rel(os.TempDir(), owned)
		if stateErr != nil || relErr != nil || !privateSerenaMigrationDPath(owned) ||
			relative == "." || relative == ".." || filepath.IsAbs(relative) ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			t.Fatalf("private migration requires TestMain's already isolated D state before parent policy: %v", stateErr)
		}
	}
	if !samePath(state, os.Getenv("MCPHUB_STATE_DIR_OVERRIDE")) {
		t.Fatal("private migration original TestMain state binding differs")
	}
	exists, original, err := api.SecureConfigLockParentPolicyObserved(ctx, path, false)
	if original != nil && !original.Snapshot().ResourcesComplete {
		privateSerenaMigrationFailStop(original, "private-directory-observation", err)
	}
	if err != nil || original == nil || !exists {
		t.Fatalf("private migration retained parent unavailable: %v", err)
	}
	if err := original.Validate(ctx, path, false); err != nil {
		t.Fatal(err)
	}
	view := original.Snapshot()
	if !view.Complete || !view.ResourcesComplete || !view.Exists || view.GateDisposition != "strict-gate" || len(view.CreatedComponents) != 0 {
		t.Fatalf("private migration requires the original strict existing private parent: %+v", view)
	}
}

func beginPrivateSerenaMigrationCase(t *testing.T) (*privateSerenaMigrationCase, bool) {
	t.Helper()
	root, err := privateSerenaMigrationRootFor(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	if retained := retainedPrivateSerenaMigrationCustody.Load(); retained != nil {
		privateSerenaMigrationFailStop(retained, "later-root-after-uncertain-custody", errors.New("original custody remains unresolved"))
	}
	casePath, caseSet := os.LookupEnv(privateSerenaMigrationCaseEnv)
	inputPath, pathSet := os.LookupEnv(privateSerenaMigrationInputEnv)
	inputSHA, shaSet := os.LookupEnv(privateSerenaMigrationInputSHAEnv)
	if caseSet {
		if pathSet || shaSet || !privateSerenaMigrationDPath(casePath) {
			t.Fatal("private migration child configuration is ambiguous")
		}
		return receivePrivateSerenaMigrationCase(t, root, casePath), false
	}
	if !pathSet && !shaSet {
		if usesSerenaQualificationFixture {
			return nil, false
		}
		if _, err := buildinfo.CurrentPublisherSource(); !errors.Is(err, buildinfo.ErrPublisherSourceUnmarked) {
			t.Fatal("a marked private migration image requires its exact child configuration")
		}
		return nil, false
	}
	if !pathSet || !shaSet {
		t.Fatal("private migration input path/hash must be supplied together")
	}
	// Only the ordinary parent dispatches in parallel. The child branch above
	// keeps its process-local environment and original test body serialized.
	t.Parallel()
	// Go may have held this root while another original entered fail-stop.
	if retained := retainedPrivateSerenaMigrationCustody.Load(); retained != nil {
		privateSerenaMigrationFailStop(retained, "later-root-after-uncertain-custody", errors.New("original custody remains unresolved"))
	}
	runPrivateSerenaMigrationChild(t, root, privateSerenaMigrationArtifact{Path: inputPath, SHA256: inputSHA})
	return nil, true
}

func runPrivateSerenaMigrationChild(t *testing.T, root privateSerenaMigrationRoot, artifact privateSerenaMigrationArtifact) {
	t.Helper()
	parentEnd, finite := t.Deadline()
	if !finite {
		t.Fatal("private migration requires the unchanged original testing deadline")
	}
	workEnd, cleanupEnd, err := privateSerenaMigrationDeadlines(parentEnd, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), workEnd)
	defer cancel()
	loaded := loadPrivateSerenaMigrationInput(t, artifact)
	if _, err := buildinfo.CurrentPublisherSource(); !errors.Is(err, buildinfo.ErrPublisherSourceUnmarked) {
		t.Fatal("ordinary migration receiver must retain its unmarked publisher contract")
	}
	if _, err := buildinfo.CurrentRustOwnerSHA256(); !errors.Is(err, buildinfo.ErrRustOwnerBuildUnmarked) {
		t.Fatal("ordinary migration receiver must retain its original unmarked contained owner")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parent, err := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{Path: executable, Role: binaryadmission.WindowsArtifactRoleProduct})
	version, commit, date := buildinfo.Get()
	if err != nil || parent.Version != version || parent.Commit != commit || parent.BuildDate != date {
		t.Fatalf("ordinary migration receiver lost its actual Product identity: %v", err)
	}
	privateSerenaMigrationPrivateParent(t, ctx, loaded.Input.EvidenceParent)
	directory, err := os.MkdirTemp(loaded.Input.EvidenceParent, "m")
	if err != nil {
		t.Fatal(err)
	}
	apitest.HardenedDir(t, directory)
	privateSerenaMigrationPrivateParent(t, ctx, directory)
	parentGo, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("ordinary receiver Go build information unavailable")
	}
	// Keep only actual compiler settings, not the module dependency closure,
	// in this bounded per-case DATA. The original issuer owns that closure.
	parentBuild := &debug.BuildInfo{GoVersion: parentGo.GoVersion, Path: parentGo.Path, Settings: append([]debug.BuildSetting(nil), parentGo.Settings...)}
	timeout := time.Until(workEnd)
	if timeout <= api.DefaultSerenaPreparationPolicy().CleanupLimit || ctx.Err() != nil {
		t.Fatal("private migration bootstrap consumed the original preparation interval")
	}
	config := privateSerenaMigrationCaseConfig{
		Schema: "private-serena-migration-case-v1", Root: root.Name, Input: artifact, Directory: directory,
		ParentEnd: parentEnd, WorkEnd: workEnd, CleanupEnd: cleanupEnd, ParentBuild: parentBuild,
		Args: []string{"-test.paniconexit0", "-test.v=test2json", "-test.count=1", "-test.timeout=" + timeout.String(), "-test.run=^" + root.Name + "$"},
	}
	configPath := filepath.Join(directory, "case.json")
	if err := publishPrivateSerenaMigrationCase(configPath, config); err != nil {
		t.Fatal(err)
	}
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded.read(t, privateSerenaMigrationArtifact{Path: configPath, SHA256: privateSerenaMigrationDigest(configBytes)}, 16<<10)
	stdout, err := os.OpenFile(filepath.Join(directory, "stdout.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.OpenFile(filepath.Join(directory, "stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if closeErr := stdout.Close(); closeErr != nil {
			privateSerenaMigrationFailStop(stdout, "capture-before-start", errors.Join(err, closeErr))
		}
		t.Fatal(err)
	}
	command := exec.Command(loaded.Input.Image.Path, config.Args...)
	command.Dir = filepath.Join(loaded.Input.SourceRoot, "internal", "cli")
	command.Env = privateSerenaMigrationChildEnvironment(loaded.Input, directory, configPath)
	process.NoConsole(command)
	observed, runErr := process.RunContainedStreamObserved(ctx, command, process.ContainedStreamOptions{
		CleanupTimeout: 10 * time.Second, AbsoluteCleanupDeadline: cleanupEnd,
		Stderr: stderr, StdoutByteLimit: 16 << 20, StderrByteLimit: 16 << 20,
	}, func(reader io.Reader) error {
		_, err := io.Copy(stdout, reader)
		return err
	})
	custody := struct {
		Original       process.ContainedStreamReceiptV1
		Cause          error
		Stdout, Stderr *os.File
	}{observed, runErr, stdout, stderr}
	if !observed.NativeSettlement.Complete() || !observed.IOSettlement.Complete() {
		privateSerenaMigrationFailStop(custody, "parent-native-or-io-incomplete", runErr)
	}
	if err := errors.Join(stdout.Sync(), stderr.Sync(), stdout.Close(), stderr.Close()); err != nil {
		privateSerenaMigrationFailStop(custody, "parent-capture-close", err)
	}
	record := map[string]any{
		"schema": "private-serena-migration-parent-v1", "root": root.Name, "config": config,
		"input_sha256": artifact.SHA256, "parent_product": parent,
		"root_identity": observed.RootIdentity, "root_wait_known": observed.RootWaitKnown,
		"root_exit_code": observed.RootExitCode, "native": observed.NativeSettlement.Snapshot(), "io": observed.IOSettlement,
		"native_complete": true, "io_complete": true, "custody_retained": false, "complete": false,
		"stdout": stdout.Name(), "stderr": stderr.Name(), "parent_cancellation": "original-contained-abort",
		"child_context": "independent-finite-context-with-the-original-absolute-bound",
	}
	if runErr != nil {
		record["original_error"] = runErr.Error()
	}
	receiptPath := filepath.Join(directory, "receipt.json")
	defer func() {
		if err := publishPrivateSerenaMigrationCase(receiptPath, record); err != nil {
			t.Errorf("private migration parent receipt: %v", err)
		}
		t.Logf("PRIVATE_SERENA_MIGRATION_RECEIPT=%s", receiptPath)
	}()
	loaded.verify(t)
	record["inputs_unchanged"] = true
	raw, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	errRaw, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	record["stdout_sha256"] = privateSerenaMigrationDigest(raw)
	record["stderr_sha256"] = privateSerenaMigrationDigest(errRaw)
	ledger, ledgerErr := privateSerenaMigrationLedger(raw, root)
	record["tests"] = ledger
	if ledgerErr != nil {
		record["ledger_error"] = ledgerErr.Error()
	}
	if runErr != nil || !observed.RootIdentity.Known || !observed.RootWaitKnown || observed.RootExitCode != 0 || ledgerErr != nil {
		t.Fatalf("private migration original child failed; receipt=%s exit=%d run=%v ledger=%v", receiptPath, observed.RootExitCode, runErr, ledgerErr)
	}
	childBytes, err := os.ReadFile(filepath.Join(directory, "child.json"))
	if err != nil || len(childBytes) > 16<<10 {
		t.Fatalf("private migration child receiving data missing/oversized: %v", err)
	}
	var child privateSerenaMigrationChildFrame
	if err := privateSerenaMigrationJSON(childBytes, &child); err != nil {
		t.Fatal(err)
	}
	record["child"] = child
	if child.Schema != "private-serena-migration-child-v1" || !child.Complete || child.Root != root.Name ||
		child.InputSHA256 != artifact.SHA256 || !child.WorkEnd.Equal(workEnd) ||
		int(child.Native.PID) != observed.RootIdentity.PID || child.Native.NativeBirthFILETIME != observed.RootIdentity.NativeBirthFILETIME ||
		child.Native.NativeBirthFILETIME == 0 || child.Image.ImageSHA256 != loaded.Input.Image.SHA256 ||
		child.Image.SourceEpoch != loaded.Input.SourceEpoch || child.Image.Commit != loaded.Input.SourceCommit || !child.Image.ResourcesClosed ||
		len(child.Operations) != root.Preparations || len(child.Seeds) != root.Preparations {
		t.Fatal("private migration child DATA differs from its original process/image/operation receiving")
	}
	for i, operation := range child.Operations {
		expectedTest := root.Name
		if len(root.Subtests) != 0 {
			expectedTest += "/" + root.Subtests[i]
		}
		if operation.Test != expectedTest || !operation.OperationComplete || operation.NonemptyCalls < 1 || operation.InstallCalls != 1 || operation.IntentPath == "" ||
			operation.ConstructorError != "" || operation.PrepareError != "" || operation.InstallError != "" || operation.CleanupError != "" {
			t.Fatal("private migration original positive preparation/consumer did not complete")
		}
	}
	if !time.Now().Before(parentEnd) {
		t.Fatal("private migration result receiving exceeded the original parent deadline")
	}
	record["complete"] = true
}

func privateSerenaMigrationChildEnvironment(input privateSerenaMigrationInput, directory, configPath string) []string {
	// The ordinary parent already has the reviewed literal environment. Keep
	// its toolchain/system inputs, remove test dispatch and package overrides,
	// and replace every writable profile/cache path used by this child.
	replacements := map[string]string{
		"TEMP": directory, "TMP": directory, "HOME": directory, "USERPROFILE": directory,
		"APPDATA": directory, "LOCALAPPDATA": directory, "XDG_STATE_HOME": directory, "XDG_CACHE_HOME": directory,
		"PATH":                 filepath.Dir(input.UV.Path) + string(os.PathListSeparator) + filepath.Dir(input.Python.Path) + string(os.PathListSeparator) + os.Getenv("PATH"),
		"MCPHUB_E2E_SCHEDULER": "none", privateSerenaMigrationCaseEnv: configPath,
	}
	var result []string
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if !ok || strings.HasPrefix(upper, "MCPHUB_") || strings.HasPrefix(upper, "UV_") || strings.HasPrefix(upper, "PYTHON") {
			continue
		}
		if _, replaced := replacements[upper]; !replaced {
			result = append(result, entry)
		}
	}
	for key, value := range replacements {
		result = append(result, key+"="+value)
	}
	return result
}

func receivePrivateSerenaMigrationCase(t *testing.T, root privateSerenaMigrationRoot, casePath string) *privateSerenaMigrationCase {
	t.Helper()
	if err := privateSerenaMigrationOrdinaryPath(casePath, false); err != nil {
		t.Fatal(err)
	}
	caseFile, err := os.Open(casePath)
	if err != nil {
		t.Fatal(err)
	}
	caseInfo, statErr := caseFile.Stat()
	raw, readErr := io.ReadAll(io.LimitReader(caseFile, (16<<10)+1))
	closeErr := caseFile.Close()
	afterCase, afterErr := os.Lstat(casePath)
	if err := errors.Join(statErr, readErr, closeErr, afterErr); err != nil || len(raw) > 16<<10 ||
		caseInfo == nil || afterCase == nil || !os.SameFile(caseInfo, afterCase) {
		t.Fatalf("private migration case input unavailable/oversized/changed: %v", err)
	}
	var config privateSerenaMigrationCaseConfig
	if err := privateSerenaMigrationJSON(raw, &config); err != nil {
		t.Fatal(err)
	}
	workEnd, cleanupEnd, err := privateSerenaMigrationDeadlines(config.ParentEnd, time.Now())
	if err != nil || config.Schema != "private-serena-migration-case-v1" || config.Root != root.Name ||
		config.Directory != filepath.Dir(casePath) || filepath.Base(casePath) != "case.json" ||
		!config.WorkEnd.Equal(workEnd) || !config.CleanupEnd.Equal(cleanupEnd) ||
		!reflect.DeepEqual(os.Args[1:], config.Args) || len(config.Args) != 5 ||
		config.Args[0] != "-test.paniconexit0" || config.Args[1] != "-test.v=test2json" ||
		config.Args[2] != "-test.count=1" || config.Args[4] != "-test.run=^"+root.Name+"$" ||
		!strings.HasPrefix(config.Args[3], "-test.timeout=") {
		t.Fatalf("private migration case/root/argv/original deadline differs: %v", err)
	}
	duration, err := time.ParseDuration(strings.TrimPrefix(config.Args[3], "-test.timeout="))
	if err != nil || duration <= 0 || duration > 5*time.Minute {
		t.Fatal("private migration child test timeout is not a bounded original remainder")
	}
	ctx, cancel := context.WithDeadline(t.Context(), workEnd)
	c := &privateSerenaMigrationCase{config: config, root: root, ctx: ctx, cancel: cancel}
	// Register first: original fixture closes and body cleanup run before this
	// final DATA publication. t.Context is canceled before testing cleanup, so
	// this callback does not revalidate a live publisher/native observation.
	t.Cleanup(func() {
		defer cancel()
		// The shared fixture cleanup has run first. Close receives its
		// original cached outcome; it never retries a failed native close.
		if c.product != nil {
			if err := c.product.Original.Close(); err != nil {
				privateSerenaMigrationFailStop(c, "original-product-close", err)
			}
		}
		if c.loaded == nil {
			return
		}
		c.loaded.verify(t)
		for _, seed := range c.frame.Seeds {
			readCanonicalNativeActorBytes(t, seed.Path, seed.SHA256, 1<<20)
		}
		c.frame.Complete = !t.Failed() && c.blocked == nil && len(c.frame.Operations) == c.root.Preparations
		if err := publishPrivateSerenaMigrationCase(filepath.Join(c.config.Directory, "child.json"), c.frame); err != nil {
			t.Errorf("private migration child receiving data: %v", err)
		}
	})
	c.loaded = loadPrivateSerenaMigrationInput(t, config.Input)
	input := c.loaded.Input
	if filepath.Dir(config.Directory) != input.EvidenceParent || !strings.HasPrefix(filepath.Base(config.Directory), "m") ||
		!samePath(os.TempDir(), config.Directory) {
		t.Fatal("private migration child must use the parent's fresh retained evidence directory")
	}
	cwd, err := os.Getwd()
	if err != nil || !samePath(cwd, filepath.Join(input.SourceRoot, "internal", "cli")) {
		t.Fatalf("private migration child lost the actual issued Source package cwd: %v", err)
	}
	if err := privateSerenaMigrationMatchBuild(config.ParentBuild, c.loaded.GoImage, input.TestStatePathEnv); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil || !samePath(self, input.Image.Path) {
		t.Fatalf("private migration child is not the original issued image: %v", err)
	}
	selfInfo, err := os.Stat(self)
	imageInfo, imageErr := os.Stat(input.Image.Path)
	if err != nil || imageErr != nil || !os.SameFile(selfInfo, imageInfo) {
		t.Fatalf("private migration child file identity differs: %v", errors.Join(err, imageErr))
	}
	source, err := buildinfo.CurrentPublisherSource()
	if err != nil || source.Validate(input.SourceEpoch) != nil {
		t.Fatalf("private migration actual compiled Source differs: %v", err)
	}
	if _, err := buildinfo.CurrentRustOwnerSHA256(); !errors.Is(err, buildinfo.ErrRustOwnerBuildUnmarked) {
		t.Fatal("private test publisher must not acquire a Product Rust owner mark")
	}
	_, productErr := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{Path: self, Role: binaryadmission.WindowsArtifactRoleProduct, SHA256: input.Image.SHA256})
	roleRefusal := fmt.Sprintf("VERSIONINFO role %q disagrees with expected role %q", binaryadmission.WindowsArtifactRolePublisherTest, binaryadmission.WindowsArtifactRoleProduct)
	if productErr == nil || !strings.Contains(productErr.Error(), roleRefusal) {
		t.Fatalf("private publisher must remain inadmissible as Product specifically for its role: %v", productErr)
	}
	privateSerenaMigrationPrivateParent(t, ctx, input.EvidenceParent)
	privateSerenaMigrationPrivateParent(t, ctx, config.Directory)
	t.Setenv("MCPHUB_CANONICAL_NATIVE_ACTOR_FIXTURE", input.ProductFixture.Path)
	t.Setenv("MCPHUB_CANONICAL_ENTRY_FIXTURE_REQUIRED", "1")
	t.Setenv("MCPHUB_CANONICAL_FIXTURE_TEST_PRODUCT", "1")
	func() {
		returned := false
		defer func() {
			if !returned {
				// A shared fixture can Goexit before returning its owner. Its
				// registered cleanup still retains that owner through this T.
				panicValue := recover()
				privateSerenaMigrationFailStop(struct {
					Case  *privateSerenaMigrationCase
					Test  *testing.T
					Panic any
				}{c, t, panicValue},
					"original-product-acquisition-did-not-return", fmt.Errorf("original Product fixture did not return its receiving outcome (panic=%v)", panicValue))
			}
		}()
		c.product = loadWindowsCanonicalNativeActorFixture(t)
		returned = true
	}()
	expectedProduct := c.product.Observation.EntrySet.ProductArtifact
	expectedProduct.Path = c.product.Targets.ProductPath
	actual, err := binaryadmission.AdmitWindowsRole(expectedProduct)
	productInfo, statErr := os.Stat(c.product.Targets.ProductPath)
	if err != nil || statErr != nil || actual.Role != binaryadmission.WindowsArtifactRoleProduct ||
		actual.Commit != input.SourceCommit || actual.SHA256 != c.product.Input.ProductSHA256 ||
		actual.Machine != 0x8664 || os.SameFile(selfInfo, productInfo) {
		t.Fatalf("private migration actual separate Product differs from its original held entry set: %v", errors.Join(err, statErr))
	}
	native, err := process.ObserveCurrentPublisherNative(ctx)
	if err != nil {
		privateSerenaMigrationFailStop(struct {
			Case   *privateSerenaMigrationCase
			Native *process.PublisherNativeObservation
		}{c, native}, "child-current-native-observation", err)
	}
	image, err := api.NewCurrentPublisherImageObservation(ctx, native)
	if err != nil {
		privateSerenaMigrationFailStop(struct {
			Case  *privateSerenaMigrationCase
			Image *api.CurrentPublisherImageObservation
		}{c, image}, "child-current-image-observation", err)
	}
	if err := image.Validate(ctx, native); err != nil {
		privateSerenaMigrationFailStop(c, "child-current-image-validation", err)
	}
	c.frame = privateSerenaMigrationChildFrame{
		Schema: "private-serena-migration-child-v1", Root: root.Name, InputSHA256: config.Input.SHA256, WorkEnd: workEnd,
		Native: native.NativeSnapshot(), Image: image.Snapshot(), Operations: []privateSerenaMigrationOperation{}, Seeds: []privateSerenaMigrationArtifact{},
	}
	if c.frame.Image.ImageSHA256 != input.Image.SHA256 || c.frame.Image.SourceEpoch != input.SourceEpoch || !c.frame.Image.ResourcesClosed {
		t.Fatal("private migration actual current-image observation differs")
	}
	return c
}

func (c *privateSerenaMigrationCase) Context() context.Context {
	if c == nil {
		return context.Background()
	}
	if c.blocked != nil {
		privateSerenaMigrationFailStop(c, "later-call-after-uncertain-operation", c.blocked)
	}
	return c.ctx
}

func (c *privateSerenaMigrationCase) TempDir(t *testing.T) string {
	t.Helper()
	if c == nil {
		return t.TempDir()
	}
	if err := c.Context().Err(); err != nil {
		t.Fatal(err)
	}
	path, err := os.MkdirTemp(c.config.Directory, "w")
	if err != nil {
		t.Fatal(err)
	}
	// This is retained evidence, with no testing RemoveAll registration.
	apitest.HardenedDir(t, path)
	return path
}

func (c *privateSerenaMigrationCase) Env(t *testing.T, opts ...migrateSerenaTestEnvOptions) (string, string) {
	t.Helper()
	if c == nil {
		if usesSerenaQualificationFixture {
			return migrateSerenaPreparedTestEnv(t)
		}
		return migrateSerenaTestEnv(t, opts...)
	}
	if len(opts) > 1 {
		t.Fatal("private migration Env accepts the original single options value")
	}
	root := c.TempDir(t)
	state := apitest.HardenedDir(t, filepath.Join(root, "s"))
	state, manifests := migrateSerenaTestEnvWithRoots(t, root, state, false, opts...)
	// The original held entry-set and public Product admission were received
	// above. This scoped string selects that operand; it grants no publisher,
	// namespace, prepared selection or Ready authority.
	restore := api.SetTestCanonicalMcphubPath(c.product.Targets.ProductPath)
	t.Cleanup(restore)
	return state, manifests
}

func (c *privateSerenaMigrationCase) SeedManifest(t *testing.T, directory, original string) string {
	t.Helper()
	if c == nil {
		if usesSerenaQualificationFixture {
			return seedPreparedSerenaManifestForTest(t, directory, original)
		}
		return seedSerenaManifest(t, directory, original)
	}
	if err := c.Context().Err(); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(c.config.Directory, directory)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatal("private migration manifest must be inside the retained case")
	}
	readCanonicalNativeActorBytes(t, c.loaded.Input.Catalog.Path, c.loaded.Input.Catalog.SHA256, 1<<20)
	raw, err := privateSerenaMigrationManifest(c.loaded.Catalog, original, c.loaded.Input.CacheSeed != nil)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "serena", "manifest.yaml")
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private migration seed destination must be absent: %v", err)
	}
	path := seedSerenaManifest(t, directory, string(raw))
	artifact := privateSerenaMigrationArtifact{Path: path, SHA256: privateSerenaMigrationDigest(raw)}
	readCanonicalNativeActorBytes(t, path, artifact.SHA256, 1<<20)
	c.frame.Seeds = append(c.frame.Seeds, artifact)
	return path
}

func (c *privateSerenaMigrationCase) Run(t *testing.T, output io.Writer) error {
	t.Helper()
	if c == nil {
		return runMigrateSerenaDynamicPool(context.Background(), output)
	}
	if err := c.Context().Err(); err != nil {
		privateSerenaMigrationFailStop(c, "original-case-context", err)
	}
	if !time.Now().Before(c.config.WorkEnd.Add(-api.DefaultSerenaPreparationPolicy().CleanupLimit)) {
		return errors.New("private migration has no remaining original preparation interval")
	}
	c.loaded.verify(t)
	// Prefer the private child's pinned tool directory, as this target's
	// preparation does. PrepareInputs still owns its anchored identity/probes.
	uvPath, err := exec.LookPath("uv")
	if err != nil {
		discovered, discoveryErr := binary_discovery.Discover(c.ctx, []string{"uv"}, binary_discovery.DefaultHints())
		if discoveryErr != nil {
			return discoveryErr
		}
		if uvPath = discovered["uv"]; uvPath != "" {
			err = nil
		}
	}
	actualUV, uvErr := os.Stat(uvPath)
	expectedUV, expectedErr := os.Stat(c.loaded.Input.UV.Path)
	if err != nil || uvErr != nil || expectedErr != nil || !os.SameFile(actualUV, expectedUV) {
		return errors.Join(errors.New("private migration original UV discovery differs from its pinned candidate"), err, uvErr, expectedErr)
	}
	uvxPath, err := exec.LookPath("uvx")
	actualUVX, uvxErr := os.Stat(uvxPath)
	expectedUVX, expectedErr := os.Stat(c.loaded.Input.UVX.Path)
	if err != nil || uvxErr != nil || expectedErr != nil || !os.SameFile(actualUVX, expectedUVX) {
		return errors.Join(errors.New("private migration uvx preflight differs from its pinned candidate"), err, uvxErr, expectedErr)
	}
	operation := privateSerenaMigrationOperation{Test: t.Name()}
	c.frame.Operations = append(c.frame.Operations, operation)
	index := len(c.frame.Operations) - 1
	current := &c.frame.Operations[index]
	returned := false
	defer func() {
		if !returned {
			// Includes panic and testing.Goexit inside an original hook. Do
			// not return to another subtest/reset with unreceived ownership.
			panicValue := recover()
			privateSerenaMigrationFailStop(struct {
				Case  *privateSerenaMigrationCase
				Panic any
			}{c, panicValue}, "original-operation-did-not-return",
				fmt.Errorf("original preparation/consumer did not return its closure outcome (panic=%v)", panicValue))
		}
	}()
	var seed []api.PrivateSerenaCacheSeed
	if artifact := c.loaded.Input.CacheSeed; artifact != nil {
		seed = []api.PrivateSerenaCacheSeed{{DescriptorPath: artifact.Path, DescriptorSHA256: artifact.SHA256}}
	}
	installer, constructorErr := api.NewAPIWithPrivateSerenaIntegrationPublisher(c.ctx, newSerenaPreparationAssembly(), seed...)
	if installer != nil {
		c.apis = append(c.apis, installer)
	}
	if constructorErr != nil {
		current.ConstructorError = constructorErr.Error()
		c.blocked = constructorErr
		privateSerenaMigrationFailStop(c, "private-original-publisher-constructor", constructorErr)
	}
	deps := defaultMigrateSerenaRunDeps()
	deps.newInstallAPI = func() *api.API { return installer }
	originalOperation := deps.newInstallOperation
	deps.newInstallOperation = func(a *api.API) (migrateSerenaInstallOperation, error) {
		original, err := originalOperation(a)
		// These original closures retain the prepared pointer and its
		// request context even when the driver returns before consumption.
		c.operations = append(c.operations, original)
		if err != nil {
			return original, err
		}
		return migrateSerenaInstallOperation{
			prepare: func(ctx context.Context, manifest *config.ServerManifest, opts api.InstallParsedManifestOpts) error {
				current.PrepareCalls++
				if len(opts.Workspaces) != 0 {
					current.NonemptyCalls++
				}
				err := original.prepare(ctx, manifest, opts)
				if err != nil {
					current.PrepareError = err.Error()
					c.blocked = err
					privateSerenaMigrationFailStop(c, "original-prepare", err)
				}
				return err
			},
			install: func(ctx context.Context, manifest *config.ServerManifest, opts api.InstallParsedManifestOpts) (string, error, error) {
				current.InstallCalls++
				path, installErr, cleanupErr := original.install(ctx, manifest, opts)
				current.IntentPath = path
				if installErr != nil {
					current.InstallError = installErr.Error()
				}
				if cleanupErr != nil {
					current.CleanupError = cleanupErr.Error()
				}
				if installErr != nil || cleanupErr != nil {
					c.blocked = errors.Join(installErr, cleanupErr)
					privateSerenaMigrationFailStop(c, "original-consumer-install-close", c.blocked)
				}
				return path, installErr, cleanupErr
			},
		}, nil
	}
	driverErr := runMigrateSerenaDynamicPoolWithDeps(c.ctx, output, deps)
	if driverErr != nil {
		current.DriverError = driverErr.Error()
	}
	current.OperationComplete = current.NonemptyCalls > 0 && current.InstallCalls == 1 && current.IntentPath != "" &&
		current.ConstructorError == "" && current.PrepareError == "" && current.InstallError == "" && current.CleanupError == ""
	if !current.OperationComplete {
		c.blocked = errors.Join(driverErr, errors.New("private migration original positive operation did not receive its terminal install/close"))
		privateSerenaMigrationFailStop(c, "unreceived-positive-operation", c.blocked)
	}
	returned = true
	// Late supervisor/readiness errors are the original assertions' subject.
	// The separate completed operation allows the second original table case.
	return driverErr
}
