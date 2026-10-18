package api

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

// Catches production composition falling back to the adjacent project path
// lock, or acquiring the state lock outside the project mutation critical
// section. The observing runner wraps the real API owner; it never replaces it.
func TestProjectCodexAdapterProductionOwnerRejectsSwappedTargetWithoutForeignMutation(t *testing.T) {
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowUnhardenedClientWriteEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "")

	type operation struct {
		name string
		run  func(*projectCodexCompositionFixture, *int, *int) error
	}
	operations := []operation{
		{name: "relocate", run: func(f *projectCodexCompositionFixture, _ *int, writerCalls *int) error {
			_, err := f.codex.RelocateHTTPEntry(clients.CodexHTTPRelocation{
				SourceEntryName: "graphify", TargetEntryName: "graphify",
				ExpectedSource: clients.CodexTransportStdio, ExpectedConfigSHA256: f.expectedSHA,
				Entry:       clients.MCPEntry{Name: "graphify", URL: "http://127.0.0.1:9300/mcp"},
				WriteConfig: func(string, []byte) error { (*writerCalls)++; return nil },
			})
			return err
		}},
		{name: "CAS restore", run: func(f *projectCodexCompositionFixture, matcherCalls, _ *int) error {
			return f.cas.CASRestoreEntryFromBytes("graphify", func(entry *clients.MCPEntry) bool {
				(*matcherCalls)++
				return entry != nil && entry.URL == "http://127.0.0.1:9300/mcp"
			}, f.projectBefore)
		}},
		{name: "CAS remove", run: func(f *projectCodexCompositionFixture, matcherCalls, _ *int) error {
			return f.cas.CASGuardedRemoveEntry("graphify", func(entry *clients.MCPEntry) bool {
				(*matcherCalls)++
				return entry != nil && entry.URL == "http://127.0.0.1:9300/mcp"
			})
		}},
	}

	for _, window := range []string{"preexisting target swap", "swap under state lock before adapter body"} {
		for _, op := range operations {
			t.Run(window+"/"+op.name, func(t *testing.T) {
				fixture := newProjectCodexCompositionFixture(t)
				swapCalls := 0
				var swapErr error
				if window == "preexisting target swap" {
					swapCalls++
					swapErr = fixture.swapProjectTarget(t)
				} else {
					fixture.runner.beforeMutation = func() error {
						swapCalls++
						return fixture.swapProjectTarget(t)
					}
				}
				if swapErr != nil {
					t.Skipf("UNVERIFIED: cannot create symlink/junction fixture: %v", swapErr)
				}
				matcherCalls := 0
				writerCalls := 0
				opErr := op.run(fixture, &matcherCalls, &writerCalls)
				if fixture.runner.beforeMutationErr != nil {
					t.Skipf("UNVERIFIED: cannot create symlink/junction fixture: %v", fixture.runner.beforeMutationErr)
				}
				if opErr == nil || strings.TrimSpace(opErr.Error()) == "" {
					t.Fatalf("operation error=%v, want nonempty project target refusal", opErr)
				}
				if swapCalls != 1 || fixture.runner.mutationCalls != 1 {
					t.Fatalf("swap/real-owner body calls=%d/%d, want 1/1", swapCalls, fixture.runner.mutationCalls)
				}
				if matcherCalls != 0 || writerCalls != 0 {
					t.Fatalf("foreign adapter body reached: matcher=%d writer=%d", matcherCalls, writerCalls)
				}
				fixture.assertForeignExact(t, window+"/"+op.name)
				fixture.assertOwnerReleased(t)
			})
		}
	}
}

type observingProjectCodexRunner struct {
	inner             clients.CodexProjectConfigLockRunner
	beforeMutation    func() error
	beforeMutationErr error
	readCalls         int
	mutationCalls     int
}

func (r *observingProjectCodexRunner) RunRead(fn func() error) error {
	r.readCalls++
	return r.inner.RunRead(fn)
}

func (r *observingProjectCodexRunner) RunMutation(fn func() error) error {
	return r.inner.RunMutation(func() error {
		r.mutationCalls++
		if r.beforeMutation != nil {
			r.beforeMutationErr = r.beforeMutation()
			if r.beforeMutationErr != nil {
				return r.beforeMutationErr
			}
		}
		return fn()
	})
}

