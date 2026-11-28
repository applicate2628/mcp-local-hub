package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/clients"
)

func TestPrepareOwnedBackendCompensationRejectsIncompleteAbsence(t *testing.T) {
	for _, injectCloseFailure := range []bool{false, true} {
		name := "pure-missing-creates-owned-record"
		if injectCloseFailure {
			name = "missing-and-close-failure-preserves-empty-root"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			closeErr := errors.New("injected compensation-reader close failure")
			faults := &stateReadNativeFaults{}
			if injectCloseFailure {
				faults.closeErrors = []error{closeErr}
			}
			ref := &ManagedBackendInstallReceiptV1{reference: "backend-fixture.json", digest: projectClientHash([]byte("capsule"))}
			calls := 0
			readBackup := func(ctx context.Context, gotRoot string, parts []string, digest string) ([]byte, error) {
				calls++
				if gotRoot != root || len(parts) != 1 || parts[0] != ref.reference+".inverse.json" || digest != "" {
					t.Fatal("compensation consumer changed its original backup-read inputs")
				}
				return readStateBeneathRootWithNativeFaults(ctx, gotRoot, parts, stateReadBeneathRootPolicy{
					maxBytes: MaxClientConfigBackupBytes, expectedSHA256: digest, allowEmptyExpectedSHA256: true,
				}, nil, faults)
			}
			record, terminal, err := prepareOwnedBackendCompensation(context.Background(), root, ref, projectClientHash([]byte("target")), readBackup)
			if calls != 1 || terminal {
				t.Errorf("consumer calls=%d terminal=%v; want one read and a nonterminal result", calls, terminal)
			}
			faults.check(t, 1, 0)
			if injectCloseFailure {
				if !errors.Is(err, closeErr) || !errors.Is(err, os.ErrNotExist) {
					t.Errorf("compensation consumer lost a combined read/close cause: %v", err)
				}
				entries, readErr := os.ReadDir(root)
				if readErr != nil || len(entries) != 0 {
					t.Errorf("incomplete absence published an inverse record: entries=%v error=%v", entries, readErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("pure absence no longer creates the owned record: %v", err)
			}
			want, marshalErr := json.Marshal(record)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			got, readErr := os.ReadFile(filepath.Join(root, ref.reference+".inverse.json"))
			if readErr != nil || !bytes.Equal(got, want) {
				t.Errorf("pure absence record bytes differ: read error=%v", readErr)
			}
		})
	}
}

func TestAcquireNoManagedBackendConsumersRetainsReadCloseFailure(t *testing.T) {
	root := t.TempDir()
	backupID, err := clients.ParseManagedRegistrationBackupID("fixture.json.bak-mcp-local-hub-20261122-000000")
	if err != nil {
		t.Fatal(err)
	}
	digest := projectClientHash([]byte("fixture"))
	capsule := managedBackendInstallCapsule{
		Schema: "managed-backend-install-v1", OperationID: "other", TargetGroupSHA256: digest,
		ManifestSHA256: digest, PlanSHA256: digest, NamespaceSHA256: projectClientHash([]byte(root)),
		PriorIntentSHA256: digest, ReceivedIntentSHA256: digest, PriorScopeSHA256: digest, ReceivedScopeSHA256: digest,
		InverseBackupID: backupID, Ownership: "reused", Publication: "received",
		Activation: managedBackendActivation{State: "unreceived", Supervisor: SupervisorLockOwner{PID: 1, StartedAt: "2000-01-01T00:00:00Z"}},
	}
	raw, err := json.Marshal(capsule)
	if err != nil {
		t.Fatal(err)
	}
	// This is inert fixture data. The consumer only validates and reads it;
	// it never adopts the synthetic PID, runs a backend, or mutates a task.
	if _, err := decodeManagedBackendInstallCapsule(raw); err != nil {
		t.Fatalf("invalid capsule fixture: %v", err)
	}
	path := filepath.Join(root, "backend-other.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	closeErr := errors.New("injected consumer-scan reader close failure")
	faults := &stateReadNativeFaults{closeErrors: []error{closeErr}}
	var capsuleReads, inverseReads int
	readBackup := func(ctx context.Context, gotRoot string, parts []string, expected string) ([]byte, error) {
		if gotRoot != root || len(parts) != 1 || expected != "" {
			t.Fatal("consumer scan changed its original backup-read inputs")
		}
		switch parts[0] {
		case "backend-other.json":
			capsuleReads++
			return ReadClientConfigBackupBeneathRootNoFollow(ctx, gotRoot, parts, expected)
		case "backend-other.json.inverse.json":
			inverseReads++
			return readStateBeneathRootWithNativeFaults(ctx, gotRoot, parts, stateReadBeneathRootPolicy{
				maxBytes: MaxClientConfigBackupBytes, allowEmptyExpectedSHA256: true,
			}, nil, faults)
		default:
			t.Fatalf("unexpected consumer read %q", parts[0])
			return nil, nil
		}
	}
	err = acquireNoManagedBackendConsumers(context.Background(), root, "fixture-server", digest, "own", readBackup)
	if capsuleReads != 1 || inverseReads != 1 {
		t.Errorf("consumer oracle not reached: capsule reads=%d inverse reads=%d error=%v", capsuleReads, inverseReads, err)
	}
	if !errors.Is(err, closeErr) || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("consumer replaced the combined read/close failure with a pending-consumer diagnostic: %v", err)
	}
	faults.check(t, 1, 0)
	unchanged, readErr := os.ReadFile(path)
	entries, dirErr := os.ReadDir(root)
	if readErr != nil || dirErr != nil || !bytes.Equal(unchanged, raw) || len(entries) != 1 {
		t.Errorf("consumer changed its fixture root: read error=%v dir error=%v entries=%v", readErr, dirErr, entries)
	}
}
