//go:build windows && test_state_path_env

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// This fixture owns a private console in the test process. It never feeds the
// operator's inherited CONIN$; the real terminal predicate and prompt stay live.
func TestMigrateSerena_RealWriterConsentReceivingWindows(t *testing.T) {
	console := migrateSerenaPrivateConsole(t)
	for _, scenario := range []string{"yes", "no", "enter", "nonterminal", "nil", "strict", "swapped target"} {
		t.Run(scenario, func(t *testing.T) {
			stateDir, manifestDir := migrateSerenaTestEnv(t)
			t.Setenv(api.RequireSingleUserHomeEnv, "")
			t.Setenv(api.AllowClientConfigSymlinkEnv, "")
			t.Setenv("MCPHUB_ALLOW_UNHARDENED_CLIENT_WRITE", "")
			if api.InteractiveSymlinkConsent != nil {
				t.Fatal("consent port present before Execute")
			}
			previousStdin := os.Stdin
			os.Stdin = console
			t.Cleanup(func() { os.Stdin = previousStdin })
			if scenario == "nonterminal" {
				input, err := os.CreateTemp(t.TempDir(), "stdin")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = input.Close() })
				os.Stdin = input
			} else if scenario == "nil" {
				os.Stdin = nil
			} else {
				answer := "yes\nyes\n"
				if scenario == "no" {
					answer = "no\n"
				} else if scenario == "enter" {
					answer = "\n"
				}
				migrateSerenaFeedPrivateConsole(t, console, answer)
			}

			manifestPath := seedSerenaManifest(t, manifestDir, legacy2DaemonManifestYAML)
			workspace := t.TempDir()
			seedSerenaWorkspaceNoPort(t, workspace)
			regPath, err := api.DefaultRegistryPath()
			if err != nil {
				t.Fatal(err)
			}
			intentPath := filepath.Join(stateDir, "supervisor-intent.json")
			intent, err := json.Marshal(&api.SupervisorIntentFile{Version: 1, StrictMode: false,
				Daemons: []api.SupervisorDaemon{{Server: "serena", Daemon: "codex", Port: 9122}}})
			if err != nil {
				t.Fatal(err)
			}
			if err := api.WriteStateFileBytesAtomic(intentPath, intent); err != nil {
				t.Fatal(err)
			}
			ownedHome := os.Getenv("USERPROFILE")
			codexHome := filepath.Join(ownedHome, ".codex")
			if err := api.SecureCreateParentDirForConfigLock(codexHome); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CODEX_HOME", codexHome)
			codex, err := clients.NewCodexCLI()
			if err != nil {
				t.Fatal(err)
			}
			claude, err := clients.NewClaudeCode()
			if err != nil {
				t.Fatal(err)
			}
			for _, adapter := range []clients.Client{codex, claude} {
				rel, err := filepath.Rel(ownedHome, adapter.ConfigPath())
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					t.Fatal("adapter escaped owned fixture")
				}
			}
			claudePreimage := []byte("{\n  \"mcpServers\": {\n    \"serena\": {\n      \"type\": \"http\",\n      \"url\": \"http://127.0.0.1:9121/mcp\"\n    }\n  }\n}\n")
			if err := os.WriteFile(claude.ConfigPath(), claudePreimage, 0o600); err != nil {
				t.Fatal(err)
			}
			// Canonical adapter-produced preimage makes the nominated Codex byte
			// oracle independent of incidental TOML fixture formatting.
			if err := codex.AddEntry(clients.MCPEntry{Name: "serena", URL: "http://127.0.0.1:9122/mcp"}); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(ownedHome, "dotfiles", ".codex", "config.toml")
			if err := api.SecureCreateParentDirForConfigLock(filepath.Dir(target)); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(codex.ConfigPath(), target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, codex.ConfigPath()); err != nil {
				t.Fatalf("BLOCKED: owned Windows link privilege: %v", err)
			}
			_, pin, was := api.ResolveClientConfigSymlink(codex.ConfigPath())
			if !was || !strings.EqualFold(filepath.Clean(pin), filepath.Clean(target)) {
				t.Fatal("full target pin differs from owned target")
			}
			alternative := filepath.Join(filepath.Dir(target), "other.toml")
			alternativePreimage := []byte("# untouched alternate target\n")
			if err := os.WriteFile(alternative, alternativePreimage, 0o600); err != nil {
				t.Fatal(err)
			}
			if scenario == "yes" {
				// Real Claude writes fail on the owned read-only destination, leaving
				// Codex's approved mutation to be compensated before any cutover.
				if err := os.Chmod(claude.ConfigPath(), 0o400); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(claude.ConfigPath(), 0o600) })
			}
			read := func(path string) []byte {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return body
			}
			if scenario == "strict" {
				// The environment helper redirects the registry into the owned
				// home, separately from the already-hardened state-root override.
				// Prepare its parent and current YAML with the existing owners
				// before enabling strict mode; preserve every payload byte.
				registryPreimage := read(regPath)
				apitest.HardenedDir(t, filepath.Dir(regPath))
				if err := api.WriteStateFileBytesAtomic(regPath, registryPreimage); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(registryPreimage, read(regPath)) {
					t.Fatal("strict fixture preparation changed registry YAML")
				}
			}
			preimages := map[string][]byte{}
			for _, path := range []string{target, regPath, intentPath, manifestPath} {
				preimages[path] = read(path)
			}
			linkBefore := migrateSerenaOwnedReparseBytes(t, codex.ConfigPath())
			if scenario == "strict" {
				t.Setenv(api.RequireSingleUserHomeEnv, "1")
			}
			var reapCalls, installCalls, startCalls int
			defer stubSupervisorRunning(t, func() (bool, error) { return true, nil })()
			defer stubReap(t, func(context.Context, io.Writer) error { reapCalls++; return fmt.Errorf("forbidden receiving reap") })()
			defer stubStart(t, func(context.Context, io.Writer) error { startCalls++; return fmt.Errorf("forbidden receiving start") })()
			defer stubInstall(t, func(context.Context, *api.API, *config.ServerManifest, api.InstallParsedManifestOpts) (string, error) {
				installCalls++
				return "", fmt.Errorf("forbidden receiving install")
			})()
			var report *api.MigrateReport
			interceptMigrateSerenaReconcile(t, func(ctx context.Context, opts api.SerenaReconcileOpts) (*api.MigrateReport, error) {
				opts.ReadPidport = func(string) (int, int, error) { return 4242, 19425, nil }
				opts.VerifyIdentity = func(context.Context, int, int) error { return nil }
				opts.Ping = func(context.Context, int) error { return nil }
				opts.Clients = map[string]clients.Client{"claude-code": claude, "codex-cli": codex}
				opts.ClientsInclude = []string{"claude-code", "codex-cli"}
				opts.McphubExePath = filepath.Join(ownedHome, ".local", "bin", "mcphub.exe")
				var err error
				report, err = api.ReconcileSerenaClientsToRouter(ctx, opts)
				return report, err
			})
			originalWriter := clients.WriteConfigFile
			writerName := runtime.FuncForPC(reflect.ValueOf(originalWriter).Pointer()).Name()
			if !strings.HasSuffix(writerName, "internal/api.secureWriteWithOperatorOpt") {
				t.Fatalf("writer is not the real API owner: %s", writerName)
			}
			var codexWrites, consentCalls int
			var codexPortPresent bool
			var allocationObserved bool
			var firstCodexErr error
			clients.WriteConfigFile = func(path string, body []byte) error {
				if path != codex.ConfigPath() && path != claude.ConfigPath() {
					t.Fatalf("writer escaped owned configs: %q", path)
				}
				if path != codex.ConfigPath() {
					return originalWriter(path, body)
				}
				codexWrites++
				registry := api.NewRegistry(regPath)
				if err := registry.Load(); err != nil {
					t.Fatal(err)
				}
				row, found := registry.GetSerena(api.WorkspaceKey(workspace))
				if !found || row.Port == 0 {
					t.Fatal("registry allocation was not performed before the real writer")
				}
				allocationObserved = true
				port := api.InteractiveSymlinkConsent
				codexPortPresent = port != nil
				if port != nil {
					api.InteractiveSymlinkConsent = func(client, original, fullPin string) bool {
						consentCalls++
						if original != path || !strings.EqualFold(filepath.Clean(fullPin), filepath.Clean(target)) {
							t.Fatal("consent did not name the complete owned target")
						}
						approved := port(client, original, fullPin)
						if scenario == "swapped target" && approved {
							if err := os.Remove(path); err != nil {
								t.Fatal(err)
							}
							if err := os.Symlink(alternative, path); err != nil {
								t.Fatal(err)
							}
						}
						return approved
					}
					defer func() { api.InteractiveSymlinkConsent = port }()
				}
				err := originalWriter(path, body)
				if codexWrites == 1 {
					firstCodexErr = err
				}
				return err
			}
			t.Cleanup(func() { clients.WriteConfigFile = originalWriter })
			var output bytes.Buffer
			cmd := newMigrateCmdReal()
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs([]string{"serena", "legacy-to-dynamic-pool"})
			runErr := cmd.Execute()
			if runErr == nil || report == nil || len(report.Failed) == 0 {
				t.Fatalf("receiving did not fail before cutover: report=%+v err=%v", report, runErr)
			}
			if reapCalls != 0 || installCalls != 0 || startCalls != 0 {
				t.Fatalf("cutover calls reap/install/start=%d/%d/%d", reapCalls, installCalls, startCalls)
			}
			if api.InteractiveSymlinkConsent != nil {
				t.Fatal("RunE consent port leaked after rollback/return")
			}
			if !allocationObserved {
				t.Error("receiving did not exercise nondegenerate registry rollback")
			}
			wantPort := scenario != "nonterminal" && scenario != "nil"
			if codexPortPresent != wantPort {
				t.Errorf("Codex writer port=%t, want %t", codexPortPresent, wantPort)
			}
			if scenario == "yes" {
				if firstCodexErr != nil || codexWrites != 2 || consentCalls != 2 || len(report.Applied) != 1 || report.Applied[0].Client != "codex-cli" {
					t.Errorf("approved writer/rollback: first=%v writes=%d consent=%d report=%+v", firstCodexErr, codexWrites, consentCalls, report)
				}
			} else if firstCodexErr == nil {
				t.Error("Codex write succeeded without admissible consent")
			}
			if scenario == "strict" && consentCalls != 0 {
				t.Errorf("strict consulted consent %d times", consentCalls)
			}
			if scenario == "swapped target" && (firstCodexErr == nil || !strings.Contains(firstCodexErr.Error(), "does not match")) {
				t.Errorf("swapped target error=%v, want pin mismatch refusal", firstCodexErr)
			}
			if wantPort && scenario != "strict" && (!bytes.Contains(output.Bytes(), []byte("[y/N]")) || !strings.Contains(output.String(), pin)) {
				t.Error("terminal prompt omitted complete target pin")
			}
			if scenario == "strict" || !wantPort {
				if bytes.Contains(output.Bytes(), []byte("[y/N]")) {
					t.Error("nonterminal/nil/strict operation prompted")
				}
			}
			for path, before := range preimages {
				if after := read(path); !bytes.Equal(before, after) {
					t.Errorf("nominated preimage changed: %s", path)
				}
			}
			if !bytes.Equal(alternativePreimage, read(alternative)) {
				t.Error("alternate target changed")
			}
			if scenario != "swapped target" && !bytes.Equal(linkBefore, migrateSerenaOwnedReparseBytes(t, codex.ConfigPath())) {
				t.Error("owned link reparse bytes changed")
			}
			var beforeClaude, afterClaude any
			if json.Unmarshal(claudePreimage, &beforeClaude) != nil || json.Unmarshal(read(claude.ConfigPath()), &afterClaude) != nil || !reflect.DeepEqual(beforeClaude, afterClaude) {
				t.Error("Claude entry semantic rollback changed")
			}
			t.Logf("writer-port=%t codex-writes=%d consent-calls=%d first-error=%v; reap/install/start=0/0/0; nominated-preimages=equal; Claude-whole-byte-equal=%t (non-gate)", codexPortPresent, codexWrites, consentCalls, firstCodexErr, bytes.Equal(claudePreimage, read(claude.ConfigPath())))
		})
	}
}

