package perftools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLLVMObjdumpTool_RequiresProjectRoot(t *testing.T) {
	tb := &PerfToolbox{tools: &ToolCatalog{
		LLVMObjdump: &ToolInfo{Installed: true, Path: "not-used"},
	}}
	args, _ := json.Marshal(map[string]any{
		"binary": "build/app.exe",
	})
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

	res, err := tb.llvmObjdumpTool(t.Context(), req)
	if err != nil {
		t.Fatalf("llvmObjdumpTool returned unexpected error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true for missing project_root")
	}
	if body := contentText(res); !strings.Contains(body, "missing required parameter: project_root") {
		t.Fatalf("expected missing project_root error, got: %s", body)
	}
}

func TestValidateLLVMObjdumpBinaryPath_AllowsBinaryInsideProjectRoot(t *testing.T) {
	projectRoot := t.TempDir()
	binary := filepath.Join(projectRoot, "build", "app.exe")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatalf("mkdir build dir: %v", err)
	}
	if err := os.WriteFile(binary, []byte("fake binary"), 0o644); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	relBinary, err := filepath.Rel(projectRoot, binary)
	if err != nil {
		t.Fatalf("rel binary: %v", err)
	}

	got, err := validateLLVMObjdumpBinaryPath(projectRoot, relBinary)
	if err != nil {
		t.Fatalf("expected binary inside project_root to pass validation: %v", err)
	}
	wantInfo, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("stat original binary: %v", err)
	}
	gotInfo, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat resolved binary: %v", err)
	}
	if !os.SameFile(wantInfo, gotInfo) {
		t.Fatalf("expected resolved binary %q to identify same file as %q", got, binary)
	}
}

func TestValidateLLVMObjdumpBinaryPath_RejectsBinaryOutsideProjectRoot(t *testing.T) {
	projectRoot := t.TempDir()
	outsideDir := t.TempDir()
	outsideBinary := filepath.Join(outsideDir, "app.exe")
	if err := os.WriteFile(outsideBinary, []byte("fake binary"), 0o644); err != nil {
		t.Fatalf("write outside binary: %v", err)
	}

	_, err := validateLLVMObjdumpBinaryPath(projectRoot, outsideBinary)
	if err == nil {
		t.Fatal("expected outside binary to fail validation")
	}
	if !strings.Contains(err.Error(), "must be inside project_root") {
		t.Fatalf("expected project_root boundary error, got: %v", err)
	}
}

func TestValidateLLVMObjdumpBinaryPath_RejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows symlink creation requires privileges on many hosts")
	}
	projectRoot := t.TempDir()
	outsideDir := t.TempDir()
	outsideBinary := filepath.Join(outsideDir, "app.exe")
	if err := os.WriteFile(outsideBinary, []byte("fake binary"), 0o644); err != nil {
		t.Fatalf("write outside binary: %v", err)
	}
	link := filepath.Join(projectRoot, "link.exe")
	if err := os.Symlink(outsideBinary, link); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	_, err := validateLLVMObjdumpBinaryPath(projectRoot, link)
	if err == nil {
		t.Fatal("expected symlink escape to fail validation")
	}
	if !strings.Contains(err.Error(), "must be inside project_root") {
		t.Fatalf("expected project_root boundary error, got: %v", err)
	}
}

// TestValidateLLVMObjdumpExtraArgs_AllowsFlags asserts the valid
// surface: short flags, long flags, and equals-shaped long flags
// pass through. These are the only forms llvm-objdump's argv grammar
// supports without smuggling additional input files.
func TestValidateLLVMObjdumpExtraArgs_AllowsFlags(t *testing.T) {
	cases := [][]string{
		{"-d"},
		{"--demangle"},
		{"--x86-asm-syntax=intel"},
		{"--no-show-raw-insn"},
		{"-d", "--demangle", "--x86-asm-syntax=intel"},
	}
	for _, c := range cases {
		if err := validateLLVMObjdumpExtraArgs(c); err != nil {
			t.Errorf("validateLLVMObjdumpExtraArgs(%q): expected ok, got %v", c, err)
		}
	}
}

// TestValidateLLVMObjdumpExtraArgs_RejectsResponseFile guards the
// '@FILE' bypass: llvm-objdump and IWYU both honor "@/path/to/file"
// as a response-file directive that injects more arguments. Allowing
// those would let an attacker smuggle alternate input files past the
// project_root path guard.
func TestValidateLLVMObjdumpExtraArgs_RejectsResponseFile(t *testing.T) {
	cases := [][]string{
		{"@evil"},
		{"@/etc/passwd"},
		{"--demangle", "@evil"},
		{`@C:\Users\user\secret.txt`},
	}
	for _, c := range cases {
		err := validateLLVMObjdumpExtraArgs(c)
		if err == nil {
			t.Errorf("validateLLVMObjdumpExtraArgs(%q): expected rejection, got nil", c)
			continue
		}
		if !strings.Contains(err.Error(), "must be a flag") {
			t.Errorf("validateLLVMObjdumpExtraArgs(%q): expected flag-only error, got %v", c, err)
		}
	}
}

