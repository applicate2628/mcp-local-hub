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
		if _, ok := c.(ConditionalEntryGroupMutator); !ok {
			t.Errorf("AllClients()[%q] is %T and lacks ConditionalEntryGroupMutator", name, c)
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

func TestConditionalEntryGroupMutation_DependencyConflictInvokesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	const legacyName = "mcp-language-server-go-legacy"
	const canonicalName = "mcp-language-server-go"
	legacyURL := "http://127.0.0.1:9200/mcp"
	operatorURL := "https://operator.example/mcp"
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"`+legacyName+`":{"url":"`+legacyURL+`"},"`+canonicalName+`":{"url":"`+operatorURL+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	concrete := &claudeCode{path: path}
	mutator, ok := newLockingClient(concrete).(ConditionalEntryGroupMutator)
	if !ok {
		t.Fatalf("wrapped adapter lacks ConditionalEntryGroupMutator")
	}
	keepN := 3
	result := mutator.ConditionalEntryGroupMutation(ConditionalEntryGroupMutationRequest{
		ConditionalEntryMutationRequest: ConditionalEntryMutationRequest{
			EntryName: legacyName,
			ExpectedLive: func(live *MCPEntry) bool {
				return live != nil && live.URL == legacyURL
			},
			BackupKeepN: &keepN,
			Operation:   EntryMutationRemove,
		},
		Dependencies: []EntryMutationDependency{{
			EntryName: canonicalName,
			ExpectedLive: func(live *MCPEntry) bool {
				return live != nil && live.URL == "http://127.0.0.1:9137/mcp"
			},
		}},
	})
	if result.Invoked || result.BackupPath != "" {
		t.Fatalf("dependency conflict invoked or backed up: %+v", result)
	}
	if !result.PreconditionConflict || result.ConflictScope != "dependency" || result.ConflictEntryName != canonicalName ||
		!errors.Is(result.PreparationErr, ErrEntryMutationPreconditionConflict) {
		t.Fatalf("result=%+v, want named dependency precondition conflict", result)
	}
	if len(result.Dependencies) != 1 || result.Dependencies[0].Before == nil || result.Dependencies[0].Before.URL != operatorURL {
		t.Fatalf("dependency observations=%+v, want operator canonical state", result.Dependencies)
	}
	legacy, err := concrete.GetEntry(legacyName)
	if err != nil {
		t.Fatal(err)
	}
	if legacy == nil || legacy.URL != legacyURL {
		t.Fatalf("dependency refusal changed legacy route: %+v", legacy)
	}
}

type dependencyReadErrorClaudeClient struct {
	*claudeCode
	failingEntry string
	failingErr   error
	backupCalls  int
	removeCalls  int
}

func (c *dependencyReadErrorClaudeClient) GetEntry(name string) (*MCPEntry, error) {
	if name == c.failingEntry {
		return nil, c.failingErr
	}
	return c.claudeCode.GetEntry(name)
}

func (c *dependencyReadErrorClaudeClient) BackupKeep(keepN int) (string, error) {
	c.backupCalls++
	return c.claudeCode.BackupKeep(keepN)
}

func (c *dependencyReadErrorClaudeClient) RemoveEntry(name string) error {
	c.removeCalls++
	return c.claudeCode.RemoveEntry(name)
}

func TestConditionalEntryGroupMutation_DependencyBeforeReadFailureInvokesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	const targetName = "mcp-language-server-go-legacy"
	const firstDependencyName = "mcp-language-server-go"
	const failingDependencyName = "mcp-language-server-go-older"
	const targetURL = "http://127.0.0.1:9200/mcp"
	const firstDependencyURL = "http://127.0.0.1:9137/mcp"
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"`+targetName+`":{"url":"`+targetURL+`"},"`+firstDependencyName+`":{"url":"`+firstDependencyURL+`"},"`+failingDependencyName+`":{"url":"http://127.0.0.1:9201/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	readErr := errors.New("induced dependency before-read failure")
	concrete := &dependencyReadErrorClaudeClient{
		claudeCode:   &claudeCode{path: path},
		failingEntry: failingDependencyName,
		failingErr:   readErr,
	}
	mutator, ok := newLockingClient(concrete).(ConditionalEntryGroupMutator)
	if !ok {
		t.Fatalf("wrapped adapter lacks ConditionalEntryGroupMutator")
	}
	keepN := 3
	prepareCalls := 0
	result := mutator.ConditionalEntryGroupMutation(ConditionalEntryGroupMutationRequest{
		ConditionalEntryMutationRequest: ConditionalEntryMutationRequest{
			EntryName: targetName,
			ExpectedLive: func(live *MCPEntry) bool {
				return live != nil && live.URL == targetURL
			},
			BackupKeepN: &keepN,
			Operation:   EntryMutationRemove,
			BeforeMutation: func(EntryMutationPreparation) error {
				prepareCalls++
				return nil
			},
		},
		Dependencies: []EntryMutationDependency{
			{
				EntryName: firstDependencyName,
				ExpectedLive: func(live *MCPEntry) bool {
					return live != nil && live.URL == firstDependencyURL
				},
			},
			{
				EntryName:    failingDependencyName,
				ExpectedLive: func(*MCPEntry) bool { return true },
			},
		},
	})
	if result.Invoked || result.BackupPath != "" || concrete.backupCalls != 0 || concrete.removeCalls != 0 || prepareCalls != 0 {
		t.Fatalf("pre-invocation dependency read failure had side effects: result=%+v backup=%d remove=%d prepare=%d", result, concrete.backupCalls, concrete.removeCalls, prepareCalls)
	}
	if result.DependencyFailure == nil ||
		result.DependencyFailure.Phase != EntryMutationDependencyFailureBeforeRead ||
		result.DependencyFailure.Kind != EntryMutationDependencyFailureObservation ||
		result.DependencyFailure.EntryName != failingDependencyName ||
		!errors.Is(result.DependencyFailure.Cause, readErr) || !errors.Is(result.ObservationErr, readErr) {
		t.Fatalf("result=%+v, want typed first failing dependency read error", result)
	}
	if len(result.Dependencies) != 2 || result.Dependencies[0].Before == nil ||
		result.Dependencies[0].Before.URL != firstDependencyURL ||
		!errors.Is(result.Dependencies[1].ObservationErr, readErr) {
		t.Fatalf("dependency observations=%+v, want ordered first-success then failing read", result.Dependencies)
	}
	if live, err := concrete.claudeCode.GetEntry(targetName); err != nil || live == nil || live.URL != targetURL {
		t.Fatalf("pre-invocation refusal changed target: live=%+v err=%v", live, err)
	}
}

type postMutationClaudeClient struct {
	*claudeCode
	afterMutation func() error
}

func (c *postMutationClaudeClient) RemoveEntry(name string) error {
	if err := c.claudeCode.RemoveEntry(name); err != nil {
		return err
	}
	return c.afterMutation()
}

func TestConditionalEntryGroupMutation_TargetAndDependenciesShareOneLockAndValidatesReadback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	const legacyName = "mcp-language-server-go-legacy"
	const canonicalName = "mcp-language-server-go"
	const siblingLegacyName = "mcp-language-server-go-older"
	const canonicalURL = "http://127.0.0.1:9137/mcp"
	const siblingLegacyURL = "http://127.0.0.1:9201/mcp"
	const operatorCanonicalURL = "https://operator.example/canonical"
	const operatorSiblingURL = "https://operator.example/sibling"
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"`+legacyName+`":{"url":"http://127.0.0.1:9200/mcp"},"`+canonicalName+`":{"url":"`+canonicalURL+`"},"`+siblingLegacyName+`":{"url":"`+siblingLegacyURL+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	concrete := &claudeCode{path: path}
	wrapped := newLockingClient(&postMutationClaudeClient{
		claudeCode: concrete,
		afterMutation: func() error {
			if err := concrete.AddEntry(MCPEntry{Name: canonicalName, URL: operatorCanonicalURL}); err != nil {
				return err
			}
			return concrete.AddEntry(MCPEntry{Name: siblingLegacyName, URL: operatorSiblingURL})
		},
	})
	mutator, ok := wrapped.(ConditionalEntryGroupMutator)
	if !ok {
		t.Fatalf("wrapped adapter lacks ConditionalEntryGroupMutator")
	}
	result := mutator.ConditionalEntryGroupMutation(ConditionalEntryGroupMutationRequest{
		ConditionalEntryMutationRequest: ConditionalEntryMutationRequest{
			EntryName: legacyName,
			ExpectedLive: func(live *MCPEntry) bool {
				return live != nil && live.URL == "http://127.0.0.1:9200/mcp"
			},
			Operation: EntryMutationRemove,
		},
		Dependencies: []EntryMutationDependency{
			{
				EntryName: canonicalName,
				ExpectedLive: func(live *MCPEntry) bool {
					return live != nil && live.URL == canonicalURL
				},
			},
			{
				EntryName: siblingLegacyName,
				ExpectedLive: func(live *MCPEntry) bool {
					return live != nil && live.URL == siblingLegacyURL
				},
			},
		},
	})
	if !result.Invoked || result.DependencyFailure == nil {
		t.Fatalf("result=%+v, want invoked operation with dependency readback failure", result)
	}
	failure := result.DependencyFailure
	if failure.Phase != EntryMutationDependencyFailureAfterPredicateMismatch ||
		failure.Kind != EntryMutationDependencyFailurePredicateMismatch || failure.EntryName != canonicalName || failure.Cause != nil {
		t.Fatalf("failure=%+v, want first-in-request-order canonical post-mutation mismatch", failure)
	}
	if len(result.Dependencies) != 2 ||
		result.Dependencies[0].Before == nil || result.Dependencies[0].Before.URL != canonicalURL ||
		result.Dependencies[0].After == nil || result.Dependencies[0].After.URL != operatorCanonicalURL ||
		result.Dependencies[0].AfterMatchesExpected == nil || *result.Dependencies[0].AfterMatchesExpected ||
		result.Dependencies[1].Before == nil || result.Dependencies[1].Before.URL != siblingLegacyURL ||
		result.Dependencies[1].After == nil || result.Dependencies[1].After.URL != operatorSiblingURL ||
		result.Dependencies[1].AfterMatchesExpected == nil || *result.Dependencies[1].AfterMatchesExpected {
		t.Fatalf("dependency readback=%+v, want every ordered dependency observed after target invocation", result.Dependencies)
	}
	legacy, err := concrete.GetEntry(legacyName)
	if err != nil {
		t.Fatal(err)
	}
	if legacy != nil {
		t.Fatalf("target was not invoked under the group lock: %+v", legacy)
	}
}
