//go:build windows

package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the actual qualification constructor and its original owners.
// The inert command supplies native/IO closure, not installation authority.
func TestPreparedSerenaQualificationProfilesUseOwnedRoot(t *testing.T) {
	r := preparedRequest()
	r.Namespace = hardenedTempDir(t)
	r.WorkLimit, r.CleanupLimit = time.Minute, time.Minute
	r.WorkDeadline = time.Now().Add(r.WorkLimit)
	r.AbsoluteCleanupDeadline = r.WorkDeadline.Add(r.CleanupLimit)
	work, cancelWork := context.WithDeadline(context.Background(), r.WorkDeadline)
	defer cancelWork()
	executable, _, err := qualifyPreparedSerenaFile(work, os.Getenv("COMSPEC"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runPreparedSerenaInvocation(work, preparedSerenaInvocation{
		OperationID: r.OperationID, Executable: executable,
		Args: []string{"/d", "/c", "exit", "/b", "0"}, Environment: os.Environ(),
		WorkingDir: r.Namespace, PolicySHA256: r.PolicySHA256,
		CleanupLimit: r.CleanupLimit, AbsoluteCleanupDeadline: r.AbsoluteCleanupDeadline,
		StdoutLimit: 4096, StderrLimit: 4096,
	})
	if err != nil || !result.Closure.complete(r.OperationID) || !result.Closure.RootExitKnown || result.Closure.RootExitCode != 0 {
		t.Fatal("original inert command did not settle", err)
	}
	key, err := r.Identity()
	if err != nil {
		t.Fatal(err)
	}
	a := &preparedSerenaAcquisition{inputs: SerenaPreparationInputs{Request: r}}
	defer func() {
		if err := a.closeOriginalRoots(); err != nil {
			t.Error("original tree close did not complete", err)
		}
	}()
	roots, err := a.acquireRoots(work, r, key)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := a.generation.Inventory(work)
	if err != nil {
		t.Fatal(err)
	}
	installed := PreparedSerenaInstallation{Closure: result.Closure, Entrypoint: executable, InventorySHA256: inventory.SHA256}
	profiles, err := AcquireUnpublishedSerenaQualificationResources(work, r, installed, "qualification-profiles")
	if profiles != nil {
		defer func() {
			proof, closeErr := profiles.CloseUnstartedObserved(work)
			if closeErr != nil || proof.Validate(work, profiles) != nil {
				t.Error("original profile cleanup unsettled", closeErr)
			}
		}()
	}
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := profiles.ReleaseReservationObserved(work)
	if err != nil || reservation.Validate(profiles) != nil {
		t.Fatal("original reservation release failed", err)
	}
	host, err := profiles.HostInputs(work, PreparedSerenaQualificationRequest{
		QualificationID: "qualification-profiles", Request: r,
		Inputs: SerenaPreparationInputs{Request: r, EnvironmentProjectionSHA256: r.BuildEnvironmentSHA256},
		Roots:  roots, Installation: installed, WorkDeadline: r.WorkDeadline,
		CleanupDeadline: r.AbsoluteCleanupDeadline, Resources: profiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	ownedRoot, err := profiles.tree.WorkingDirectory(work)
	if err != nil {
		t.Fatal(err)
	}
	environment := preparedSerenaEnvironmentMap(host.Environment)
	if err := ValidatePreparedSerenaEnvironment(environment); err != nil {
		t.Fatal("qualification environment broke import isolation", err)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA"} {
		if environment[key] != ownedRoot {
			t.Fatalf("%s is not bound to the original qualification root", key)
		}
	}
	// No ambient profile may satisfy the consumers below. This test is not
	// parallel, and t.Setenv restores the original process environment.
	for _, key := range []string{"HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA"} {
		t.Setenv(key, "")
	}
	if _, err := os.UserHomeDir(); err == nil {
		t.Fatal("closed environment unexpectedly resolved an ambient home")
	}
	for _, entry := range host.Environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			t.Fatal("invalid qualification environment entry")
		}
		t.Setenv(key, value)
	}
	for _, consumer := range []struct {
		name string
		read func() (string, error)
	}{{"home", os.UserHomeDir}, {"config", os.UserConfigDir}, {"cache", os.UserCacheDir}} {
		path, err := consumer.read()
		if err != nil || filepath.Clean(path) != ownedRoot {
			t.Fatalf("%s resolved outside original qualification root: %q, %v", consumer.name, path, err)
		}
	}
	// This target retains parent observations under the original work context.
	// Keep it live until the same resource owner completes cleanup.
	proof, err := profiles.CloseUnstartedObserved(work)
	if err != nil || proof.Validate(work, profiles) != nil || !proof.tree.Complete() {
		t.Fatal("original profile cleanup did not complete", err)
	}
	if _, err := os.Stat(ownedRoot); !os.IsNotExist(err) {
		t.Fatal("profile tree still present", err)
	}
}