func migrateSerenaPrivateConsole(t *testing.T) *os.File {
	t.Helper()
	if os.Stdin == nil || term.IsTerminal(int(os.Stdin.Fd())) {
		t.Fatal("BLOCKED: private console requires redirected original stdin")
	}
	previousStdin := os.Stdin
	var originalHandles [3]windows.Handle
	ids := []uint32{windows.STD_INPUT_HANDLE, windows.STD_OUTPUT_HANDLE, windows.STD_ERROR_HANDLE}
	for i, id := range ids {
		h, err := windows.GetStdHandle(id)
		if err != nil {
			t.Fatalf("BLOCKED: retain original standard handle: %v", err)
		}
		originalHandles[i] = h
	}
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	free := kernel.NewProc("FreeConsole")
	var console *os.File
	allocated := false
	t.Cleanup(func() {
		os.Stdin = previousStdin
		if console != nil {
			if err := console.Close(); err != nil {
				t.Errorf("close private input: %v", err)
			}
		}
		if allocated {
			if ok, _, err := free.Call(); ok == 0 {
				t.Errorf("release private test console: %v", err)
			}
		}
		for i, id := range ids {
			if err := windows.SetStdHandle(id, originalHandles[i]); err != nil {
				t.Errorf("restore original standard handle: %v", err)
			}
		}
	})
	if ok, _, err := free.Call(); ok == 0 && err != windows.ERROR_INVALID_HANDLE {
		t.Fatalf("BLOCKED: detach only this test process: %v", err)
	}
	if ok, _, err := kernel.NewProc("AllocConsole").Call(); ok == 0 {
		t.Fatalf("BLOCKED: allocate private test console: %v", err)
	}
	allocated = true
	window, _, _ := kernel.NewProc("GetConsoleWindow").Call()
	if window == 0 {
		t.Fatal("BLOCKED: private console has no owned window identity")
	}
	windows.NewLazySystemDLL("user32.dll").NewProc("ShowWindow").Call(window, 0)
	var err error
	console, err = os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil || !term.IsTerminal(int(console.Fd())) {
		t.Fatalf("BLOCKED: private input is not a real terminal: %v", err)
	}
	t.Log("private test-process console allocated, hidden; original pipe handles retained")
	return console
}

