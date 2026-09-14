package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type installLeaseTestEntry struct {
	name string
	run  func(*API, InstallOpts, string) error
}

func installLeaseTestEntries() []installLeaseTestEntry {
	return []installLeaseTestEntry{
		{"single", func(a *API, opts InstallOpts, _ string) error { return a.Install(opts) }},
		{"command", func(a *API, opts InstallOpts, _ string) error {
			return a.installWithFrozenPlan(context.Background(), opts, nil)
		}},
		{"bulk-embed", func(a *API, opts InstallOpts, _ string) error { return a.installUsingEmbedFirst(opts) }},
		{"bulk-directory", func(a *API, opts InstallOpts, dir string) error { return a.installFromManifestDir(opts, dir) }},
	}
}

func TestInstallManifestLeasePrecedesManifestRead(t *testing.T) {
	for _, entry := range installLeaseTestEntries() {
		t.Run(entry.name, func(t *testing.T) {
			const name = "install-lease-missing"
			_, dir, _ := setupAdoptTestEnv(t, name, "[mcp_servers]\n")
			lease, acquired, err := tryAcquireAdoptManifestLease(name)
			if err != nil || !acquired {
				t.Fatalf("acquire recovery lease: acquired=%v err=%v", acquired, err)
			}
			t.Cleanup(func() { _ = lease.Unlock() })
			opts := InstallOpts{Server: name, Writer: io.Discard}
			err = entry.run(NewAPI(), opts, dir)
			var failure *LeaseFailure
			if !errors.As(err, &failure) || !failure.Retryable {
				t.Fatalf("install error=%v, want retryable lease refusal before loading the absent manifest", err)
			}
			if err := lease.Unlock(); err != nil {
				t.Fatal(err)
			}
			lease = nil
			// A fresh attempt after recovery closes must reload, never reuse a frozen plan.
			if err := entry.run(NewAPI(), opts, dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("install after recovery error=%v, want missing manifest", err)
			}
			again, acquired, err := tryAcquireAdoptManifestLease(name)
			if err != nil || !acquired {
				t.Fatalf("failed install leaked lease: acquired=%v err=%v", acquired, err)
			}
			if err := again.Unlock(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInstallManifestLeasePreventsRecoveryAfterPlanFreeze(t *testing.T) {
	for _, entry := range installLeaseTestEntries() {
		t.Run(entry.name, func(t *testing.T) {
			name := "install-lease-frozen-" + entry.name
			dir, stateDir, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
			a := NewAPI()
			recovery, err := a.BuildDeAdoptPlan(name)
			if err != nil || !recovery.ProviderRecoveryExecutable() {
				t.Fatalf("recovery=%+v err=%v", recovery, err)
			}
			preparePreflightBinaryChecks(t)
			oldPort, oldAdmission := preflightPortInUse, frozenStdioBridgeAdmissionFn
			t.Cleanup(func() { preflightPortInUse = oldPort; frozenStdioBridgeAdmissionFn = oldAdmission })
			preflightPortInUse = func(int) bool { return false }
			stop := errors.New("stop install at frozen-plan boundary")
			calls := 0
			frozenStdioBridgeAdmissionFn = func(context.Context, []FrozenStdioBridgeAdmissionRequest) error {
				calls++
				_, recoveryErr := a.executeDeAdoptPlanWithOpts(recovery, io.Discard, ExecuteDeAdoptOpts{
					providerDeps: providerTransactionDeps{source: provider},
				})
				if recoveryErr == nil || !strings.Contains(recoveryErr.Error(), "concurrent operation") {
					t.Errorf("recovery during frozen Install error=%v, want lease contention before any teardown", recoveryErr)
				}
				// This lease-only seam starts no process and therefore explicitly
				// certifies cleanup. An unqualified error now correctly preserves
				// started history; actual uncertain/reaped processes have separate tests.
				return &stdioBridgeAdmissionReapedError{cause: stop}
			}
			if err := entry.run(a, InstallOpts{Server: name, Writer: io.Discard}, dir); !errors.Is(err, stop) {
				t.Fatalf("install error=%v, want frozen admission stop", err)
			}
			if calls != 1 {
				t.Fatalf("frozen admission calls=%d, want 1", calls)
			}
			if provider.calls != 0 || provider.entry.Enabled {
				t.Fatalf("provider restored under frozen Install: calls=%d enabled=%v", provider.calls, provider.entry.Enabled)
			}
			current, found, err := ReadAdoptProvenance(name)
			if err != nil || !found || current.OperationState != AdoptOperationStateAdopting {
				t.Fatalf("recovery changed provenance: found=%v rec=%+v err=%v", found, current, err)
			}
			if _, err := os.Stat(filepath.Join(dir, name, "manifest.yaml")); err != nil {
				t.Fatalf("recovery removed frozen manifest: %v", err)
			}
			// The failed Install releases its lease; the very same real recovery may now finish.
			if _, err := a.executeDeAdoptPlanWithOpts(recovery, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err != nil {
				t.Fatalf("recovery after Install exits: %v", err)
			}
			assertDeAdoptClosed(t, dir, stateDir, *rec)
		})
	}
}

func TestInstallManifestLeaseHeldAtAudit(t *testing.T) {
	for _, entry := range installLeaseTestEntries() {
		t.Run(entry.name, func(t *testing.T) {
			name := "install-lease-audit-" + entry.name
			dir, _, _, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
			preparePreflightBinaryChecks(t)
			oldPort, oldAdmission, oldAudit := preflightPortInUse, frozenStdioBridgeAdmissionFn, appendIntentAuditFn
			t.Cleanup(func() {
				preflightPortInUse = oldPort
				frozenStdioBridgeAdmissionFn = oldAdmission
				appendIntentAuditFn = oldAudit
			})
			preflightPortInUse = func(int) bool { return false }
			frozenStdioBridgeAdmissionFn = func(context.Context, []FrozenStdioBridgeAdmissionRequest) error { return nil }
			stop := errors.New("stop install at audit barrier")
			calls := 0
			appendIntentAuditFn = func(e IntentAuditEntry) error {
				if e.Action != AuditActionServerInstall {
					return nil
				}
				calls++
				other, acquired, err := tryAcquireAdoptManifestLease(name + "-unrelated")
				if err != nil || !acquired {
					t.Fatalf("unrelated manifest serialized: acquired=%v err=%v", acquired, err)
				}
				if err := other.Unlock(); err != nil {
					t.Fatal(err)
				}
				same, acquired, err := tryAcquireAdoptManifestLease(name)
				if acquired {
					_ = same.Unlock()
					t.Error("Install released its manifest lease before apply")
				}
				if err != nil {
					var failure *LeaseFailure
					if !errors.As(err, &failure) || !failure.Retryable {
						t.Fatalf("unexpected lease probe failure: %v", err)
					}
				}
				return stop
			}
			if err := entry.run(NewAPI(), InstallOpts{Server: name, Writer: io.Discard}, dir); !errors.Is(err, stop) {
				t.Fatalf("install error=%v, want audit refusal", err)
			}
			if calls != 1 {
				t.Fatalf("audit calls=%d, want 1", calls)
			}
		})
	}
}

func TestInstallManifestLeaseDryRunDoesNotContend(t *testing.T) {
	for _, entry := range installLeaseTestEntries() {
		t.Run(entry.name, func(t *testing.T) {
			name := "install-lease-dry-" + entry.name
			dir, _, _, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
			preparePreflightBinaryChecks(t)
			oldPort := preflightPortInUse
			t.Cleanup(func() { preflightPortInUse = oldPort })
			preflightPortInUse = func(int) bool { return false }
			lease, acquired, err := tryAcquireAdoptManifestLease(name)
			if err != nil || !acquired {
				t.Fatalf("acquire recovery lease: acquired=%v err=%v", acquired, err)
			}
			t.Cleanup(func() { _ = lease.Unlock() })
			if err := entry.run(NewAPI(), InstallOpts{Server: name, DryRun: true, Writer: io.Discard}, dir); err != nil {
				t.Fatalf("read-only preview contended with recovery: %v", err)
			}
			phase, err := readProviderInstallPhase(name)
			if err != nil || phase != providerInstallPhaseNotStarted {
				t.Fatalf("dry-run mutated Install history: phase=%q err=%v", phase, err)
			}
		})
	}
}

func TestInstallManifestLeaseReportsReleaseFailure(t *testing.T) {
	const name = "install-lease-release"
	_, dir, _ := setupAdoptTestEnv(t, name, "[mcp_servers]\n")
	cause := errors.New("injected release observation")
	old := adoptLeaseUnlockFailureHook
	t.Cleanup(func() { adoptLeaseUnlockFailureHook = old })
	adoptLeaseUnlockFailureHook = func() error { return cause }
	err := NewAPI().Install(InstallOpts{Server: name, Writer: io.Discard})
	if !errors.Is(err, os.ErrNotExist) || !errors.Is(err, cause) {
		t.Fatalf("install error=%v, want both manifest and release errors", err)
	}
	if _, err := os.Stat(filepath.Join(dir, name, "manifest.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed install wrote manifest: %v", err)
	}
}

// Exercise the complete ordinary Install with no managed process or remote calls.
func setupInstallLeaseRemoteFixture(t *testing.T, name string) string {
	t.Helper()
	_, dir, _ := setupAdoptTestEnv(t, name, "[mcp_servers]\n")
	body := fmt.Sprintf("name: %s\nkind: global\ntransport: %s\nurl: https://example.invalid/mcp\nclient_bindings: []\n", name, config.TransportRemoteHTTP)
	if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	preparePreflightBinaryChecks(t)
	oldScheduler := schedulerFactoryFn
	t.Cleanup(func() { schedulerFactoryFn = oldScheduler })
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return nil, errors.New("test scheduler unavailable") }
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil }))
	return dir
}

func TestInstallManifestLeasePreservesOrdinaryLeaseSuffixName(t *testing.T) {
	const name = "ordinary-server.lease"
	dir := setupInstallLeaseRemoteFixture(t, name)
	for _, entry := range installLeaseTestEntries() {
		t.Run(entry.name, func(t *testing.T) {
			if err := entry.run(NewAPI(), InstallOpts{Server: name, Writer: io.Discard}, dir); err != nil {
				t.Fatalf("ordinary .lease install: %v", err)
			}
		})
	}
	// Snapshot-owning adoption must retain the collision refusal.
	if lease, acquired, err := tryAcquireAdoptManifestLease(name); err == nil || acquired || lease != nil {
		t.Fatalf("adopt accepted reserved snapshot name: acquired=%v err=%v", acquired, err)
	}
}

func TestInstallManifestLeaseCompletionFollowsRelease(t *testing.T) {
	for _, failRelease := range []bool{false, true} {
		label := "settled"
		if failRelease {
			label = "release-failed"
		}
		t.Run(label, func(t *testing.T) {
			const name = "install-completion-release"
			setupInstallLeaseRemoteFixture(t, name)
			var output bytes.Buffer
			var receipt InstallMutationReceiptV1
			cause := errors.New("injected install lease release failure")
			old := adoptLeaseUnlockFailureHook
			t.Cleanup(func() { adoptLeaseUnlockFailureHook = old })
			adoptLeaseUnlockFailureHook = func() error {
				if strings.Contains(output.String(), "Install complete") {
					t.Error("terminal success was printed before lease release settled")
				}
				if failRelease {
					return cause
				}
				return nil
			}
			err := NewAPI().installWithFrozenPlan(context.Background(), InstallOpts{Server: name, Writer: &output}, func(r InstallMutationReceiptV1) { receipt = r })
			if failRelease && !errors.Is(err, cause) || !failRelease && err != nil {
				t.Fatalf("Install error=%v, failRelease=%v", err, failRelease)
			}
			if !receipt.Committed {
				t.Fatal("lease settlement error lost the already committed mutation receipt")
			}
			if strings.Contains(output.String(), "Install complete") == failRelease {
				t.Fatalf("terminal completion mismatch: failRelease=%v output=%s", failRelease, output.String())
			}
		})
	}
}
