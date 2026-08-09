package clients

import (
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
		{Key: "COPILOT_HOME", Unset: true},
		{Key: "KIMI_CODE_HOME", Unset: true},
		{Key: "MIMOCODE_HOME", Unset: true},
		{Key: "MIMOCODE_CONFIG", Unset: true},
		{Key: "MIMOCODE_CONFIG_DIR", Unset: true},
		{Key: "MIMOCODE_CONFIG_CONTENT", Unset: true},
	}
}

// ApplyClientConfigSandboxEnvironment applies the descriptor to this test
// process and returns a restoration function. Callers own its lifetime; this
// function deliberately has no production call sites.
func ApplyClientConfigSandboxEnvironment(root string) func() {
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
	}
}
