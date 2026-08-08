package gui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

var errAuditLockPostRenameTest = errors.New("injected occurrence-store writer failure")

type auditLockWriterFailureMode string

const (
	auditLockWriterNormal        auditLockWriterFailureMode = "normal"
	auditLockWriterBeforeWrite   auditLockWriterFailureMode = "before-write"
	auditLockWriterAfterWrite    auditLockWriterFailureMode = "write-then-error"
	auditLockWriterThirdState    auditLockWriterFailureMode = "third-state"
	auditLockWriterRereadFailure auditLockWriterFailureMode = "reread-failure"
)

func installAuditLockWriterMode(t *testing.T, adapter *auditLockAdapter, mode auditLockWriterFailureMode) {
	t.Helper()
	adapter.writeStateFileLockHeld = func(path string, raw []byte) error {
		switch mode {
		case auditLockWriterNormal:
			return api.WriteStateFileBytesLockHeld(path, raw)
		case auditLockWriterBeforeWrite:
			return errAuditLockPostRenameTest
		case auditLockWriterAfterWrite:
			if err := api.WriteStateFileBytesLockHeld(path, raw); err != nil {
				return err
			}
			return errAuditLockPostRenameTest
		case auditLockWriterThirdState:
			store, err := decodeAuditLockStore(raw)
			if err != nil {
				return err
			}
			store.ActiveServerInstance = "44444444-4444-4444-8444-444444444444"
			third, err := json.MarshalIndent(store, "", "  ")
			if err != nil {
				return err
			}
			third = append(third, '\n')
			if err := api.WriteStateFileBytesLockHeld(path, third); err != nil {
				return err
			}
			return errAuditLockPostRenameTest
		case auditLockWriterRereadFailure:
			if err := os.Remove(path); err != nil {
				return err
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				return err
			}
			return errAuditLockPostRenameTest
		default:
			t.Fatalf("unknown writer mode %q", mode)
			return errors.New("unreachable writer mode")
		}
	}
}

