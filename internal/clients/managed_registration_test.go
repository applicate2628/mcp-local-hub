package clients

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type w10ManagedRegistrationBackupIO struct {
	contents []byte
	sha256   string
	creates  int
}

type w10ManagedRegistrationBackupFailureIO struct {
	createReceipt ManagedRegistrationBackupCreateReceipt
	createErr     error
	readContents  []byte
	readErr       error
	createLeaves  []string
	readLeaves    []string
}

func (io *w10ManagedRegistrationBackupFailureIO) Create(_ context.Context, _ string, leaf string, _ []byte, _ string) (ManagedRegistrationBackupCreateReceipt, error) {
	io.createLeaves = append(io.createLeaves, leaf)
	return io.createReceipt, io.createErr
}

func (io *w10ManagedRegistrationBackupFailureIO) Read(_ context.Context, _ string, leaf string, _ string) ([]byte, error) {
	io.readLeaves = append(io.readLeaves, leaf)
	return append([]byte(nil), io.readContents...), io.readErr
}

func (io *w10ManagedRegistrationBackupIO) Create(_ context.Context, _ string, _ string, contents []byte, expectedSHA256 string) (ManagedRegistrationBackupCreateReceipt, error) {
	io.creates++
	if managedRegistrationBytesSHA256(contents).String() != expectedSHA256 {
		return ManagedRegistrationBackupCreateReceipt{Disposition: ManagedRegistrationBackupNotCreated, CleanupDisposition: ManagedRegistrationBackupCleanupNotRequired}, errors.New("unexpected backup checksum")
	}
	io.contents = append(io.contents[:0], contents...)
	io.sha256 = expectedSHA256
	return ManagedRegistrationBackupCreateReceipt{Disposition: ManagedRegistrationBackupCommitted, CleanupDisposition: ManagedRegistrationBackupCleanupNotRequired}, nil
}

func (io *w10ManagedRegistrationBackupIO) Read(_ context.Context, _ string, _ string, expectedSHA256 string) ([]byte, error) {
	if io.sha256 != expectedSHA256 {
		return nil, errors.New("unexpected backup read checksum")
	}
	return append([]byte(nil), io.contents...), nil
}

type w10TimeoutDroppingCodex struct{ *codexCLI }

func (c *w10TimeoutDroppingCodex) AddEntry(entry MCPEntry) error {
	if err := c.codexCLI.AddEntry(entry); err != nil {
		return err
	}
	document, err := c.readTOML()
	if err != nil {
		return err
	}
	delete(document["mcp_servers"].(map[string]any)[entry.Name].(map[string]any), "tool_timeout_sec")
	return c.writeTOML(document)
}

