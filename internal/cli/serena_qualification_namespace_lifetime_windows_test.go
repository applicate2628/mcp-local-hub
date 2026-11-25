//go:build windows && serena_qualification && test_state_path_env

package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// This test runs only in the source-issued qualification image. It observes
// native namespace ownership without installing a package or selecting Product.
func TestSerenaQualificationNamespaceLifetime(t *testing.T) {
	stateDir, _ := migrateSerenaTestEnv(t)
	deadline := time.Now().Add(2 * time.Minute)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Before(deadline) {
		deadline = testDeadline
	}
	// Distinct live deadlines exercise renewal without waiting for expiration.
	requestDeadline := deadline.Add(-time.Second)
	if !time.Now().Before(requestDeadline) {
		t.Fatal("qualification namespace lifetime test requires a live bounded request")
	}
	scopeCtx, cancelScope := context.WithDeadline(context.Background(), deadline)
	requestCtx, cancelRequest := context.WithDeadline(scopeCtx, requestDeadline)
	scope, err := api.OpenSerenaQualificationNamespace(scopeCtx, stateDir)
	if err != nil {
		cancelRequest()
		cancelScope()
		t.Fatalf("open real qualification namespace: %v", err)
	}
	t.Cleanup(func() {
		closeErr := scope.Close()
		cancelRequest()
		cancelScope()
		if closeErr != nil {
			t.Errorf("close real qualification namespace: %v", closeErr)
		}
	})

	path, observation, err := api.ResolveDaemonStateDirObserved(requestCtx)
	if err != nil || path != stateDir || observation == nil {
		t.Fatalf("qualified resolution = %q, observation=%t, err=%v; want %q", path, observation != nil, err, stateDir)
	}
	if err := observation.Validate(requestCtx, stateDir); err != nil {
		t.Fatalf("validate original namespace observation: %v", err)
	}
	view := observation.Snapshot()
	if view.Schema != "state-dir-serena-qualification-v1" || view.Version != 1 ||
		!view.Complete || view.Qualification == nil || view.PrimaryError != nil || len(view.CleanupErrors) != 0 ||
		view.ReleaseKind != "borrowed-retained-directory" {
		t.Fatalf("private namespace snapshot lacks original qualification facts: %+v", view)
	}
	if view.HRESULTDefined || view.HRESULT != nil || view.AllocationAcquired ||
		view.DecodeSucceeded || view.FreeCalled || view.FreeReturned {
		t.Fatalf("private namespace claimed unobserved Known Folder allocation or release: %+v", view)
	}

	overlapping, err := api.OpenSerenaQualificationNamespace(scopeCtx, stateDir)
	if overlapping != nil {
		closeErr := overlapping.Close()
		t.Fatalf("overlapping scope was returned: open=%v, close=%v", err, closeErr)
	}
	if err == nil {
		t.Fatal("overlapping qualification scope was accepted")
	}

	assertResolutionRefused := func(t *testing.T, ctx context.Context) {
		t.Helper()
		path, received, err := api.ResolveDaemonStateDirObserved(ctx)
		if err == nil || path != "" || received != nil {
			t.Fatalf("resolution = %q, observation=%t, err=%v; want refusal without a result", path, received != nil, err)
		}
		if err := observation.Validate(ctx, stateDir); err == nil {
			t.Fatal("original namespace observation survived an invalid binding or lifetime")
		}
	}
	for _, env := range []string{"MCPHUB_STATE_DIR_OVERRIDE", "MCPHUB_SERENA_QUALIFICATION_RUN_ROOT"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, filepath.Join(stateDir, "different-root"))
			assertResolutionRefused(t, requestCtx)
		})
	}
	t.Run("in-memory state root", func(t *testing.T) {
		t.Cleanup(api.SetDaemonStateRootForTest(filepath.Join(stateDir, "different-root")))
		assertResolutionRefused(t, requestCtx)
	})
	if err := observation.Validate(requestCtx, stateDir); err != nil {
		t.Fatalf("restored original namespace binding did not validate: %v", err)
	}
	if err := observation.Validate(requestCtx, filepath.Join(stateDir, "different-root")); err == nil {
		t.Fatal("original observation validated a different resolved path")
	}

	// The longer request is valid for this scope. Only the older observation
	// must reject it; acquiring a new observation cannot renew the old one.
	longerPath, longerObservation, err := api.ResolveDaemonStateDirObserved(scopeCtx)
	if err != nil || longerPath != stateDir || longerObservation == nil {
		t.Fatalf("live longer request failed to resolve: path=%q, observation=%t, err=%v", longerPath, longerObservation != nil, err)
	}
	if err := longerObservation.Validate(scopeCtx, stateDir); err != nil {
		t.Fatalf("validate newly observed longer request: %v", err)
	}
	if err := observation.Validate(scopeCtx, stateDir); err == nil ||
		!strings.Contains(err.Error(), "SERENA_QUALIFICATION_RESOLUTION_DEADLINE_RENEWED") {
		t.Fatalf("old observation renewed its deadline or failed for another reason: %v", err)
	}
	t.Run("canceled request", func(t *testing.T) {
		canceledCtx, cancel := context.WithCancel(requestCtx)
		cancel()
		assertResolutionRefused(t, canceledCtx)
	})
	if err := observation.Validate(requestCtx, stateDir); err != nil {
		t.Fatalf("canceling a separate request invalidated the live original scope: %v", err)
	}

	if err := scope.Close(); err != nil {
		t.Fatalf("close original namespace scope: %v", err)
	}
	if err := scope.Close(); err != nil {
		t.Fatalf("repeat close did not preserve the completed result: %v", err)
	}
	assertResolutionRefused(t, requestCtx)

	reopenedCtx, cancelReopened := context.WithDeadline(context.Background(), deadline)
	reopenedRequestCtx, cancelReopenedRequest := context.WithDeadline(reopenedCtx, requestDeadline)
	reopened, err := api.OpenSerenaQualificationNamespace(reopenedCtx, stateDir)
	if err != nil {
		cancelReopenedRequest()
		cancelReopened()
		t.Fatalf("reopen same native directory under a new scope: %v", err)
	}
	t.Cleanup(func() {
		closeErr := reopened.Close()
		cancelReopenedRequest()
		cancelReopened()
		if closeErr != nil {
			t.Errorf("close reopened namespace scope: %v", closeErr)
		}
	})
	reopenedPath, reopenedObservation, err := api.ResolveDaemonStateDirObserved(reopenedRequestCtx)
	if err != nil || reopenedPath != stateDir || reopenedObservation == nil {
		t.Fatalf("new scope resolution = %q, observation=%t, err=%v", reopenedPath, reopenedObservation != nil, err)
	}
	if err := reopenedObservation.Validate(reopenedRequestCtx, stateDir); err != nil {
		t.Fatalf("validate new scope's own observation: %v", err)
	}
	// Keep the original deadline: rejection must follow the scope identity,
	// even when path, physical directory, source and request bound are reused.
	if err := observation.Validate(reopenedRequestCtx, stateDir); err == nil ||
		!strings.Contains(err.Error(), "SERENA_QUALIFICATION_RESOLUTION_BINDING_DIFFERS") {
		t.Fatalf("closed observation survived replacement by a new scope or failed for another reason: %v", err)
	}
}