func readAuditLockStoreForTest(t *testing.T, path string) auditLockOccurrenceStore {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err := decodeAuditLockStore(raw)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestAuditLockReserveWriterReconciliationMatrix(t *testing.T) {
	tests := []struct {
		mode            auditLockWriterFailureMode
		wantStatus      int
		wantCode        daemonRecoverErrorCode
		wantNovel       bool
		wantSettlements int
	}{
		{mode: auditLockWriterNormal, wantNovel: true, wantSettlements: 1},
		{mode: auditLockWriterBeforeWrite, wantStatus: http.StatusInternalServerError, wantCode: daemonRecoverErrorAuditLockAdapterInit},
		{mode: auditLockWriterAfterWrite, wantNovel: true, wantSettlements: 1},
		{mode: auditLockWriterThirdState, wantStatus: http.StatusConflict, wantCode: daemonRecoverErrorOutcomeUncertain},
		{mode: auditLockWriterRereadFailure, wantStatus: http.StatusConflict, wantCode: daemonRecoverErrorOutcomeUncertain},
	}
	for index, test := range tests {
		t.Run(string(test.mode), func(t *testing.T) {
			events := NewBroadcaster()
			defer events.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			published := events.Subscribe(ctx)
			adapter := newDirectTestAuditLockAdapterInStateDir(events, t.TempDir())
			defer adapter.close()
			before := readAuditLockStoreForTest(t, adapter.storePath)
			installAuditLockWriterMode(t, adapter, test.mode)
			correlation := validAuditLockCorrelation(adapter.serverInstance, 200+index)
			binding := auditLockOccurrenceBinding{serverInstance: correlation.ServerInstance, taskName: `\mcp-local-hub-reconcile-` + string(test.mode), confirm: true}
			reservation, routeErr := adapter.reserve(context.Background(), correlation, binding)

			if test.wantStatus == 0 {
				if routeErr != nil || reservation.Novel != test.wantNovel || reservation.Settlement == nil {
					t.Fatalf("reservation=%+v err=%v", reservation, routeErr)
				}
			} else if routeErr == nil || routeErr.status != test.wantStatus || routeErr.code != string(test.wantCode) || reservation.Novel {
				t.Fatalf("reservation=%+v err=%v want status=%d code=%s", reservation, routeErr, test.wantStatus, test.wantCode)
			}
			if got := auditLockSettlementCount(adapter); got != test.wantSettlements {
				t.Fatalf("settlements=%d want=%d", got, test.wantSettlements)
			}

			switch test.mode {
			case auditLockWriterNormal, auditLockWriterAfterWrite:
				store := readAuditLockStoreForTest(t, adapter.storePath)
				if len(store.Records) != 1 || store.Records[0].OccurrenceID != correlation.OccurrenceID {
					t.Fatalf("durable store=%+v", store)
				}
				replay, replayErr := adapter.reserve(context.Background(), correlation, binding)
				if replayErr != nil || replay.Novel {
					t.Fatalf("replay=%+v err=%v", replay, replayErr)
				}
			case auditLockWriterBeforeWrite:
				if store := readAuditLockStoreForTest(t, adapter.storePath); !reflect.DeepEqual(store, before) {
					t.Fatalf("store changed: got=%+v before=%+v", store, before)
				}
			}

			if test.mode == auditLockWriterAfterWrite {
				select {
				case event := <-published:
					if event.Type != "daemon-recovery-occurrence-store-write-reconciled" || event.Body["operation"] != "reserve occurrence" || event.Body["failure_id"] != "post_rename_exact_reread" || event.Body["data_outcome"] != "durable_proven" {
						t.Fatalf("event=%+v", event)
					}
				case <-time.After(time.Second):
					t.Fatal("reconciled reserve event not published")
				}
			}
		})
	}
}

func prepareTerminalAuditLockRecord(t *testing.T, adapter *auditLockAdapter, correlation auditLockCorrelation, taskName string) {
	t.Helper()
	binding := auditLockOccurrenceBinding{serverInstance: correlation.ServerInstance, taskName: taskName, confirm: true}
	reservation, routeErr := adapter.reserve(context.Background(), correlation, binding)
	if routeErr != nil || !reservation.Novel {
		t.Fatalf("reserve=%+v err=%v", reservation, routeErr)
	}
	if _, routeErr := adapter.terminalize(reservation, auditLockOccurrenceNotCommitted, auditLockAuthorizationNone, notCommittedTerminalEvidence()); routeErr != nil {
		t.Fatalf("terminalize: %v", routeErr)
	}
}

func TestAuditLockAcknowledgeWriterReconciliationMatrix(t *testing.T) {
	tests := []struct {
		name       string
		mode       auditLockWriterFailureMode
		nonFinal   bool
		wantStatus int
		wantCode   daemonRecoverErrorCode
	}{
		{name: "final normal", mode: auditLockWriterNormal},
		{name: "final before write", mode: auditLockWriterBeforeWrite, wantStatus: 500, wantCode: daemonRecoverErrorAuditLockAdapterInit},
		{name: "final write then error", mode: auditLockWriterAfterWrite},
		{name: "final third state", mode: auditLockWriterThirdState, wantStatus: 409, wantCode: daemonRecoverErrorOutcomeUncertain},
		{name: "final reread failure", mode: auditLockWriterRereadFailure, wantStatus: 409, wantCode: daemonRecoverErrorOutcomeUncertain},
		{name: "nonfinal normal", mode: auditLockWriterNormal, nonFinal: true},
		{name: "nonfinal before write", mode: auditLockWriterBeforeWrite, nonFinal: true, wantStatus: 500, wantCode: daemonRecoverErrorAuditLockAdapterInit},
		{name: "nonfinal write then error", mode: auditLockWriterAfterWrite, nonFinal: true},
		{name: "nonfinal third state", mode: auditLockWriterThirdState, nonFinal: true, wantStatus: 409, wantCode: daemonRecoverErrorOutcomeUncertain},
		{name: "nonfinal reread failure", mode: auditLockWriterRereadFailure, nonFinal: true, wantStatus: 409, wantCode: daemonRecoverErrorOutcomeUncertain},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := newDirectTestAuditLockAdapterInStateDir(nil, t.TempDir())
			defer adapter.close()
			first := validAuditLockCorrelation(adapter.serverInstance, 300+index*2)
			prepareTerminalAuditLockRecord(t, adapter, first, `\mcp-local-hub-ack-first-`+test.name)
			if test.nonFinal {
				second := validAuditLockCorrelation(adapter.serverInstance, 301+index*2)
				second.AttemptID = "22222222-2222-4222-8222-222222222222"
				prepareTerminalAuditLockRecord(t, adapter, second, `\mcp-local-hub-ack-second-`+test.name)
			}
			beforeInstance, beforeGeneration := adapter.currentIdentity()
			beforeSettlements := auditLockSettlementCount(adapter)
			installAuditLockWriterMode(t, adapter, test.mode)
			routeErr := adapter.acknowledge(context.Background(), first)

			if test.wantStatus == 0 {
				if routeErr != nil {
					t.Fatalf("acknowledge: %v", routeErr)
				}
				if got := auditLockSettlementCount(adapter); got != beforeSettlements {
					t.Fatalf("settlements=%d want=%d", got, beforeSettlements)
				}
				instance, generation := adapter.currentIdentity()
				if test.nonFinal {
					if instance != beforeInstance || generation != beforeGeneration {
						t.Fatalf("nonfinal identity=(%s,%d) before=(%s,%d)", instance, generation, beforeInstance, beforeGeneration)
					}
				} else if instance == beforeInstance || generation != beforeGeneration+1 {
					t.Fatalf("final identity=(%s,%d) before=(%s,%d)", instance, generation, beforeInstance, beforeGeneration)
				}
				adapter.writeStateFileLockHeld = api.WriteStateFileBytesLockHeld
				next := validAuditLockCorrelation(instance, 500+index)
				next.AttemptID = "33333333-3333-4333-8333-333333333333"
				nextReservation, nextErr := adapter.reserve(context.Background(), next, auditLockOccurrenceBinding{serverInstance: instance, taskName: `\mcp-local-hub-after-ack-` + test.name, confirm: true})
				if nextErr != nil || !nextReservation.Novel {
					t.Fatalf("immediate reserve=%+v err=%v", nextReservation, nextErr)
				}
			} else {
				if routeErr == nil || routeErr.status != test.wantStatus || routeErr.code != string(test.wantCode) {
					t.Fatalf("ack err=%v want status=%d code=%s", routeErr, test.wantStatus, test.wantCode)
				}
				instance, generation := adapter.currentIdentity()
				if instance != beforeInstance || generation != beforeGeneration || auditLockSettlementCount(adapter) != beforeSettlements {
					t.Fatalf("failed ack changed memory identity=(%s,%d) settlements=%d", instance, generation, auditLockSettlementCount(adapter))
				}
			}
		})
	}
}

