package api

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParsedSerenaInvocationValidatedWhenWorkspaceSelectionIsEmpty(t *testing.T) {
	for _, disappeared := range []bool{false, true} {
		name := "empty caller selection"
		if disappeared {
			name = "last workspace disappeared before install"
		}
		t.Run(name, func(t *testing.T) {
			root := daemonIntentTestHelper(t)
			preparePreflightBinaryChecks(t)
			workspace := t.TempDir()
			workspaces := []WorkspaceEntry{}
			if disappeared {
				trash := filepath.Join(t.TempDir(), "trash")
				if err := os.Rename(workspace, trash); err != nil {
					t.Fatal(err)
				}
				workspaces = []WorkspaceEntry{{WorkspacePath: workspace, Language: SerenaLanguageSentinel, Port: 9401}}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			deadline, _ := ctx.Deadline()
			// Deliberately unissued input must be refused before a now-empty
			// selection can skip owner validation or acquire an intent lease.
			invocation := &PreparedSerenaConsumerInvocation{
				deadline: deadline, io: PreparedSerenaRecordIO{Root: root, IntentPath: filepath.Join(root, supervisorIntentFileLeaf)},
				request: PreparedSerenaConsumerInvocationRequest{ExpectedDescriptors: []SupervisorDaemon{{TaskName: SerenaTaskNameForWorkspace(workspace), Workspace: workspace}}},
			}
			_, err := NewAPI().InstallParsedManifest(ctx, serenaTemplateManifest(), InstallParsedManifestOpts{
				ManifestHash: testExactManifestHash, Workspaces: workspaces, Writer: io.Discard, SerenaPreparedInvocation: invocation,
			})
			if err == nil || !strings.Contains(err.Error(), "SERENA_PREPARED_COLD_PUBLICATION_UNAVAILABLE") {
				t.Fatalf("empty selection bypassed original owner validation: %v", err)
			}
			for _, leaf := range []string{supervisorIntentFileLeaf, supervisorIntentFileLeaf + ".lock"} {
				if _, err := os.Stat(filepath.Join(root, leaf)); !os.IsNotExist(err) {
					t.Fatalf("unissued invocation reached intent state %s: %v", leaf, err)
				}
			}
		})
	}
}

func TestParsedSerenaFinalProjectionRefusesDisappearingWorkspaces(t *testing.T) {
	for _, removeAll := range []bool{false, true} {
		name := "one of two workspaces disappears"
		if removeAll {
			name = "all workspaces disappear"
		}
		t.Run(name, func(t *testing.T) {
			root := daemonIntentTestHelper(t)
			preparePreflightBinaryChecks(t)
			manifest := serenaTemplateManifest()
			manifest.Command = "uvx"
			manifest.BaseArgs = []string{"--from", "git+https://example.invalid/serena@" + strings.Repeat("a", 40), "serena", "start-mcp-server"}
			// This test exercises only the pure descriptor projection and its
			// actual filesystem filter. These metadata values are not an issued
			// publication: no owner is bound and no install/mutation is invoked.
			record := syntheticPreparedRecord(preparedRequest())
			record.RequestIdentitySHA256 = strings.Repeat("a", 64)
			record.EntrypointIdentity.Path = filepath.Join(root, "metadata-only-serena.exe")
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := record.Binding(raw)
			if err != nil {
				t.Fatal(err)
			}
			plan := &Plan{serenaPreparedRecord: &record, serenaPreparedBinding: &binding}
			workspaces := []WorkspaceEntry{
				{WorkspacePath: t.TempDir(), Language: SerenaLanguageSentinel, Port: 9401},
				{WorkspacePath: t.TempDir(), Language: SerenaLanguageSentinel, Port: 9402},
			}
			expected, err := serenaOrPlanDaemons(manifest, plan, testExactManifestHash, workspaces, "", io.Discard)
			if err != nil || len(expected) != 2 {
				t.Fatalf("initial descriptor projection: rows=%d err=%v", len(expected), err)
			}
			invocation := &PreparedSerenaConsumerInvocation{request: PreparedSerenaConsumerInvocationRequest{ExpectedDescriptors: expected}}
			if rows, err := parsedSerenaConsumerDaemons(manifest, plan, testExactManifestHash, workspaces, io.Discard, invocation); err != nil || !reflect.DeepEqual(rows, expected) {
				t.Fatalf("unchanged pure projection differs: %v", err)
			}
			removeCount := 1
			if removeAll {
				removeCount = len(workspaces)
			}
			for _, workspace := range workspaces[:removeCount] {
				trash := filepath.Join(t.TempDir(), "trash")
				if err := os.Rename(workspace.WorkspacePath, trash); err != nil {
					t.Fatal(err)
				}
			}
			prior := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{TaskName: `\mcp-local-hub-sibling-default`, Server: "sibling", Daemon: "default"}}}
			before := cloneSupervisorIntentFile(prior)
			merged, _, _, err := NewAPI().buildMergedSupervisorIntentFromPriorWithProjection(manifest, workspaces, "", prior, true, func() ([]SupervisorDaemon, error) {
				return parsedSerenaConsumerDaemons(manifest, plan, testExactManifestHash, workspaces, io.Discard, invocation)
			})
			if merged != nil || err == nil || !strings.Contains(err.Error(), "SERENA_PARSED_DESCRIPTOR_SCOPE_DRIFT") {
				t.Fatalf("changed final workspace set reached merge: merged=%+v err=%v", merged, err)
			}
			if !reflect.DeepEqual(prior, before) {
				t.Fatal("scope refusal changed sibling/prior intent")
			}
			if _, err := os.Stat(filepath.Join(root, supervisorIntentFileLeaf)); !os.IsNotExist(err) {
				t.Fatalf("pure projection test published intent: %v", err)
			}
		})
	}
}
