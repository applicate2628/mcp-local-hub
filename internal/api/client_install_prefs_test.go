package api

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/clients"
)

// tempPrefsPath returns a gui-preferences.yaml path inside a fresh temp dir.
func tempPrefsPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "gui-preferences.yaml")
}

func TestDefaultInstallClientNames_AbsentFileNoOverride(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	override, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		t.Fatalf("override read: %v", err)
	}
	if override != nil {
		t.Fatalf("override = %v, want nil for absent file", override)
	}
	eff, err := a.DefaultInstallClientNamesEffectiveIn(path)
	if err != nil {
		t.Fatalf("effective read: %v", err)
	}
	if want := clients.DefaultInstallClientNames(); !reflect.DeepEqual(eff, want) {
		t.Fatalf("effective = %v, want compile-time default %v", eff, want)
	}
}

func TestSetAndReadDefaultInstallClientNames_RoundTrip(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	// Pick a set that differs from the compile-time default set: add an opt-in,
	// drop one default. Every name must be supported.
	want := []string{"claude-code", "vscode"}
	if err := a.SetDefaultInstallClientNamesIn(path, want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("override = %v, want %v", got, want)
	}
	eff, err := a.DefaultInstallClientNamesEffectiveIn(path)
	if err != nil {
		t.Fatalf("effective: %v", err)
	}
	if !reflect.DeepEqual(eff, want) {
		t.Fatalf("effective = %v, want override %v", eff, want)
	}
}

func TestSetDefaultInstallClientNames_PreservesOtherKeys(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	// Seed an unrelated settings key via the normal settings writer.
	if err := a.SettingsSetIn(path, "appearance.theme", "dark"); err != nil {
		t.Fatalf("seed setting: %v", err)
	}
	if err := a.SetDefaultInstallClientNamesIn(path, []string{"cursor"}); err != nil {
		t.Fatalf("set clients: %v", err)
	}
	// The pre-existing setting must survive the override write.
	v, err := a.SettingsGetIn(path, "appearance.theme")
	if err != nil {
		t.Fatalf("get theme: %v", err)
	}
	if v != "dark" {
		t.Fatalf("appearance.theme = %q, want dark (override write clobbered it)", v)
	}
	got, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		t.Fatalf("read clients: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"cursor"}) {
		t.Fatalf("override = %v, want [cursor]", got)
	}
}

