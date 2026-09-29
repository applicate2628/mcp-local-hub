package clients

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

type immediateCodexProjectConfigLockRunner struct{}

func (immediateCodexProjectConfigLockRunner) RunRead(fn func() error) error     { return fn() }
func (immediateCodexProjectConfigLockRunner) RunMutation(fn func() error) error { return fn() }

func immediateCodexProjectConfigLockRunnerFactory(CodexProjectIdentity) (CodexProjectConfigLockRunner, error) {
	return immediateCodexProjectConfigLockRunner{}, nil
}

type recordingCodexProjectConfigLockRunner struct {
	readCalls     int
	mutationCalls int
	readErr       error
	mutationErr   error
}

func (r *recordingCodexProjectConfigLockRunner) RunRead(fn func() error) error {
	r.readCalls++
	if r.readErr != nil {
		return r.readErr
	}
	return fn()
}

func (r *recordingCodexProjectConfigLockRunner) RunMutation(fn func() error) error {
	r.mutationCalls++
	if r.mutationErr != nil {
		return r.mutationErr
	}
	return fn()
}

type nilCodexProjectConfigLockRunner struct{}

func (*nilCodexProjectConfigLockRunner) RunRead(func() error) error {
	panic("typed-nil runner invoked")
}
func (*nilCodexProjectConfigLockRunner) RunMutation(func() error) error {
	panic("typed-nil runner invoked")
}

func writeCodexProjectFixture(t *testing.T) (string, string, []byte) {
	t.Helper()
	root := t.TempDir()
	configPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("[mcp_servers.graphify]\ncommand = 'graphify-mcp'\n")
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return root, configPath, content
}

// Catches the identity helper consulting the filesystem, accepting a dirty or
// relative root, hashing the raw spelling instead of CanonicalProjectKey, or
// manufacturing a physically validated identity without a config read.
func TestCodexProjectIdentitySHA256ForRootIsPureAndCanonical(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "Missing-Project")
	if _, err := os.Stat(missingRoot); !os.IsNotExist(err) {
		t.Fatalf("fixture root unexpectedly exists: %v", err)
	}
	wantKey := filepath.ToSlash(filepath.Clean(filepath.FromSlash(missingRoot)))
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		wantKey = strings.ToLower(wantKey)
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(wantKey)))
	got, err := CodexProjectIdentitySHA256ForRoot(missingRoot)
	if err != nil || got != want {
		t.Fatalf("CodexProjectIdentitySHA256ForRoot = %q/%v, want %q/nil", got, err, want)
	}
	if _, err := os.Stat(missingRoot); !os.IsNotExist(err) {
		t.Fatalf("identity helper accessed or created missing root: %v", err)
	}

	dirtyAbsolute := missingRoot + string(filepath.Separator) + "." + string(filepath.Separator) + "child"
	for _, tc := range []struct {
		name string
		root string
	}{
		{name: "empty"},
		{name: "relative", root: filepath.Join("relative", "project")},
		{name: "dirty absolute", root: dirtyAbsolute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fingerprint, err := CodexProjectIdentitySHA256ForRoot(tc.root)
			if fingerprint != "" || !errors.Is(err, ErrCodexProjectSourceRootInvalid) {
				t.Fatalf("identity = %q/%v, want empty/CODEX_PROJECT_SOURCE_ROOT_INVALID", fingerprint, err)
			}
		})
	}
}

