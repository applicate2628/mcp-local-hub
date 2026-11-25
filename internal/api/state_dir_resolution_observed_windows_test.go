//go:build windows && !serena_qualification

package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStateDirObserved_SelectedBuildSource(t *testing.T) {
	nativeParent, err := realKnownFolderLocalAppData()
	if err != nil {
		t.Fatalf("read native KnownFolder baseline: %v", err)
	}
	nativeRoot := joinStateRoot(nativeParent)
	for _, tc := range []struct {
		name                                             string
		memory, environment, knownFolder, local, profile bool
	}{
		{name: "memory overrides environment", memory: true, environment: true, knownFolder: true, local: true, profile: true},
		{name: "environment overrides known folder", environment: true, knownFolder: true, local: true, profile: true},
		{name: "known folder before fallback", knownFolder: true, local: true, profile: true},
		{name: "local appdata fallback", local: true, profile: true},
		{name: "user profile fallback", profile: true},
		{name: "no tagged source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statePathsHelper(t)
			root := t.TempDir()
			memoryRoot := filepath.Join(root, "memory-state")
			environmentRoot := filepath.Join(root, "environment-state")
			knownParent := filepath.Join(root, "known-parent")
			localParent := filepath.Join(root, "local-parent")
			profileParent := filepath.Join(root, "profile-parent")
			envValue := func(enabled bool, value string) string {
				if enabled {
					return value
				}
				return ""
			}
			daemonStateRootOverride = envValue(tc.memory, memoryRoot)
			t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", envValue(tc.environment, environmentRoot))
			t.Setenv("LOCALAPPDATA", envValue(tc.local, localParent))
			t.Setenv("USERPROFILE", envValue(tc.profile, profileParent))
			knownCalls := 0
			installKnownFolderStub(t, func() (string, error) {
				knownCalls++
				if tc.knownFolder {
					return knownParent, nil
				}
				return "", errors.New("controlled KnownFolder refusal")
			})

			want := nativeRoot
			testSource := tc.memory || testEnvFallbackBuild
			wantFailure := false
			switch {
			case tc.memory:
				want = memoryRoot
			case !testEnvFallbackBuild:
				// The normal Windows entry must ignore environment inputs and
				// the legacy string-only KnownFolder test seam.
			case tc.environment:
				want = environmentRoot
			case tc.knownFolder:
				want = joinStateRoot(knownParent)
			case tc.local:
				want = joinStateRoot(localParent)
			case tc.profile:
				want = joinStateRoot(filepath.Join(profileParent, "AppData", "Local"))
			default:
				wantFailure = true
			}
			if testSource && !wantFailure {
				if _, err := os.Stat(want); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("test destination must start absent: %q, %v", want, err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			got, observation, err := ResolveDaemonStateDirObserved(ctx)
			if wantFailure {
				if !errors.Is(err, errKnownFolderUnavailable) || got != "" || observation == nil || observation.Snapshot().Complete {
					t.Fatalf("missing tagged source: path=%q observation=%+v error=%v", got, observation, err)
				}
				if observation.Validate(ctx, filepath.Join(root, "unissued")) == nil {
					t.Fatal("failed source issued a valid observation")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveDaemonStateDirObserved: %v", err)
			}
			assertStateDirObservedSource(t, ctx, got, observation, want, testSource)
			if testSource {
				if _, err := os.Stat(want); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("observed resolution created its destination: %q, %v", want, err)
				}
			}
			if (tc.memory || tc.environment || !testEnvFallbackBuild) && knownCalls != 0 {
				t.Fatalf("higher-priority source consulted legacy KnownFolder seam %d time(s)", knownCalls)
			}
		})
	}
}

func assertStateDirObservedSource(t *testing.T, ctx context.Context, got string, observation *StateDirResolutionObservation, want string, testSource bool) {
	t.Helper()
	if got != want {
		t.Fatalf("observed state path = %q, want %q", got, want)
	}
	if err := observation.Validate(ctx, got); err != nil {
		t.Fatalf("original state-path observation did not validate: %v", err)
	}
	view := observation.Snapshot()
	if !view.Complete || view.PathSHA256 != stateFileReadObservationDigest([]byte(want)) {
		t.Fatalf("path receipt = %+v", view)
	}
	if testSource {
		source := "test-state-path-v1|testing.Testing|daemonStateRootOverride"
		if testEnvFallbackBuild {
			source = "test-state-path-v1|test_state_path_env|daemonStateDirReadOnly"
		}
		if view.SourceSHA256 != stateFileReadObservationDigest([]byte(source)) || view.ReleaseKind != "test-state-path-readonly" {
			t.Fatalf("test source was not identified explicitly: %+v", view)
		}
		if view.HRESULTDefined || view.HRESULT != nil || view.AllocationAcquired || view.DecodeSucceeded || view.FreeCalled || view.FreeReturned {
			t.Fatalf("test path claimed native KnownFolder allocation/release facts: %+v", view)
		}
		return
	}
	nativeSource := "canonical-state-dir-v1|shell32!SHGetKnownFolderPath|FOLDERID_LocalAppData|flags=0|token=current-null|" + stateDirName
	if view.SourceSHA256 != stateFileReadObservationDigest([]byte(nativeSource)) || view.ReleaseKind != "CoTaskMemFree-void" ||
		!view.HRESULTDefined || view.HRESULT == nil || int32(*view.HRESULT) < 0 || !view.AllocationAcquired || !view.DecodeSucceeded || !view.FreeCalled || !view.FreeReturned {
		t.Fatalf("native source lost original allocation/release facts: %+v", view)
	}
}

func TestStateDirObserved_TestPathRejectsDriftAndForgedFacts(t *testing.T) {
	statePathsHelper(t)
	root := filepath.Join(t.TempDir(), "selected-state")
	daemonStateRootOverride = root
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, observation, err := ResolveDaemonStateDirObserved(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertStateDirObservedSource(t, ctx, got, observation, root, true)
	if observation.Validate(ctx, root+"-different") == nil {
		t.Fatal("observation admitted a different path")
	}
	forged := &StateDirResolutionObservation{path: root, view: observation.Snapshot()}
	if forged.Validate(ctx, root) == nil {
		t.Fatal("snapshot without its original source issued authority")
	}
	before := observation.view
	for _, tc := range []struct {
		name   string
		mutate func(*StateDirResolutionSnapshotV1)
	}{
		{"path digest", func(v *StateDirResolutionSnapshotV1) { v.PathSHA256 = strings.Repeat("0", 64) }},
		{"source digest", func(v *StateDirResolutionSnapshotV1) { v.SourceSHA256 = strings.Repeat("0", 64) }},
		{"native allocation", func(v *StateDirResolutionSnapshotV1) { v.AllocationAcquired = true }},
		{"native release", func(v *StateDirResolutionSnapshotV1) { v.FreeCalled, v.FreeReturned = true, true }},
		{"native result", func(v *StateDirResolutionSnapshotV1) { value := uint32(0); v.HRESULTDefined, v.HRESULT = true, &value }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observation.view = before
			defer func() { observation.view = before }()
			tc.mutate(&observation.view)
			if observation.Validate(ctx, root) == nil {
				t.Fatal("altered facts validated against the original test source")
			}
		})
	}
	daemonStateRootOverride = root + "-changed"
	if observation.Validate(ctx, root) == nil {
		t.Fatal("observation did not recheck the original selected source")
	}
}

func TestStateDirObserved_RejectsInvalidTestPath(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
		relative     bool
	}{
		{name: "relative", suffix: "relative-state", relative: true},
		{name: "nul", suffix: "\x00invalid"},
		{name: "invalid utf8", suffix: string([]byte{0xff})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statePathsHelper(t)
			daemonStateRootOverride = filepath.Join(t.TempDir(), "state") + tc.suffix
			if tc.relative {
				daemonStateRootOverride = tc.suffix
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			got, observation, err := ResolveDaemonStateDirObserved(ctx)
			if err == nil || got != "" || observation == nil || observation.Snapshot().Complete {
				t.Fatalf("invalid test path admitted: path=%q observation=%+v error=%v", got, observation, err)
			}
		})
	}
}

func TestStateDirObserved_TestPathHonorsContext(t *testing.T) {
	statePathsHelper(t)
	root := filepath.Join(t.TempDir(), "state")
	daemonStateRootOverride = root
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, observation, err := ResolveDaemonStateDirObserved(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	assertStateDirObservedSource(t, ctx, got, observation, root, true)
	cancel()
	if err := observation.Validate(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed context validation = %v, want context.Canceled", err)
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want error
	}{
		{name: "canceled", ctx: ctx, want: context.Canceled},
		{name: "expired", want: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testCtx := tc.ctx
			if testCtx == nil {
				var stop context.CancelFunc
				testCtx, stop = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer stop()
			}
			got, failed, err := ResolveDaemonStateDirObserved(testCtx)
			if !errors.Is(err, tc.want) || got != "" || failed == nil || failed.Snapshot().Complete {
				t.Fatalf("closed-context source: path=%q observation=%+v error=%v", got, failed, err)
			}
		})
	}
}
