//go:build windows

package clients

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/process"
)

type configuredFixtureReadRunner struct {
	reads            int
	failRelease      bool
	undefinedRelease bool
}

func (r *configuredFixtureReadRunner) RunRead(fn func() error) error { return fn() }
func (r *configuredFixtureReadRunner) RunMutation(func() error) error {
	return errors.New("fixture mutation forbidden")
}
func (r *configuredFixtureReadRunner) RunReadContext(ctx context.Context, fn func() error) (ProjectConfigLockExecution, error) {
	r.reads++
	if err := ctx.Err(); err != nil {
		return ProjectConfigLockExecution{PrimaryError: err}, err
	}
	err := fn()
	released := !r.failRelease
	observed := ProjectConfigLockExecution{BodyEntered: true, ReleaseDefined: true, ReleaseSucceeded: &released, PrimaryError: err}
	if r.failRelease {
		observed.CleanupError = errors.New("synthetic release refusal")
	}
	if r.undefinedRelease {
		observed.ReleaseDefined, observed.ReleaseSucceeded = false, nil
	}
	return observed, errors.Join(err, observed.CleanupError)
}

func configuredFixtureJWT(payload string) string {
	return "e30." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".synthetic"
}

func configuredSourceFixture(t *testing.T) (*lockingClient, codexConfiguredReadIO, *configuredFixtureReadRunner, string, string) {
	t.Helper()
	root, projectPath, _ := writeCodexProjectFixture(t)
	if err := os.WriteFile(projectPath, []byte("[mcp_servers.graphify]\nargs = []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	system := t.TempDir()
	t.Setenv("USERPROFILE", home)
	for _, key := range []string{"HOME", "CODEX_HOME"} {
		old, present := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(key, old)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	write := func(path string, raw []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	globalPath := filepath.Join(home, ".codex", "config.toml")
	write(globalPath, []byte(fmt.Sprintf("[projects.%q]\ntrust_level = 'trusted'\n[mcp_servers.graphify]\ncommand = 'synthetic-reader'\nenabled = false\n", root)))
	write(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/synthetic\n"))
	clock := time.Now().UTC()
	auth := map[string]any{"auth_mode": "chatgpt", "tokens": map[string]any{
		"id_token":     configuredFixtureJWT(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"plus"}}`),
		"access_token": configuredFixtureJWT(fmt.Sprintf(`{"exp":%d}`, clock.Add(20*time.Minute).Unix())), "refresh_token": "synthetic-never-used",
	}}
	authBytes, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(home, ".codex", "auth.json"), authBytes)
	packageRoot := filepath.Join(t.TempDir(), "node_modules", "@openai", "codex")
	nativeRoot := filepath.Join(packageRoot, "node_modules", "@openai", "codex-win32-x64")
	nativePath := filepath.Join(nativeRoot, "vendor", "x86_64-pc-windows-msvc", "bin", "codex.exe")
	write(filepath.Join(packageRoot, "package.json"), []byte(`{"name":"@openai/codex","version":"0.162.0","bin":{"codex":"bin/codex.js"},"optionalDependencies":{"@openai/codex-win32-x64":"npm:@openai/codex@0.162.0-win32-x64"}}`))
	write(filepath.Join(nativeRoot, "package.json"), []byte(`{"name":"@openai/codex","version":"0.162.0-win32-x64","os":["win32"],"cpu":["x64"]}`))
	write(filepath.Join(packageRoot, "bin", "codex.js"), []byte("// synthetic entrypoint; never executed\n"))
	write(nativePath, []byte("synthetic executable; never executed\n"))
	oldVerify := SecureVerifyParentDir
	SecureVerifyParentDir = func(ctx context.Context, path string) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		info, err := os.Lstat(path)
		return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0, err
	}
	t.Cleanup(func() { SecureVerifyParentDir = oldVerify })
	runner := &configuredFixtureReadRunner{}
	client, _, err := NewCodexCLIForProject(root, "graphify", func(CodexProjectIdentity) (CodexProjectConfigLockRunner, error) { return runner, nil })
	if err != nil {
		t.Fatal(err)
	}
	readIO := codexConfiguredReadIO{
		now:     time.Now,
		environ: func() []string { return []string{"USERPROFILE=" + home, "PATH=synthetic-unused"} },
		lookup: func(name string) (string, error) {
			if name != "codex" {
				return "", errors.New("unexpected fixture lookup")
			}
			return nativePath, nil
		},
		knownFolders: func() (string, string, error) { return home, system, nil },
		run: func(ctx context.Context, invocation process.StrictRunInvocation) (process.StrictRunResult, error) {
			if invocation.Command.Dir != root || len(invocation.Command.Args) < 5 || strings.Join(invocation.Command.Args[len(invocation.Command.Args)-4:], "|") != "mcp|get|graphify|--json" || len(invocation.Command.Env) != 2 || len(invocation.Input) != 0 || invocation.InputLimit != 1 || invocation.StdoutLimit != codexProviderInventoryStdoutLimit || invocation.StderrLimit != codexProviderInventoryStderrLimit {
				t.Fatal("native invocation escaped its admitted bound context")
			}
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > codexProviderInventoryTimeout {
				t.Fatal("native budget not inherited")
			}
			return process.StrictRunResult{Stdout: process.StrictRunCapture{Prefix: []byte(configuredStdioFixture), Bytes: len(configuredStdioFixture)}, SettledExit: &process.StrictRunSettledExit{}}, nil
		},
	}
	return client.(*lockingClient), readIO, runner, globalPath, system
}

func TestCodexConfiguredSourceIssuesAndReentersBoundFactory(t *testing.T) {
	for _, version := range []string{"0.162.0", "0.162.1"} {
		t.Run(version, func(t *testing.T) {
			client, readIO, runner, _, _ := configuredSourceFixture(t)
			configuredFixtureVersion(t, readIO, version)
			first, err := client.readConfiguredProjectProfile(context.Background(), "graphify", readIO)
			if err != nil || first == nil {
				t.Fatalf("positive issuer: %v", err)
			}
			second, err := client.readConfiguredProjectProfile(context.Background(), "graphify", readIO)
			if err != nil || first.fingerprint != second.fingerprint || runner.reads != 2 {
				t.Fatalf("fresh equivalent reentry: %v", err)
			}
			if !first.globalRead.bodyEntered || !first.globalRead.releaseDefined || !first.globalRead.releaseConfirmed || !first.projectRead.BodyEntered || !first.projectRead.ReleaseDefined || first.projectRead.ReleaseSucceeded == nil || !*first.projectRead.ReleaseSucceeded {
				t.Fatal("final original successful read observations lost")
			}
			if enabled, err := first.ConfiguredEnabled(); err != nil || enabled {
				t.Fatal("configured OFF became enabled")
			}
			encoded, err := json.Marshal(first)
			if err != nil || string(encoded) != "{}" {
				t.Fatal("private source values leaked through JSON")
			}
			var zero CodexConfiguredProjectProfile
			if _, err := zero.Fingerprint(); err == nil {
				t.Fatal("zero source supplied authority")
			}
			if err := client.VerifyConfiguredProjectProfile(context.Background(), &zero); err == nil {
				t.Fatal("foreign source supplied reentry authority")
			}
		})
	}
	t.Run("PATH-NODE-ROUTE-SINGLE-RESOLUTION", func(t *testing.T) {
		client, readIO, _, _, _ := configuredSourceFixture(t)
		shim, nodeA, nodeB := configuredNPMFixture(t, readIO)
		for _, stable := range []bool{true, false} {
			nodeLookups, launchesA, launchesB := 0, 0, 0
			readIO.lookup = func(name string) (string, error) {
				if name == "codex" {
					return shim, nil
				}
				if name != "node" {
					return "", errors.New("unexpected lookup")
				}
				nodeLookups++
				if !stable && nodeLookups == 2 {
					return nodeB, nil
				}
				return nodeA, nil
			}
			baseRun := readIO.run
			readIO.run = func(ctx context.Context, invocation process.StrictRunInvocation) (process.StrictRunResult, error) {
				if invocation.Command.Path == nodeB {
					launchesB++
				} else if invocation.Command.Path == nodeA {
					launchesA++
				} else {
					t.Fatal("unqualified interpreter")
				}
				return baseRun(ctx, invocation)
			}
			profile, err := client.readConfiguredProjectProfile(context.Background(), "graphify", readIO)
			if launchesA != 1 || launchesB != 0 || nodeLookups != 2 {
				t.Fatal("capture was re-resolved or unqualified B executed")
			}
			if stable && (err != nil || profile == nil) {
				t.Fatalf("stable npm route refused: %v", err)
			}
			if !stable && (!errors.Is(err, ErrProjectClientPlanStale) || profile != nil) {
				t.Fatal("postcapture route drift did not refuse")
			}
			readIO.run = baseRun
		}
	})
}

func configuredNPMFixture(t *testing.T, readIO codexConfiguredReadIO) (string, string, string) {
	t.Helper()
	native, err := readIO.lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	nativeRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(native))))
	mainRoot := filepath.Dir(filepath.Dir(filepath.Dir(nativeRoot)))
	shimRoot := filepath.Dir(filepath.Dir(filepath.Dir(mainRoot)))
	shim := filepath.Join(shimRoot, "codex.cmd")
	body := "@ECHO off\r\nSETLOCAL\r\nCALL :find_dp0\r\nIF EXIST \"%dp0%\\node.exe\" SET _prog=\"%dp0%\\node.exe\"\r\n\"%_prog%\" \"%dp0%\\node_modules\\@openai\\codex\\bin\\codex.js\" %*\r\n"
	if err := os.WriteFile(shim, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	nodes := t.TempDir()
	nodeA, nodeB := filepath.Join(nodes, "node-a.exe"), filepath.Join(nodes, "node-b.exe")
	for _, path := range []string{nodeA, nodeB} {
		if err := os.WriteFile(path, []byte("synthetic node; never executed"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return shim, nodeA, nodeB
}

func configuredFixtureVersion(t *testing.T, readIO codexConfiguredReadIO, version string) {
	t.Helper()
	native, err := readIO.lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	nativeRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(native))))
	mainRoot := filepath.Dir(filepath.Dir(filepath.Dir(nativeRoot)))
	main := fmt.Sprintf(`{"name":"@openai/codex","version":%q,"bin":{"codex":"bin/codex.js"},"optionalDependencies":{"@openai/codex-win32-x64":%q}}`, version, "npm:@openai/codex@"+version+"-win32-x64")
	platform := fmt.Sprintf(`{"name":"@openai/codex","version":%q,"os":["win32"],"cpu":["x64"]}`, version+"-win32-x64")
	if err := os.WriteFile(filepath.Join(mainRoot, "package.json"), []byte(main), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nativeRoot, "package.json"), []byte(platform), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexConfiguredSourceNativeRootFallback(t *testing.T) {
	client, readIO, _, _, _ := configuredSourceFixture(t)
	codex, _ := client.projectCodex()
	marker := filepath.Join(filepath.Dir(filepath.Dir(codex.path)), ".git")
	if err := os.Remove(filepath.Join(marker, "HEAD")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	profile, err := client.readConfiguredProjectProfile(context.Background(), "graphify", readIO)
	if err != nil || profile == nil {
		t.Fatalf("native no-marker cwd fallback refused: %v", err)
	}
}

func TestCodexConfiguredSourceRefusesChangedInputsAndIncompleteOutcomes(t *testing.T) {
	for _, name := range []string{"source drift", "extra layer", "auth clock", "release", "undefined release", "incomplete capture", "native failure", "invalid native DTO", "unsupported version", "native package mismatch", "global acquisition", "global release", "early logical", "original shim close"} {
		t.Run(name, func(t *testing.T) {
			client, readIO, runner, globalPath, system := configuredSourceFixture(t)
			originalRun := readIO.run
			calls := 0
			closeCalls := 0
			globalFailure := errors.New("synthetic original global read failure")
			closeFailure := errors.New("synthetic original shim close failure")
			readIO.run = func(ctx context.Context, invocation process.StrictRunInvocation) (process.StrictRunResult, error) {
				calls++
				result, err := originalRun(ctx, invocation)
				switch name {
				case "source drift":
					file, err := os.OpenFile(globalPath, os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					_, writeErr := file.WriteString("\n# changed during native read\n")
					if err := errors.Join(writeErr, file.Close()); err != nil {
						t.Fatal(err)
					}
				case "extra layer":
					path := filepath.Join(system, "OpenAI", "Codex", "requirements.toml")
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("synthetic=true\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				case "release":
					runner.failRelease = true
				case "undefined release":
					runner.undefinedRelease = true
				case "invalid native DTO":
					result.Stdout.Prefix, result.Stdout.Bytes = []byte("{}"), 2
				case "incomplete capture":
					result.Stdout.Truncated = true
				case "native failure":
					return result, errors.New("synthetic-private-token-secret")
				}
				return result, err
			}
			if name == "auth clock" {
				readIO.now = func() time.Time { return time.Now().Add(time.Hour) }
			}
			if name == "global acquisition" {
				priorVerify := SecureVerifyParentDir
				SecureVerifyParentDir = func(ctx context.Context, path string) (bool, error) {
					if filepath.Clean(path) == filepath.Dir(globalPath) {
						return false, globalFailure
					}
					return priorVerify(ctx, path)
				}
				t.Cleanup(func() { SecureVerifyParentDir = priorVerify })
			}
			if name == "global release" {
				priorUnlock := configFlockUnlockFn
				var held []*flock.Flock
				configFlockUnlockFn = func(lock *flock.Flock) error {
					if lock.Path() == globalPath+".lock" {
						held = append(held, lock)
						return globalFailure
					}
					return priorUnlock(lock)
				}
				t.Cleanup(func() {
					configFlockUnlockFn = priorUnlock
					settled := true
					for _, lock := range held {
						if err := lock.Unlock(); err != nil {
							settled = false
							t.Errorf("original fixture lock cleanup failed: %v", err)
						}
					}
					if settled {
						unconfirmedConfigLockReleasesMu.Lock()
						delete(unconfirmedConfigLockReleases, globalPath+".lock")
						unconfirmedConfigLockReleasesMu.Unlock()
					}
				})
			}
			if name == "original shim close" {
				shim, nodeA, _ := configuredNPMFixture(t, readIO)
				readIO.lookup = func(name string) (string, error) {
					if name == "codex" {
						return shim, nil
					}
					if name == "node" {
						return nodeA, nil
					}
					return "", errors.New("unexpected lookup")
				}
				readIO.shimClose = func(file *os.File) error { closeCalls++; return errors.Join(file.Close(), closeFailure) }
			}
			if name == "unsupported version" {
				configuredFixtureVersion(t, readIO, "0.163.0")
			}
			if name == "native package mismatch" {
				native, _ := readIO.lookup("codex")
				path := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(native)))), "package.json")
				if err := os.WriteFile(path, []byte(`{"name":"@openai/codex","version":"0.162.1-win32-x64","os":["win32"],"cpu":["x64"]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			logical := "graphify"
			if name == "early logical" {
				logical = "other"
			}
			profile, err := client.readConfiguredProjectProfile(context.Background(), logical, readIO)
			if err == nil || profile != nil || err.Error() != "PROJECT_CLIENT_SOURCE_PROFILE_UNAVAILABLE" || strings.Contains(err.Error(), "secret") {
				t.Fatal("unsupported input issued or disclosed a source")
			}
			if (name == "auth clock" || name == "unsupported version" || name == "native package mismatch" || name == "global acquisition" || name == "early logical" || name == "original shim close") && calls != 0 {
				t.Fatal("pre-admission refusal still executed native read")
			}
			var observation *codexConfiguredSourceFailure
			if !errors.As(err, &observation) {
				t.Fatal("original source observations absent")
			}
			if name == "early logical" || name == "global acquisition" {
				if observation.globalRead.bodyEntered || observation.globalRead.releaseDefined || observation.projectRead.BodyEntered || observation.projectRead.ReleaseDefined || observation.projectRead.ReleaseSucceeded != nil {
					t.Fatal("unacquired read manufactured settlement")
				}
			}
			if name == "global acquisition" && !errors.Is(observation.globalRead.primaryErr, globalFailure) {
				t.Fatal("original global acquisition cause lost")
			}
			if name == "invalid native DTO" || name == "global release" || name == "release" || name == "undefined release" {
				if observation.result.SettledExit == nil || !observation.globalRead.bodyEntered || !observation.globalRead.releaseDefined || !observation.projectRead.BodyEntered {
					t.Fatal("late failure lost original native/read results")
				}
			}
			if name == "invalid native DTO" && (!observation.globalRead.releaseConfirmed || observation.projectRead.ReleaseSucceeded == nil || !*observation.projectRead.ReleaseSucceeded || observation.globalRead.bodyErr == nil || observation.projectRead.PrimaryError == nil) {
				t.Fatal("DTO failure lost successful releases or primary read errors")
			}
			if name == "release" && (observation.projectRead.ReleaseSucceeded == nil || *observation.projectRead.ReleaseSucceeded || observation.projectRead.CleanupError == nil || !errors.Is(err, observation.projectRead.CleanupError)) {
				t.Fatal("original failed project release lost")
			}
			if name == "undefined release" && (observation.projectRead.ReleaseDefined || observation.projectRead.ReleaseSucceeded != nil) {
				t.Fatal("undefined project release rewritten")
			}
			if name == "global release" && (observation.globalRead.releaseConfirmed || !errors.Is(observation.globalRead.releaseErr, globalFailure) || !errors.Is(err, globalFailure)) {
				t.Fatal("original global release cause lost")
			}
			if name == "original shim close" && (closeCalls != 1 || !errors.Is(err, closeFailure)) {
				t.Fatal("original shim close result lost")
			}
			if name == "native failure" {
				var failure *codexConfiguredSourceFailure
				if !errors.As(err, &failure) || failure.result.SettledExit == nil || len(failure.result.Stdout.Prefix) != len(configuredStdioFixture) || !strings.Contains(failure.cause.Error(), "secret") {
					t.Fatal("original failure/capture erased")
				}
			}
		})
	}
	t.Run("NATIVE-JWT-LINEBREAK-NO-GETTER", func(t *testing.T) {
		for _, row := range []struct{ name, field, character string }{
			{"ID-CR", "id_token", "\r"}, {"ID-LF", "id_token", "\n"},
			{"ACCESS-CR-STALE", "access_token", "\r"}, {"ACCESS-LF-STALE", "access_token", "\n"},
		} {
			t.Run(row.name, func(t *testing.T) {
				client, readIO, _, globalPath, _ := configuredSourceFixture(t)
				path := filepath.Join(filepath.Dir(globalPath), "auth.json")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var auth map[string]any
				if err := json.Unmarshal(raw, &auth); err != nil {
					t.Fatal(err)
				}
				tokens := auth["tokens"].(map[string]any)
				token := tokens[row.field].(string)
				tokens[row.field] = strings.Replace(token, "e30.", "e30."+row.character, 1)
				auth["last_refresh"] = time.Now().Add(-9 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
				raw, err = json.Marshal(auth)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				getterCalls := 0
				readIO.run = func(context.Context, process.StrictRunInvocation) (process.StrictRunResult, error) {
					getterCalls++
					return process.StrictRunResult{}, errors.New("forbidden getter")
				}
				profile, err := client.readConfiguredProjectProfile(context.Background(), "graphify", readIO)
				if profile != nil || err == nil || getterCalls != 0 || err.Error() != "PROJECT_CLIENT_SOURCE_PROFILE_UNAVAILABLE" {
					t.Fatal("native-invalid JWT permitted getter or changed masked channel")
				}
			})
		}
	})
	t.Run("ORIGINAL-SHIM-CLOSE-PARSER-CAUSE", func(t *testing.T) {
		_, readIO, _, _, _ := configuredSourceFixture(t)
		shim, nodeA, _ := configuredNPMFixture(t, readIO)
		lookup := func(string) (string, error) { return nodeA, nil }
		for _, kind := range []codexNativeReadKind{codexNativeInventory, codexNativeMCPGet} {
			logical, tail := "", "plugin|list|--json"
			if kind == codexNativeMCPGet {
				logical, tail = "graphify", "mcp|get|graphify|--json"
			}
			command, err := resolveCodexProviderCommandWithLookup(shim, lookup, kind, logical)
			if err != nil || command == nil || strings.Join(command.Args[2:], "|") != tail {
				t.Fatalf("ordinary closed argv changed: %v", err)
			}
		}
		if err := os.WriteFile(shim, []byte("unsupported synthetic shim"), 0o600); err != nil {
			t.Fatal(err)
		}
		closeCause, closes := errors.New("synthetic original close cause"), 0
		command, err := resolveCodexProviderCommandWithLookup(shim, lookup, codexNativeMCPGet, "graphify", func(file *os.File) error { closes++; return errors.Join(file.Close(), closeCause) })
		if command != nil || err == nil || closes != 1 || !errors.Is(err, closeCause) || !strings.Contains(err.Error(), "unsupported codex cmd shim") {
			t.Fatal("parser and original Close causes were not both received")
		}
	})
}