// Catches a snapshot re-reading after capture, exposing its private byte slice,
// hashing different bytes than it returns, or accidentally serializing root or
// config content into a plan/public JSON value.
func TestReadCodexProjectConfigSnapshotIsImmutableAndPathOpaque(t *testing.T) {
	root, configPath, original := writeCodexProjectFixture(t)
	snapshot, err := ReadCodexProjectConfigSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	wantIdentity := fmt.Sprintf("%x", sha256.Sum256([]byte(CanonicalProjectKey(root))))
	wantConfigSHA := fmt.Sprintf("%x", sha256.Sum256(original))
	if snapshot.Identity().CanonicalRoot() != root || snapshot.Identity().ProjectIdentitySHA256() != wantIdentity {
		t.Fatalf("snapshot identity = root %q fingerprint %q", snapshot.Identity().CanonicalRoot(), snapshot.Identity().ProjectIdentitySHA256())
	}
	if snapshot.ConfigSHA256() != wantConfigSHA {
		t.Fatalf("ConfigSHA256 = %q, want %q", snapshot.ConfigSHA256(), wantConfigSHA)
	}
	first := snapshot.Bytes()
	if !bytes.Equal(first, original) {
		t.Fatalf("snapshot bytes = %q, want %q", first, original)
	}
	first[0] ^= 0xff
	if got := snapshot.Bytes(); !bytes.Equal(got, original) {
		t.Fatalf("Bytes accessor exposed mutable storage: got %q want %q", got, original)
	}
	replacement := []byte("[mcp_servers.graphify]\ncommand = 'later-edit'\n")
	if err := os.WriteFile(configPath, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Bytes(); !bytes.Equal(got, original) || snapshot.ConfigSHA256() != wantConfigSHA {
		t.Fatalf("snapshot drifted after file edit: bytes=%q sha=%q", got, snapshot.ConfigSHA256())
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || string(encoded) != "{}" {
		t.Fatalf("snapshot JSON = %q/%v, want {}/nil", encoded, err)
	}
	if _, statErr := os.Stat(configPath + ".lock"); !os.IsNotExist(statErr) {
		t.Fatalf("snapshot created adjacent lock: %v", statErr)
	}
}

// Catches the snapshot API treating an absent fixed target as a valid identity
// or returning partial bytes/hash state on physical validation failure.
func TestReadCodexProjectConfigSnapshotRejectsAbsentTarget(t *testing.T) {
	root := t.TempDir()
	snapshot, err := ReadCodexProjectConfigSnapshot(root)
	if snapshot.Identity() != (CodexProjectIdentity{}) || snapshot.ConfigSHA256() != "" || len(snapshot.Bytes()) != 0 || !errors.Is(err, ErrCodexProjectSourceRootInvalid) {
		t.Fatalf("snapshot = %#v/%v, want zero/CODEX_PROJECT_SOURCE_ROOT_INVALID", snapshot, err)
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("absent snapshot read changed root: read=%v entries=%v", readErr, entries)
	}
}

// Catches constructor validation diverging from the public snapshot identity or
// invoking its lock factory before the physically validated snapshot exists.
func TestNewCodexCLIForProjectReusesSnapshotIdentityBeforeFactory(t *testing.T) {
	root, _, _ := writeCodexProjectFixture(t)
	snapshot, err := ReadCodexProjectConfigSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	var factoryIdentity CodexProjectIdentity
	client, identity, err := NewCodexCLIForProject(root, "graphify", func(got CodexProjectIdentity) (CodexProjectConfigLockRunner, error) {
		factoryIdentity = got
		return immediateCodexProjectConfigLockRunner{}, nil
	})
	if err != nil || client == nil {
		t.Fatalf("NewCodexCLIForProject = %T/%v", client, err)
	}
	if identity != snapshot.Identity() || factoryIdentity != snapshot.Identity() {
		t.Fatalf("constructor/factory identity = %#v/%#v, want snapshot %#v", identity, factoryIdentity, snapshot.Identity())
	}
}

func TestNewCodexCLIForProjectRequiresRunnerAfterIdentityValidation(t *testing.T) {
	root, configPath, original := writeCodexProjectFixture(t)
	wantFingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(CanonicalProjectKey(root))))
	factoryCause := errors.New("injected project lock owner failure")
	var typedNil *nilCodexProjectConfigLockRunner
	tests := []struct {
		name    string
		factory CodexProjectConfigLockRunnerFactory
		cause   error
	}{
		{name: "nil factory"},
		{name: "nil runner", factory: func(CodexProjectIdentity) (CodexProjectConfigLockRunner, error) { return nil, nil }},
		{name: "typed nil runner", factory: func(CodexProjectIdentity) (CodexProjectConfigLockRunner, error) { return typedNil, nil }},
		{name: "factory error", factory: func(CodexProjectIdentity) (CodexProjectConfigLockRunner, error) { return nil, factoryCause }, cause: factoryCause},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, identity, err := NewCodexCLIForProject(root, "graphify", tc.factory)
			if client != nil || !errors.Is(err, ErrCodexProjectConfigLockUnavailable) {
				t.Fatalf("NewCodexCLIForProject = (%T, %v), want nil/CODEX_PROJECT_CONFIG_LOCK_UNAVAILABLE", client, err)
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("error %v does not preserve factory cause %v", err, tc.cause)
			}
			if identity.CanonicalRoot() != root || identity.ProjectIdentitySHA256() != wantFingerprint {
				t.Fatalf("identity = root %q fingerprint %q, want validated identity before factory failure", identity.CanonicalRoot(), identity.ProjectIdentitySHA256())
			}
			if _, statErr := os.Stat(configPath + ".lock"); !os.IsNotExist(statErr) {
				t.Fatalf("adjacent lock exists after factory refusal: %v", statErr)
			}
			got, readErr := os.ReadFile(configPath)
			if readErr != nil || !bytes.Equal(got, original) {
				t.Fatalf("project config changed after factory refusal: read=%v before=%q after=%q", readErr, original, got)
			}
		})
	}

	factoryCalls := 0
	client, identity, err := NewCodexCLIForProject(filepath.Join(root, "missing"), "graphify", func(CodexProjectIdentity) (CodexProjectConfigLockRunner, error) {
		factoryCalls++
		return immediateCodexProjectConfigLockRunner{}, nil
	})
	if client != nil || identity != (CodexProjectIdentity{}) || !errors.Is(err, ErrCodexProjectSourceRootInvalid) || factoryCalls != 0 {
		t.Fatalf("invalid target = (%T, %#v, %v), factory calls=%d; want nil/zero/root-invalid/0", client, identity, err, factoryCalls)
	}
}

