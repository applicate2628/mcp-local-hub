package clients

import (
	"fmt"
	"os"
	"path/filepath"
)

// ClientConfigSandboxEnv describes one process environment input used while a
// client adapter resolves its configuration path in a test binary. Every entry
// is either redirected below the caller-owned sandbox root or explicitly
// unset. Keep this descriptor beside the construction audit: the descriptor
// makes tests safe by construction, while the audit rejects any future adapter
// input that escapes it.
//
// It is test support, not production policy. Release binaries never call
// ApplyClientConfigSandboxEnvironment, so client adapter resolution remains
// unchanged outside tests.
type ClientConfigSandboxEnv struct {
	Key   string
	Value string
	Unset bool
}

// ClientConfigSandboxEnvironment is the sole inventory of adapter-path
// environment inputs. Relative paths are intentionally conventional only;
// safety comes from every redirected value being beneath root.
func ClientConfigSandboxEnvironment(root string) []ClientConfigSandboxEnv {
	redirect := func(key, relative string) ClientConfigSandboxEnv {
		value := root
		if relative != "" {
			value = filepath.Join(root, relative)
		}
		return ClientConfigSandboxEnv{Key: key, Value: value}
	}
	return []ClientConfigSandboxEnv{
		redirect("HOME", ""),
		redirect("USERPROFILE", ""),
		redirect("LOCALAPPDATA", filepath.Join("AppData", "Local")),
		redirect("APPDATA", filepath.Join("AppData", "Roaming")),
		redirect("XDG_CONFIG_HOME", ".config"),
		redirect("XDG_DATA_HOME", filepath.Join(".local", "share")),
		redirect("XDG_STATE_HOME", filepath.Join(".local", "state")),
		redirect("ProgramData", "ProgramData"),
		redirect("MIMOCODE_TEST_MANAGED_CONFIG_DIR", filepath.Join("ProgramData", "opencode")),
		{Key: "CODEX_HOME", Unset: true},
		{Key: "COPILOT_HOME", Unset: true},
		{Key: "KIMI_CODE_HOME", Unset: true},
		{Key: "MIMOCODE_HOME", Unset: true},
		{Key: "MIMOCODE_CONFIG", Unset: true},
		{Key: "MIMOCODE_CONFIG_DIR", Unset: true},
		{Key: "MIMOCODE_CONFIG_CONTENT", Unset: true},
	}
}

// PrepareClientConfigSandboxDirectories materializes the descriptor's base
// directories inside an existing caller-owned fixture root. The caller owns
// freshness, privacy and cleanup, including any partial preparation on error.
func PrepareClientConfigSandboxDirectories(root string) error {
	if root == "" || !filepath.IsAbs(root) {
		return fmt.Errorf("client config sandbox requires an existing absolute fixture root")
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("client config sandbox root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("client config sandbox root is not a directory")
	}
	normalRoot := normalizeSandboxPath(root)
	seen := make(map[string]bool)
	directories := make([]string, 0)
	// Validate the entire descriptor before any directory creation.
	for _, entry := range ClientConfigSandboxEnvironment(root) {
		if entry.Unset {
			continue
		}
		key := normalizeSandboxPath(entry.Value)
		if !filepath.IsAbs(entry.Value) || !withinSandboxRoot(key, normalRoot) {
			return fmt.Errorf("client config sandbox directory %s escapes its fixture root", entry.Key)
		}
		if !seen[key] {
			seen[key] = true
			directories = append(directories, entry.Value)
		}
	}
	for _, directory := range directories {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("prepare client config sandbox directory: %w", err)
		}
	}
	return nil
}

// ApplyClientConfigSandboxEnvironment applies the descriptor to this test
// process after namespace preparation and returns a restoration function. Callers own its lifetime; this
// function deliberately has no production call sites.
func ApplyClientConfigSandboxEnvironment(root string) (restore func(), err error) {
	if err := PrepareClientConfigSandboxDirectories(root); err != nil {
		return nil, err
	}
	type previous struct {
		value string
		set   bool
	}
	entries := ClientConfigSandboxEnvironment(root)
	prior := make([]previous, len(entries))
	for i, entry := range entries {
		prior[i].value, prior[i].set = os.LookupEnv(entry.Key)
		if entry.Unset {
			_ = os.Unsetenv(entry.Key)
		} else {
			_ = os.Setenv(entry.Key, entry.Value)
		}
	}
	return func() {
		for i := len(entries) - 1; i >= 0; i-- {
			if prior[i].set {
				_ = os.Setenv(entries[i].Key, prior[i].value)
			} else {
				_ = os.Unsetenv(entries[i].Key)
			}
		}
	}, nil
}