func TestSetDefaultInstallClientNamesReadsUnderStateFileFlock(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	if err := a.SettingsSetIn(path, "appearance.theme", "dark"); err != nil {
		t.Fatalf("seed setting: %v", err)
	}

	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatalf("lock settings state file: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- a.SetDefaultInstallClientNamesIn(path, []string{"vscode"})
	}()

	select {
	case err := <-done:
		_ = lock.Unlock()
		t.Fatalf("SetDefaultInstallClientNamesIn returned before the flock holder published the concurrent setting; err=%v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := WriteStateFileBytesLockHeld(path, []byte("appearance.theme: light\n")); err != nil {
		_ = lock.Unlock()
		t.Fatalf("publish concurrent setting under held flock: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("unlock settings state file: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("SetDefaultInstallClientNamesIn after flock release: %v", err)
	}
	raw, err := readRawSettingsMap(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if raw["appearance.theme"] != "light" {
		t.Fatalf("appearance.theme = %q, want light from concurrent writer", raw["appearance.theme"])
	}
	if raw[defaultInstallClientsKey] != "vscode" {
		t.Fatalf("%s = %q, want vscode", defaultInstallClientsKey, raw[defaultInstallClientsKey])
	}
}

func TestSetDefaultInstallClientNames_RejectsUnknownAndEmpty(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	if err := a.SetDefaultInstallClientNamesIn(path, []string{"not-a-client"}); err == nil {
		t.Fatal("expected error for unknown client, got nil")
	}
	if err := a.SetDefaultInstallClientNamesIn(path, []string{"   ", ""}); err == nil {
		t.Fatal("expected error for empty set, got nil")
	}
	// The rejected writes must not have created an override on disk.
	got, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		t.Fatalf("read after rejected writes: %v", err)
	}
	if got != nil {
		t.Fatalf("override = %v, want nil after rejected writes", got)
	}
}

func TestSetLSPRouterDisabledClients_RoundTripRejectsUnknownAndClears(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	if err := a.SettingsSetIn(path, "appearance.theme", "dark"); err != nil {
		t.Fatalf("seed setting: %v", err)
	}

	if err := a.SetLSPRouterDisabledClientsIn(path, []string{" codex-cli ", "antigravity", "codex-cli", ""}); err != nil {
		t.Fatalf("set disabled clients: %v", err)
	}
	raw, err := readRawSettingsMap(path)
	if err != nil {
		t.Fatalf("read raw settings: %v", err)
	}
	if raw["appearance.theme"] != "dark" {
		t.Fatalf("appearance.theme = %q, want preserved dark", raw["appearance.theme"])
	}
	if raw[lspRouterDisabledClientsKey] != "codex-cli,antigravity" {
		t.Fatalf("%s = %q, want codex-cli,antigravity", lspRouterDisabledClientsKey, raw[lspRouterDisabledClientsKey])
	}
	got, err := a.LSPRouterDisabledClientSetIn(path)
	if err != nil {
		t.Fatalf("read disabled set: %v", err)
	}
	if !got["codex-cli"] || !got["antigravity"] || len(got) != 2 {
		t.Fatalf("disabled set = %v, want codex-cli + antigravity only", got)
	}

	if err := a.SetLSPRouterDisabledClientsIn(path, []string{"not-a-client"}); err == nil {
		t.Fatal("expected unknown client error, got nil")
	}
	afterReject, err := readRawSettingsMap(path)
	if err != nil {
		t.Fatalf("read after reject: %v", err)
	}
	if afterReject[lspRouterDisabledClientsKey] != "codex-cli,antigravity" {
		t.Fatalf("disabled key changed after rejected write: %q", afterReject[lspRouterDisabledClientsKey])
	}

	if err := a.SetLSPRouterDisabledClientsIn(path, []string{" ", ""}); err != nil {
		t.Fatalf("clear disabled clients: %v", err)
	}
	cleared, err := a.LSPRouterDisabledClientSetIn(path)
	if err != nil {
		t.Fatalf("read cleared set: %v", err)
	}
	if len(cleared) != 0 {
		t.Fatalf("disabled set after clear = %v, want empty", cleared)
	}
	rawCleared, err := readRawSettingsMap(path)
	if err != nil {
		t.Fatalf("read raw after clear: %v", err)
	}
	if _, ok := rawCleared[lspRouterDisabledClientsKey]; ok {
		t.Fatalf("%s still present after clear: %q", lspRouterDisabledClientsKey, rawCleared[lspRouterDisabledClientsKey])
	}
}

func TestDisableLSPRouterClientConcurrentCallsPreserveBothOptOuts(t *testing.T) {
	a := NewAPI()
	t.Setenv("LOCALAPPDATA", t.TempDir())
	opts := LSPClientRouterOpts{
		GUIPort: 9125,
		Clients: map[string]clients.Client{
			"codex-cli":   nil,
			"antigravity": nil,
		},
	}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, name := range []string{"codex-cli", "antigravity"} {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.DisableLSPRouterClient(name, opts)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("DisableLSPRouterClient: %v", err)
		}
	}
	got, err := a.LSPRouterDisabledClientSet()
	if err != nil {
		t.Fatalf("read disabled set: %v", err)
	}
	if !got["codex-cli"] || !got["antigravity"] || len(got) != 2 {
		t.Fatalf("disabled set = %v, want codex-cli + antigravity", got)
	}
}

const lspRouterToggleZeroPortChildEnv = "MCPHUB_TEST_LSP_ROUTER_TOGGLE_ZERO_PORT_CHILD"

func TestLSPRouterClientToggleZeroGUIPortCompletes(t *testing.T) {
	if mode := os.Getenv(lspRouterToggleZeroPortChildEnv); mode != "" {
		runLSPRouterClientToggleZeroGUIPortChild(t, mode)
		return
	}

	for _, mode := range []string{"disable", "enable"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLSPRouterClientToggleZeroGUIPortCompletes$")
			cmd.Env = lspRouterToggleZeroPortChildEnvironment(os.Environ(), mode, runtime.GOOS == "windows")
			out, err := cmd.CombinedOutput()
			if ctx.Err() == context.DeadlineExceeded {
				t.Fatalf("%s with zero GUIPort timed out; possible settings self-deadlock\n%s", mode, out)
			}
			if err != nil {
				t.Fatalf("%s child failed: %v\n%s", mode, err, out)
			}
		})
	}
}