func TestCodexProjectLockingForwardersUseInjectedRunnerWithoutAdjacentLock(t *testing.T) {
	root, configPath, original := writeCodexProjectFixture(t)
	runner := &recordingCodexProjectConfigLockRunner{}
	client, _, err := NewCodexCLIForProject(root, "graphify", func(CodexProjectIdentity) (CodexProjectConfigLockRunner, error) {
		return runner, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if created, initErr := client.InitEmpty(); initErr != nil || created {
		t.Fatalf("InitEmpty = %v/%v, want false/nil", created, initErr)
	}
	cas, ok := AsCASEntryMutator(client)
	if !ok {
		t.Fatalf("project client type %T lacks CAS capability", client)
	}
	snapshot, present, err := cas.EntryRawSubtree(original, "graphify")
	if err != nil || !present {
		t.Fatalf("snapshot subtree = present %v error %v", present, err)
	}
	if verdict, classifyErr := cas.ClassifyEntryUnderLock("graphify", func(*MCPEntry) bool { return false }, snapshot); classifyErr != nil || verdict != ClassifyRestoreDone {
		t.Fatalf("classification = %v/%v, want restore-done/nil", verdict, classifyErr)
	}
	if restoreErr := cas.CASRestoreEntryFromBytes("graphify", func(*MCPEntry) bool { return false }, original); !errors.Is(restoreErr, ErrCASConflict) {
		t.Fatalf("CAS restore error = %v, want conflict", restoreErr)
	}
	if removeErr := cas.CASGuardedRemoveEntry("graphify", func(*MCPEntry) bool { return false }); !errors.Is(removeErr, ErrCASConflict) {
		t.Fatalf("CAS remove error = %v, want conflict", removeErr)
	}
	conditional, ok := client.(ConditionalEntryMutator)
	if !ok {
		t.Fatalf("project client type %T lacks conditional mutation capability", client)
	}
	observed := conditional.ConditionalEntryMutation(ConditionalEntryMutationRequest{
		Operation: EntryMutationRemove,
		EntryName: "graphify",
		ExpectedLive: func(*MCPEntry) bool {
			return false
		},
	})
	if !observed.PreconditionConflict || !errors.Is(observed.PreparationErr, ErrEntryMutationPreconditionConflict) || observed.Invoked {
		t.Fatalf("conditional refusal = conflict %v invoked %v error %v", observed.PreconditionConflict, observed.Invoked, observed.PreparationErr)
	}
	if runner.readCalls != 1 || runner.mutationCalls != 4 {
		t.Fatalf("runner calls = read %d mutation %d, want 1/4", runner.readCalls, runner.mutationCalls)
	}
	if _, statErr := os.Stat(configPath + ".lock"); !os.IsNotExist(statErr) {
		t.Fatalf("adjacent project lock exists: %v", statErr)
	}
}

func TestProjectBoundLockingClientWithoutRunnerRefusesBeforeAdjacentLock(t *testing.T) {
	_, configPath, original := writeCodexProjectFixture(t)
	client := newLockingClient(&codexCLI{
		path:                  configPath,
		projectIdentitySHA256: strings.Repeat("a", 64),
		projectLogicalEntry:   "graphify",
	})
	created, err := client.InitEmpty()
	if created || !errors.Is(err, ErrCodexProjectConfigLockUnavailable) {
		t.Fatalf("InitEmpty = %v/%v, want false/CODEX_PROJECT_CONFIG_LOCK_UNAVAILABLE", created, err)
	}
	if _, statErr := os.Stat(configPath + ".lock"); !os.IsNotExist(statErr) {
		t.Fatalf("adjacent project lock exists: %v", statErr)
	}
	got, readErr := os.ReadFile(configPath)
	if readErr != nil || !bytes.Equal(got, original) {
		t.Fatalf("project config changed: read=%v before=%q after=%q", readErr, original, got)
	}
}

// Catches project construction falling back to NewCodexCLI (and therefore
// binding the global config) instead of fixing the selected project's target.
func TestNewCodexCLIForProjectBindsExistingProjectConfig(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("[mcp_servers.graphify]\ncommand = 'graphify-mcp'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client, identity, err := NewCodexCLIForProject(root, "graphify", immediateCodexProjectConfigLockRunnerFactory)
	if err != nil {
		t.Fatalf("NewCodexCLIForProject: %v", err)
	}
	if client.ConfigPath() != configPath {
		t.Fatalf("ConfigPath = %q, want fixed project target %q", client.ConfigPath(), configPath)
	}
	if client.Name() != "codex-cli" {
		t.Fatalf("Name = %q, want codex-cli", client.Name())
	}
	wantFingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(CanonicalProjectKey(root))))
	if identity.CanonicalRoot() != root || identity.ProjectIdentitySHA256() != wantFingerprint {
		t.Fatalf("identity = root %q fingerprint %q, want root %q fingerprint %q", identity.CanonicalRoot(), identity.ProjectIdentitySHA256(), root, wantFingerprint)
	}
	if _, ok := client.(*lockingClient); !ok {
		t.Fatalf("client type = %T, want locking adapter", client)
	}
}

// Catches a project-bound adapter accepting a relocation without the pinned
// full-file compare-and-set precondition required by project adoption.
func TestCodexProjectRelocationRequiresExpectedConfigSHA256(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("[mcp_servers.graphify]\ncommand = 'graphify-mcp'\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	client, _, err := NewCodexCLIForProject(root, "graphify", immediateCodexProjectConfigLockRunnerFactory)
	if err != nil {
		t.Fatal(err)
	}
	codex, ok := AsCodexTransportClient(client)
	if !ok {
		t.Fatalf("project client type %T lacks Codex transport capability", client)
	}
	writes := 0
	_, err = codex.RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "graphify",
		TargetEntryName: "graphify",
		ExpectedSource:  CodexTransportStdio,
		Entry:           MCPEntry{Name: "graphify", URL: "http://127.0.0.1:9300/mcp"},
		WriteConfig: func(path string, data []byte) error {
			writes++
			return os.WriteFile(path, data, 0o600)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "CODEX_PROJECT_SOURCE_CHANGED") {
		t.Fatalf("RelocateHTTPEntry error = %v, want CODEX_PROJECT_SOURCE_CHANGED", err)
	}
	if writes != 0 {
		t.Fatalf("project relocation writes = %d, want 0 without expected config hash", writes)
	}
	after, readErr := os.ReadFile(configPath)
	if readErr != nil || !bytes.Equal(after, original) {
		t.Fatalf("project config changed on refusal: read=%v before=%q after=%q", readErr, original, after)
	}
}

// Catches project relocation reporting the global settlement shape, targeting
// the global config, or altering a selected project's unrelated values.
func TestCodexProjectRelocationCommitsSameNameInSelectedProjectOnly(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	projectBefore := []byte("title = 'selected'\n\n[mcp_servers.graphify]\ncommand = 'graphify-mcp'\nargs = ['--stdio']\n\n[mcp_servers.sibling]\ncommand = 'sibling-mcp'\n")
	if err := os.WriteFile(configPath, projectBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	globalHome := filepath.Join(t.TempDir(), "global-codex")
	if err := os.MkdirAll(globalHome, 0o700); err != nil {
		t.Fatal(err)
	}
	globalPath := filepath.Join(globalHome, "config.toml")
	globalBefore := []byte("[mcp_servers.graphify]\ncommand = 'global-graphify'\n")
	if err := os.WriteFile(globalPath, globalBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", globalHome)
	foreignRoot := t.TempDir()
	foreignPath := filepath.Join(foreignRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(foreignPath), 0o700); err != nil {
		t.Fatal(err)
	}
	foreignBefore := []byte("[mcp_servers.graphify]\ncommand = 'foreign-graphify'\n")
	if err := os.WriteFile(foreignPath, foreignBefore, 0o600); err != nil {
		t.Fatal(err)
	}

	client, _, err := NewCodexCLIForProject(root, "graphify", immediateCodexProjectConfigLockRunnerFactory)
	if err != nil {
		t.Fatal(err)
	}
	codex, ok := AsCodexTransportClient(client)
	if !ok {
		t.Fatalf("project client type %T lacks Codex transport capability", client)
	}
	projectSHA := fmt.Sprintf("%x", sha256.Sum256(projectBefore))
	result, err := codex.RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName:      "graphify",
		TargetEntryName:      "graphify",
		ExpectedSource:       CodexTransportStdio,
		ExpectedConfigSHA256: projectSHA,
		Entry:                MCPEntry{Name: "graphify", URL: "http://127.0.0.1:9300/mcp"},
	})
	if err != nil {
		t.Fatalf("RelocateHTTPEntry: %v", err)
	}
	if result.Outcome != CodexHTTPRelocationCommitted || result.Readback != CodexHTTPRelocationReadbackExact ||
		result.WriteTarget != "codex_project" || result.Action != "replace" || result.CollisionReason != "" {
		t.Fatalf("project relocation result = %#v, want committed exact codex_project replacement", result)
	}
	afterBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if err := toml.Unmarshal(afterBytes, &after); err != nil {
		t.Fatal(err)
	}
	if after["title"] != "selected" {
		t.Fatalf("unrelated top-level value = %#v, want selected", after["title"])
	}
	servers := after["mcp_servers"].(map[string]any)
	if !reflect.DeepEqual(servers["sibling"], map[string]any{"command": "sibling-mcp"}) {
		t.Fatalf("unrelated sibling changed: %#v", servers["sibling"])
	}
	if target, ok := servers["graphify"].(map[string]any); !ok || target["url"] != "http://127.0.0.1:9300/mcp" {
		t.Fatalf("same-name project target = %#v, want hub HTTP table", target)
	}
	for path, before := range map[string][]byte{globalPath: globalBefore, foreignPath: foreignBefore} {
		got, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(got, before) {
			t.Fatalf("protected fixture changed at %s: read=%v before=%q after=%q", filepath.Base(filepath.Dir(path)), readErr, before, got)
		}
	}
}

// Catches the project adapter comparing only the selected entry and therefore
// overwriting an intervening sibling edit after provenance capture.
func TestCodexProjectRelocationRejectsStaleFullFileSHA(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	captured := []byte("[mcp_servers.graphify]\ncommand = 'graphify-mcp'\n")
	if err := os.WriteFile(configPath, captured, 0o600); err != nil {
		t.Fatal(err)
	}
	client, _, err := NewCodexCLIForProject(root, "graphify", immediateCodexProjectConfigLockRunnerFactory)
	if err != nil {
		t.Fatal(err)
	}
	intervening := append(append([]byte(nil), captured...), []byte("\n[mcp_servers.operator-added]\ncommand = 'operator-mcp'\n")...)
	if err := os.WriteFile(configPath, intervening, 0o600); err != nil {
		t.Fatal(err)
	}
	writes := 0
	_, err = client.(CodexTransportClient).RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName:      "graphify",
		TargetEntryName:      "graphify",
		ExpectedSource:       CodexTransportStdio,
		ExpectedConfigSHA256: fmt.Sprintf("%x", sha256.Sum256(captured)),
		Entry:                MCPEntry{Name: "graphify", URL: "http://127.0.0.1:9300/mcp"},
		WriteConfig: func(path string, data []byte) error {
			writes++
			return os.WriteFile(path, data, 0o600)
		},
	})
	if !errors.Is(err, ErrCodexProjectSourceChanged) || writes != 0 {
		t.Fatalf("stale relocation err/writes = %v/%d, want CODEX_PROJECT_SOURCE_CHANGED/0", err, writes)
	}
	after, readErr := os.ReadFile(configPath)
	if readErr != nil || !bytes.Equal(after, intervening) {
		t.Fatalf("stale refusal changed project bytes: read=%v before=%q after=%q", readErr, intervening, after)
	}
}