func migrateSerenaFeedPrivateConsole(t *testing.T, input *os.File, answer string) {
	t.Helper()
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	if ok, _, err := kernel.NewProc("FlushConsoleInputBuffer").Call(input.Fd()); ok == 0 {
		t.Fatalf("private input flush: %v", err)
	}
	// INPUT_RECORD/KEY_EVENT_RECORD layout is a Windows wire invariant.
	type inputRecord struct {
		kind, padding               uint16
		down                        int32
		repeat, virtual, scan, char uint16
		control                     uint32
	}
	if unsafe.Sizeof(inputRecord{}) != 20 {
		t.Fatal("Windows INPUT_RECORD layout mismatch")
	}
	var records []inputRecord
	for _, ch := range strings.ReplaceAll(answer, "\n", "\r") {
		record := inputRecord{kind: 1, down: 1, repeat: 1, char: uint16(ch)}
		if ch == '\r' {
			record.virtual = 13
		}
		records = append(records, record)
	}
	var written uint32
	if ok, _, err := kernel.NewProc("WriteConsoleInputW").Call(input.Fd(), uintptr(unsafe.Pointer(&records[0])), uintptr(len(records)), uintptr(unsafe.Pointer(&written))); ok == 0 || int(written) != len(records) {
		t.Fatalf("owned console input write: count=%d err=%v", written, err)
	}
}

func migrateSerenaOwnedReparseBytes(t *testing.T, path string) []byte {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := windows.CloseHandle(h); err != nil {
			t.Errorf("close owned reparse handle: %v", err)
		}
	}()
	buffer := make([]byte, 16*1024)
	var count uint32
	if err := windows.DeviceIoControl(h, windows.FSCTL_GET_REPARSE_POINT, nil, 0, &buffer[0], uint32(len(buffer)), &count, nil); err != nil {
		t.Fatal(err)
	}
	return buffer[:count]
}