func lspRouterToggleZeroPortChildEnvironment(env []string, mode string, windowsEnvKeys bool) []string {
	childEnv := append([]string(nil), env...)
	goraceIndex := -1
	for index, entry := range childEnv {
		key, _, ok := strings.Cut(entry, "=")
		if ok && (key == "GORACE" || windowsEnvKeys && strings.ToLower(key) == "gorace") {
			goraceIndex = index
		}
	}
	if goraceIndex == -1 {
		childEnv = append(childEnv, "GORACE=atexit_sleep_ms=0")
	} else {
		key, value, _ := strings.Cut(childEnv[goraceIndex], "=")
		childEnv[goraceIndex] = key + "=" + lspRouterToggleZeroPortChildGORACE(value)
	}
	return append(childEnv, lspRouterToggleZeroPortChildEnv+"="+mode)
}

func lspRouterToggleZeroPortChildGORACE(options string) string {
	normalized := make([]string, 0, len(strings.Fields(options))+1)
	for _, option := range strings.Fields(options) {
		key, _, ok := strings.Cut(option, "=")
		if !ok || key != "atexit_sleep_ms" {
			normalized = append(normalized, option)
		}
	}
	return strings.Join(append(normalized, "atexit_sleep_ms=0"), " ")
}

func TestLSPRouterClientToggleZeroGUIPortChildEnvironment(t *testing.T) {
	processGORACE := os.Getenv("GORACE")
	for _, tc := range []struct {
		name           string
		windowsEnvKeys bool
		env            []string
		want           []string
	}{
		{
			name:           "replaces exit sleep and preserves other tokens",
			windowsEnvKeys: false,
			env: []string{
				"PATH=test-path",
				"GORACE=log_path=child-race-log exitcode=77 atexit_sleep_ms=2500 halt_on_error=1 history_size=7",
			},
			want: []string{
				"PATH=test-path",
				"GORACE=log_path=child-race-log exitcode=77 halt_on_error=1 history_size=7 atexit_sleep_ms=0",
				lspRouterToggleZeroPortChildEnv + "=disable",
			},
		},
		{
			name:           "normalizes duplicate exit sleep tokens",
			windowsEnvKeys: false,
			env:            []string{"GORACE=atexit_sleep_ms=2500 history_size=3 atexit_sleep_ms=100"},
			want: []string{
				"GORACE=history_size=3 atexit_sleep_ms=0",
				lspRouterToggleZeroPortChildEnv + "=disable",
			},
		},
		{
			name:           "sets exit sleep when GORACE is empty",
			windowsEnvKeys: false,
			env:            []string{"GORACE="},
			want: []string{
				"GORACE=atexit_sleep_ms=0",
				lspRouterToggleZeroPortChildEnv + "=disable",
			},
		},
		{
			name:           "sets exit sleep when GORACE is unset",
			windowsEnvKeys: false,
			env:            []string{"PATH=test-path"},
			want: []string{
				"PATH=test-path",
				"GORACE=atexit_sleep_ms=0",
				lspRouterToggleZeroPortChildEnv + "=disable",
			},
		},
		{
			name:           "windows lowercase GORACE preserves key spelling and index",
			windowsEnvKeys: true,
			env:            []string{"PATH=test-path", "gorace=history_size=7 atexit_sleep_ms=2500", "HOME=test-home"},
			want: []string{
				"PATH=test-path",
				"gorace=history_size=7 atexit_sleep_ms=0",
				"HOME=test-home",
				lspRouterToggleZeroPortChildEnv + "=disable",
			},
		},
		{
			name:           "windows mixed case GORACE preserves spelling",
			windowsEnvKeys: true,
			env:            []string{"GoRaCe=history_size=7 atexit_sleep_ms=2500"},
			want: []string{
				"GoRaCe=history_size=7 atexit_sleep_ms=0",
				lspRouterToggleZeroPortChildEnv + "=disable",
			},
		},
		{
			name:           "windows duplicates normalize only last matching assignment",
			windowsEnvKeys: true,
			env: []string{
				"GORACE=history_size=3 atexit_sleep_ms=2500",
				"PATH=test-path",
				"gOrAcE=log_path=effective halt_on_error=1 atexit_sleep_ms=100",
			},
			want: []string{
				"GORACE=history_size=3 atexit_sleep_ms=2500",
				"PATH=test-path",
				"gOrAcE=log_path=effective halt_on_error=1 atexit_sleep_ms=0",
				lspRouterToggleZeroPortChildEnv + "=disable",
			},
		},
		{
			name:           "POSIX duplicates normalize only last matching assignment",
			windowsEnvKeys: false,
			env: []string{
				"GORACE=history_size=3 atexit_sleep_ms=2500",
				"PATH=test-path",
				"GORACE=log_path=effective halt_on_error=1 atexit_sleep_ms=100",
			},
			want: []string{
				"GORACE=history_size=3 atexit_sleep_ms=2500",
				"PATH=test-path",
				"GORACE=log_path=effective halt_on_error=1 atexit_sleep_ms=0",
				lspRouterToggleZeroPortChildEnv + "=disable",
			},
		},
		{
			name:           "POSIX lowercase GORACE remains unrelated",
			windowsEnvKeys: false,
			env:            []string{"gorace=history_size=7 atexit_sleep_ms=2500"},
			want: []string{
				"gorace=history_size=7 atexit_sleep_ms=2500",
				"GORACE=atexit_sleep_ms=0",
				lspRouterToggleZeroPortChildEnv + "=disable",
			},
		},
		{
			name:           "POSIX selects exact uppercase GORACE only",
			windowsEnvKeys: false,
			env:            []string{"gorace=history_size=7 atexit_sleep_ms=2500", "GORACE=log_path=effective atexit_sleep_ms=100"},
			want: []string{
				"gorace=history_size=7 atexit_sleep_ms=2500",
				"GORACE=log_path=effective atexit_sleep_ms=0",
				lspRouterToggleZeroPortChildEnv + "=disable",
			},
		},
		{
			name:           "preserves malformed atexit sleep token",
			windowsEnvKeys: false,
			env:            []string{"GORACE=log_path=x atexit_sleep_ms history_size=7 atexit_sleep_ms=2500"},
			want: []string{
				"GORACE=log_path=x atexit_sleep_ms history_size=7 atexit_sleep_ms=0",
				lspRouterToggleZeroPortChildEnv + "=disable",
			},
		},
		{
			name:           "preserves unrelated token order",
			windowsEnvKeys: false,
			env:            []string{"GORACE=log_path=x exitcode=77 halt_on_error=1 history_size=7 unknown=value atexit_sleep_ms=2500"},
			want: []string{
				"GORACE=log_path=x exitcode=77 halt_on_error=1 history_size=7 unknown=value atexit_sleep_ms=0",
				lspRouterToggleZeroPortChildEnv + "=disable",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := append([]string(nil), tc.env...)
			if got := lspRouterToggleZeroPortChildEnvironment(input, "disable", tc.windowsEnvKeys); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("child environment = %q, want %q", got, tc.want)
			}
			if !reflect.DeepEqual(input, tc.env) {
				t.Fatalf("input environment mutated to %q, want %q", input, tc.env)
			}
			if got := os.Getenv("GORACE"); got != processGORACE {
				t.Fatalf("process GORACE mutated to %q, want %q", got, processGORACE)
			}
		})
	}
}