// Catches project mode retaining the global alias-relocation contract instead
// of requiring one same-name replacement in the selected file.
func TestCodexProjectRelocationRejectsNonSameNameTargets(t *testing.T) {
	for _, tc := range []struct {
		name, source, target, entryName string
	}{
		{name: "different target", source: "graphify", target: "graphify-mcphub", entryName: "graphify-mcphub"},
		{name: "entry name mismatch", source: "graphify", target: "graphify", entryName: "other"},
		{name: "different logical entry", source: "other", target: "other", entryName: "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(root, ".codex", "config.toml")
			if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
				t.Fatal(err)
			}
			original := []byte("[mcp_servers.graphify]\ncommand = 'graphify-mcp'\n\n[mcp_servers.other]\ncommand = 'other-mcp'\n")
			if err := os.WriteFile(configPath, original, 0o600); err != nil {
				t.Fatal(err)
			}
			client, _, err := NewCodexCLIForProject(root, "graphify", immediateCodexProjectConfigLockRunnerFactory)
			if err != nil {
				t.Fatal(err)
			}
			writes := 0
			_, err = client.(CodexTransportClient).RelocateHTTPEntry(CodexHTTPRelocation{
				SourceEntryName: tc.source, TargetEntryName: tc.target,
				ExpectedSource:       CodexTransportStdio,
				ExpectedConfigSHA256: fmt.Sprintf("%x", sha256.Sum256(original)),
				Entry:                MCPEntry{Name: tc.entryName, URL: "http://127.0.0.1:9300/mcp"},
				WriteConfig: func(path string, data []byte) error {
					writes++
					return os.WriteFile(path, data, 0o600)
				},
			})
			if err == nil || writes != 0 {
				t.Fatalf("non-same-name relocation err/writes = %v/%d, want refusal/0", err, writes)
			}
			after, readErr := os.ReadFile(configPath)
			if readErr != nil || !bytes.Equal(after, original) {
				t.Fatalf("target refusal changed bytes: read=%v before=%q after=%q", readErr, original, after)
			}
		})
	}
}

// Catches a failed project write leaving the newly published HTTP table instead
// of restoring and proving the exact pre-operation file bytes.
func TestCodexProjectRelocationReadbackFailureRestoresExactBytes(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("title = 'preserve formatting'\n\n[mcp_servers.graphify]\ncommand = 'graphify-mcp'\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	client, _, err := NewCodexCLIForProject(root, "graphify", immediateCodexProjectConfigLockRunnerFactory)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.(CodexTransportClient).RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName:      "graphify",
		TargetEntryName:      "graphify",
		ExpectedSource:       CodexTransportStdio,
		ExpectedConfigSHA256: fmt.Sprintf("%x", sha256.Sum256(original)),
		Entry:                MCPEntry{Name: "graphify", URL: "http://127.0.0.1:9300/mcp"},
		WriteConfig: func(path string, _ []byte) error {
			return os.WriteFile(path, []byte("not valid TOML = {{{"), 0o600)
		},
	})
	if !errors.Is(err, ErrCodexConfigReadbackFailed) {
		t.Fatalf("RelocateHTTPEntry error = %v, want CODEX_CLIENT_CONFIG_READBACK_FAILED", err)
	}
	after, readErr := os.ReadFile(configPath)
	if readErr != nil || !bytes.Equal(after, original) {
		t.Fatalf("project rollback bytes: read=%v before=%q after=%q", readErr, original, after)
	}
}

