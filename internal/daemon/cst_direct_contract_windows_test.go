//go:build windows

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"mcp-local-hub/internal/process"

	"golang.org/x/sys/windows"
)

func cstDirectOwnerSource(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("W01 test invariant: runtime.Caller failed")
	}
	var combined strings.Builder
	for _, name := range []string{"host.go", "launch_capability.go", "launch_capability_windows.go"} {
		value, err := os.ReadFile(filepath.Join(filepath.Dir(here), name))
		if err != nil {
			t.Fatalf("W01 test invariant: read %s: %v", name, err)
		}
		combined.Write(value)
	}
	return combined.String()
}

func TestCstDirectSysProcAttrAppliesOnlySingletonCapability(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	p := &windowsLaunchCapabilityPipe{read: read, write: write}
	cmd := exec.Command("cmd.exe")
	process.NoConsole(cmd)
	if err := p.apply(cmd); err != nil {
		t.Fatalf("apply exact compatible SysProcAttr: %v", err)
	}
	if got := cmd.SysProcAttr.AdditionalInheritedHandles; len(got) != 1 || got[0] != syscall.Handle(read.Fd()) {
		t.Fatalf("additional handles=%v, want singleton %d", got, read.Fd())
	}
	if err := p.apply(cmd); err == nil {
		t.Fatal("second apply must reject preexisting AdditionalInheritedHandles")
	}
}

func TestCstDirectSysProcAttrRejectsEveryConflict(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	p := &windowsLaunchCapabilityPipe{read: read, write: write}
	base := func() *exec.Cmd {
		cmd := exec.Command("cmd.exe")
		process.NoConsole(cmd)
		return cmd
	}
	cases := map[string]func(*syscall.SysProcAttr){
		"token":               func(a *syscall.SysProcAttr) { a.Token = 1 },
		"parent":              func(a *syscall.SysProcAttr) { a.ParentProcess = 1 },
		"process-security":    func(a *syscall.SysProcAttr) { a.ProcessAttributes = &syscall.SecurityAttributes{} },
		"thread-security":     func(a *syscall.SysProcAttr) { a.ThreadAttributes = &syscall.SecurityAttributes{} },
		"no-inherit":          func(a *syscall.SysProcAttr) { a.NoInheritHandles = true },
		"preexisting-handle":  func(a *syscall.SysProcAttr) { a.AdditionalInheritedHandles = []syscall.Handle{1} },
		"custom-command-line": func(a *syscall.SysProcAttr) { a.CmdLine = "cmd.exe /c exit" },
		"extra-creation-flag": func(a *syscall.SysProcAttr) { a.CreationFlags |= windows.CREATE_SUSPENDED },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cmd := base()
			mutate(cmd.SysProcAttr)
			if err := p.apply(cmd); err == nil {
				t.Fatal("conflicting SysProcAttr accepted")
			}
		})
	}
}

func TestVerifyCstDirectImageBindsW2ImageAndManifest(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(root, "servers", "electromagnetics-mcp", "native", "cst-runtime", "mcphub-cst-runtime.exe")
	manifestPath := filepath.Join(root, "servers", "electromagnetics-mcp", "native", "cst-runtime", "cst-native-runtime-manifest-v1.json")
	hashFile := func(path string) string {
		value, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		digest := sha256.Sum256(value)
		return hex.EncodeToString(digest[:])
	}
	receipt := &CstDirectImageReceiptV1{
		Version: 1, LaunchProfile: CstDirectLaunchProfileV1,
		ImagePath: imagePath, ImageSHA256: hashFile(imagePath),
		RuntimeManifestPath: manifestPath, RuntimeManifestSHA256: hashFile(manifestPath),
		ProvisionedPackageIdentitySchema: provisionedPackageIdentitySchemaV1,
		ProvisionedPackageIdentitySHA256: strings.Repeat("a", 64),
		FrontendArgs:                     []string{"--role=frontend"},
	}
	verified, err := verifyCstDirectImage(receipt)
	if err != nil {
		t.Fatalf("verify exact W2 image/manifest with synthetic test-only provision binding: %v", err)
	}
	defer verified.close()
	if err := verified.verify(); err != nil {
		t.Fatalf("immediate pre-start reverify: %v", err)
	}
	receipt.ImageSHA256 = strings.Repeat("0", 64)
	if _, err := verifyCstDirectImage(receipt); err == nil {
		t.Fatal("wrong image identity accepted")
	}
}

func TestCstDirectImageAdmissionIsOwnedByExistingSpawnPath(t *testing.T) {
	source := cstDirectOwnerSource(t)
	for _, required := range []string{"cst-direct-v1", "CstDirectImageReceiptV1", "verifyCstDirectImage"} {
		if !strings.Contains(source, required) {
			t.Errorf("W01 gap: existing Go spawn owner lacks %q", required)
		}
	}
	if strings.Contains(source, "exec.Command(") {
		t.Error("W01 gap: capability route must retain the existing CommandContext owner")
	}
}

func TestInheritedHandleFrontendTupleIsExactlyFour(t *testing.T) {
	prepared := &preparedLaunchCapability{}
	got := prepared.handleInventory()
	want := []string{"stdin", "stdout", "stderr", "capability-read"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("frontend inherited-handle tuple=%v want=%v", got, want)
	}
	source := cstDirectOwnerSource(t)
	if !strings.Contains(source, "len(cmd.SysProcAttr.AdditionalInheritedHandles) != 0") {
		t.Error("W01 gap: singleton AdditionalInheritedHandles precondition is not enforced")
	}
}

func TestInheritedHandleConflictingSysProcAttrFailsClosed(t *testing.T) {
	source := cstDirectOwnerSource(t)
	for _, conflict := range []string{
		"Token", "ParentProcess", "ProcessAttributes", "ThreadAttributes",
		"NoInheritHandles", "AdditionalInheritedHandles",
	} {
		if !strings.Contains(source, conflict) {
			t.Errorf("W01 gap: conflicting SysProcAttr field %s has no rejection owner", conflict)
		}
	}
}