func TestLSPRouterToggleZeroPortChildPreservesMalformedGORACEDiagnostic(t *testing.T) {
	if !lspRouterToggleZeroPortRaceEnabled() {
		return
	}

	env := append([]string(nil), os.Environ()...)
	env = append(env, "GORACE=atexit_sleep_ms")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = lspRouterToggleZeroPortChildEnvironment(env, "race-diagnostic", runtime.GOOS == "windows")
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("race child timed out; expected malformed GORACE diagnostic\n%s", out)
	}
	if err == nil {
		t.Fatalf("race child succeeded with malformed GORACE; want non-zero exit\n%s", out)
	}
	if !strings.Contains(string(out), "expected '=' in GORACE") {
		t.Fatalf("race child output missing malformed GORACE diagnostic: %v\n%s", err, out)
	}
}

func lspRouterToggleZeroPortRaceEnabled() bool {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	for _, setting := range info.Settings {
		if setting.Key == "-race" {
			return setting.Value == "true"
		}
	}
	return false
}

func runLSPRouterClientToggleZeroGUIPortChild(t *testing.T, mode string) {
	a := NewAPI()
	t.Setenv("LOCALAPPDATA", t.TempDir())

	entryName := LSPRouterEntryName("go")
	client := newLSPRouterFakeClient(t, "codex-cli", true)
	opts := LSPClientRouterOpts{
		Languages:   []string{"go"},
		BackupKeepN: 1,
		Clients:     map[string]clients.Client{"codex-cli": client},
	}

	switch mode {
	case "disable":
		client.entries[entryName] = clients.MCPEntry{
			Name: entryName,
			URL:  LSPRouterURL(9125, "go"),
		}
		if _, err := a.DisableLSPRouterClient("codex-cli", opts); err != nil {
			t.Fatalf("DisableLSPRouterClient zero GUIPort: %v", err)
		}
		if got, err := client.GetEntry(entryName); err != nil {
			t.Fatalf("GetEntry after disable: %v", err)
		} else if got != nil {
			t.Fatalf("router entry after disable = %+v, want removed", got)
		}
	case "enable":
		if _, err := a.EnableLSPRouterClient("codex-cli", opts); err != nil {
			t.Fatalf("EnableLSPRouterClient zero GUIPort: %v", err)
		}
		if got, err := client.GetEntry(entryName); err != nil {
			t.Fatalf("GetEntry after enable: %v", err)
		} else if got == nil || got.URL != LSPRouterURL(9125, "go") {
			t.Fatalf("router entry after enable = %+v, want default-port router URL", got)
		}
	default:
		t.Fatalf("unknown child mode %q", mode)
	}
}