// Catches project construction accepting an ambiguous/missing target or reading
// CODEX_HOME while validating the explicitly supplied root.
func TestNewCodexCLIForProjectRejectsInvalidRootAndTargetWithoutGlobalMutation(t *testing.T) {
	globalHome := t.TempDir()
	globalPath := filepath.Join(globalHome, "config.toml")
	globalBefore := []byte("not TOML {{{")
	if err := os.WriteFile(globalPath, globalBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", globalHome)

	validRoot := t.TempDir()
	validConfig := filepath.Join(validRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(validConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(validConfig, []byte("[mcp_servers.graphify]\ncommand = 'graphify-mcp'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingConfigRoot := t.TempDir()
	configDirectoryRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configDirectoryRoot, ".codex", "config.toml"), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, root, entry string }{
		{name: "relative root", root: "relative-project", entry: "graphify"},
		{name: "unclean root", root: validRoot + string(filepath.Separator) + ".", entry: "graphify"},
		{name: "missing root", root: filepath.Join(t.TempDir(), "missing"), entry: "graphify"},
		{name: "missing config", root: missingConfigRoot, entry: "graphify"},
		{name: "config is directory", root: configDirectoryRoot, entry: "graphify"},
		{name: "empty logical entry", root: validRoot, entry: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, identity, err := NewCodexCLIForProject(tc.root, tc.entry, immediateCodexProjectConfigLockRunnerFactory)
			if !errors.Is(err, ErrCodexProjectSourceRootInvalid) || client != nil || identity != (CodexProjectIdentity{}) {
				t.Fatalf("NewCodexCLIForProject = (%T, %#v, %v), want nil/zero/CODEX_PROJECT_SOURCE_ROOT_INVALID", client, identity, err)
			}
		})
	}

	realRoot := t.TempDir()
	realConfig := filepath.Join(realRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(realConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realConfig, []byte("[mcp_servers.graphify]\ncommand = 'graphify-mcp'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(t.TempDir(), "project-link")
	if runtime.GOOS == "windows" {
		createCodexJunctionForTest(t, aliasRoot, realRoot)
	} else if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Fatal(err)
	}
	if client, identity, err := NewCodexCLIForProject(aliasRoot, "graphify", immediateCodexProjectConfigLockRunnerFactory); !errors.Is(err, ErrCodexProjectSourceRootInvalid) || client != nil || identity != (CodexProjectIdentity{}) {
		t.Fatalf("linked root = (%T, %#v, %v), want nil/zero/CODEX_PROJECT_SOURCE_ROOT_INVALID", client, identity, err)
	}
	globalAfter, err := os.ReadFile(globalPath)
	if err != nil || !bytes.Equal(globalAfter, globalBefore) {
		t.Fatalf("global config changed during project validation: read=%v before=%q after=%q", err, globalBefore, globalAfter)
	}
}

// Catches the project factory losing the existing CASEntryMutator capability or
// restoring the whole captured file over a later sibling edit.
func TestCodexProjectCASRestorePreservesLaterSiblingEdit(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("[mcp_servers.graphify]\ncommand = 'graphify-mcp'\nargs = ['--stdio']\n\n[mcp_servers.sibling]\ncommand = 'before'\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	client, _, err := NewCodexCLIForProject(root, "graphify", immediateCodexProjectConfigLockRunnerFactory)
	if err != nil {
		t.Fatal(err)
	}
	codex := client.(CodexTransportClient)
	if _, err := codex.RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "graphify", TargetEntryName: "graphify",
		ExpectedSource:       CodexTransportStdio,
		ExpectedConfigSHA256: fmt.Sprintf("%x", sha256.Sum256(original)),
		Entry:                MCPEntry{Name: "graphify", URL: "http://127.0.0.1:9300/mcp"},
	}); err != nil {
		t.Fatalf("RelocateHTTPEntry: %v", err)
	}
	doc, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := toml.Unmarshal(doc, &parsed); err != nil {
		t.Fatal(err)
	}
	parsed["mcp_servers"].(map[string]any)["sibling"] = map[string]any{"command": "after"}
	doc, err = toml.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	mutator, ok := AsCASEntryMutator(client)
	if !ok {
		t.Fatalf("project client type %T lacks CASEntryMutator", client)
	}
	matchHub := func(entry *MCPEntry) bool { return entry != nil && entry.URL == "http://127.0.0.1:9300/mcp" }
	if err := mutator.CASRestoreEntryFromBytes("graphify", matchHub, original); err != nil {
		t.Fatalf("CASRestoreEntryFromBytes: %v", err)
	}
	got, err := (&codexCLI{path: configPath}).readTOML()
	if err != nil {
		t.Fatal(err)
	}
	servers := got["mcp_servers"].(map[string]any)
	if !reflect.DeepEqual(servers["graphify"], map[string]any{"command": "graphify-mcp", "args": []any{"--stdio"}}) ||
		!reflect.DeepEqual(servers["sibling"], map[string]any{"command": "after"}) {
		t.Fatalf("CAS restored document = %#v, want original graphify plus later sibling", servers)
	}
}

// Catches project support accidentally changing the existing global constructor
// into a project-CAS-only adapter.
func TestNewCodexCLIGlobalRelocationStillAllowsExistingNoHashRequest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	configPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(configPath, []byte("[mcp_servers.serena]\ncommand = 'uvx'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewCodexCLI()
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.(CodexTransportClient).RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "serena", TargetEntryName: "serena",
		ExpectedSource: CodexTransportStdio,
		Entry:          MCPEntry{Name: "serena", URL: "http://127.0.0.1:9300/mcp"},
	})
	if err != nil || result.WriteTarget != "codex_global" || result.Action != "relocate" {
		t.Fatalf("global relocation result/error = %#v/%v, want existing codex_global relocate", result, err)
	}
}

// Catches a project target that becomes non-regular after construction being
// reclassified as a generic global collision or reaching the writer.
func TestCodexProjectRelocationRevalidatesFixedTargetBeforeWrite(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("[mcp_servers.graphify]\ncommand = 'graphify-mcp'\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	client, _, err := NewCodexCLIForProject(root, "graphify", immediateCodexProjectConfigLockRunnerFactory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(configPath, 0o700); err != nil {
		t.Fatal(err)
	}
	writes := 0
	_, err = client.(CodexTransportClient).RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "graphify", TargetEntryName: "graphify",
		ExpectedSource:       CodexTransportStdio,
		ExpectedConfigSHA256: fmt.Sprintf("%x", sha256.Sum256(original)),
		Entry:                MCPEntry{Name: "graphify", URL: "http://127.0.0.1:9300/mcp"},
		WriteConfig: func(string, []byte) error {
			writes++
			return nil
		},
	})
	if !errors.Is(err, ErrCodexProjectSourceRootInvalid) || writes != 0 {
		t.Fatalf("invalidated target err/writes = %v/%d, want CODEX_PROJECT_SOURCE_ROOT_INVALID/0", err, writes)
	}
}

