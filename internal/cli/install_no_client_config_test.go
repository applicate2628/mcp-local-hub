package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInstallNoClientConfigFlagContract(t *testing.T) {
	t.Run("flag is public", func(t *testing.T) {
		cmd := newInstallCmdReal()
		if cmd.Flags().Lookup("no-client-config") == nil {
			t.Fatal("install command does not expose --no-client-config")
		}
		if !strings.Contains(cmd.Long, "--no-client-config") {
			t.Fatal("install long help does not document --no-client-config")
		}
	})

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"requires server", []string{"--no-client-config"}, "--no-client-config requires --server"},
		{"clients conflict", []string{"--server", "demo", "--no-client-config", "--clients", "claude-code"}, "--no-client-config is mutually exclusive with --clients/--all-clients"},
		{"all-clients conflict", []string{"--server", "demo", "--no-client-config", "--all-clients"}, "--no-client-config is mutually exclusive with --clients/--all-clients"},
		{"all conflict", []string{"--server", "demo", "--no-client-config", "--all"}, "--no-client-config is mutually exclusive with --all"},
		{"reconcile conflict", []string{"--server", "demo", "--no-client-config", "--reconcile-hub-mode"}, "--no-client-config is mutually exclusive with --reconcile-hub-mode"},
		{"upgrade conflict", []string{"--no-client-config", "--upgrade"}, "--upgrade is mutually exclusive"},
	} {
		t.Run(tc.name+" before bootstrap", func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("USERPROFILE", home)
			t.Setenv("HOME", home)
			t.Setenv("PATH", "")

			cmd := newInstallCmdReal()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Execute(%v) error = %v, want substring %q", tc.args, err, tc.want)
			}
			if _, statErr := os.Stat(filepath.Join(home, ".local", "bin", mcphubShortName)); !os.IsNotExist(statErr) {
				t.Fatalf("invalid invocation reached bootstrap; canonical path stat error = %v", statErr)
			}
		})
	}
}

func TestInstallNoClientConfigCheckAllowsDaemonScope(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "Local"))
	t.Setenv("APPDATA", filepath.Join(home, "Roaming"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))

	manifestRoot := t.TempDir()
	serverDir := filepath.Join(manifestRoot, "demo")
	if err := os.MkdirAll(serverDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: go\ndaemons:\n  - name: selected\n    port: 51234\n"
	if err := os.WriteFile(filepath.Join(serverDir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)

	cmd := newInstallCmdReal()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--server", "demo", "--daemon", "selected", "--no-client-config", "--check"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("no-client check with daemon scope failed: %v\noutput:\n%s", err, out.String())
	}
}

func TestNoClientCheckAndDryRunNoMutation(t *testing.T) {
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("resolve go for hermetic manifest: %v", err)
	}
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "Local"))
	t.Setenv("APPDATA", filepath.Join(home, "Roaming"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("PATH", filepath.Dir(goPath))

	manifestRoot := t.TempDir()
	serverDir := filepath.Join(manifestRoot, "demo")
	if err := os.MkdirAll(serverDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "name: demo\nkind: global\ntransport: stdio-bridge\ncommand: go\ndaemons:\n  - name: selected\n    port: 61234\n"
	if err := os.WriteFile(filepath.Join(serverDir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)

	canonical := filepath.Join(home, ".local", "bin", mcphubShortName)
	if err := os.MkdirAll(filepath.Dir(canonical), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, []byte("canonical-sentinel\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	clientConfig := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(clientConfig, []byte("client-sentinel\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := cliFileSnapshot(t, home)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "check", args: []string{"--server", "demo", "--daemon", "selected", "--no-client-config", "--check"}},
		{name: "dry-run", args: []string{"--server", "demo", "--daemon", "selected", "--no-client-config", "--dry-run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newInstallCmdReal()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute(%v): %v\noutput:\n%s", tc.args, err, out.String())
			}
			if tc.name == "dry-run" && !strings.Contains(out.String(), "No changes made.") {
				t.Fatalf("dry-run did not render no-mutation marker:\n%s", out.String())
			}
			if tc.name == "dry-run" && (!strings.Contains(out.String(), "Client configs to update (0)") || !strings.Contains(out.String(), "mcp-local-hub-demo-selected")) {
				t.Fatalf("dry-run did not render zero clients plus selected daemon intent:\n%s", out.String())
			}
			if after := cliFileSnapshot(t, home); !reflect.DeepEqual(after, before) {
				t.Fatalf("%s changed home artifacts:\nbefore=%v\nafter=%v", tc.name, before, after)
			}
		})
	}

	if err := os.Remove(canonical); err != nil {
		t.Fatalf("remove canonical fixture for missing-prerequisite case: %v", err)
	}
	missingBefore := cliFileSnapshot(t, home)
	cmd := newInstallCmdReal()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--server", "demo", "--daemon", "selected", "--no-client-config", "--dry-run"})
	err = cmd.Execute()
	if err == nil || !strings.Contains(out.String(), "canonical mcphub binary not installed") {
		t.Fatalf("dry-run without canonical error = %v, want existing prerequisite failure\noutput:\n%s", err, out.String())
	}
	if after := cliFileSnapshot(t, home); !reflect.DeepEqual(after, missingBefore) {
		t.Fatalf("missing-canonical dry-run mutated home artifacts:\nbefore=%v\nafter=%v", missingBefore, after)
	}
}

func cliFileSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		got[rel] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return got
}
