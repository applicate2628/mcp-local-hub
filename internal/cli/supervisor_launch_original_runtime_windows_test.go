//go:build windows

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

// Exercise the command's real composition with native original/staged files.
// Both starts deliberately fail the namespace gate before any child is created.
// Only the legacy metadata lookup is supplied so the readiness branch remains
// observable; this test grants no process-control or successful health proof.
func TestUpgradeSupervisorReadinessFollowsStartedBranchForIdenticalProduct(t *testing.T) {
	f := newQualifiedUpgradeGUICommandFixture(t)
	api.EnableSupervisorIPCTestPipeIsolation()
	if !strings.Contains(api.SupervisorIPCAddress(f.stateDir), "-test-") {
		t.Fatal("readiness dispatch test requires an isolated absent pipe")
	}
	source, err := binaryadmission.CanonicalEntryTargetsV1(filepath.Join(filepath.Dir(filepath.Dir(f.candidate)), binaryadmission.WindowsCanonicalEntryRelativePathV1))
	if err != nil {
		t.Fatal(err)
	}
	targets, err := binaryadmission.CanonicalEntryTargetsV1(f.target)
	if err != nil {
		t.Fatal(err)
	}
	apitest.HardenedDir(t, filepath.Dir(targets.ProductPath))
	for _, pair := range [][2]string{
		{source.CanonicalEntryPath, targets.CanonicalEntryPath},
		{source.ProductPath, targets.ProductPath},
		{source.IdentityPath, targets.IdentityPath},
	} {
		body, err := os.ReadFile(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pair[1], body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stop := errors.New("readiness composition exercised before transaction construction")
	factoryCalls := 0
	priorFactory, priorLookup := newWindowsProductTxnFn, processLookupIdentityFn
	t.Cleanup(func() {
		newWindowsProductTxnFn, processLookupIdentityFn = priorFactory, priorLookup
	})
	newWindowsProductTxnFn = func(req windowsProductTxnRequest) (*WindowsProductTxn, error) {
		factoryCalls++
		if req.EntrySet == nil || req.EntrySet.original == nil || req.EntrySet.prior == nil {
			t.Fatal("native command composition did not retain the complete prior")
		}
		original, err := req.EntrySet.prior.RuntimeProduct(req.Context)
		if err != nil || original == nil {
			t.Fatalf("receive original runtime: %v", err)
		}
		staged, err := req.EntrySet.original.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		artifact := original.Artifact()
		if artifact.Path != req.ProductPath || artifact.SHA256 != staged.Product.SHA256 {
			t.Fatal("test must use identical prior/successor Product path and digest")
		}
		if err := req.EntrySet.prior.Close(); err != nil {
			t.Fatal(err)
		}
		retainedPath := filepath.Join(filepath.Dir(req.ProductPath), "prior-kept.exe")
		if err := os.Rename(req.ProductPath, retainedPath); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(req.ProductPath, f.priorBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		// Windows FileInfo may resolve its file ID lazily in SameFile. Keep
		// both distinct objects addressable at the paths recorded by Stat.
		before, err := os.Stat(retainedPath)
		if err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(req.ProductPath)
		if err != nil || os.SameFile(before, after) {
			t.Fatalf("same-byte successor did not become a different native object: %v", err)
		}
		owner := api.SupervisorLockOwner{PID: 4242, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if err := api.WriteStateFileAtomic(filepath.Join(req.StateDir, "supervisor.lock.owner.json"), owner); err != nil {
			t.Fatal(err)
		}
		createdUnix := time.Now().Add(-time.Second).Unix()
		processLookupIdentityFn = func(int) (process.ProcessIdentity, error) {
			return process.ProcessIdentity{ExecutablePath: req.ProductPath, CommandLine: `"` + req.ProductPath + `" supervise`, CreationDateUnix: createdUnix}, nil
		}
		// StartSupervisor's existing namespace guard refuses both branches before
		// spawning. Readiness must follow the selected branch even on that error.
		restoreState := api.SetDaemonStateRootForTest(apitest.HardenedTempDir(t))
		defer restoreState()
		for _, branch := range []string{"successor", "prior", "successor"} {
			start := req.StartSupervisor
			if branch == "prior" {
				start = req.RestartPrior
			}
			launch, startErr := start(req.ProductPath)
			if launch != nil || startErr == nil || !strings.Contains(startErr.Error(), "namespace/target differs") {
				if launch != nil {
					retainCanonicalGUIReplayCleanup(t, launch)
				}
				t.Fatalf("%s start crossed the no-process namespace boundary: %v", branch, startErr)
			}
			readyCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			readyErr := req.WaitSupervisorReady(readyCtx, req.ProductPath, artifact)
			cancel()
			want := "DialPipe"
			if branch == "prior" {
				want = "validate original supervisor runtime"
			}
			if readyErr == nil || !strings.Contains(readyErr.Error(), want) {
				t.Fatalf("%s readiness selected the wrong image authority: %v", branch, readyErr)
			}
		}
		return nil, stop
	}
	cmd := newUpgradeCmdReal()
	cmd.SetContext(t.Context())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err = runV5UpgradeWindowsWithPathsAndDepsAndInput(cmd, f.candidate, f.target, upgradeGUIFencedDeps{}, f.input)
	if !errors.Is(err, stop) || err.Error() != stop.Error() || factoryCalls != 1 {
		t.Fatalf("command composition/cleanup=(%v, factory calls %d)", err, factoryCalls)
	}
}
