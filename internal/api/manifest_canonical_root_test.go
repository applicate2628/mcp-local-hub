package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const canonicalManifestDirHelperEnv = "MCPHUB_TEST_CANONICAL_MANIFEST_DIR_HELPER"

func TestCanonicalManifestDirHelperProcess(t *testing.T) {
	canonicalExecutable := os.Getenv(canonicalManifestDirHelperEnv)
	if canonicalExecutable == "" {
		return
	}
	t.Cleanup(SetTestCanonicalMcphubPath(canonicalExecutable))
	dir, err := CanonicalManifestDir()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("CANONICAL_MANIFEST_DIR=%s\n", dir)
}

func TestCanonicalManifestDirIgnoresExecutableContextAndExistence(t *testing.T) {
	root := t.TempDir()
	canonicalExecutable := filepath.Join(root, "canonical-bin", "mcphub.exe")
	want := filepath.Join(root, "canonical-bin", "servers")
	invokingExecutables := []string{
		filepath.Join(root, "npm-package", "node_modules", "platform", "api.test.exe"),
		filepath.Join(root, "direct-product", "api.test.exe"),
	}
	sourceExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, invokingExecutable := range invokingExecutables {
		if err := copyCanonicalManifestTestExecutable(sourceExecutable, invokingExecutable); err != nil {
			t.Fatalf("copy helper executable: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, invokingExecutable, "-test.run=^TestCanonicalManifestDirHelperProcess$")
		cmd.Env = append(os.Environ(), canonicalManifestDirHelperEnv+"="+canonicalExecutable)
		output, runErr := cmd.CombinedOutput()
		cancel()
		if runErr != nil {
			t.Fatalf("helper context %q: %v: %s", invokingExecutable, runErr, output)
		}
		got := ""
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "CANONICAL_MANIFEST_DIR=") {
				got = strings.TrimSpace(strings.TrimPrefix(line, "CANONICAL_MANIFEST_DIR="))
				break
			}
		}
		if got != want {
			t.Fatalf("CanonicalManifestDir for context %q = %q, want %q; output=%q", invokingExecutable, got, want, output)
		}
		if got == filepath.Join(filepath.Dir(invokingExecutable), "servers") {
			t.Fatalf("canonical root followed invoking executable context %q", invokingExecutable)
		}
	}
	if _, err := os.Stat(filepath.Dir(canonicalExecutable)); !os.IsNotExist(err) {
		t.Fatalf("read-only canonical resolution touched absent product parent: %v", err)
	}
}