type projectCodexCompositionFixture struct {
	projectCodexDir string
	projectConfig   string
	projectBefore   []byte
	foreignDir      string
	foreignConfig   string
	foreignBefore   []byte
	expectedSHA     string
	codex           clients.CodexTransportClient
	cas             clients.CASEntryMutator
	runner          *observingProjectCodexRunner
	owner           *CodexProjectConfigMutationOwner
}

func newProjectCodexCompositionFixture(t *testing.T) *projectCodexCompositionFixture {
	t.Helper()
	fixture := &projectCodexCompositionFixture{}
	projectRoot := hardenedTempDir(t)
	fixture.projectCodexDir = filepath.Join(projectRoot, ".codex")
	fixture.projectConfig = filepath.Join(fixture.projectCodexDir, "config.toml")
	if err := os.Mkdir(fixture.projectCodexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.projectBefore = []byte("[mcp_servers.graphify]\ncommand = 'graphify-mcp'\n")
	if err := os.WriteFile(fixture.projectConfig, fixture.projectBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := hardenedTempDir(t)
	previousStateRoot := daemonStateRootOverride
	daemonStateRootOverride = stateDir
	t.Cleanup(func() { daemonStateRootOverride = previousStateRoot })
	client, _, err := clients.NewCodexCLIForProject(projectRoot, "graphify", func(identity clients.CodexProjectIdentity) (clients.CodexProjectConfigLockRunner, error) {
		runner, ownerErr := NewCodexProjectConfigMutationOwner(identity)
		if ownerErr != nil {
			return nil, ownerErr
		}
		fixture.owner = runner.(*CodexProjectConfigMutationOwner)
		fixture.runner = &observingProjectCodexRunner{inner: runner}
		return fixture.runner, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.codex, _ = clients.AsCodexTransportClient(client)
	fixture.cas, _ = clients.AsCASEntryMutator(client)
	if fixture.codex == nil || fixture.cas == nil {
		t.Fatalf("project client type %T lacks relocation or CAS capability", client)
	}
	fixture.expectedSHA = fmt.Sprintf("%x", sha256.Sum256(fixture.projectBefore))

	fixture.foreignDir = hardenedTempDir(t)
	fixture.foreignConfig = filepath.Join(fixture.foreignDir, "config.toml")
	fixture.foreignBefore = []byte("title = 'foreign'\n\n[mcp_servers.graphify]\nurl = 'http://127.0.0.1:9300/mcp'\n\n[mcp_servers.sibling]\ncommand = 'foreign-sibling'\n")
	if err := os.WriteFile(fixture.foreignConfig, fixture.foreignBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.assertForeignExact(t, "before swap")
	return fixture
}

func (f *projectCodexCompositionFixture) assertOwnerReleased(t *testing.T) {
	t.Helper()
	release, err := lockLeafLedgered(f.owner.lockPath)
	if err != nil {
		t.Fatalf("reacquire project state lock after operation: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release project state lock after reacquire: %v", err)
	}
}

func (f *projectCodexCompositionFixture) swapProjectTarget(t *testing.T) error {
	t.Helper()
	if err := os.Remove(f.projectConfig); err != nil {
		return err
	}
	if err := os.Remove(f.projectCodexDir); err != nil {
		return err
	}
	if err := createProjectCodexCompositionLink(f.projectCodexDir, f.foreignDir); err != nil {
		return err
	}
	t.Cleanup(func() {
		if err := os.Remove(f.projectCodexDir); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove project symlink/junction: %v", err)
		}
	})
	return nil
}

func (f *projectCodexCompositionFixture) assertForeignExact(t *testing.T, stage string) {
	t.Helper()
	got, readErr := os.ReadFile(f.foreignConfig)
	if readErr != nil || !bytes.Equal(got, f.foreignBefore) {
		t.Fatalf("%s foreign config changed: read=%v before=%q after=%q", stage, readErr, f.foreignBefore, got)
	}
	entries, readDirErr := os.ReadDir(f.foreignDir)
	if readDirErr != nil {
		t.Fatalf("%s read foreign directory: %v", stage, readDirErr)
	}
	if len(entries) != 1 || entries[0].Name() != "config.toml" || !entries[0].Type().IsRegular() {
		t.Fatalf("%s foreign directory changed (lock/backup/prune residue included): %+v", stage, entries)
	}
}

func createProjectCodexCompositionLink(link, target string) error {
	if runtime.GOOS == "windows" {
		output, err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mklink /J: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	return os.Symlink(target, link)
}