// TestValidateLLVMObjdumpExtraArgs_RejectsPositional guards the
// "additional input file" bypass: llvm-objdump accepts multiple input
// files positionally. Allowing those would let extra_args=["foo.o"]
// or extra_args=["/etc/passwd"] read alternate files alongside the
// path-validated `binary` argument.
func TestValidateLLVMObjdumpExtraArgs_RejectsPositional(t *testing.T) {
	cases := [][]string{
		{"foo.o"},
		{"/etc/passwd"},
		{"--demangle", "secret.bin"},
		{`C:\Users\user\secret.bin`},
		{""},
	}
	for _, c := range cases {
		err := validateLLVMObjdumpExtraArgs(c)
		if err == nil {
			t.Errorf("validateLLVMObjdumpExtraArgs(%q): expected rejection, got nil", c)
			continue
		}
		if !strings.Contains(err.Error(), "must be a flag") &&
			!strings.Contains(err.Error(), "empty entry") {
			t.Errorf("validateLLVMObjdumpExtraArgs(%q): expected flag-only or empty error, got %v", c, err)
		}
	}
}

// TestValidateLLVMObjdumpExtraArgs_RejectsPathValuedFlags guards the
// path-valued-flag bypass that the original "starts with '-'" check
// missed. llvm-objdump accepts several flags whose value is a
// filesystem path (file or directory); allowing them in extra_args
// would let a caller read paths outside project_root despite the
// `binary` guard. Both the equals-form (`--flag=PATH`) and the
// space-separated form (`--flag PATH`) must be rejected.
func TestValidateLLVMObjdumpExtraArgs_RejectsPathValuedFlags(t *testing.T) {
	cases := [][]string{
		{"--build-id=/tmp/foo"},
		{"--build-id"},
		{"--debug-file-directory=/tmp/foo"},
		{"--debug-file-directory"},
		{"--dsym=/tmp/foo.dSYM"},
		{"--dsym"},
		{"--prefix=/tmp/foo"},
		{"--prefix-strip=/tmp/foo"},
		{"--demangle", "--build-id=/etc"},
	}
	for _, c := range cases {
		err := validateLLVMObjdumpExtraArgs(c)
		if err == nil {
			t.Errorf("validateLLVMObjdumpExtraArgs(%q): expected rejection, got nil", c)
			continue
		}
		if !strings.Contains(err.Error(), "path-valued") {
			t.Errorf("validateLLVMObjdumpExtraArgs(%q): expected path-valued error, got %v", c, err)
		}
	}
}

// TestMatchesForbiddenFlagPrefix_DoesNotFalseMatch asserts that the
// prefix matcher rejects ONLY exact equality and the equals-form
// (`--flag=...`), so a future longer flag that happens to start with
// `--build-id` (e.g. a hypothetical `--build-id-something-unrelated`)
// does not get incorrectly flagged.
func TestMatchesForbiddenFlagPrefix_DoesNotFalseMatch(t *testing.T) {
	prefixes := []string{"--build-id", "--prefix"}
	// These must NOT match (false positive guard).
	notMatch := []string{
		"--build-id-something-unrelated",
		"--prefix-but-different",
		"--build",
		"-b",
	}
	for _, a := range notMatch {
		if matchesForbiddenFlagPrefix(a, prefixes) {
			t.Errorf("matchesForbiddenFlagPrefix(%q) = true, want false", a)
		}
	}
	// These MUST match (true positives).
	match := []string{
		"--build-id",
		"--build-id=/tmp",
		"--prefix",
		"--prefix=/tmp",
	}
	for _, a := range match {
		if !matchesForbiddenFlagPrefix(a, prefixes) {
			t.Errorf("matchesForbiddenFlagPrefix(%q) = false, want true", a)
		}
	}
}

// TestLLVMObjdumpTool_RejectsExtraArgsBypass asserts the tool handler
// itself returns IsError for the same surface — i.e. callers can't
// reach runCaptureLimited with hostile extra_args.
func TestLLVMObjdumpTool_RejectsExtraArgsBypass(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "build", "app.exe")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatalf("mkdir build dir: %v", err)
	}
	if err := os.WriteFile(bin, []byte("fake"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	tb := &PerfToolbox{tools: &ToolCatalog{
		LLVMObjdump: &ToolInfo{Installed: true, Path: "not-used"},
	}}

	cases := []struct {
		name      string
		extraArgs []string
	}{
		{"response-file", []string{"@/etc/passwd"}},
		{"positional-absolute", []string{"/etc/passwd"}},
		{"positional-relative", []string{"foo.o"}},
		{"flag-then-positional", []string{"--demangle", "secret.bin"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(map[string]any{
				"binary":       bin,
				"project_root": root,
				"extra_args":   tc.extraArgs,
			})
			req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: args}}

			res, err := tb.llvmObjdumpTool(t.Context(), req)
			if err != nil {
				t.Fatalf("llvmObjdumpTool: %v", err)
			}
			if !res.IsError {
				t.Fatalf("expected IsError=true for hostile extra_args %v", tc.extraArgs)
			}
			if body := contentText(res); !strings.Contains(body, "must be a flag") {
				t.Fatalf("expected flag-only error, got: %s", body)
			}
		})
	}
}
