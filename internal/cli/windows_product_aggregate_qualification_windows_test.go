package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/gui"
)

func TestWindowsProductAggregateIssuerRefusal(t *testing.T) {
	t.Run("nonterminal_gui_restart", func(t *testing.T) {
		stateDir := apitest.HardenedTempDir(t)
		namespace, err := api.OpenWindowsProductStateNamespace(t.Context(), stateDir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := namespace.Close(); err != nil {
				t.Error(err)
			}
		})
		if record, err := readQuiescentAggregateGUIRestartBound(t.Context(), stateDir, namespace); record != nil || err != nil {
			t.Fatalf("genuine absent marker: %v %v", record, err)
		}
		store := gui.NewHandoffMarkerStore(stateDir, gui.DefaultRestartDeadlines())
		if _, err := store.Begin(gui.HandoffBegin{Generation: "isolated-qualification-fixture", Route: gui.HandoffRouteSamePort, OldPort: 12341, NewPort: 12341, OldPID: 1}); err != nil {
			t.Fatal(err)
		}
		if record, err := readQuiescentAggregateGUIRestartBound(t.Context(), stateDir, namespace); record != nil || err == nil {
			t.Fatalf("nonterminal marker was admitted: %v %v", record, err)
		}
		if original, err := store.Read(); err != nil || original.Phase != gui.HandoffPhaseInProgress {
			t.Fatalf("refusal changed owner marker: %v %v", original, err)
		}
		if _, err := readQuiescentAggregateGUIRestartBound(t.Context(), filepath.Join(stateDir, "unknown\x00"), namespace); err == nil {
			t.Fatal("unknown marker namespace admitted")
		}
	})
	t.Run("before_native_decision", func(t *testing.T) {
		veto := errors.New("original qualification guard refusal")
		txn := &WindowsProductTxn{aroundCommitDecision: func(context.Context, func() error) error { return veto }}
		attempted, confirmed, err := txn.recordProductCommitDecision(t.Context())
		if attempted || confirmed || !errors.Is(err, veto) {
			t.Fatalf("guard refusal was changed into a native decision: %t %t %v", attempted, confirmed, err)
		}
		var q productAggregateQualification
		if err := q.capturePrepared(txn); err == nil {
			t.Fatal("issuer admitted missing native Prepare")
		}
		invoked := false
		if err := q.beforeDecision(t.Context(), func() error { invoked = true; return nil }); err == nil || invoked {
			t.Fatal("issuer attempted writer without authenticated successor")
		}
	})
}

func TestUpgradeProductAggregateAppliesOnceAfterRelease(t *testing.T) {
	stateDir := withTempStateDir(t)
	fence, err := api.AcquireUpgradeFence(t.Context(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fence.Release() })
	var order []string
	guiLease := &upgradeGUICommandLease{stateDir: stateDir, order: &order}
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	calls := 0
	const completion = "fixture Product upgrade complete"
	next := upgradeProductContinuation{apply: func() error {
		calls++
		if guiLease.released != 1 || output.Len() != 0 {
			t.Error("Apply ran before GUI release or after success output")
		}
		reopened, acquired, err := api.TryAcquireUpgradeFence(t.Context(), stateDir)
		if err != nil || !acquired {
			return errors.Join(errors.New("Apply ran before original Product fence release"), err)
		}
		return reopened.Release()
	}}
	err = finishV5UpgradeWindowsCommand(cmd, false, completion, nil, nil, guiLease, fence, next)
	if err != nil || calls != 1 || guiLease.released != 1 || output.String() != completion+"\n" {
		t.Fatalf("finalizer result=%v, Apply=%d, GUI releases=%d, output=%q", err, calls, guiLease.released, output.String())
	}
}

func TestUpgradeProductAggregateReleaseErrorWithholdsApply(t *testing.T) {
	stateDir := withTempStateDir(t)
	fence, err := api.AcquireUpgradeFence(t.Context(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fence.Release() })
	var order []string
	releaseErr := errors.New("original GUI reservation release unconfirmed")
	guiLease := &upgradeGUICommandLease{stateDir: stateDir, order: &order, releaseErr: releaseErr}
	var output bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&output)
	calls := 0
	next := upgradeProductContinuation{apply: func() error {
		calls++
		return nil
	}}
	err = finishV5UpgradeWindowsCommand(cmd, false, "fixture Product upgrade complete", nil, nil, guiLease, fence, next)
	if !errors.Is(err, releaseErr) || calls != 0 || guiLease.released != 1 || output.Len() != 0 {
		t.Fatalf("finalizer result=%v, Apply=%d, GUI releases=%d, output=%q", err, calls, guiLease.released, output.String())
	}
	reopened, acquired, err := api.TryAcquireUpgradeFence(t.Context(), stateDir)
	if err != nil || !acquired {
		t.Fatal("original Product fence did not settle", err)
	}
	if err := reopened.Release(); err != nil {
		t.Fatal(err)
	}
}
