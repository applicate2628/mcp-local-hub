package clients

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Reconstructing a transport entry from the hub's whitelist loses operator
// policy, timeout presence, future keys, and the file's lexical presentation.
func TestCodexTransportReplacementPreservesOperatorEntry(t *testing.T) {
	const policy = "enabled = false # activation\nrequired = true\nenabled_tools = ['read', 'search']\ndisabled_tools = [\n  'write', # tool note\n]\napproval = { default = 'auto' } # approval\nfuture_scalar = 1_024\n"
	const prefix = "# file note\ntitle = 'private fixture'  # untouched\n\n"
	const suffix = "\n# sibling note\n[mcp_servers.sibling]\ncommand = 'foreign-tool' # untouched sibling\n"
	for _, tc := range []struct {
		name, source, target, body string
		project                    bool
		transport                  CodexTransport
		timeouts                   string
	}{
		{
			name: "project-stdio-explicit-timeouts", source: "code.graph", target: "code.graph", project: true,
			transport: CodexTransportStdio,
			body:      "[ mcp_servers . \"code.graph\" ] # entry note\ncommand = 'private-tool' # command note\nargs = [\n # argument note\n 'serve', # argument tail\n]\n" + policy + "startup_timeout_sec = 30.0 # startup\ntool_timeout_sec = 90 # tool timeout\n[mcp_servers.\"code.graph\".env] # environment table\nPRIVATE_FIXTURE = 'synthetic' # environment note\n[mcp_servers.\"code.graph\".future]\nmode = 'keep' # future note\n",
			timeouts:  "startup_timeout_sec = 30.0 # startup\ntool_timeout_sec = 90 # tool timeout\n",
		},
		{
			name: "global-http-alias-omitted-timeouts", source: "code.graph", target: "code.graph-hub",
			transport: CodexTransportHTTP,
			body:      "[mcp_servers.\"code.graph\"] # entry note\nurl = 'http://127.0.0.1:9000/old' # URL note\n" + policy + "[mcp_servers.\"code.graph\".http_headers] # header table\nX-Old = 'synthetic' # header note\n[mcp_servers.\"code.graph\".future]\nmode = 'keep' # future note\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(prefix + tc.body + suffix)
			root := filepath.Clean(t.TempDir())
			path := filepath.Join(root, "config.toml")
			if tc.project {
				path = filepath.Join(root, ".codex", "config.toml")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			var client Client = newLockingClient(&codexCLI{path: path})
			if tc.project {
				projectClient, _, err := NewCodexCLIForProject(root, tc.source, immediateCodexProjectConfigLockRunnerFactory)
				if err != nil {
					t.Fatal(err)
				}
				client = projectClient
			}
			adapter := client.(CodexTransportClient)
			managed := client.(ManagedRegistrationClient)
			baseline, err := managed.SnapshotManagedRegistration(context.Background(), tc.target)
			if err != nil {
				t.Fatal(err)
			}
			request := CodexHTTPRelocation{
				SourceEntryName: tc.source, TargetEntryName: tc.target, ExpectedSource: tc.transport,
				ExpectedConfigSHA256: fmt.Sprintf("%x", sha256.Sum256(original)),
				Entry:                MCPEntry{Name: tc.target, URL: "http://127.0.0.1:9300/mcp", Headers: map[string]string{"X-New": "synthetic"}, ToolTimeoutSec: 999},
			}
			result, err := adapter.RelocateHTTPEntry(request)
			if err != nil || result.Outcome != CodexHTTPRelocationCommitted {
				t.Fatalf("RelocateHTTPEntry = %#v, %v", result, err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			document, err := decodeCodexTOMLBytes(after)
			if err != nil {
				t.Fatal(err)
			}
			servers := document["mcp_servers"].(map[string]any)
			raw := servers[tc.target].(map[string]any)
			for key, want := range map[string]any{
				"enabled": false, "required": true, "enabled_tools": []any{"read", "search"},
				"disabled_tools": []any{"write"}, "approval": map[string]any{"default": "auto"},
				"future_scalar": int64(1024), "future": map[string]any{"mode": "keep"},
				"url": "http://127.0.0.1:9300/mcp", "http_headers": map[string]any{"X-New": "synthetic"},
			} {
				if !reflect.DeepEqual(raw[key], want) {
					t.Errorf("%s = %#v, want %#v", key, raw[key], want)
				}
			}
			if tc.timeouts == "" {
				for _, key := range []string{"startup_timeout_sec", "tool_timeout_sec"} {
					if _, present := raw[key]; present {
						t.Errorf("omitted %s was introduced", key)
					}
				}
			} else if raw["startup_timeout_sec"] != 30.0 || raw["tool_timeout_sec"] != int64(90) || !bytes.Contains(after, []byte(tc.timeouts)) {
				t.Errorf("explicit timeouts or their bytes changed: %#v", raw)
			}
			if _, present := raw["command"]; present {
				t.Error("old command remains")
			}
			if _, present := raw["env"]; present {
				t.Error("old environment remains")
			}
			if !bytes.HasPrefix(after, []byte(prefix)) || !bytes.HasSuffix(after, []byte(suffix)) || !bytes.Contains(after, []byte(policy)) {
				t.Errorf("unrelated or nontransport presentation changed:\n%s", after)
			}
			for _, line := range strings.Split(string(original), "\n") {
				if start := strings.Index(line, "#"); start >= 0 {
					comment := line[start:]
					if bytes.Count(after, []byte(comment)) != bytes.Count(original, []byte(comment)) {
						t.Errorf("comment token %q was lost or duplicated:\n%s", comment, after)
					}
				}
			}
			inverse := result.ExactInverse()
			if inverse == nil {
				t.Fatal("committed relocation has no exact inverse")
			}
			tamperedInverse := result.ExactInverse()
			tamperedInverse.Target.Headers["X-New"] = "changed-request"
			if _, err := adapter.RestoreRelocatedHTTPEntry(*tamperedInverse); !errors.Is(err, ErrCodexLayerCollisionUnowned) {
				t.Fatalf("modified inverse target proof = %v, want refusal", err)
			}
			encoded, err := json.Marshal(result)
			if err != nil || bytes.Contains(encoded, []byte("# file note")) || bytes.Contains(encoded, []byte(path)) {
				t.Fatalf("private preimage escaped result serialization: %s, %v", encoded, err)
			}
			request.SourceSnapshot = result.SourceSnapshot
			request.ExpectedConfigSHA256 = fmt.Sprintf("%x", sha256.Sum256(after))
			repeated, err := adapter.RelocateHTTPEntry(request)
			if err != nil || repeated.Outcome != CodexHTTPRelocationAlreadyConfigured {
				t.Fatalf("proven repeat = %#v, %v", repeated, err)
			}
			if got, _ := os.ReadFile(path); !bytes.Equal(got, after) {
				t.Fatal("proven repeat changed bytes")
			}
			if err := client.AddEntry(request.Entry); err != nil {
				t.Fatalf("existing AddEntry: %v", err)
			}
			if got, _ := os.ReadFile(path); !bytes.Equal(got, after) {
				t.Fatal("existing AddEntry rebuilt or changed the identical transport")
			}
			applied, err := managed.SnapshotManagedRegistration(context.Background(), tc.target)
			if err != nil {
				t.Fatal(err)
			}
			backupIO := &w10ManagedRegistrationBackupIO{}
			backupID, err := client.(*lockingClient).createManagedRegistrationBackup(context.Background(), backupIO, path, original, baseline.ConfigSHA256.String())
			if err != nil {
				t.Fatal(err)
			}
			persistentInverse := *inverse
			persistentInverse.exactInverse = nil
			if _, err := adapter.RestoreRelocatedHTTPEntry(persistentInverse); !errors.Is(err, ErrCodexLayerCollisionUnowned) {
				t.Fatalf("semantic-only inverse = %v, want explicit proof refusal", err)
			}
			observed := managed.ConditionalManagedRegistrationBackupRestore(context.Background(), ManagedRegistrationConditionalBackupRestoreRequest{
				EntryName: tc.target, BackupID: backupID, BackupIO: backupIO, CodexInverse: &persistentInverse,
				ExpectedApplied: applied, ExpectedBaseline: baseline,
				ExpectedTargetGroupSHA256: managedRegistrationDomainSHA256("fixture-group", tc.target),
				BeforeWrite:               func(ManagedRegistrationBackupRestorePreparation) error { return nil },
			})
			if observed.Failure != ManagedRegistrationFailureNone || observed.Settlement != ManagedRegistrationSettlementVerified {
				t.Fatalf("pinned conditional inverse = %#v", observed)
			}
			if got, _ := os.ReadFile(path); !bytes.Equal(got, original) {
				t.Fatal("rehydrated conditional inverse did not restore exact preimage")
			}
			request.SourceSnapshot = nil
			request.ExpectedConfigSHA256 = fmt.Sprintf("%x", sha256.Sum256(original))
			result, err = adapter.RelocateHTTPEntry(request)
			if err != nil || result.ExactInverse() == nil {
				t.Fatalf("forward after conditional inverse: %v", err)
			}
			inverse = result.ExactInverse()
			foreign := bytes.Replace(after, []byte("# entry note"), []byte("# foreign selected note"), 1)
			if err := os.WriteFile(path, foreign, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.RestoreRelocatedHTTPEntry(*inverse); !errors.Is(err, ErrCodexTargetNameConflict) {
				t.Fatalf("foreign selected-comment inverse = %v, want conflict", err)
			}
			if got, _ := os.ReadFile(path); !bytes.Equal(got, foreign) {
				t.Fatal("foreign-comment refusal wrote config")
			}
			changedSibling := bytes.Replace(after, []byte("foreign-tool"), []byte("later-sibling"), 1)
			if err := os.WriteFile(path, changedSibling, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.RestoreRelocatedHTTPEntry(*inverse); err != nil {
				t.Fatalf("exact inverse with later sibling edit: %v", err)
			}
			wantRestored := bytes.Replace(original, []byte("foreign-tool"), []byte("later-sibling"), 1)
			if got, _ := os.ReadFile(path); !bytes.Equal(got, wantRestored) {
				t.Fatalf("inverse did not restore exact owned bytes/preserve sibling:\n%s", got)
			}
			baseline, err = managed.SnapshotManagedRegistration(context.Background(), tc.target)
			if err != nil {
				t.Fatal(err)
			}
			desired := MCPEntry{Name: tc.target, URL: request.Entry.URL, ToolTimeoutSec: 999}
			targetFingerprint, err := ManagedRegistrationTargetFingerprint(desired, false)
			if err != nil {
				t.Fatal(err)
			}
			mutation := ManagedRegistrationMutationRequest{
				EntryName: tc.target, Desired: desired, BackupIO: &w10ManagedRegistrationBackupIO{},
				ExpectedConfigSHA256: baseline.ConfigSHA256, ExpectedEntrySHA256: baseline.EntrySHA256,
				ExpectedConfigPathSHA256: baseline.ConfigPathSHA256, ExpectedEntryKeySHA256: baseline.EntryKeySHA256,
				ExpectedBaseline: baseline, ExpectedTargetSHA256: targetFingerprint,
				CodexRelocation: &ManagedRegistrationCodexRelocation{SourceEntryName: tc.source, TargetEntryName: tc.target, ExpectedSource: tc.transport},
				BeforeWrite:     func(ManagedRegistrationMutationPreparation) error { return nil },
			}
			if baseline.Exists {
				observed = managed.ConditionalManagedRegistrationMutation(context.Background(), mutation)
			} else {
				observed = managed.ConditionalManagedRegistrationExactCreate(context.Background(), mutation)
			}
			if observed.Failure != ManagedRegistrationFailureNone || observed.Settlement != ManagedRegistrationSettlementVerified {
				t.Fatalf("source-preserving conditional mutation = %#v", observed)
			}
			finalBytes, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			finalDocument, err := decodeCodexTOMLBytes(finalBytes)
			if err != nil {
				t.Fatal(err)
			}
			finalServers, ok := finalDocument["mcp_servers"].(map[string]any)
			if !ok {
				t.Fatal("final conditional config has no server table")
			}
			finalRaw, ok := finalServers[tc.target].(map[string]any)
			if !ok {
				t.Fatal("final conditional config has no target entry")
			}
			for key, want := range map[string]any{
				"enabled": false, "required": true, "enabled_tools": []any{"read", "search"},
				"disabled_tools": []any{"write"}, "approval": map[string]any{"default": "auto"},
				"future_scalar": int64(1024), "future": map[string]any{"mode": "keep"},
				"url": desired.URL,
			} {
				if !reflect.DeepEqual(finalRaw[key], want) {
					t.Errorf("final conditional %s = %#v, want %#v", key, finalRaw[key], want)
				}
			}
			if tc.timeouts == "" {
				for _, key := range []string{"startup_timeout_sec", "tool_timeout_sec"} {
					if _, present := finalRaw[key]; present {
						t.Errorf("final conditional introduced omitted %s", key)
					}
				}
			} else if finalRaw["startup_timeout_sec"] != 30.0 || finalRaw["tool_timeout_sec"] != int64(90) || !bytes.Contains(finalBytes, []byte(tc.timeouts)) {
				t.Errorf("final conditional changed explicit timeouts or their bytes: %#v", finalRaw)
			}
			for _, key := range []string{"command", "args", "env", "cwd", "env_vars", "http_headers"} {
				if _, present := finalRaw[key]; present {
					t.Errorf("final conditional retained obsolete transport %s", key)
				}
			}
			finalSuffix := strings.Replace(suffix, "foreign-tool", "later-sibling", 1)
			if !bytes.HasPrefix(finalBytes, []byte(prefix)) || !bytes.HasSuffix(finalBytes, []byte(finalSuffix)) || !bytes.Contains(finalBytes, []byte(policy)) {
				t.Errorf("final conditional changed unrelated or nontransport presentation:\n%s", finalBytes)
			}
			for _, line := range strings.Split(string(original), "\n") {
				if start := strings.Index(line, "#"); start >= 0 {
					comment := line[start:]
					if bytes.Count(finalBytes, []byte(comment)) != bytes.Count(original, []byte(comment)) {
						t.Errorf("final conditional comment token %q was lost or duplicated:\n%s", comment, finalBytes)
					}
				}
			}
		})
	}
	finalOriginal := []byte("[mcp_servers.final]\nurl = 'http://127.0.0.1:9000/old' # retained final transport note\n")
	finalPath := setupCodexConfig(t, string(finalOriginal))
	finalClient := newLockingClient(&codexCLI{path: finalPath}).(CodexTransportClient)
	finalResult, err := finalClient.RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "final", TargetEntryName: "final", ExpectedSource: CodexTransportHTTP,
		Entry: MCPEntry{Name: "final", URL: "http://127.0.0.1:9300/mcp"},
	})
	if err != nil || finalResult.ExactInverse() == nil {
		t.Fatalf("final transport comment forward: %v", err)
	}
	if _, err := finalClient.RestoreRelocatedHTTPEntry(*finalResult.ExactInverse()); err != nil {
		t.Fatalf("owned final transport comment inverse: %v", err)
	}
	if got, _ := os.ReadFile(finalPath); !bytes.Equal(got, finalOriginal) {
		t.Fatal("owned final transport comment inverse changed preimage")
	}
}

func TestCodexProvenanceInverseRestoresSelectedBytes(t *testing.T) {
	const selected = "[mcp_servers.\"proof.original\"] # selected\ncommand = 'private-tool' # source transport\nargs = ['--stdio']\nenabled = true\nstartup_timeout_sec = 30.0 # startup\ntool_timeout_sec = 75 # tool timeout\nfuture = { mode = 'keep' } # future policy\n"
	const sibling = "\n[mcp_servers.sibling]\ncommand = 'before'\n"
	original := []byte("title = 'private fixture'\n\n" + selected + sibling)
	path := setupCodexConfig(t, string(original))
	adapter := newLockingClient(&codexCLI{path: path}).(CodexTransportClient)
	target := MCPEntry{Name: "proof.receiver", URL: "http://127.0.0.1:9300/mcp", ToolTimeoutSec: 75}
	forward, err := adapter.RelocateHTTPEntry(CodexHTTPRelocation{
		SourceEntryName: "proof.original", TargetEntryName: target.Name, ExpectedSource: CodexTransportStdio, Entry: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	req := CodexHTTPInverseRelocation{SourceEntryName: "proof.original", TargetEntryName: target.Name, Target: target, SourceSnapshot: forward.SourceSnapshot}
	hash := fmt.Sprintf("%x", sha256.Sum256(original))
	assertUnchanged := func(want []byte) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("proof refusal changed private config: %v\n%s", err, got)
		}
	}
	if _, err := adapter.RestoreRelocatedHTTPEntryFromProvenance(req, original, ""); !errors.Is(err, ErrCodexLayerCollisionUnowned) {
		t.Fatalf("missing hash = %v, want refusal", err)
	}
	assertUnchanged(applied)
	tampered := bytes.Replace(original, []byte("private-tool"), []byte("changed-tool"), 1)
	if _, err := adapter.RestoreRelocatedHTTPEntryFromProvenance(req, tampered, hash); !errors.Is(err, ErrCodexLayerCollisionUnowned) {
		t.Fatalf("tampered snapshot = %v, want refusal", err)
	}
	assertUnchanged(applied)
	wrongTuple := req
	wrongTuple.Target.URL = "http://127.0.0.1:9301/mcp"
	if _, err := adapter.RestoreRelocatedHTTPEntryFromProvenance(wrongTuple, original, hash); !errors.Is(err, ErrCodexTargetNameConflict) {
		t.Fatalf("changed recorded tuple = %v, want refusal", err)
	}
	assertUnchanged(applied)
	foreign := bytes.Replace(applied, []byte("# selected"), []byte("# foreign selected"), 1)
	if err := os.WriteFile(path, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.RestoreRelocatedHTTPEntryFromProvenance(req, original, hash); !errors.Is(err, ErrCodexTargetNameConflict) {
		t.Fatalf("foreign selected comment = %v, want refusal", err)
	}
	assertUnchanged(foreign)
	withSiblingEdit := bytes.Replace(applied, []byte("command = 'before'"), []byte("command = 'after' # later sibling"), 1)
	if err := os.WriteFile(path, withSiblingEdit, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := adapter.RestoreRelocatedHTTPEntryFromProvenance(req, original, hash)
	if err != nil || result.Outcome != CodexHTTPInverseRestored {
		t.Fatalf("valid modern provenance inverse = %#v, %v", result, err)
	}
	want := bytes.Replace(original, []byte("command = 'before'"), []byte("command = 'after' # later sibling"), 1)
	assertUnchanged(want)
	result, err = adapter.RestoreRelocatedHTTPEntryFromProvenance(req, original, hash)
	if err != nil || result.Outcome != CodexHTTPInverseAlreadyRestored {
		t.Fatalf("proven inverse repeat = %#v, %v", result, err)
	}
	assertUnchanged(want)

	// Literal canonical legacy output is independent of the production renderer.
	legacy := []byte("title = 'private fixture'\n\n[mcp_servers]\n[mcp_servers.'proof.receiver']\nstartup_timeout_sec = 10.0\ntool_timeout_sec = 75.0\nurl = 'http://127.0.0.1:9300/mcp'\n\n[mcp_servers.sibling]\ncommand = 'after' # later sibling\n")
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = adapter.RestoreRelocatedHTTPEntryFromProvenance(req, original, hash)
	if err != nil || result.Outcome != CodexHTTPInverseRestored {
		t.Fatalf("valid canonical legacy inverse = %#v, %v", result, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(got, []byte(selected)) || !bytes.HasSuffix(got, []byte("\n[mcp_servers.sibling]\ncommand = 'after' # later sibling\n")) || bytes.Contains(got, []byte("proof.receiver")) {
		t.Fatalf("legacy inverse lost selected bytes or sibling: %v\n%s", err, got)
	}
}
