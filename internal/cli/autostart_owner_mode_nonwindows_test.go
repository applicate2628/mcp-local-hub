//go:build !windows

package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/autostart"
)

type posixAutostartBackend struct {
	enableCalls []autostart.Options
	statusCalls []autostart.Options
	status      autostart.State
}

func (b *posixAutostartBackend) Enable(opts autostart.Options) error {
	b.enableCalls = append(b.enableCalls, opts)
	return nil
}
func (b *posixAutostartBackend) Disable() error { return nil }
func (b *posixAutostartBackend) Status(opts autostart.Options) (autostart.State, error) {
	b.statusCalls = append(b.statusCalls, opts)
	return b.status, nil
}
func (b *posixAutostartBackend) StatusSnapshot(opts autostart.Options) (autostart.StatusSnapshot, error) {
	state, err := b.Status(opts)
	return autostart.StatusSnapshot{State: state}, err
}

func withPosixAutostartBackend(t *testing.T, backend *posixAutostartBackend) {
	t.Helper()
	previous := autostartBackendFactoryFn
	autostartBackendFactoryFn = func() (autostart.Backend, error) { return backend, nil }
	t.Cleanup(func() { autostartBackendFactoryFn = previous })
}

func assertNoOwnerPolicyState(t *testing.T, stateDir string) {
	t.Helper()
	for _, leaf := range []string{
		"supervisor-intent.json",
		"strict-mode-mutation-incomplete.json",
		"migration.lock",
		"--once.lock",
	} {
		if _, err := os.Stat(filepath.Join(stateDir, leaf)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("POSIX autostart created owner-policy state %s: %v", leaf, err)
		}
	}
}

func TestAutostartPOSIXPlainPathsRemainDirectAndStateFree(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)
	backend := &posixAutostartBackend{status: autostart.StateEnabledRunning}
	withPosixAutostartBackend(t, backend)

	enable := newAutostartCmd()
	enable.SetArgs([]string{"enable", "--strict-mode"})
	if err := enable.Execute(); err != nil {
		t.Fatalf("plain enable: %v", err)
	}
	if len(backend.enableCalls) != 1 || !backend.enableCalls[0].StrictMode || backend.enableCalls[0].OwnerMode != "" {
		t.Fatalf("POSIX direct enable options=%+v, want strict direct call without owner mode", backend.enableCalls)
	}

	status := newAutostartCmd()
	var out bytes.Buffer
	status.SetOut(&out)
	status.SetArgs([]string{"status"})
	if err := status.Execute(); err != nil {
		t.Fatalf("plain status: %v", err)
	}
	if got := out.String(); got != "enabled-running\n" {
		t.Fatalf("plain status output=%q", got)
	}
	if len(backend.statusCalls) != 1 || backend.statusCalls[0].StrictMode || backend.statusCalls[0].OwnerMode != "" {
		t.Fatalf("POSIX direct status options=%+v, want legacy direct defaults", backend.statusCalls)
	}
	assertNoOwnerPolicyState(t, stateDir)
}

func TestAutostartPOSIXRejectsOwnerModeBeforeBackendMutation(t *testing.T) {
	backend := &posixAutostartBackend{}
	withPosixAutostartBackend(t, backend)
	cmd := newAutostartCmd()
	cmd.SetArgs([]string{"enable", "--owner-mode", "supervise"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("owner-mode succeeded on POSIX")
	}
	if len(backend.enableCalls) != 0 {
		t.Fatalf("owner-mode mutated POSIX backend: %+v", backend.enableCalls)
	}
}