// TestW10_CodexFrozenPhysicalTargetAndTimeout catches a managed-registration
// adapter that either omits the locked snapshot capability or snapshots the
// logical/project-layer entry instead of the frozen global physical target.
// Changing the target table's ToolTimeoutSec must change its captured entry
// fingerprint, while foreign global and project tables remain untouched.
func TestW10_CodexFrozenPhysicalTargetAndTimeout(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(root, "global", "config.toml")
	projectPath := filepath.Join(root, "project", ".codex", "config.toml")
	global := []byte(`[mcp_servers.serena-mcphub]
url = "http://127.0.0.1:9137/mcp"
startup_timeout_sec = 10
tool_timeout_sec = 45

[notifications]
enabled = true
`)
	project := []byte(`[mcp_servers.serena]
command = "uvx"
args = ["serena", "start-mcp-server"]
`)
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(projectPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(globalPath, global, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, project, 0o600); err != nil {
		t.Fatal(err)
	}

	client := newLockingClient(&codexCLI{path: globalPath})
	snapshotMethod := reflect.ValueOf(client).MethodByName("SnapshotManagedRegistration")
	if !snapshotMethod.IsValid() {
		t.Fatal("locking client is missing SnapshotManagedRegistration")
	}
	first := callW10ManagedSnapshot(t, snapshotMethod, "serena-mcphub")
	firstFingerprint := first.FieldByName("EntrySHA256").MethodByName("String").Call(nil)[0].String()
	if firstFingerprint == "" {
		t.Fatal("initial managed snapshot has an empty entry fingerprint")
	}

	if err := os.WriteFile(globalPath, []byte(`[mcp_servers.serena-mcphub]
url = "http://127.0.0.1:9137/mcp"
startup_timeout_sec = 10
tool_timeout_sec = 46

[notifications]
enabled = true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	second := callW10ManagedSnapshot(t, snapshotMethod, "serena-mcphub")
	secondFingerprint := second.FieldByName("EntrySHA256").MethodByName("String").Call(nil)[0].String()
	if secondFingerprint == firstFingerprint {
		t.Fatal("managed snapshot entry fingerprint ignored ToolTimeoutSec")
	}
	projectAfter, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(projectAfter) != string(project) {
		t.Fatal("managed snapshot mutated the read-only Codex project layer")
	}
}

func callW10ManagedSnapshot(t *testing.T, method reflect.Value, entryName string) reflect.Value {
	t.Helper()
	if method.Type().NumIn() != 2 {
		t.Fatalf("SnapshotManagedRegistration input count = %d, want context and entry name", method.Type().NumIn())
	}
	result := method.Call([]reflect.Value{reflect.ValueOf(context.Background()), reflect.ValueOf(entryName)})
	if len(result) != 2 {
		t.Fatalf("SnapshotManagedRegistration output count = %d, want snapshot and error", len(result))
	}
	if !result[1].IsNil() {
		t.Fatalf("SnapshotManagedRegistration(%q): %v", entryName, result[1].Interface())
	}
	return result[0]
}

// TestW10_CodexMutationRejectsToolTimeoutReadbackDrift catches the historical
// managed-registration comparator, which did not include the current Codex
// tool-call timeout in its desired/readback equality proof.
func TestW10_CodexMutationRejectsToolTimeoutReadbackDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`[mcp_servers.serena]
url = "http://127.0.0.1:9137/mcp"
startup_timeout_sec = 10
tool_timeout_sec = 15
`), 0o600); err != nil {
		t.Fatal(err)
	}
	client := newLockingClient(&w10TimeoutDroppingCodex{codexCLI: &codexCLI{path: path}})
	managed := requireW10ManagedRegistrationClient(t, client)
	before, err := managed.SnapshotManagedRegistration(context.Background(), "serena")
	if err != nil {
		t.Fatal(err)
	}
	desired := MCPEntry{Name: "serena", URL: "http://127.0.0.1:9137/mcp", ToolTimeoutSec: 45}
	target, err := ManagedRegistrationTargetFingerprint(desired, false)
	if err != nil {
		t.Fatal(err)
	}
	observed := managed.ConditionalManagedRegistrationMutation(context.Background(), ManagedRegistrationMutationRequest{
		EntryName:                desired.Name,
		ExpectedConfigSHA256:     before.ConfigSHA256,
		ExpectedEntrySHA256:      before.EntrySHA256,
		ExpectedConfigPathSHA256: before.ConfigPathSHA256,
		ExpectedEntryKeySHA256:   before.EntryKeySHA256,
		ExpectedBaseline:         before,
		ExpectedTargetSHA256:     target,
		Desired:                  desired,
		BackupKeepN:              1,
		BackupIO:                 &w10ManagedRegistrationBackupIO{},
		BeforeWrite:              func(ManagedRegistrationMutationPreparation) error { return nil },
	})
	if !observed.Invoked {
		t.Fatalf("managed mutation did not reach the adapter: %+v", observed)
	}
	if observed.Phase != ManagedRegistrationPhaseReadback || observed.Failure != ManagedRegistrationFailureReadbackMismatch || observed.Settlement != ManagedRegistrationSettlementWriteUncertain {
		t.Fatalf("managed mutation = %+v, want timeout readback mismatch", observed)
	}
}

// TestW10_CancelledMutationDoesNotEnterBackupOrWriter catches cancellation
// observed after the managed operation starts waiting for the existing config
// lock. It must never be reported as a neutral observation or reach a writer.
func TestW10_CancelledMutationDoesNotEnterBackupOrWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(`[mcp_servers.serena]
url = "http://127.0.0.1:9137/mcp"
startup_timeout_sec = 10
`), 0o600); err != nil {
		t.Fatal(err)
	}
	client := newLockingClient(&codexCLI{path: path})
	managed := requireW10ManagedRegistrationClient(t, client)
	before, err := managed.SnapshotManagedRegistration(context.Background(), "serena")
	if err != nil {
		t.Fatal(err)
	}
	desired := MCPEntry{Name: "serena", URL: "http://127.0.0.1:9137/mcp"}
	target, err := ManagedRegistrationTargetFingerprint(desired, false)
	if err != nil {
		t.Fatal(err)
	}
	backup := &w10ManagedRegistrationBackupIO{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	observed := managed.ConditionalManagedRegistrationMutation(ctx, ManagedRegistrationMutationRequest{
		EntryName:                desired.Name,
		ExpectedConfigSHA256:     before.ConfigSHA256,
		ExpectedEntrySHA256:      before.EntrySHA256,
		ExpectedConfigPathSHA256: before.ConfigPathSHA256,
		ExpectedEntryKeySHA256:   before.EntryKeySHA256,
		ExpectedBaseline:         before,
		ExpectedTargetSHA256:     target,
		Desired:                  desired,
		BackupIO:                 backup,
		BeforeWrite:              func(ManagedRegistrationMutationPreparation) error { return nil },
	})
	if observed.Invoked || backup.creates != 0 || observed.Phase != ManagedRegistrationPhaseBackup || observed.Failure != ManagedRegistrationFailureBackup {
		t.Fatalf("cancelled managed mutation = %+v backup creates=%d, want backup failure with zero writer calls", observed, backup.creates)
	}
}

// TestW10_BackupPortFailuresDoNotReachPrepareOrWriter covers the three
// operation-scoped backup failures that must retain the candidate identity for
// recovery and stop before durable prepare or a client-config writer.
func TestW10_BackupPortFailuresDoNotReachPrepareOrWriter(t *testing.T) {
	tests := []struct {
		name   string
		backup w10ManagedRegistrationBackupFailureIO
	}{
		{
			name: "commit_uncertain",
			backup: w10ManagedRegistrationBackupFailureIO{
				createReceipt: ManagedRegistrationBackupCreateReceipt{
					Disposition:        ManagedRegistrationBackupCommitUncertain,
					CleanupDisposition: ManagedRegistrationBackupCleanupUnconfirmed,
				},
				createErr: errors.New("injected create uncertainty"),
			},
		},
		{
			name: "read_error",
			backup: w10ManagedRegistrationBackupFailureIO{
				createReceipt: ManagedRegistrationBackupCreateReceipt{
					Disposition:        ManagedRegistrationBackupCommitted,
					CleanupDisposition: ManagedRegistrationBackupCleanupNotRequired,
				},
				readErr: errors.New("injected backup read failure"),
			},
		},
		{
			name: "checksum_mismatch",
			backup: w10ManagedRegistrationBackupFailureIO{
				createReceipt: ManagedRegistrationBackupCreateReceipt{
					Disposition:        ManagedRegistrationBackupCommitted,
					CleanupDisposition: ManagedRegistrationBackupCleanupNotRequired,
				},
				readContents: []byte("not-the-captured-config"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(`[mcp_servers.serena]
url = "http://127.0.0.1:9137/mcp"
startup_timeout_sec = 10
`), 0o600); err != nil {
				t.Fatal(err)
			}
			client := newLockingClient(&codexCLI{path: path})
			managed := requireW10ManagedRegistrationClient(t, client)
			before, err := managed.SnapshotManagedRegistration(context.Background(), "serena")
			if err != nil {
				t.Fatal(err)
			}
			desired := MCPEntry{Name: "serena", URL: "http://127.0.0.1:9137/mcp"}
			target, err := ManagedRegistrationTargetFingerprint(desired, false)
			if err != nil {
				t.Fatal(err)
			}
			beforeWrites := 0
			writerCalls := 0
			originalWriter := WriteConfigFile
			WriteConfigFile = func(string, []byte) error {
				writerCalls++
				return errors.New("unexpected client config write")
			}
			t.Cleanup(func() { WriteConfigFile = originalWriter })

			observed := managed.ConditionalManagedRegistrationMutation(context.Background(), ManagedRegistrationMutationRequest{
				EntryName:                desired.Name,
				ExpectedConfigSHA256:     before.ConfigSHA256,
				ExpectedEntrySHA256:      before.EntrySHA256,
				ExpectedConfigPathSHA256: before.ConfigPathSHA256,
				ExpectedEntryKeySHA256:   before.EntryKeySHA256,
				ExpectedBaseline:         before,
				ExpectedTargetSHA256:     target,
				Desired:                  desired,
				BackupIO:                 &tt.backup,
				BeforeWrite: func(ManagedRegistrationMutationPreparation) error {
					beforeWrites++
					return nil
				},
			})
			if observed.Invoked || observed.Phase != ManagedRegistrationPhaseBackup || observed.Failure != ManagedRegistrationFailureBackup || observed.Settlement == ManagedRegistrationSettlementVerified {
				t.Fatalf("backup failure observation = %+v, want typed pre-writer backup failure", observed)
			}
			if beforeWrites != 0 || writerCalls != 0 {
				t.Fatalf("backup failure reached prepare=%d writer=%d, want both zero", beforeWrites, writerCalls)
			}
			if len(tt.backup.createLeaves) != 1 || tt.backup.createLeaves[0] == "" {
				t.Fatalf("candidate backup identity = %#v, want one retained opaque leaf", tt.backup.createLeaves)
			}
			if tt.name == "commit_uncertain" && len(tt.backup.readLeaves) != 0 {
				t.Fatalf("commit-uncertain create performed a read: %#v", tt.backup.readLeaves)
			}
			if tt.name != "commit_uncertain" && len(tt.backup.readLeaves) != 1 {
				t.Fatalf("committed create read count = %d, want one", len(tt.backup.readLeaves))
			}
		})
	}
}

func requireW10ManagedRegistrationClient(t *testing.T, client Client) ManagedRegistrationClient {
	t.Helper()
	managed, ok := AsManagedRegistrationClient(client)
	if !ok {
		t.Fatalf("AsManagedRegistrationClient(%T) = false", client)
	}
	return managed
}