func TestAuditLockReconciledWriteCloseFailurePreservesContinuations(t *testing.T) {
	t.Run("reserve", func(t *testing.T) {
		factory := newScriptedOccurrenceStoreLockFactory()
		factory.specs[2] = scriptedOccurrenceStoreLockSpec{closeErr: errors.New("injected close failure")}
		adapter := newDirectTestAuditLockAdapterInStateDirWithStoreLockDeps(nil, t.TempDir(), factory.newLock, nil)
		defer adapter.close()
		installAuditLockWriterMode(t, adapter, auditLockWriterAfterWrite)
		correlation := validAuditLockCorrelation(adapter.serverInstance, 601)
		reservation, routeErr := adapter.reserve(context.Background(), correlation, auditLockOccurrenceBinding{serverInstance: correlation.ServerInstance, taskName: `\mcp-local-hub-close-reserve`, confirm: true})
		assertOccurrenceStoreStrandedRoute(t, routeErr)
		if !reservation.Novel || reservation.Settlement == nil || auditLockSettlementCount(adapter) != 1 || routeErr.auditLockStateProjection == nil {
			t.Fatalf("reservation=%+v settlements=%d projection=%+v", reservation, auditLockSettlementCount(adapter), routeErr.auditLockStateProjection)
		}
	})

	t.Run("final acknowledge", func(t *testing.T) {
		factory := newScriptedOccurrenceStoreLockFactory()
		factory.specs[4] = scriptedOccurrenceStoreLockSpec{closeErr: errors.New("injected close failure")}
		adapter := newDirectTestAuditLockAdapterInStateDirWithStoreLockDeps(nil, t.TempDir(), factory.newLock, nil)
		defer adapter.close()
		correlation := validAuditLockCorrelation(adapter.serverInstance, 602)
		prepareTerminalAuditLockRecord(t, adapter, correlation, `\mcp-local-hub-close-ack`)
		beforeInstance, beforeGeneration := adapter.currentIdentity()
		installAuditLockWriterMode(t, adapter, auditLockWriterAfterWrite)
		routeErr := adapter.acknowledge(context.Background(), correlation)
		assertOccurrenceStoreStrandedRoute(t, routeErr)
		instance, generation := adapter.currentIdentity()
		if instance == beforeInstance || generation != beforeGeneration+1 || auditLockSettlementCount(adapter) != 0 {
			t.Fatalf("identity=(%s,%d) before=(%s,%d) settlements=%d", instance, generation, beforeInstance, beforeGeneration, auditLockSettlementCount(adapter))
		}
	})
}