// Catches project CAS restore/remove following a project .codex directory that
// was replaced by a link after construction, reading or mutating the foreign
// target before the adapter reports the invalid project binding.
func TestCodexProjectCASMutationsRevalidateFixedTargetBeforeReadOrWrite(t *testing.T) {
	for _, operation := range []string{"restore", "remove"} {
		t.Run(operation, func(t *testing.T) {
			projectRoot := t.TempDir()
			projectConfig := filepath.Join(projectRoot, ".codex", "config.toml")
			if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
				t.Fatal(err)
			}
			projectBefore := []byte("[mcp_servers.graphify]\ncommand = 'project-native'\n")
			if err := os.WriteFile(projectConfig, projectBefore, 0o600); err != nil {
				t.Fatal(err)
			}
			client, _, err := NewCodexCLIForProject(projectRoot, "graphify", immediateCodexProjectConfigLockRunnerFactory)
			if err != nil {
				t.Fatal(err)
			}

			foreignCodexDir := t.TempDir()
			foreignConfig := filepath.Join(foreignCodexDir, "config.toml")
			foreignBefore := []byte("[mcp_servers.graphify]\nurl = 'http://127.0.0.1:9300/mcp'\nstartup_timeout_sec = 10\n")
			if err := os.WriteFile(foreignConfig, foreignBefore, 0o600); err != nil {
				t.Fatal(err)
			}
			preservedProjectDir := filepath.Join(projectRoot, ".codex-before-swap")
			if err := os.Rename(filepath.Dir(projectConfig), preservedProjectDir); err != nil {
				t.Fatal(err)
			}
			linkPath := filepath.Join(projectRoot, ".codex")
			if runtime.GOOS == "windows" {
				createCodexJunctionForTest(t, linkPath, foreignCodexDir)
			} else {
				if err := os.Symlink(foreignCodexDir, linkPath); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Remove(linkPath); err != nil && !errors.Is(err, os.ErrNotExist) {
						t.Errorf("remove project config link: %v", err)
					}
				})
			}

			matcherCalls := 0
			matchHub := func(entry *MCPEntry) bool {
				matcherCalls++
				return entry != nil && entry.URL == "http://127.0.0.1:9300/mcp"
			}
			mutator, ok := AsCASEntryMutator(client)
			if !ok {
				t.Fatalf("project client type %T lacks CASEntryMutator", client)
			}
			switch operation {
			case "restore":
				err = mutator.CASRestoreEntryFromBytes("graphify", matchHub, projectBefore)
			case "remove":
				err = mutator.CASGuardedRemoveEntry("graphify", matchHub)
			}
			if !errors.Is(err, ErrCodexProjectSourceRootInvalid) {
				t.Errorf("%s error = %v, want CODEX_PROJECT_SOURCE_ROOT_INVALID", operation, err)
			}
			if matcherCalls != 0 {
				t.Errorf("%s matcher calls = %d, want 0 before foreign read projection", operation, matcherCalls)
			}
			foreignAfter, readErr := os.ReadFile(foreignConfig)
			if readErr != nil || !bytes.Equal(foreignAfter, foreignBefore) {
				t.Errorf("%s changed foreign config: read=%v before=%q after=%q", operation, readErr, foreignBefore, foreignAfter)
			}
			projectAfter, readErr := os.ReadFile(filepath.Join(preservedProjectDir, "config.toml"))
			if readErr != nil || !bytes.Equal(projectAfter, projectBefore) {
				t.Fatalf("%s changed preserved project config: read=%v before=%q after=%q", operation, readErr, projectBefore, projectAfter)
			}
		})
	}
}

// Catches the locked classifier bypassing the project-bound byte reader and
// projecting a linked foreign entry through the caller's ownership matcher.
func TestCodexProjectClassifierRevalidatesFixedTargetBeforeRead(t *testing.T) {
	projectRoot := t.TempDir()
	projectConfig := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	projectBefore := []byte("[mcp_servers.graphify]\ncommand = 'project-native'\n")
	if err := os.WriteFile(projectConfig, projectBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	client, _, err := NewCodexCLIForProject(projectRoot, "graphify", immediateCodexProjectConfigLockRunnerFactory)
	if err != nil {
		t.Fatal(err)
	}
	foreignCodexDir := t.TempDir()
	foreignConfig := filepath.Join(foreignCodexDir, "config.toml")
	foreignBefore := []byte("[mcp_servers.graphify]\nurl = 'http://127.0.0.1:9300/mcp'\nstartup_timeout_sec = 10\n")
	if err := os.WriteFile(foreignConfig, foreignBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Dir(projectConfig), filepath.Join(projectRoot, ".codex-before-swap")); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(projectRoot, ".codex")
	if runtime.GOOS == "windows" {
		createCodexJunctionForTest(t, linkPath, foreignCodexDir)
	} else {
		if err := os.Symlink(foreignCodexDir, linkPath); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Remove(linkPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove project config link: %v", err)
			}
		})
	}
	matcherCalls := 0
	matchHub := func(entry *MCPEntry) bool {
		matcherCalls++
		return entry != nil && entry.URL == "http://127.0.0.1:9300/mcp"
	}
	mutator, ok := AsCASEntryMutator(client)
	if !ok {
		t.Fatalf("project client type %T lacks CASEntryMutator", client)
	}
	verdict, err := mutator.ClassifyEntryUnderLock("graphify", matchHub, map[string]any{"command": "project-native"})
	if !errors.Is(err, ErrCodexProjectSourceRootInvalid) || verdict != ClassifyUnreadable {
		t.Errorf("classifier verdict/error = %d/%v, want unreadable/CODEX_PROJECT_SOURCE_ROOT_INVALID", verdict, err)
	}
	if matcherCalls != 0 {
		t.Errorf("classifier matcher calls = %d, want 0 before foreign projection", matcherCalls)
	}
	foreignAfter, readErr := os.ReadFile(foreignConfig)
	if readErr != nil || !bytes.Equal(foreignAfter, foreignBefore) {
		t.Fatalf("classifier changed foreign config: read=%v before=%q after=%q", readErr, foreignBefore, foreignAfter)
	}
}