func copyCanonicalManifestTestExecutable(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func TestCanonicalManifestDirRejectsUncleanCanonicalPathWithoutIO(t *testing.T) {
	root := t.TempDir()
	canonicalParent := filepath.Join(root, "canonical-bin")
	dirtyCanonical := canonicalParent + string(filepath.Separator) + ".." + string(filepath.Separator) + "canonical-bin" + string(filepath.Separator) + "mcphub.exe"
	t.Cleanup(SetTestCanonicalMcphubPath(dirtyCanonical))
	if got, err := CanonicalManifestDir(); err == nil || got != "" {
		t.Fatalf("CanonicalManifestDir dirty result = %q, %v; want empty error", got, err)
	}
	if _, err := os.Stat(canonicalParent); !os.IsNotExist(err) {
		t.Fatalf("failed canonical resolution touched filesystem: %v", err)
	}
}

func TestDefaultManifestLifecycleUsesCanonicalRootAcrossContexts(t *testing.T) {
	root := t.TempDir()
	canonicalExecutable := filepath.Join(root, "canonical-bin", "mcphub.exe")
	t.Cleanup(SetTestCanonicalMcphubPath(canonicalExecutable))
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", "")
	name := "canonical-context-manifest"
	canonicalRoot := filepath.Join(filepath.Dir(canonicalExecutable), "servers")
	cleanupExactManifestTestTarget(t, canonicalRoot, name)
	for _, legacyRoot := range invokingExecutableManifestRootsForTest(t) {
		cleanupExactManifestTestTarget(t, legacyRoot, name)
	}
	initial := manifestRootTestYAML(name, 9381, "echo")
	updated := manifestRootTestYAML(name, 9381, "updated")

	npmContext := NewAPI()
	if err := npmContext.ManifestCreate(name, initial); err != nil {
		t.Fatalf("npm-context ManifestCreate: %v", err)
	}
	canonicalPath := filepath.Join(canonicalRoot, name, "manifest.yaml")
	if bytes, err := os.ReadFile(canonicalPath); err != nil || string(bytes) != initial {
		t.Fatalf("canonical manifest bytes = %q, %v", bytes, err)
	}
	for _, legacyRoot := range invokingExecutableManifestRootsForTest(t) {
		if _, err := os.Stat(filepath.Join(legacyRoot, name)); !os.IsNotExist(err) {
			t.Fatalf("manifest leaked into invoking-executable root %q: %v", legacyRoot, err)
		}
	}

	canonicalContext := NewAPI()
	if got, err := canonicalContext.ManifestGet(name); err != nil || got != initial {
		t.Fatalf("canonical-context ManifestGet = %q, %v", got, err)
	}
	got, hash, err := canonicalContext.ManifestGetWithHash(name)
	if err != nil || got != initial || hash != ManifestHashContent([]byte(initial)) {
		t.Fatalf("ManifestGetWithHash = %q/%q/%v", got, hash, err)
	}
	if exists, err := canonicalContext.ManifestExists(name); err != nil || !exists {
		t.Fatalf("ManifestExists = %v, %v", exists, err)
	}
	if names, err := canonicalContext.ManifestList(); err != nil || !containsManifestRootTestName(names, name) {
		t.Fatalf("ManifestList contains=%v err=%v names=%v", containsManifestRootTestName(names, name), err, names)
	}
	newHash, err := canonicalContext.ManifestEditWithHash(name, updated, hash)
	if err != nil || newHash != ManifestHashContent([]byte(updated)) {
		t.Fatalf("ManifestEditWithHash = %q, %v", newHash, err)
	}
	if err := canonicalContext.ManifestDelete(name); err != nil {
		t.Fatalf("ManifestDelete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(canonicalRoot, name)); !os.IsNotExist(err) {
		t.Fatalf("canonical manifest survived delete: %v", err)
	}
}

func TestDefaultManifestOperationsFailCanonicalResolutionBeforeIO(t *testing.T) {
	root := t.TempDir()
	canonicalParent := filepath.Join(root, "canonical-bin")
	dirtyCanonical := canonicalParent + string(filepath.Separator) + ".." + string(filepath.Separator) + "canonical-bin" + string(filepath.Separator) + "mcphub.exe"
	t.Cleanup(SetTestCanonicalMcphubPath(dirtyCanonical))
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", "")
	name := "canonical-root-failure"
	for _, legacyRoot := range invokingExecutableManifestRootsForTest(t) {
		cleanupExactManifestTestTarget(t, legacyRoot, name)
	}
	a := NewAPI()
	checks := []struct {
		name string
		run  func() error
	}{
		{name: "get", run: func() error { _, err := a.ManifestGet(name); return err }},
		{name: "get with hash", run: func() error { _, _, err := a.ManifestGetWithHash(name); return err }},
		{name: "exists", run: func() error { _, err := a.ManifestExists(name); return err }},
		{name: "list", run: func() error { _, err := a.ManifestList(); return err }},
		{name: "edit", run: func() error { return a.ManifestEdit(name, manifestRootTestYAML(name, 9382, "edit")) }},
		{name: "edit with hash", run: func() error {
			_, err := a.ManifestEditWithHash(name, manifestRootTestYAML(name, 9382, "edit"), strings.Repeat("0", 64))
			return err
		}},
		{name: "delete", run: func() error { return a.ManifestDelete(name) }},
		{name: "adopt port", run: func() error { _, err := pickNextFreeAdoptPort(); return err }},
		{name: "adopt plan", run: func() error { _, err := a.BuildAdoptPlan(AdoptOpts{EntryName: name, Client: "codex-cli"}); return err }},
		{name: "install", run: func() error { return a.Install(InstallOpts{Server: "memory", DryRun: true}) }},
		{name: "scan", run: func() error {
			_, err := a.ScanFrom(ScanOpts{ConfigPaths: map[string]string{}, ShadowWarningTracker: NewDiskShadowWarningTracker()})
			return err
		}},
		{name: "create", run: func() error { return a.ManifestCreate(name, manifestRootTestYAML(name, 9382, "create")) }},
	}
	for _, check := range checks {
		if err := check.run(); err == nil || !strings.Contains(err.Error(), "canonical manifest root") {
			t.Errorf("%s error = %v, want canonical manifest root failure", check.name, err)
		}
	}
	if _, err := os.Stat(canonicalParent); !os.IsNotExist(err) {
		t.Fatalf("resolver failure touched canonical parent: %v", err)
	}
	if _, err := a.BuildAdoptProfileUpdatePlan(name, "stdio-http-legacy-2024-11-05"); err == nil {
		t.Fatal("profile plan accepted unresolved canonical manifest root")
	} else {
		var failure *AdoptProfileUpdateFailure
		if !errors.As(err, &failure) || failure.Stage != "manifest-root" {
			t.Fatalf("profile plan error = %v, want manifest-root stage", err)
		}
	}
}

func TestExplicitManifestDirectoryBypassesCanonicalRootResolution(t *testing.T) {
	root := t.TempDir()
	dirtyCanonical := root + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(root) + string(filepath.Separator) + "mcphub.exe"
	t.Cleanup(SetTestCanonicalMcphubPath(dirtyCanonical))
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", "")
	explicit := filepath.Join(root, "explicit-servers")
	name := "explicit-manifest-root"
	yaml := manifestRootTestYAML(name, 9383, "explicit")
	a := NewAPI()
	if err := a.ManifestCreateIn(explicit, name, yaml); err != nil {
		t.Fatal(err)
	}
	if got, err := a.ManifestGetIn(explicit, name); err != nil || got != yaml {
		t.Fatalf("ManifestGetIn = %q, %v", got, err)
	}
	if names, err := a.ManifestListIn(explicit); err != nil || len(names) != 1 || names[0] != name {
		t.Fatalf("ManifestListIn = %v, %v", names, err)
	}
	if err := a.ManifestDeleteIn(explicit, name); err != nil {
		t.Fatal(err)
	}
}

func manifestRootTestYAML(name string, port int, command string) string {
	return fmt.Sprintf("name: %s\nkind: global\ntransport: stdio-bridge\ncommand: %s\ndaemons:\n  - name: default\n    port: %d\nclient_bindings: []\nweekly_refresh: false\n", name, command, port)
}

func containsManifestRootTestName(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}
	return false
}

func invokingExecutableManifestRootsForTest(t *testing.T) []string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(executable)
	return []string{filepath.Join(dir, "servers"), filepath.Clean(filepath.Join(dir, "..", "servers"))}
}

func cleanupExactManifestTestTarget(t *testing.T, root, name string) {
	t.Helper()
	if err := CheckManifestName(name); err != nil || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		t.Fatalf("unsafe test cleanup root=%q name=%q err=%v", root, name, err)
	}
	target := filepath.Join(root, name)
	if filepath.Dir(target) != root {
		t.Fatalf("test cleanup target escaped root: %q", target)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(target); err != nil {
			t.Errorf("cleanup exact manifest %q: %v", target, err)
		}
	})
}

func mustCanonicalManifestDir(t *testing.T) string {
	t.Helper()
	dir, err := CanonicalManifestDir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
