//go:build windows

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/api/apitest"
)

func fixturePassiveBackendRunHold() SupervisorBackendRunHold {
	return SupervisorBackendRunHold{
		Version: 1, Installation: strings.Repeat("a", 64), StopID: strings.Repeat("b", 64),
		Original: SupervisorLockOwner{PID: 31, StartedAt: "2026-11-20T00:00:00Z"},
		Work: []SupervisorBackendStopWork{
			{Operation: 1, Task: "fixture", Outcome: SupervisorBackendStopPending, PendingPhase: "fixture-settlement"},
		},
	}
}

func fixturePassiveBackendIntent(t *testing.T, file *SupervisorIntentFile) (string, []byte) {
	t.Helper()
	root := apitest.HardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(root))
	path := filepath.Join(root, "supervisor-intent.json")
	raw, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	// Seed data in the private test only. The ordinary writer under test may
	// receive an existing hold, but it has no authority to originate one.
	if err := WriteStateFileBytesAtomic(path, raw); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

func TestSupervisorIntentWriterPreservesPassiveBackendRunHold(t *testing.T) {
	hold := fixturePassiveBackendRunHold()
	path, _ := fixturePassiveBackendIntent(t, &SupervisorIntentFile{Version: 1, BackendRunHold: hold})
	desired := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{TaskName: "fixture-added"}}}
	if err := WriteSupervisorIntent(path, desired); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.BackendRunHold, hold) || !errors.Is(got.BackendStartError(), ErrSupervisorBackendHeld) ||
		len(got.Daemons) != 1 || got.Daemons[0].TaskName != "fixture-added" {
		t.Fatal("ordinary write lost the hold or failed to publish the descriptor")
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	different := fixturePassiveBackendRunHold()
	different.StopID = strings.Repeat("c", 64)
	if err := WriteSupervisorIntent(path, &SupervisorIntentFile{Version: 1, BackendRunHold: different}); !errors.Is(err, ErrSupervisorBackendHoldInvalid) {
		t.Fatal("ordinary write accepted a different hold")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("refused ordinary write changed private intent bytes: %v", err)
	}
}

func TestSupervisorIntentWriterCannotMintPassiveBackendRunHold(t *testing.T) {
	path, before := fixturePassiveBackendIntent(t, &SupervisorIntentFile{Version: 1})
	desired := &SupervisorIntentFile{Version: 1, BackendRunHold: fixturePassiveBackendRunHold()}
	if err := WriteSupervisorIntent(path, desired); !errors.Is(err, ErrSupervisorBackendHoldInvalid) {
		t.Fatal("ordinary writer minted a hold")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("refused mint changed private intent bytes: %v", err)
	}
}

func TestSupervisorIntentWriterRefusesMalformedPassiveBackendRunHold(t *testing.T) {
	for _, tc := range []struct{ name, replacement string }{
		{"omitted_settled", ""},
		{"null_settled", "\"settled\":null,"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, valid := fixturePassiveBackendIntent(t, &SupervisorIntentFile{Version: 1, BackendRunHold: fixturePassiveBackendRunHold()})
			before := bytes.Replace(valid, []byte("\"settled\":false,"), []byte(tc.replacement), 1)
			if bytes.Equal(before, valid) {
				t.Fatal("malformed boolean fixture did not change")
			}
			if err := WriteStateFileBytesAtomic(path, before); err != nil {
				t.Fatal(err)
			}
			if err := WriteSupervisorIntent(path, &SupervisorIntentFile{Version: 1}); !errors.Is(err, ErrSupervisorBackendHoldInvalid) {
				t.Fatalf("ordinary writer did not refuse malformed current hold: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("ordinary writer changed malformed hold bytes: %v", err)
			}
		})
	}
}

func TestSupervisorIntentLastDescriptorRemovalPreservesPassiveBackendRunHold(t *testing.T) {
	for _, name := range []string{"hold_only_remainder", "legacy_empty_removed"} {
		t.Run(name, func(t *testing.T) {
			initial := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{TaskName: "fixture-last"}}}
			if name == "hold_only_remainder" {
				initial.BackendRunHold = fixturePassiveBackendRunHold()
			}
			path, _ := fixturePassiveBackendIntent(t, initial)
			if err := removeSupervisorIntentDescriptorAndStop(path, "fixture-last", true, supervisorStopArtifacts{}); err != nil {
				t.Fatal(err)
			}
			if name == "legacy_empty_removed" {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("empty V1 intent retained after last descriptor removal: %v", err)
				}
				return
			}
			got, err := ReadSupervisorIntent(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Daemons) != 0 || !reflect.DeepEqual(got.BackendRunHold, initial.BackendRunHold) {
				t.Fatal("last descriptor removal did not preserve a hold-only file")
			}
		})
	}
}

func TestParsedObservedSupervisorIntentPreparationPreservesPassiveBackendRunHold(t *testing.T) {
	hold := fixturePassiveBackendRunHold()
	path, prior := fixturePassiveBackendIntent(t, &SupervisorIntentFile{Version: 1, BackendRunHold: hold, IntentGeneration: 4})
	// These are the actual shared semantic steps beneath observed publication
	// and preflight. This test does not acquire the native observed namespace.
	desired := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{TaskName: "fixture-replacement"}}}
	prepare := prepareObservedSupervisorIntent(path, func(current *SupervisorIntentFile) (bool, error) {
		replacement, err := cloneSupervisorIntentReplacement(current, desired)
		if err != nil {
			return false, err
		}
		*current = *replacement
		return true, nil
	})
	after, changed, err := prepare(true, prior)
	if err != nil || !changed {
		t.Fatalf("parsed replacement did not prepare: %v", err)
	}
	got, _, err := decodeSupervisorIntentFile(path, after)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.BackendRunHold, hold) || !errors.Is(got.BackendStartError(), ErrSupervisorBackendHeld) ||
		got.IntentGeneration != 5 || len(got.Daemons) != 1 || got.Daemons[0].TaskName != "fixture-replacement" {
		t.Fatal("prepared observed replacement did not carry the exact hold")
	}

	// Direct observed mutations cannot bypass the parsed replacement rules
	// and remove the original hold with an unrestricted typed callback.
	clear := prepareObservedSupervisorIntent(path, func(current *SupervisorIntentFile) (bool, error) {
		current.BackendRunHold = SupervisorBackendRunHold{}
		return true, nil
	})
	if raw, _, err := clear(true, prior); !errors.Is(err, ErrSupervisorBackendHoldInvalid) || len(raw) != 0 {
		t.Fatal("direct observed mutation prepared a hold clear")
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, prior) {
		t.Fatalf("semantic preparation wrote private intent bytes: %v", err)
	}
}