func TestCodexProjectLegacyBackupFamilyRefusesBeforeLockOrIO(t *testing.T) {
	type legacyOperation struct {
		name string
		run  func(Client, string, *int) (string, error)
	}
	operations := []legacyOperation{
		{name: "backup", run: func(c Client, _ string, _ *int) (string, error) { return c.Backup() }},
		{name: "backup keep", run: func(c Client, _ string, _ *int) (string, error) { return c.BackupKeep(2) }},
		{name: "restore whole file", run: func(c Client, backup string, _ *int) (string, error) { return "", c.Restore(backup) }},
		{name: "latest backup path", run: func(c Client, _ string, _ *int) (string, error) {
			path, ok, err := c.LatestBackupPath()
			if path != "" || ok {
				return fmt.Sprintf("%s/%v", path, ok), err
			}
			return "", err
		}},
		{name: "backup contains entry", run: func(c Client, backup string, _ *int) (string, error) {
			has, err := c.BackupContainsEntry(backup, "graphify")
			if has {
				return "true", err
			}
			return "", err
		}},
		{name: "backup entry is hub managed", run: func(c Client, backup string, _ *int) (string, error) {
			managed, err := c.BackupEntryIsHubManaged(backup, "graphify")
			if managed {
				return "true", err
			}
			return "", err
		}},
		{name: "restore entry", run: func(c Client, backup string, _ *int) (string, error) {
			return "", c.RestoreEntryFromBackup(backup, "graphify")
		}},
		{name: "restore entry rollback", run: func(c Client, backup string, _ *int) (string, error) {
			return "", c.RestoreEntryFromBackupForRollback(backup, "graphify")
		}},
		{name: "restore entry rollback scoped writer", run: func(c Client, backup string, writerCalls *int) (string, error) {
			scoped, ok := c.(interface {
				RestoreEntryFromBackupForRollbackWithConfigWriter(string, string, WriteConfigFileFunc) error
			})
			if !ok {
				return "", fmt.Errorf("client %T lacks scoped rollback writer", c)
			}
			return "", scoped.RestoreEntryFromBackupForRollbackWithConfigWriter(backup, "graphify", func(string, []byte) error {
				*writerCalls++
				return nil
			})
		}},
	}
	for _, level := range []string{"wrapped", "concrete"} {
		for _, operation := range operations {
			t.Run(level+"/"+operation.name, func(t *testing.T) {
				root, configPath, original := writeCodexProjectFixture(t)
				foreignDir := t.TempDir()
				backupPath := filepath.Join(foreignDir, "candidate.toml")
				backupBytes := []byte("[mcp_servers.graphify]\nurl = 'http://127.0.0.1:9300/mcp'\n")
				if err := os.WriteFile(backupPath, backupBytes, 0o600); err != nil {
					t.Fatal(err)
				}
				runner := &recordingCodexProjectConfigLockRunner{}
				client, _, err := NewCodexCLIForProject(root, "graphify", func(CodexProjectIdentity) (CodexProjectConfigLockRunner, error) {
					return runner, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if level == "concrete" {
					client = client.(*lockingClient).Client
				}
				writerCalls := 0
				result, operationErr := operation.run(client, backupPath, &writerCalls)
				if result != "" || !errors.Is(operationErr, ErrCodexProjectLegacyBackupUnsupported) {
					t.Fatalf("result/error = %q/%v, want empty/CODEX_PROJECT_LEGACY_BACKUP_UNSUPPORTED", result, operationErr)
				}
				if writerCalls != 0 || runner.readCalls != 0 || runner.mutationCalls != 0 {
					t.Fatalf("calls = writer %d runner read %d mutation %d, want all zero", writerCalls, runner.readCalls, runner.mutationCalls)
				}
				got, readErr := os.ReadFile(configPath)
				if readErr != nil || !bytes.Equal(got, original) {
					t.Fatalf("project config changed: read=%v before=%q after=%q", readErr, original, got)
				}
				gotBackup, readErr := os.ReadFile(backupPath)
				if readErr != nil || !bytes.Equal(gotBackup, backupBytes) {
					t.Fatalf("foreign backup changed/read: read=%v before=%q after=%q", readErr, backupBytes, gotBackup)
				}
				projectEntries, readDirErr := os.ReadDir(filepath.Dir(configPath))
				if readDirErr != nil || len(projectEntries) != 1 || projectEntries[0].Name() != "config.toml" {
					t.Fatalf("project directory changed: read=%v entries=%v", readDirErr, projectEntries)
				}
			})
		}
	}
}

type codexSwappedProjectFixture struct {
	client                  Client
	projectBefore           []byte
	preservedProjectConfig  string
	foreignConfig           string
	foreignBefore           []byte
	foreignBackup           string
	foreignDirectoryEntries []string
}

func newCodexSwappedProjectFixture(t *testing.T) codexSwappedProjectFixture {
	t.Helper()
	projectRoot := t.TempDir()
	projectConfig := filepath.Join(projectRoot, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(projectConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	projectBefore := []byte("[mcp_servers.graphify]\ncommand = 'project-native'\n")
	if err := os.WriteFile(projectConfig, projectBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	client, _, err := NewCodexCLIForProject(projectRoot, "graphify", immediateCodexProjectConfigLockRunnerFactory)
	if err != nil {
		t.Fatal(err)
	}
	foreignCodexDir := t.TempDir()
	foreignConfig := filepath.Join(foreignCodexDir, "config.toml")
	foreignBefore := []byte("[mcp_servers.graphify]\nurl = 'http://127.0.0.1:9300/mcp'\nstartup_timeout_sec = 10\n")
	if err := os.WriteFile(foreignConfig, foreignBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	foreignBackup := foreignConfig + ".bak-mcp-local-hub-20260101-010101"
	if err := os.WriteFile(foreignBackup, projectBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	preservedProjectDir := filepath.Join(projectRoot, ".codex-before-swap")
	if err := os.Rename(filepath.Dir(projectConfig), preservedProjectDir); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(projectRoot, ".codex")
	if runtime.GOOS == "windows" {
		createCodexJunctionForTest(t, linkPath, foreignCodexDir)
	} else {
		if err := os.Symlink(foreignCodexDir, linkPath); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Remove(linkPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove project config link: %v", err)
			}
		})
	}
	entries, err := os.ReadDir(foreignCodexDir)
	if err != nil {
		t.Fatal(err)
	}
	entryNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		entryNames = append(entryNames, entry.Name())
	}
	return codexSwappedProjectFixture{
		client: client, projectBefore: projectBefore,
		preservedProjectConfig: filepath.Join(preservedProjectDir, "config.toml"),
		foreignConfig:          foreignConfig, foreignBefore: foreignBefore,
		foreignBackup: foreignBackup, foreignDirectoryEntries: entryNames,
	}
}

func assertCodexSwappedProjectFixtureUnchanged(t *testing.T, fixture codexSwappedProjectFixture) {
	t.Helper()
	foreignAfter, err := os.ReadFile(fixture.foreignConfig)
	if err != nil || !bytes.Equal(foreignAfter, fixture.foreignBefore) {
		t.Errorf("foreign config changed: read=%v before=%q after=%q", err, fixture.foreignBefore, foreignAfter)
	}
	projectAfter, err := os.ReadFile(fixture.preservedProjectConfig)
	if err != nil || !bytes.Equal(projectAfter, fixture.projectBefore) {
		t.Errorf("preserved project config changed: read=%v before=%q after=%q", err, fixture.projectBefore, projectAfter)
	}
	entries, err := os.ReadDir(filepath.Dir(fixture.foreignConfig))
	if err != nil {
		t.Errorf("read foreign directory: %v", err)
		return
	}
	entryNames := make([]string, 0, len(entries))
	for _, entry := range entries {
		entryNames = append(entryNames, entry.Name())
	}
	if !reflect.DeepEqual(entryNames, fixture.foreignDirectoryEntries) {
		t.Errorf("foreign directory entries = %q, want unchanged %q", entryNames, fixture.foreignDirectoryEntries)
	}
}

// Catches any remaining path-bound Client method following or mutating a
// post-construction project target escape instead of using the project byte
// owner or refusing before its external filesystem operation.
func TestCodexProjectBoundOperationsRejectSwappedTarget(t *testing.T) {
	operations := []struct {
		name    string
		wantErr error
		run     func(c Client, fixture codexSwappedProjectFixture) error
	}{
		{name: "get entry", wantErr: ErrCodexProjectSourceRootInvalid, run: func(c Client, _ codexSwappedProjectFixture) error { _, err := c.GetEntry("graphify"); return err }},
		{name: "add entry", wantErr: ErrCodexProjectSourceRootInvalid, run: func(c Client, _ codexSwappedProjectFixture) error {
			return c.AddEntry(MCPEntry{Name: "graphify", URL: "http://127.0.0.1:9300/mcp"})
		}},
		{name: "remove entry", wantErr: ErrCodexProjectSourceRootInvalid, run: func(c Client, _ codexSwappedProjectFixture) error { return c.RemoveEntry("graphify") }},
		{name: "all stdio", wantErr: ErrCodexProjectSourceRootInvalid, run: func(c Client, _ codexSwappedProjectFixture) error { _, err := c.AllStdioEntries(); return err }},
		{name: "find language server", wantErr: ErrCodexProjectSourceRootInvalid, run: func(c Client, _ codexSwappedProjectFixture) error {
			_, err := c.FindStdioLanguageServerEntries()
			return err
		}},
		{name: "init empty", wantErr: ErrCodexProjectSourceRootInvalid, run: func(c Client, _ codexSwappedProjectFixture) error { _, err := c.InitEmpty(); return err }},
		{name: "backup", wantErr: ErrCodexProjectLegacyBackupUnsupported, run: func(c Client, _ codexSwappedProjectFixture) error { _, err := c.Backup(); return err }},
		{name: "backup keep", wantErr: ErrCodexProjectLegacyBackupUnsupported, run: func(c Client, _ codexSwappedProjectFixture) error { _, err := c.BackupKeep(2); return err }},
		{name: "restore whole backup", wantErr: ErrCodexProjectLegacyBackupUnsupported, run: func(c Client, fixture codexSwappedProjectFixture) error { return c.Restore(fixture.foreignBackup) }},
		{name: "latest backup", wantErr: ErrCodexProjectLegacyBackupUnsupported, run: func(c Client, _ codexSwappedProjectFixture) error { _, _, err := c.LatestBackupPath(); return err }},
		{name: "backup contains", wantErr: ErrCodexProjectLegacyBackupUnsupported, run: func(c Client, fixture codexSwappedProjectFixture) error {
			_, err := c.BackupContainsEntry(fixture.foreignBackup, "graphify")
			return err
		}},
		{name: "backup hub managed", wantErr: ErrCodexProjectLegacyBackupUnsupported, run: func(c Client, fixture codexSwappedProjectFixture) error {
			_, err := c.BackupEntryIsHubManaged(fixture.foreignBackup, "graphify")
			return err
		}},
		{name: "restore entry", wantErr: ErrCodexProjectLegacyBackupUnsupported, run: func(c Client, fixture codexSwappedProjectFixture) error {
			return c.RestoreEntryFromBackup(fixture.foreignBackup, "graphify")
		}},
		{name: "restore entry rollback", wantErr: ErrCodexProjectLegacyBackupUnsupported, run: func(c Client, fixture codexSwappedProjectFixture) error {
			return c.RestoreEntryFromBackupForRollback(fixture.foreignBackup, "graphify")
		}},
	}
	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			fixture := newCodexSwappedProjectFixture(t)
			err := operation.run(fixture.client, fixture)
			if !errors.Is(err, operation.wantErr) {
				t.Errorf("operation error = %v, want %v", err, operation.wantErr)
			}
			assertCodexSwappedProjectFixtureUnchanged(t, fixture)
		})
	}
	t.Run("exists", func(t *testing.T) {
		fixture := newCodexSwappedProjectFixture(t)
		if fixture.client.Exists() {
			t.Error("Exists returned true for swapped project target")
		}
		assertCodexSwappedProjectFixtureUnchanged(t, fixture)
	})
}

// Pure bytes participants must remain independent of the live filesystem; the
// safe-read owner belongs to the caller that acquires these bytes.
func TestCodexProjectPureBytesParticipantsRemainPathIndependent(t *testing.T) {
	fixture := newCodexSwappedProjectFixture(t)
	checker := fixture.client.(EntryBytesChecker)
	present, err := checker.EntryPresentInBytes(fixture.projectBefore, "graphify")
	if err != nil || !present {
		t.Fatalf("EntryPresentInBytes = %v/%v, want true/nil", present, err)
	}
	mutator, ok := AsCASEntryMutator(fixture.client)
	if !ok {
		t.Fatalf("project client type %T lacks CASEntryMutator", fixture.client)
	}
	if _, present, err := mutator.EntryRawSubtree(fixture.projectBefore, "graphify"); err != nil || !present {
		t.Fatalf("EntryRawSubtree = present %v error %v, want true/nil", present, err)
	}
	if _, present, err := mutator.EntryPhysicalToken(fixture.projectBefore, "graphify"); err != nil || !present {
		t.Fatalf("EntryPhysicalToken = present %v error %v, want true/nil", present, err)
	}
	assertCodexSwappedProjectFixtureUnchanged(t, fixture)
}

func TestCodexProjectConfigSnapshotEntryParsesCapturedHTTPAndRejectsMixed(t *testing.T) {
	snapshot := CodexProjectConfigSnapshot{configBytes: []byte("[mcp_servers.graphify]\nurl = \"http://127.0.0.1:9137/mcp\"\nstartup_timeout_sec = 10\ntool_timeout_sec = 19\n")}
	entry, err := snapshot.Entry("graphify")
	if err != nil {
		t.Fatalf("Entry(HTTP): %v", err)
	}
	if entry == nil || entry.Name != "graphify" || entry.URL != "http://127.0.0.1:9137/mcp" || entry.ToolTimeoutSec != 19 || entry.Disabled {
		t.Fatalf("Entry(HTTP) = %#v", entry)
	}

	snapshot.configBytes = []byte("[mcp_servers.graphify]\ncommand = \"graphify\"\nurl = \"http://127.0.0.1:9137/mcp\"\n")
	if _, err := snapshot.Entry("graphify"); !errors.Is(err, ErrCodexLayerParseFailed) {
		t.Fatalf("Entry(mixed) error = %v, want CODEX_LAYER_PARSE_FAILED", err)
	}
}
