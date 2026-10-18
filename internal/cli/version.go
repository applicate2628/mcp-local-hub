package cli

import (
	"context"
	"encoding/json"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/process"
)

// normalizeExePath resolves symlinks and cleans an executable path so two
// references to the same on-disk binary compare equal. Best-effort: a
// resolve error returns the cleaned-but-unresolved path so the caller can
// still compare, and the caller treats a normalize failure as "no warning".
func normalizeExePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

// versionIdentityDiagnostic compares the process's native executable with the
// selected runtime image. It intentionally does not inspect PATH: an
// npm-generated command shim has script bytes and is not the executable that
// the shim already started.
func versionIdentityDiagnostic(runningExe string, runtimeTarget func() (string, error)) string {
	if runningExe == "" {
		return ""
	}
	canonicalExe, err := runtimeTarget()
	if err != nil || canonicalExe == "" {
		return fmt.Sprintf("identity-unverified: selected runtime image cannot be resolved for this running binary (%s): %v; %s", runningExe, err, identityReconciliationCommand())
	}
	return binaryIdentityDiagnostic(runningExe, canonicalExe)
}

// binaryIdentityDiagnostic compares the running executable against the
// selected runtime image. Different paths with the same bytes are an
// informational alternate path. Different or unreadable identities are an
// operator warning. Paths are compared case-insensitively only on Windows,
// where executable paths are case-insensitive; POSIX keeps exact case so
// case-only distinct paths on case-sensitive filesystems still compare bytes.
func binaryIdentityDiagnostic(runningExe, canonicalExe string) string {
	if runningExe == "" || canonicalExe == "" {
		return ""
	}
	a, errA := normalizeExePath(runningExe)
	b, errB := normalizeExePath(canonicalExe)
	if errA != nil || errB != nil {
		return ""
	}
	if sameExePath(a, b) {
		return ""
	}
	runningSHA256, errRunning := executableSHA256(a)
	canonicalSHA256, errCanonical := executableSHA256(b)
	if errRunning != nil || errCanonical != nil {
		return fmt.Sprintf("identity-unverified: selected runtime image (%s) cannot be verified against this running binary (%s): %v; %s", b, a, errors.Join(errRunning, errCanonical), identityReconciliationCommand())
	}
	if runningSHA256 == canonicalSHA256 {
		return fmt.Sprintf("equivalent runtime binary: selected runtime image (%s) is byte-identical to this running binary (%s).", b, a)
	}
	return fmt.Sprintf("binary identity differs: selected runtime image (%s) is NOT this running binary (%s); %s", b, a, identityReconciliationCommand())
}

func executableSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func identityReconciliationCommand() string {
	return "Exit the active Hub before replacing its native installation; then relaunch mcphub."
}

func sameExePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// SetBuildInfo retains the cli package's existing public surface so
// main.main() doesn't need to switch to a new import path. It
// forwards into the canonical buildinfo store, which both this
// package's `mcphub version` command and the gui's /api/version
// handler read from.
func SetBuildInfo(version, commit, date string) {
	buildinfo.Set(version, commit, date)
}

func newVersionCmdReal() *cobra.Command {
	var observe bool
	var deadline observationDeadlineFlag
	command := &cobra.Command{
		Use:   "version",
		Short: "Print version, commit, build metadata, and project homepage",
		Long: `Print build metadata: semantic version, short git commit, build date,
Go toolchain version, target platform, plus homepage / issue tracker
/ commercial licensing / license / author links.

Values are baked in at build time via build.sh / build.ps1 (which
injects ldflags). On Windows, only 'pwsh ./build.ps1' produces an
installable product binary and runs PE subsystem admission; bare
'go build' is a non-product compile check.

Example:
  mcphub version
  → mcp-local-hub 0.3.0
      commit:     38f6349
      build date: 2026-04-18T20:56:14Z
      go version: go1.26.2
      platform:   windows/amd64`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if observe {
				return observeRuntimePrerequisites(cmd.Context(), deadline, cmd.OutOrStdout(), versionObservationOperations{process.ObserveCurrentPublisherNative, api.NewCurrentPublisherImageObservation, api.ResolveDaemonStateDirObserved})
			}
			version, commit, date := buildinfo.Get()
			cmd.Printf("mcp-local-hub %s\n", version)
			cmd.Printf("  commit:     %s\n", commit)
			cmd.Printf("  build date: %s\n", date)
			cmd.Printf("  go version: %s\n", runtime.Version())
			cmd.Printf("  platform:   %s/%s\n", runtime.GOOS, runtime.GOARCH)
			// Running-binary location + canonical binary identity diagnostic.
			// This compares native executables only: npm command shims are not
			// binaries and therefore cannot produce a false identity warning.
			runningExe, _ := os.Executable()
			if runningExe != "" {
				cmd.Printf("  running:    %s\n", runningExe)
			}
			if diagnostic := versionIdentityDiagnostic(runningExe, api.RuntimeMcphubExecutable); diagnostic != "" {
				prefix := "⚠ identity"
				if strings.HasPrefix(diagnostic, "equivalent runtime binary:") {
					prefix = "ℹ identity"
				}
				cmd.Printf("  %s:   %s\n", prefix, diagnostic)
			}
			cmd.Println()
			cmd.Println("  homepage:   https://github.com/applicate2628/mcp-local-hub")
			cmd.Println("  issues:     https://github.com/applicate2628/mcp-local-hub/issues")
			cmd.Println("  commercial: https://github.com/applicate2628/mcp-local-hub/blob/master/README.md#license")
			cmd.Println("  license:    MPL-2.0")
			cmd.Println("  author:     Dmitry Denisenko (@applicate2628)")
			return nil
		},
	}
	command.Flags().BoolVar(&observe, "observe-runtime-prerequisites", false, "Observe this Product's native self, image and canonical state root")
	command.Flags().Var(&deadline, "observation-deadline", "Original prearmed absolute deadline (RFC3339Nano)")
	return command
}

