//go:build windows

package scheduler

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const schedulerCreateFixtureRoot = "MCPHUB_TEST_SCHEDULER_CREATE_ROOT"
const schedulerCreateFixtureName = "\\fixture-create-policy"

// The existing schtasksPath seam points to this test executable. The helper
// models command-side conflict/replacement behavior; it never calls Windows
// Task Scheduler and does not qualify native schtasks prompt/EOF semantics.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "/Create" {
		time.AfterFunc(10*time.Second, func() { os.Exit(124) })
		os.Exit(runSchedulerCreateFixture(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runSchedulerCreateFixture(args []string) int {
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, err)
		return 98
	}
	root := os.Getenv(schedulerCreateFixtureRoot)
	if !filepath.IsAbs(root) {
		return fail(errors.New("fixture root must be absolute"))
	}
	if len(args) != 5 && len(args) != 6 {
		return fail(fmt.Errorf("unexpected argument count: %d", len(args)))
	}
	if args[0] != "/Create" || args[1] != "/TN" || args[2] != schedulerCreateFixtureName || args[3] != "/XML" {
		return fail(errors.New("unexpected fixture request"))
	}
	force := len(args) == 6
	if force && args[5] != "/F" {
		return fail(errors.New("unexpected replacement option"))
	}
	// Production Create/ImportXML must not inherit an interactive "yes".
	input, err := io.ReadAll(io.LimitReader(os.Stdin, 1))
	if err != nil {
		return fail(err)
	}
	if len(input) != 0 {
		return fail(errors.New("unexpected interactive input"))
	}
	current := filepath.Join(root, "task.xml")
	if _, err := os.Stat(current); err == nil {
		if !force {
			fmt.Fprintln(os.Stderr, "fixture task already exists")
			return 17
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	raw, err := os.ReadFile(args[4])
	if err != nil {
		return fail(err)
	}
	if !bytes.HasPrefix(raw, []byte{0xFF, 0xFE}) || !validUTF16LEBOM(raw) {
		return fail(errors.New("fixture requires UTF-16LE task XML"))
	}
	if err := os.WriteFile(current, raw, 0600); err != nil {
		return fail(err)
	}
	return 0
}

func TestSchedulerCreateConflictPreservesDefinitionAndImportReplaces(t *testing.T) {
	root := t.TempDir()
	t.Setenv(schedulerCreateFixtureRoot, root)
	t.Setenv("TEMP", root)
	t.Setenv("TMP", root)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := &windowsScheduler{username: "fixture-user", schtasksPath: executable}
	spec := TaskSpec{
		Name:        schedulerCreateFixtureName,
		Command:     "D:\\fixture\\original.exe",
		Description: "original definition",
	}
	if err := s.Create(spec); err != nil {
		t.Fatalf("fresh Create: %v", err)
	}
	path := filepath.Join(root, "task.xml")
	prior, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	spec.Command = "D:\\fixture\\replacement.exe"
	spec.Description = "conflicting definition"
	createErr := s.Create(spec)
	var exitErr *exec.ExitError
	if !errors.As(createErr, &exitErr) || exitErr.ExitCode() != 17 {
		t.Fatalf("duplicate Create error=%v, want fixture conflict exit 17", createErr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, prior) {
		t.Fatal("duplicate Create changed the existing definition")
	}

	spec.Command = "D:\\fixture\\restored.exe"
	spec.Description = "explicit restore Ж§"
	restoreXML := buildCreateXML(spec, "fixture-user")
	if err := s.ImportXML(spec.Name, []byte(restoreXML)); err != nil {
		t.Fatalf("explicit ImportXML: %v", err)
	}
	after, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, EncodeXMLUTF16LEBOM(restoreXML)) {
		t.Fatal("ImportXML did not replace the definition with the requested XML")
	}
}
