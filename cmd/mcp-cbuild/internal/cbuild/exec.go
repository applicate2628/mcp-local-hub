package cbuild

import (
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

// maxCapturedOutput is the hard ceiling on how much child stdout+stderr is
// retained in memory. A verbose or runaway build can emit gigabytes; we keep
// only the last maxCapturedOutput bytes (a ring buffer) so a single build can
// never OOM the server. parseDiagnostics and raw_tail both run off this bounded
// sink; only the leading (oldest) output is dropped when the cap is exceeded.
const maxCapturedOutput = 1 << 20 // 1 MiB

// hardCapTimeout is the absolute ceiling on any single exec, regardless of the
// per-call timeout requested by the client.
const hardCapTimeout = 60 * time.Minute

// waitDelay bounds how long Wait may block AFTER a timeout/cancel kill before it
// is forced to return. A killed build's grandchild (ninja/cl.exe/link.exe) can
// keep the output pipe open; without this the call would wedge past its
// deadline. See runCommand.
const waitDelay = 8 * time.Second

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

// boundedBuffer is an io.Writer safe for concurrent writes from the stdout and
// stderr pump goroutines. It retains only the last `max` bytes: once the total
// written exceeds the cap, the oldest bytes are discarded so memory stays
// bounded regardless of how much the child emits. Interleaving order is
// preserved well enough for diagnostic parsing.
type boundedBuffer struct {
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool // set once any leading output was dropped
}

func newBoundedBuffer(max int) *boundedBuffer {
	return &boundedBuffer{max: max}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		over := len(b.buf) - b.max
		// Retain only the last b.max bytes; copy into a fresh slice so the old
		// head is released to the GC rather than pinned by a re-slice.
		trimmed := make([]byte, b.max)
		copy(trimmed, b.buf[over:])
		b.buf = trimmed
		b.truncated = true
	}
	return n, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// runCommand executes bin with args in dir, bounded by timeout (clamped to the
// hard cap) and cancellable via ctx. stdout and stderr are captured together
// into a bounded ring buffer. There is NO shell: args are passed verbatim to
// exec.
//
// On timeout or cancellation the WHOLE process tree is killed (not just the
// direct child) via the per-OS process-group seam, and cmd.WaitDelay guarantees
// Wait returns even if an orphaned grandchild still holds the output pipe — so
// the call can never wedge past its deadline.
func runCommand(ctx context.Context, timeout time.Duration, dir, bin string, args []string) execResult {
	if timeout <= 0 || timeout > hardCapTimeout {
		timeout = hardCapTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out := newBoundedBuffer(maxCapturedOutput)
	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	// If the process (or a grandchild holding the pipe) does not exit promptly
	// after the kill, WaitDelay forces Wait to return so runCommand cannot block
	// past the deadline.
	cmd.WaitDelay = waitDelay

	pg := newProcGroup()
	pg.configure(cmd)
	defer pg.close()
	// Replace os/exec's default single-process cancel with a whole-tree kill so
	// ninja/msbuild/cl.exe/link.exe grandchildren are reaped, not orphaned.
	cmd.Cancel = func() error {
		pg.kill(cmd)
		return nil
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return execResult{
			Combined: out.String(),
			WallMs:   time.Since(start).Milliseconds(),
			startErr: err,
			ExitCode: -1,
		}
	}
	// Assign the freshly-started child to the process group / Job Object so every
	// descendant it spawns is reaped with it.
	pg.start(cmd)

	err := cmd.Wait()
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
// refuses an unresolved-macro path, a path equal to or outside sourceDir, and a
// filesystem root. It applies the containment check TWICE — once lexically and
// once on the symlink-resolved REAL paths — so an intermediate junction /
// symlink whose lexical spelling stays inside the tree but whose real target
// escapes it is refused. Only a path that passes both may be handed to RemoveAll.
func resolveBuildDirWithinSource(binaryDir, sourceDir, presetName string) (string, error) {
	return resolveBuildDirWithinSourceContext(binaryDir, presetMacroContext{
		sourceDir:  sourceDir,
		presetName: presetName,
		fileDir:    sourceDir,
	})
}

func resolveBuildDirWithinSourceContext(binaryDir string, macroCtx presetMacroContext) (string, error) {
	buildAbs, srcAbs, err := expandBinaryDirToAbsContext(binaryDir, macroCtx)
	if err != nil {
		return "", err
	}

	// 1) Lexical containment (cheap, catches `..`, absolute-outside, cross-drive).
	if err := containmentCheck(srcAbs, buildAbs, "binaryDir"); err != nil {
		return "", err
	}

	// 2) Symlink-resolved containment. Resolve BOTH sides against real
	// filesystem paths, then containment-check the real paths. Fail closed on
	// any resolution error — never RemoveAll a path we could not fully resolve.
	srcReal, err := filepath.EvalSymlinks(srcAbs)
	if err != nil {
		return "", fmt.Errorf("resolve source directory %q: %w — refusing to purge", srcAbs, err)
	}
	buildReal, err := evalExistingPrefix(buildAbs)
	if err != nil {
		return "", fmt.Errorf("resolve build directory %q: %w — refusing to purge", buildAbs, err)
	}
	if err := containmentCheck(srcReal, buildReal, "binaryDir (symlink-resolved)"); err != nil {
		return "", err
	}
	// Hand the caller the CANONICAL, symlink-resolved target (buildReal =
	// real existing-prefix + remaining lexical suffix), not the lexical buildAbs
	// that was only containment-checked. RemoveAll must operate on the same path
	// we validated: returning buildAbs would let an intermediate component swapped
	// to a symlink between the check and the delete redirect the RemoveAll to an
	// out-of-tree target (TOCTOU).
	return buildReal, nil
}

// expandBinaryDirToAbs expands a preset's binaryDir macros and resolves it to an
// absolute, lexically-cleaned path (build) plus the absolute source directory
// (src). It fails closed on any macro left unexpanded after substitution — a bare
// ${...}, $env{...}, $penv{...}, $vendor{...}, or ANY $<namespace>{...} form.
// CMake itself rejects an unknown macro namespace, and a leftover macro is
// dangerous here: filepath.Clean would collapse e.g. "$vendor{x}/../src" into a
// real in-tree "src" path, which the purge caller would then RemoveAll. It does
// NOT enforce containment; the purge caller (resolveBuildDirWithinSource) layers
// the containment + symlink checks on top, while the non-destructive
// `cmake --build <dir> --target clean` caller accepts any concrete build dir CMake
// itself configured, including a legitimate out-of-source tree.
func expandBinaryDirToAbs(binaryDir, sourceDir, presetName string) (build, src string, err error) {
	return expandBinaryDirToAbsContext(binaryDir, presetMacroContext{
		sourceDir:  sourceDir,
		presetName: presetName,
		fileDir:    sourceDir,
	})
}

func expandBinaryDirToAbsContext(binaryDir string, macroCtx presetMacroContext) (build, src string, err error) {
	if binaryDir == "" {
		return "", "", errors.New("preset declares no binaryDir")
	}
	expanded, err := expandPresetMacros(binaryDir, macroCtx)
	if err != nil {
		return "", "", err
	}
	if containsUnexpandedMacro(expanded) {
		return "", "", fmt.Errorf("binaryDir contains an unresolved or unknown-namespace macro after expansion: %q — refusing to resolve", expanded)
	}
	src, err = filepath.Abs(macroCtx.sourceDir)
	if err != nil {
		return "", "", fmt.Errorf("resolve sourceDir: %w", err)
	}
	src = filepath.Clean(src)

	build = expanded
	if !filepath.IsAbs(build) {
		build = filepath.Join(src, build)
	}
	build = filepath.Clean(build)
	return build, src, nil
}

// containsUnexpandedMacro reports whether s still contains a CMake preset macro
// token of the form ${...}, $env{...}, $penv{...}, $vendor{...}, or any other
// $<namespace>{...} — that is, a '$', optional identifier characters, then '{'.
// The $env/$penv fail-closed expansion in expandPresetMacros resolves the known
// namespaces; this catch-all additionally refuses the vendor namespace and any
// future/unknown namespace CMake would reject, so an unexpanded macro can never
// reach filepath.Clean (which would silently collapse it into a real path).
func containsUnexpandedMacro(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '$' {
			continue
		}
		j := i + 1
		for j < len(s) && isMacroNamespaceByte(s[j]) {
			j++
		}
		if j < len(s) && s[j] == '{' {
			return true
		}
	}
	return false
}

// isMacroNamespaceByte reports whether b may appear in a CMake macro namespace
// (the identifier between '$' and '{', e.g. the "env"/"penv"/"vendor" in
// $env{}/$penv{}/$vendor{}). The empty namespace (${...}) is still matched by
// containsUnexpandedMacro because '$' is then immediately followed by '{'.
func isMacroNamespaceByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9')
}

// containmentCheck rejects a target that is not strictly inside base: the base
// itself (`.`), an escape (`..`), or an unrelated tree (absolute Rel = a
// different Windows drive). label names the value for the error message.
func containmentCheck(base, target, label string) error {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return fmt.Errorf("%s not resolvable relative to source directory: %w", label, err)
	}
	if rel == "." {
		return fmt.Errorf("%s resolves to the source directory itself — refusing to purge", label)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%s %q is outside the source directory %q — refusing to purge", label, target, base)
	}
	return nil
}

// evalExistingPrefix resolves symlinks in the longest EXISTING prefix of path
// and re-appends the not-yet-existing remainder, so a build directory that has
// not been created yet still has its real (symlink-resolved) location computed
// for the containment check. It fails closed on any stat error other than
// "does not exist".
func evalExistingPrefix(path string) (string, error) {
	path = filepath.Clean(path)
	cur := path
	var tail []string
	for {
		if _, err := os.Lstat(cur); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat %q: %w", cur, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the volume root without an existing component; resolve the
			// root and re-append everything below it.
			break
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = parent
	}
	realCur, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", cur, err)
	}
	return filepath.Join(append([]string{realCur}, tail...)...), nil
}