type enableConflictLSPRouterClient struct {
	*lspRouterFakeClient
	beforeGroupMutation func(clients.ConditionalEntryGroupMutationRequest)
	groupCalls          int
	groupObserved       clients.ConditionalEntryGroupMutationObserved
}

func (c *enableConflictLSPRouterClient) ConditionalEntryGroupMutation(req clients.ConditionalEntryGroupMutationRequest) clients.ConditionalEntryGroupMutationObserved {
	c.groupCalls++
	c.beforeGroupMutation(req)
	c.groupObserved = c.lspRouterFakeClient.ConditionalEntryGroupMutation(req)
	return c.groupObserved
}

func TestEnableLSPRouterClient_CanonicalChangeBeforeLegacyRemovalConflicts(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	seedLSPRouterManifest(t, []string{"go"})
	restoreRegistry := overrideLSPRouterRegistry(t)
	defer restoreRegistry()

	const clientName = "codex-cli"
	const legacyName = "mcp-language-server-go-legacy"
	const legacyURL = "http://127.0.0.1:9200/mcp"
	const operatorURL = "https://operator.example/lsp/go/mcp"
	canonicalName := LSPRouterEntryName("go")
	seedLegacyLSPWorkspace(t, clientName, legacyName)

	a := NewAPI()
	if err := a.SetLSPRouterDisabledClients([]string{clientName}); err != nil {
		t.Fatalf("seed persisted LSP-router opt-out: %v", err)
	}
	base := newLSPRouterFakeClient(t, clientName, true)
	base.entries[canonicalName] = clients.MCPEntry{Name: canonicalName, URL: LSPRouterURL(9137, "go")}
	base.entries[legacyName] = clients.MCPEntry{Name: legacyName, URL: legacyURL}
	operatorCanonical := clients.MCPEntry{Name: canonicalName, URL: operatorURL}
	client := &enableConflictLSPRouterClient{
		lspRouterFakeClient: base,
		beforeGroupMutation: func(req clients.ConditionalEntryGroupMutationRequest) {
			if req.EntryName == legacyName {
				base.entries[canonicalName] = operatorCanonical
			}
		},
	}

	report, err := a.EnableLSPRouterClient(clientName, LSPClientRouterOpts{
		GUIPort:     9137,
		Languages:   []string{"go"},
		BackupKeepN: 3,
		Clients:     map[string]clients.Client{clientName: client},
	})
	if err == nil {
		t.Fatal("EnableLSPRouterClient returned nil after canonical dependency conflict")
	}
	if report == nil || len(report.Removed) != 0 || base.removeCalls != 0 || len(base.backupPaths) != 0 {
		t.Fatalf("report=%+v removeCalls=%d backups=%v, want no legacy mutation or backup", report, base.removeCalls, base.backupPaths)
	}
	if len(report.Failed) != 1 || report.Failed[0].EntryName != legacyName || report.Failed[0].Op != "precondition" ||
		report.Failed[0].Err != ErrLSPRouterPlanPreconditionConflict.Error() {
		t.Fatalf("failed=%+v, want one legacy precondition conflict", report.Failed)
	}
	if client.groupCalls != 1 || client.groupObserved.Invoked || !client.groupObserved.PreconditionConflict ||
		client.groupObserved.ConflictScope != "dependency" || client.groupObserved.ConflictEntryName != canonicalName {
		t.Fatalf("group calls=%d observed=%+v, want one uninvoked canonical dependency conflict", client.groupCalls, client.groupObserved)
	}
	if got, exists := base.entries[canonicalName]; !exists || !reflect.DeepEqual(got, operatorCanonical) {
		t.Fatalf("operator canonical entry=%+v exists=%v, want unchanged injected entry=%+v", got, exists, operatorCanonical)
	}
	if got, exists := base.entries[legacyName]; !exists || got.URL != legacyURL {
		t.Fatalf("legacy entry=%+v exists=%v, want unchanged legacy URL %q", got, exists, legacyURL)
	}
}

