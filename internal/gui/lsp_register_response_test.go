package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

func TestLSPRegisterResponseJSONExactKeys(t *testing.T) {
	fullEntry := workspaceEntryDTO{
		WorkspaceKey: "key", WorkspacePath: "path", Language: "go", Backend: "gopls-mcp",
		Port: 9125, TaskName: "task", ClientEntries: map[string]string{"cursor": "entry"}, Lifecycle: "ready", LastError: "err",
	}
	base := lspRegisterResponse{
		Workspace: "path", WorkspaceKey: "key", Entries: []workspaceEntryDTO{fullEntry},
		Results: []lspRegisterLanguageResult{{Language: "go", Status: "ok"}},
	}
	for _, tc := range []struct {
		name       string
		response   lspRegisterResponse
		wantTop    []string
		wantResult []string
	}{
		{name: "clean success", response: base, wantTop: []string{"entries", "results", "workspace", "workspace_key"}, wantResult: []string{"language", "status"}},
		{name: "warning success", response: func() lspRegisterResponse { v := base; v.Warnings = []string{"warning"}; return v }(), wantTop: []string{"entries", "results", "warnings", "workspace", "workspace_key"}, wantResult: []string{"language", "status"}},
		{name: "error", response: func() lspRegisterResponse {
			v := base
			v.Error = "failed"
			v.Code = "LSP_REGISTER_FAILED"
			v.Results = []lspRegisterLanguageResult{{Language: "go", Status: "error", Error: "failed"}}
			return v
		}(), wantTop: []string{"code", "entries", "error", "results", "workspace", "workspace_key"}, wantResult: []string{"error", "language", "status"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.response)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if got := sortedJSONKeys(decoded); !slices.Equal(got, tc.wantTop) {
				t.Fatalf("top keys=%v want=%v", got, tc.wantTop)
			}
			entries := decoded["entries"].([]any)
			entry := entries[0].(map[string]any)
			wantEntry := []string{"backend", "client_entries", "language", "last_error", "lifecycle", "port", "task_name", "workspace_key", "workspace_path"}
			if got := sortedJSONKeys(entry); !slices.Equal(got, wantEntry) {
				t.Fatalf("entry keys=%v want=%v", got, wantEntry)
			}
			clientEntries := entry["client_entries"].(map[string]any)
			if got := sortedJSONKeys(clientEntries); !slices.Equal(got, []string{"cursor"}) {
				t.Fatalf("client entry keys=%v", got)
			}
			results := decoded["results"].([]any)
			if got := sortedJSONKeys(results[0].(map[string]any)); !slices.Equal(got, tc.wantResult) {
				t.Fatalf("result keys=%v want=%v", got, tc.wantResult)
			}
		})
	}
}

func TestLSPRegisterTrustedRootWarningIsWireSafe(t *testing.T) {
	const (
		rootSentinel  = `D:\secret\trusted-root`
		tokenSentinel = "password=hunter2"
		errorSentinel = "raw-bless-error"
	)
	prevEnsure := ensureLSPRegisteredForGUI
	prevBless := blessLSPTrustedRootForGUI
	t.Cleanup(func() {
		ensureLSPRegisteredForGUI = prevEnsure
		blessLSPTrustedRootForGUI = prevBless
	})
	ensureLSPRegisteredForGUI = func(context.Context, string, string, string) (api.WorkspaceEntry, error) {
		return api.WorkspaceEntry{
			WorkspaceKey: "project", WorkspacePath: "D:/dev/project",
			Language: "go", Backend: "gopls-mcp", Port: 9201, TaskName: "task",
		}, nil
	}
	blessLSPTrustedRootForGUI = func(string) error {
		return errors.New(rootSentinel + " " + tokenSentinel + " " + errorSentinel)
	}

	server := NewServer(Config{Port: 9125, Version: "test", PID: 1})
	server.lspRegistrar = realLSPRegistrar{}
	req := httptest.NewRequest(http.MethodPost, "/api/lsp/register",
		strings.NewReader(`{"workspace_path":"D:/dev/project","language":"go"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	recorder := httptest.NewRecorder()
	server.mux.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("warning-only register status=%d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	var response lspRegisterResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	body := recorder.Body.String()
	if len(response.Warnings) != 1 || response.Warnings[0] != lspRegisterTrustedRootRecordFailedPublic {
		t.Fatalf("warnings=%v, want fixed trusted-root warning", response.Warnings)
	}
	for _, sentinel := range []string{rootSentinel, tokenSentinel, errorSentinel} {
		if strings.Contains(body, sentinel) {
			t.Fatalf("wire response leaked %q: %s", sentinel, body)
		}
	}
}

func TestRegistrationHTTPBodies_NoRawDiagnosticSentinels(t *testing.T) {
	const rawSentinel lspRegisterWarningCode = `D:\secret\warning password=hunter2`
	response := lspRegisterResponseFromReport(&lspRegisterReport{Warnings: []lspRegisterWarningCode{rawSentinel}})
	if len(response.Warnings) != 1 || response.Warnings[0] != lspRegisterUnknownWarningPublic {
		t.Fatalf("warnings=%v, want fixed fallback", response.Warnings)
	}
	if strings.Contains(response.Warnings[0], string(rawSentinel)) || strings.Contains(response.Warnings[0], "hunter2") {
		t.Fatalf("fallback leaked private warning code: %v", response.Warnings)
	}
}

func sortedJSONKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
