package clients

import (
	"path/filepath"
	"testing"
)

// TestAllClientsAreLockWrapped guards against a future constructor (or an
// AllClients entry) returning an UNWRAPPED adapter: every production client
// mutation MUST go through withConfigLock. Regression guard for the qwen-cli
// constructor, which shipped unwrapped in the first cut of the config-lock
// decorator while the other 14 were wrapped (a missed mutation path that left
// ~/.qwen/settings.json exposed to the torn-write / last-writer-wins race the
// decorator exists to close).
func TestAllClientsAreLockWrapped(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("APPDATA", filepath.Join(tmp, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(tmp, "AppData", "Local"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, ".config"))

	all := AllClients()
	if len(all) == 0 {
		t.Fatal("AllClients() returned no clients under a temp HOME — env redirect insufficient")
	}
	// qwen-cli is the constructor that regressed; assert it is present so the
	// wrap check below is not vacuously satisfied by a dropped constructor.
	if _, ok := all["qwen-cli"]; !ok {
		t.Fatal("qwen-cli absent from AllClients() under temp HOME — cannot verify its lock wrap")
	}
	for name, c := range all {
		if _, ok := c.(*lockingClient); !ok {
			t.Errorf("AllClients()[%q] is %T, not *lockingClient — its mutating methods bypass withConfigLock (torn-write/LWW race)", name, c)
		}
	}
}
