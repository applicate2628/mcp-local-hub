package cli

import (
	"bytes"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

type fakeLSPRouterCLIAPI struct {
	disabled map[string]bool
	statuses []api.LSPRouterClientStatus

	setCalls        [][]string
	rollbackClients []string
	ensureCalls     int
	ensureOpts      []api.LSPClientRouterOpts

	rollbackReport *api.LSPClientRouterReport
	ensureReport   *api.LSPClientRouterReport
}

func (f *fakeLSPRouterCLIAPI) LSPRouterDisabledClientSet() (map[string]bool, error) {
	out := map[string]bool{}
	for k, v := range f.disabled {
		out[k] = v
	}
	return out, nil
}

func (f *fakeLSPRouterCLIAPI) SetLSPRouterDisabledClients(names []string) error {
	cp := append([]string(nil), names...)
	f.setCalls = append(f.setCalls, cp)
	f.disabled = map[string]bool{}
	for _, name := range cp {
		f.disabled[name] = true
	}
	return nil
}

func (f *fakeLSPRouterCLIAPI) RollbackLSPRouterClientEntriesForClient(clientName string, _ api.LSPClientRouterOpts) (*api.LSPClientRouterReport, error) {
	f.rollbackClients = append(f.rollbackClients, clientName)
	if f.rollbackReport != nil {
		return f.rollbackReport, nil
	}
	return &api.LSPClientRouterReport{}, nil
}

func (f *fakeLSPRouterCLIAPI) EnsureLSPRouterClientEntries(opts api.LSPClientRouterOpts) (*api.LSPClientRouterReport, error) {
	f.ensureCalls++
	f.ensureOpts = append(f.ensureOpts, opts)
	if f.ensureReport != nil {
		return f.ensureReport, nil
	}
	return &api.LSPClientRouterReport{}, nil
}

func (f *fakeLSPRouterCLIAPI) LSPRouterClientStatuses(api.LSPClientRouterOpts) ([]api.LSPRouterClientStatus, error) {
	return f.statuses, nil
}

func runLSPRouterCommandWithFake(t *testing.T, fake *fakeLSPRouterCLIAPI, args ...string) (string, error) {
	t.Helper()
	prior := newLSPRouterCLIAPI
	newLSPRouterCLIAPI = func() lspRouterCLIAPI { return fake }
	t.Cleanup(func() { newLSPRouterCLIAPI = prior })

	root := NewRootCmd()
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs(append([]string{"lsp-router"}, args...))
	err := root.Execute()
	return buf.String(), err
}

func TestLSPRouterDisablePersistsAndRollsBackClientImmediately(t *testing.T) {
	fake := &fakeLSPRouterCLIAPI{
		disabled: map[string]bool{"claude-code": true},
		rollbackReport: &api.LSPClientRouterReport{
			Removed: []api.LSPClientRouterChange{{
				Client:    "codex-cli",
				Language:  "go",
				EntryName: "mcp-language-server-go",
			}},
			Skipped: []api.LSPClientRouterChange{{
				Client:    "codex-cli",
				Language:  "typescript",
				EntryName: "mcp-language-server-typescript",
			}},
		},
	}

	out, err := runLSPRouterCommandWithFake(t, fake, "disable", "--client", "codex-cli")
	if err != nil {
		t.Fatalf("disable command: %v\n%s", err, out)
	}
	if len(fake.setCalls) != 1 || strings.Join(fake.setCalls[0], ",") != "claude-code,codex-cli" {
		t.Fatalf("set calls = %+v, want [claude-code codex-cli]", fake.setCalls)
	}
	if len(fake.rollbackClients) != 1 || fake.rollbackClients[0] != "codex-cli" {
		t.Fatalf("rollback clients = %+v, want codex-cli", fake.rollbackClients)
	}
	if !strings.Contains(out, "removed codex-cli entry mcp-language-server-go") {
		t.Fatalf("output missing removed entry line:\n%s", out)
	}
	if !strings.Contains(out, "skipped foreign LSP-like entry mcp-language-server-typescript") {
		t.Fatalf("output missing skipped foreign entry line:\n%s", out)
	}
	if !strings.Contains(out, "future `mcphub setup` runs will not re-add") {
		t.Fatalf("output missing setup no-readd line:\n%s", out)
	}
}

func TestLSPRouterEnablePersistsAndRunsEnsure(t *testing.T) {
	fake := &fakeLSPRouterCLIAPI{
		disabled: map[string]bool{"claude-code": true, "codex-cli": true},
		ensureReport: &api.LSPClientRouterReport{
			Applied: []api.LSPClientRouterChange{{
				Client:    "codex-cli",
				Language:  "go",
				EntryName: "mcp-language-server-go",
				URL:       "http://127.0.0.1:7777/lsp/go/mcp",
			}},
		},
	}

	out, err := runLSPRouterCommandWithFake(t, fake, "enable", "--client", "codex-cli")
	if err != nil {
		t.Fatalf("enable command: %v\n%s", err, out)
	}
	if len(fake.setCalls) != 1 || strings.Join(fake.setCalls[0], ",") != "claude-code" {
		t.Fatalf("set calls = %+v, want [claude-code]", fake.setCalls)
	}
	if fake.ensureCalls != 1 {
		t.Fatalf("ensure calls = %d, want 1", fake.ensureCalls)
	}
	if len(fake.ensureOpts) != 1 || fake.ensureOpts[0].ForceClientName != "codex-cli" {
		t.Fatalf("ensure opts = %+v, want ForceClientName codex-cli", fake.ensureOpts)
	}
	if len(fake.ensureOpts[0].Clients) != 1 || fake.ensureOpts[0].Clients["codex-cli"] == nil {
		t.Fatalf("ensure Clients scope = %+v, want only codex-cli", fake.ensureOpts[0].Clients)
	}
	if !strings.Contains(out, "codex-cli enabled for LSP router setup") {
		t.Fatalf("output missing enable confirmation:\n%s", out)
	}
	if !strings.Contains(out, "codex-cli") || !strings.Contains(out, "http://127.0.0.1:7777/lsp/go/mcp") {
		t.Fatalf("output missing ensure result:\n%s", out)
	}
}

func TestLSPRouterEnableForcesRequestedOptInClientEnsure(t *testing.T) {
	fake := &fakeLSPRouterCLIAPI{
		disabled: map[string]bool{"antigravity": true},
		ensureReport: &api.LSPClientRouterReport{
			Applied: []api.LSPClientRouterChange{{
				Client:    "antigravity",
				Language:  "go",
				EntryName: "mcp-language-server-go",
				URL:       "http://127.0.0.1:7777/lsp/go/mcp",
			}},
		},
	}

	out, err := runLSPRouterCommandWithFake(t, fake, "enable", "--client", "antigravity")
	if err != nil {
		t.Fatalf("enable command: %v\n%s", err, out)
	}
	if len(fake.setCalls) != 1 || len(fake.setCalls[0]) != 0 {
		t.Fatalf("set calls = %+v, want cleared disabled list", fake.setCalls)
	}
	if len(fake.ensureOpts) != 1 || fake.ensureOpts[0].ForceClientName != "antigravity" {
		t.Fatalf("ensure opts = %+v, want ForceClientName antigravity", fake.ensureOpts)
	}
	if !strings.Contains(out, "antigravity enabled for LSP router setup") {
		t.Fatalf("output missing enable confirmation:\n%s", out)
	}
	if !strings.Contains(out, "antigravity") || !strings.Contains(out, "mcp-language-server-go") {
		t.Fatalf("output missing forced ensure result:\n%s", out)
	}
}

func TestLSPRouterStatusPrintsDisabledListAndPresentClientEntries(t *testing.T) {
	fake := &fakeLSPRouterCLIAPI{
		disabled: map[string]bool{"codex-cli": true},
		statuses: []api.LSPRouterClientStatus{
			{
				Client:          "claude-code",
				ConfigPath:      "D:/tmp/.claude.json",
				ExistingEntries: []string{"mcp-language-server-go"},
			},
			{
				Client:         "codex-cli",
				ConfigPath:     "D:/tmp/.codex/config.toml",
				Disabled:       true,
				MissingEntries: []string{"mcp-language-server-go"},
			},
		},
	}

	out, err := runLSPRouterCommandWithFake(t, fake, "status")
	if err != nil {
		t.Fatalf("status command: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Disabled clients: codex-cli") {
		t.Fatalf("output missing disabled list:\n%s", out)
	}
	if !strings.Contains(out, "claude-code") || !strings.Contains(out, "1/1 router entries present") {
		t.Fatalf("output missing enabled client status:\n%s", out)
	}
	if !strings.Contains(out, "codex-cli") || !strings.Contains(out, "disabled") || !strings.Contains(out, "0/1 router entries present") {
		t.Fatalf("output missing disabled client status:\n%s", out)
	}
}