// The parsed instant is time-bound input only. Root owns its outer allowance.
type observationDeadlineFlag struct { value string; count int }
func (f *observationDeadlineFlag) Set(value string) error { f.value = value; f.count++; return nil }
func (f *observationDeadlineFlag) String() string { return f.value }
func (f *observationDeadlineFlag) Type() string { return "RFC3339Nano" }

func observationContext(parent context.Context, flag observationDeadlineFlag) (context.Context, context.CancelFunc, error) {
	if parent == nil || flag.count != 1 || flag.value == "" { return nil, nil, errors.New("PRODUCT_OBSERVATION_DEADLINE_REQUIRED") }
	if err := parent.Err(); err != nil { return nil, nil, err }
	end, err := time.Parse(time.RFC3339Nano, flag.value)
	if err != nil { return nil, nil, errors.New("PRODUCT_OBSERVATION_DEADLINE_INVALID") }
	if parentEnd, finite := parent.Deadline(); finite && parentEnd.Before(end) { end = parentEnd }
	if !time.Now().Before(end) { return nil, nil, context.DeadlineExceeded }
	ctx, cancel := context.WithDeadline(parent, end)
	if err := ctx.Err(); err != nil { cancel(); return nil, nil, err }
	return ctx, cancel, nil
}

type versionObservationOperations struct {
	native func(context.Context) (*process.PublisherNativeObservation, error)
	image func(context.Context, *process.PublisherNativeObservation) (*api.CurrentPublisherImageObservation, error)
	root func(context.Context) (string, *api.StateDirResolutionObservation, error)
}

type currentProductObservationEnvelope struct {
	Schema string `json:"schema"`
	Root *api.StateDirResolutionSnapshotV1 `json:"root"`
	Native *process.PublisherNativeIncarnationV1 `json:"native"`
	Image *api.PublisherImageSnapshotV1 `json:"image"`
	Complete bool `json:"complete"`
	PrimaryError string `json:"primary_error"`
	CleanupErrors []string `json:"cleanup_errors"`
}

// Error rendering never calls an inner native error's Error method. Original
// causes remain available through Unwrap; wire diagnostics use fixed stages.
type currentProductObservationError struct { cause error }
func (e *currentProductObservationError) Error() string { return "PRODUCT_OBSERVATION_INCOMPLETE" }
func (e *currentProductObservationError) Unwrap() error { return e.cause }

func observeRuntimePrerequisites(parent context.Context, flag observationDeadlineFlag, out io.Writer, ops versionObservationOperations) error {
	view := currentProductObservationEnvelope{Schema: "current-product-observation-v1", CleanupErrors: []string{}}
	ctx, cancel, err := observationContext(parent, flag)
	if err != nil {
		view.PrimaryError = "PRODUCT_OBSERVATION_DEADLINE_REFUSED"
		writeErr := json.NewEncoder(out).Encode(view)
		return &currentProductObservationError{cause: errors.Join(err, writeErr)}
	}
	defer cancel()
	if ops.native == nil || ops.image == nil || ops.root == nil { return errors.New("PRODUCT_OBSERVATION_OWNER_MISSING") }
	native, nativeErr := ops.native(ctx)
	var image *api.CurrentPublisherImageObservation
	var imageErr error
	if native != nil {
		snapshot := native.NativeSnapshot(); view.Native = &snapshot
		_, cleanupErr := native.CaptureErrors()
		if cleanupErr != nil { view.CleanupErrors = append(view.CleanupErrors, "PRODUCT_NATIVE_CLOSE_FAILED") }
	}
	if nativeErr == nil { nativeErr = native.Validate(ctx) }
	if nativeErr == nil {
		image, imageErr = ops.image(ctx, native)
		if image != nil { snapshot := image.Snapshot(); view.Image = &snapshot }
	}
	// The independent original resolver is received even if native/image fails.
	path, root, rootErr := ops.root(ctx)
	if root != nil {
		snapshot := root.Snapshot(); view.Root = &snapshot
		for range snapshot.CleanupErrors { view.CleanupErrors = append(view.CleanupErrors, "PRODUCT_STATE_ROOT_CLOSE_FAILED") }
	}
	if nativeErr == nil { nativeErr = native.Validate(ctx) }
	if nativeErr == nil && imageErr == nil { imageErr = image.Validate(ctx, native) }
	if rootErr == nil { rootErr = root.Validate(ctx, path) }
	if nativeErr != nil { view.PrimaryError = "PRODUCT_NATIVE_OBSERVATION_FAILED" } else if imageErr != nil { view.PrimaryError = "PRODUCT_IMAGE_OBSERVATION_FAILED" } else if rootErr != nil { view.PrimaryError = "PRODUCT_STATE_ROOT_OBSERVATION_FAILED" } else if ctx.Err() != nil { view.PrimaryError = "PRODUCT_OBSERVATION_DEADLINE_EXPIRED" }
	err = errors.Join(nativeErr, imageErr, rootErr, ctx.Err())
	view.Complete = err == nil && len(view.CleanupErrors) == 0
	writeErr := json.NewEncoder(out).Encode(view)
	if err != nil || writeErr != nil || ctx.Err() != nil { return &currentProductObservationError{cause: errors.Join(err, writeErr, ctx.Err())} }
	return nil
}