type blockingRemoveLSPClient struct {
	*lspRouterFakeClient
	removeStarted sync.Once
	started       chan struct{}
	unblock       chan struct{}
}

func (c *blockingRemoveLSPClient) RemoveEntry(name string) error {
	c.removeStarted.Do(func() {
		close(c.started)
		<-c.unblock
	})
	return c.lspRouterFakeClient.RemoveEntry(name)
}

func TestLSPRouterClientToggleSerializesPreferenceAndConfigMutation(t *testing.T) {
	a := NewAPI()
	t.Setenv("LOCALAPPDATA", t.TempDir())

	base := newLSPRouterFakeClient(t, "codex-cli", true)
	entryName := LSPRouterEntryName("go")
	base.entries[entryName] = clients.MCPEntry{
		Name: entryName,
		URL:  LSPRouterURL(7777, "go"),
	}
	client := &blockingRemoveLSPClient{
		lspRouterFakeClient: base,
		started:             make(chan struct{}),
		unblock:             make(chan struct{}),
	}
	opts := LSPClientRouterOpts{
		GUIPort:     7777,
		Languages:   []string{"go"},
		BackupKeepN: 1,
		Clients:     map[string]clients.Client{"codex-cli": client},
	}

	disableDone := make(chan error, 1)
	go func() {
		_, err := a.DisableLSPRouterClient("codex-cli", opts)
		disableDone <- err
	}()

	select {
	case <-client.started:
	case <-time.After(2 * time.Second):
		t.Fatal("disable did not reach blocked RemoveEntry")
	}

	enableDone := make(chan error, 1)
	go func() {
		_, err := a.EnableLSPRouterClient("codex-cli", opts)
		enableDone <- err
	}()

	select {
	case err := <-enableDone:
		t.Fatalf("EnableLSPRouterClient completed before disable rollback released settings lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(client.unblock)
	if err := <-disableDone; err != nil {
		t.Fatalf("DisableLSPRouterClient: %v", err)
	}
	if err := <-enableDone; err != nil {
		t.Fatalf("EnableLSPRouterClient: %v", err)
	}

	disabled, err := a.LSPRouterDisabledClientSet()
	if err != nil {
		t.Fatalf("read disabled set: %v", err)
	}
	if disabled["codex-cli"] {
		t.Fatalf("codex-cli remains disabled after final enable: %v", disabled)
	}
	got, err := client.GetEntry(entryName)
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	if got == nil || got.URL != LSPRouterURL(7777, "go") {
		t.Fatalf("final router entry = %+v, want enabled router URL", got)
	}
}

func TestDefaultInstallClientNames_StaleUnknownNameDropped(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	// Hand-write a CSV containing an unknown name alongside a valid one,
	// simulating a stale gui-preferences.yaml from a build that knew a
	// client this build no longer understands.
	if err := WriteStateFileBytesAtomic(path, []byte("clients.default_install: cursor,ghost-client\n")); err != nil {
		t.Fatalf("write stale prefs: %v", err)
	}
	got, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"cursor"}) {
		t.Fatalf("override = %v, want [cursor] (unknown name should be dropped)", got)
	}
}

