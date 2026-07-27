package clients

import (
	"errors"
	"os"
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
		if _, ok := c.(ConditionalEntryMutator); !ok {
			t.Errorf("AllClients()[%q] is %T and lacks ConditionalEntryMutator", name, c)
		}
	}
}

func TestMCPFrontV3_ConditionalMutationRejectsInterveningEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	concrete := &claudeCode{path: path}
	wrapped := newLockingClient(concrete)
	mutator, ok := wrapped.(ConditionalEntryMutator)
	if !ok {
		t.Fatalf("wrapped adapter %T lacks ConditionalEntryMutator", wrapped)
	}

	// The captured pre-state was absence. An operator entry appears after
	// capture but before the conditional invocation.
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"serena":{"url":"https://operator.example/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result := mutator.ConditionalEntryMutation(ConditionalEntryMutationRequest{
		EntryName:    "serena",
		ExpectedLive: func(live *MCPEntry) bool { return live == nil },
		Operation:    EntryMutationAdd,
		Entry:        MCPEntry{Name: "serena", URL: "http://127.0.0.1:9137/serena/mcp"},
	})
	if result.Invoked {
		t.Fatal("intervening operator edit still invoked AddEntry")
	}
	if !result.PreconditionConflict || !errors.Is(result.PreparationErr, ErrEntryMutationPreconditionConflict) {
		t.Fatalf("result=%+v, want exact precondition conflict", result)
	}
	live, err := concrete.GetEntry("serena")
	if err != nil {
		t.Fatal(err)
	}
	if live == nil || live.URL != "https://operator.example/mcp" {
		t.Fatalf("conditional refusal changed operator state: %+v", live)
	}
}

func TestMCPFrontV3_ConditionalPrepareFailureNeverInvokesAdapter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	concrete := &claudeCode{path: path}
	mutator := newLockingClient(concrete).(ConditionalEntryMutator)
	prepareErr := errors.New("induced durable prepare failure")
	result := mutator.ConditionalEntryMutation(ConditionalEntryMutationRequest{
		EntryName:    "serena",
		ExpectedLive: func(live *MCPEntry) bool { return live == nil },
		Operation:    EntryMutationAdd,
		Entry:        MCPEntry{Name: "serena", URL: "http://127.0.0.1:9137/serena/mcp"},
		BeforeMutation: func(EntryMutationPreparation) error {
			return prepareErr
		},
	})
	if result.Invoked {
		t.Fatal("prepare failure still invoked AddEntry")
	}
	if !errors.Is(result.PreparationErr, prepareErr) {
		t.Fatalf("PreparationErr=%v, want %v", result.PreparationErr, prepareErr)
	}
	live, err := concrete.GetEntry("serena")
	if err != nil {
		t.Fatal(err)
	}
	if live != nil {
		t.Fatalf("prepare failure wrote entry: %+v", live)
	}
}
