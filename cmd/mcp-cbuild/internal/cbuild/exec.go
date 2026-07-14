package cbuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// maxRawTail bounds the raw output echoed back in every tool result so nothing
// parseDiagnostics dropped is silently lost, without returning unbounded spew.
const maxRawTail = 8 * 1024

// hardCapTimeout is the absolute ceiling on any single exec, regardless of the
// per-call timeout requested by the client.
const hardCapTimeout = 60 * time.Minute

// execResult is the outcome of a single external-command run.
type execResult struct {
	ExitCode int
	Combined string // interleaved stdout+stderr
	TimedOut bool
	Canceled bool
	// startErr is set when the process could not be started or waited on for a
	// reason other than a non-zero exit (e.g. binary vanished, ctx canceled).
	startErr error
	WallMs   int64
}

// syncBuffer is an io.Writer safe for concurrent writes from the stdout and
// stderr pump goroutines, preserving interleaving order well enough for
// diagnostic parsing.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// runCommand executes bin with args in dir, bounded by timeout (clamped to the
// hard cap) and cancellable via ctx. stdout and stderr are captured together.
// There is NO shell: args are passed verbatim to exec.
func runCommand(ctx context.Context, timeout time.Duration, dir, bin string, args []string) execResult {
	if timeout <= 0 || timeout > hardCapTimeout {
		timeout = hardCapTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var out syncBuffer
	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Dir = dir
	cmd.Stdout = &out
	cmd.Stderr = &out

	start := time.Now()
	err := cmd.Run()
	wall := time.Since(start).Milliseconds()

	res := execResult{Combined: out.String(), WallMs: wall}

	if err == nil {
		res.ExitCode = 0
		return res
	}

	// Distinguish a clean non-zero exit from a start/wait failure.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
	} else {
		res.startErr = err
		res.ExitCode = -1
	}

	// Attribute deadline/cancel. Parent cancellation beats the local timeout.
	if ctx.Err() != nil {
		res.Canceled = true
	} else if runCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	return res
}

// rawTail returns the last maxRawTail bytes of s, prefixed with a truncation
// marker when the output was clipped.
func rawTail(s string) string {
	if len(s) <= maxRawTail {
		return s
	}
	tail := s[len(s)-maxRawTail:]
	// Start at the next newline so the tail begins on a clean line boundary.
	if nl := strings.IndexByte(tail, '\n'); nl >= 0 && nl < len(tail)-1 {
		tail = tail[nl+1:]
	}
	return "...[truncated]...\n" + tail
}

// exeName appends the platform executable suffix.
func exeName(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".exe"
	}
	return base
}

// resolveCMake resolves the cmake binary: CMAKE_BIN env override first, then
// PATH. Returns a clear fail-closed error when absent.
func resolveCMake() (string, error) {
	if p := os.Getenv("CMAKE_BIN"); p != "" {
		if fileExists(p) {
			return p, nil
		}
		return "", fmt.Errorf("CMAKE_BIN=%q does not point at an existing file", p)
	}
	p, err := exec.LookPath("cmake")
	if err != nil {
		return "", errors.New("cmake not found: set CMAKE_BIN or add cmake to PATH")
	}
	return p, nil
}

// resolveCTest resolves ctest, preferring the directory of CMAKE_BIN (ctest
// ships alongside cmake) before falling back to PATH.
func resolveCTest() (string, error) {
	if p := os.Getenv("CMAKE_BIN"); p != "" {
		cand := filepath.Join(filepath.Dir(p), exeName("ctest"))
		if fileExists(cand) {
			return cand, nil
		}
	}
	p, err := exec.LookPath("ctest")
	if err != nil {
		return "", errors.New("ctest not found: set CMAKE_BIN (ctest ships beside cmake) or add ctest to PATH")
	}
	return p, nil
}

// resolveVcpkg resolves the vcpkg binary: VCPKG_ROOT env (the vcpkg install
// root, binary lives at its top) first, then PATH. Returns a fail-closed error
// when absent — never a silent no-op.
func resolveVcpkg() (string, error) {
	if root := os.Getenv("VCPKG_ROOT"); root != "" {
		cand := filepath.Join(root, exeName("vcpkg"))
		if fileExists(cand) {
			return cand, nil
		}
		return "", fmt.Errorf("VCPKG_ROOT=%q set but %s not found under it", root, exeName("vcpkg"))
	}
	p, err := exec.LookPath("vcpkg")
	if err != nil {
		return "", errors.New("vcpkg not found: set VCPKG_ROOT or add vcpkg to PATH")
	}
	return p, nil
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

// resolveBuildDirWithinSource resolves a preset's binaryDir to a concrete
// absolute path and enforces that it is STRICTLY inside sourceDir. It is the
// single owner of the cmake_clean --purge_build_dir path-escape guard: it
// refuses an unresolved-macro path, a path equal to or outside sourceDir, and
// a filesystem root. Only a path that passes may be handed to RemoveAll.
func resolveBuildDirWithinSource(binaryDir, sourceDir, presetName string) (string, error) {
	if binaryDir == "" {
		return "", errors.New("preset declares no binaryDir")
	}
	expanded := expandPresetMacros(binaryDir, sourceDir, presetName)
	if strings.Contains(expanded, "${") || strings.Contains(expanded, "$env{") || strings.Contains(expanded, "$penv{") {
		return "", fmt.Errorf("binaryDir contains unresolved macros after expansion: %q — refusing to purge", expanded)
	}

	srcAbs, err := filepath.Abs(sourceDir)
	if err != nil {
		return "", fmt.Errorf("resolve sourceDir: %w", err)
	}
	srcAbs = filepath.Clean(srcAbs)

	buildAbs := expanded
	if !filepath.IsAbs(buildAbs) {
		buildAbs = filepath.Join(srcAbs, buildAbs)
	}
	buildAbs = filepath.Clean(buildAbs)

	rel, err := filepath.Rel(srcAbs, buildAbs)
	if err != nil {
		return "", fmt.Errorf("binaryDir not resolvable relative to sourceDir: %w", err)
	}
	// Reject escapes (`..`), the source dir itself (`.`), and anything not
	// contained. On Windows, Rel across drives yields an absolute path.
	if rel == "." {
		return "", errors.New("binaryDir resolves to the source directory itself — refusing to purge")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("binaryDir %q is outside the source directory %q — refusing to purge", buildAbs, srcAbs)
	}
	return buildAbs, nil
}