func TestDefaultInstallClientNames_AllUnknownFallsBackToCompileDefault(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)
	if err := WriteStateFileBytesAtomic(path, []byte("clients.default_install: ghost-a,ghost-b\n")); err != nil {
		t.Fatalf("write stale prefs: %v", err)
	}
	override, err := a.DefaultInstallClientNamesOverrideIn(path)
	if err != nil {
		t.Fatalf("override read: %v", err)
	}
	if override != nil {
		t.Fatalf("override = %v, want nil when no name survives sanitize", override)
	}
	eff, err := a.DefaultInstallClientNamesEffectiveIn(path)
	if err != nil {
		t.Fatalf("effective read: %v", err)
	}
	if want := clients.DefaultInstallClientNames(); !reflect.DeepEqual(eff, want) {
		t.Fatalf("effective = %v, want compile-time default %v", eff, want)
	}
}

func TestClientInstallToggleView_OverrideAndDefault(t *testing.T) {
	a := NewAPI()
	path := tempPrefsPath(t)

	// No override → compile-time default set selected, override inactive.
	snap, err := a.ClientInstallToggleViewIn(path)
	if err != nil {
		t.Fatalf("view (no override): %v", err)
	}
	if snap.OverrideActive {
		t.Fatal("OverrideActive = true, want false with no persisted override")
	}
	compileDefault := map[string]bool{}
	for _, n := range clients.DefaultInstallClientNames() {
		compileDefault[n] = true
	}
	for _, row := range snap.Rows {
		if row.Selected != compileDefault[row.Name] {
			t.Fatalf("row %q selected=%v, want %v (compile default)", row.Name, row.Selected, compileDefault[row.Name])
		}
		if row.CompileDefault != compileDefault[row.Name] {
			t.Fatalf("row %q compileDefault=%v, want %v", row.Name, row.CompileDefault, compileDefault[row.Name])
		}
	}
	if len(snap.Rows) != len(clients.SupportedClientNames()) {
		t.Fatalf("rows = %d, want %d (one per supported client)", len(snap.Rows), len(clients.SupportedClientNames()))
	}

	// Persist an override → those names selected, override active.
	if err := a.SetDefaultInstallClientNamesIn(path, []string{"vscode"}); err != nil {
		t.Fatalf("set override: %v", err)
	}
	snap2, err := a.ClientInstallToggleViewIn(path)
	if err != nil {
		t.Fatalf("view (override): %v", err)
	}
	if !snap2.OverrideActive {
		t.Fatal("OverrideActive = false, want true after persisting override")
	}
	for _, row := range snap2.Rows {
		wantSel := row.Name == "vscode"
		if row.Selected != wantSel {
			t.Fatalf("row %q selected=%v, want %v (override is [vscode])", row.Name, row.Selected, wantSel)
		}
	}
}
