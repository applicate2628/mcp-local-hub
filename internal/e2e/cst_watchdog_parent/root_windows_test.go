//go:build windows && cst_vptr_probe

package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestF4ParentRootAndJobAdmission(t *testing.T) {
	parent, err := os.MkdirTemp("", "cst-f4-root-test-")
	if err != nil {
		t.Fatal(err)
	}
	parent, err = filepath.Abs(parent)
	if err != nil {
		t.Fatal(err)
	}
	var root *f4OwnedRoot
	var proved bool
	defer func() {
		if root != nil {
			if err := root.Close(); err != nil {
				t.Errorf("root handle close: %v", err)
				proved = false
			}
		}
		if !proved {
			t.Logf("uncertain test-owned root preserved: %s", parent)
			return
		}
		for _, leaf := range []string{f4EvidenceName, f4MarkerName} {
			if err := os.Remove(filepath.Join(root.Path, leaf)); err != nil {
				t.Errorf("remove exact test leaf %s: %v", leaf, err)
				return
			}
		}
		if err := os.Remove(root.Path); err != nil {
			t.Errorf("remove empty exact root: %v", err)
			return
		}
		if err := os.Remove(parent); err != nil {
			t.Errorf("remove empty exact parent: %v", err)
		}
	}()
	root, err = f4CreateBoundRoot(parent, "silence")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f4CreateBoundRoot(parent, ""); err == nil {
		t.Fatal("missing cell admitted another root")
	}
	if filepath.Dir(root.Path) != parent {
		t.Fatalf("root escaped approved parent: %s", root.Path)
	}
	var marker f4RootMarker
	data, err := os.ReadFile(filepath.Join(root.Path, f4MarkerName))
	if err != nil || json.Unmarshal(data, &marker) != nil {
		t.Fatalf("pending marker read/decode: %v", err)
	}
	if marker.Version != "CSTF4RootV1" || marker.Cell != "silence" || marker.Token != hex.EncodeToString(root.Token[:]) || marker.Identity != root.Identity || marker.State != "pending" {
		t.Fatalf("root marker was not bound to token/cell/identity: %+v", marker)
	}
	evidence, err := os.Lstat(filepath.Join(root.Path, f4EvidenceName))
	if err != nil || !evidence.Mode().IsRegular() || evidence.Size() != 0 {
		t.Fatalf("evidence leaf not pre-registered empty: info=%v err=%v", evidence, err)
	}
	opened, err := f4OpenRootNoFollow(root.Path)
	if err != nil {
		t.Fatal(err)
	}
	actual, infoErr := f4Identity(opened, true)
	closeErr := windows.CloseHandle(opened)
	if infoErr != nil || closeErr != nil || actual != root.Identity {
		t.Fatalf("root identity changed: want=%+v got=%+v info=%v close=%v", root.Identity, actual, infoErr, closeErr)
	}
	job, err := f4NewOuterJob()
	if err != nil {
		t.Fatal(err)
	}
	if job.Handle() == 0 {
		t.Fatal("outer Job lacks original handle")
	}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var returned uint32
	if err := windows.QueryInformationJobObject(job.Handle(), windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)), &returned); err != nil {
		t.Fatal(err)
	}
	flags := limits.BasicLimitInformation.LimitFlags
	if flags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 || flags&(windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK|windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK) != 0 {
		t.Fatalf("outer Job limits violate sole-parent custody: flags=%#x", flags)
	}
	if err := job.Close(); err != nil {
		t.Fatal(err)
	}
	proved = true
}
