package api

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestUpgradeFenceTryAcquireBusyThenReacquiresFromInertLeaf(t *testing.T) {
	stateDir := t.TempDir()
	first, err := AcquireUpgradeFence(context.Background(), stateDir)
	if err != nil {
		t.Fatalf("AcquireUpgradeFence: %v", err)
	}

	second, acquired, err := TryAcquireUpgradeFence(context.Background(), stateDir)
	if err != nil || acquired || second != nil {
		t.Fatalf("contended TryAcquireUpgradeFence = lease=%v acquired=%v err=%v, want nil/false/nil", second, acquired, err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, UpgradeTransactionLockFileLeaf)); err != nil {
		t.Fatalf("lock leaf should remain inert after release: %v", err)
	}

	third, acquired, err := TryAcquireUpgradeFence(context.Background(), stateDir)
	if err != nil || !acquired || third == nil {
		t.Fatalf("reacquire from inert leaf = lease=%v acquired=%v err=%v, want lease/true/nil", third, acquired, err)
	}
	if err := third.Release(); err != nil {
		t.Fatalf("third Release: %v", err)
	}
}

func TestUpgradeFenceAcquireHonorsContextWhileBusy(t *testing.T) {
	stateDir := t.TempDir()
	holder, err := AcquireUpgradeFence(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Release() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if lease, err := AcquireUpgradeFence(ctx, stateDir); !errors.Is(err, context.Canceled) || lease != nil {
		t.Fatalf("cancelled acquire = lease=%v err=%v, want nil/context.Canceled", lease, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancelled acquire took %s", elapsed)
	}
}

func TestUpgradeFenceRejectsNonFileLeaf(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(stateDir, UpgradeTransactionLockFileLeaf), 0o700); err != nil {
		t.Fatal(err)
	}
	if lease, acquired, err := TryAcquireUpgradeFence(context.Background(), stateDir); err == nil || acquired || lease != nil {
		t.Fatalf("non-file leaf = lease=%v acquired=%v err=%v, want nil/false/error", lease, acquired, err)
	}
}

func TestUpgradeFenceReleaseIsOneShotAndPoisonsUnconfirmedLeaf(t *testing.T) {
	stateDir := t.TempDir()
	want := errors.New("injected upgrade-fence unlock failure")
	previous := flockUnlockFn
	var stranded *flock.Flock
	flockUnlockFn = func(fl *flock.Flock) error {
		stranded = fl
		return want
	}
	t.Cleanup(func() {
		flockUnlockFn = previous
		if stranded != nil {
			_ = stranded.Unlock()
		}
	})

	lease, err := AcquireUpgradeFence(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	first := lease.Release()
	second := lease.Release()
	if !errors.Is(first, ErrLockReleaseUnconfirmed) || !errors.Is(first, want) {
		t.Fatalf("first release error = %v", first)
	}
	if first.Error() != second.Error() {
		t.Fatalf("repeated release changed outcome: first=%v second=%v", first, second)
	}
	if later, _, err := TryAcquireUpgradeFence(context.Background(), stateDir); !errors.Is(err, ErrLockReleaseUnconfirmed) || later != nil {
		t.Fatalf("poisoned reacquire = lease=%v err=%v, want nil/ErrLockReleaseUnconfirmed", later, err)
	}
}

func TestUpgradeFenceReacquiresAfterHolderProcessDeath(t *testing.T) {
	if os.Getenv("MCPHUB_TEST_UPGRADE_FENCE_HOLDER") == "1" {
		stateDir := os.Getenv("MCPHUB_TEST_UPGRADE_FENCE_STATE_DIR")
		lease, err := AcquireUpgradeFence(context.Background(), stateDir)
		if err != nil || lease == nil {
			os.Exit(91)
		}
		if err := os.WriteFile(filepath.Join(stateDir, "holder-ready"), []byte("ready"), 0o600); err != nil {
			os.Exit(92)
		}
		for {
			time.Sleep(time.Hour)
		}
	}

	stateDir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestUpgradeFenceReacquiresAfterHolderProcessDeath$")
	cmd.Env = append(os.Environ(),
		"MCPHUB_TEST_UPGRADE_FENCE_HOLDER=1",
		"MCPHUB_TEST_UPGRADE_FENCE_STATE_DIR="+stateDir,
	)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(stateDir, "holder-ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child holder did not acquire fence")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = cmd.Process.Wait()

	lease, err := AcquireUpgradeFence(context.Background(), stateDir)
	if err != nil {
		t.Fatalf("reacquire after holder death: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("release after holder death: %v", err)
	}
}
