//go:build windows

package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Use the existing request fixture with actual unstarted acquisition. These
// tests create inert bytes; they launch no UV/Serena and assert no Ready.
func privateCacheSeedOwnedFixture(t *testing.T) (*preparedSerenaAcquisition, context.Context, *privateSerenaCacheSeed, []byte) {
	t.Helper()
	r := preparedRequest()
	r.Namespace = hardenedTempDir(t)
	r.WorkLimit, r.CleanupLimit = time.Minute, time.Minute
	r.WorkDeadline = time.Now().Add(r.WorkLimit)
	r.AbsoluteCleanupDeadline = r.WorkDeadline.Add(r.CleanupLimit)
	work, cancel := context.WithDeadline(context.Background(), r.WorkDeadline)
	t.Cleanup(cancel)
	key, err := r.Identity()
	if err != nil {
		t.Fatal(err)
	}
	a := &preparedSerenaAcquisition{inputs: SerenaPreparationInputs{Request: r}}
	t.Cleanup(func() {
		if err := a.closeOriginalRoots(); err != nil {
			t.Error(err)
		}
	})
	if _, err = a.acquireRoots(work, r, key); err != nil {
		t.Fatal(err)
	}
	raw, d, inventory := privateCacheSeedTarFixture(t)
	d.Archive.Path = "cache.tar"
	d.Manifest.TotalBytes = inventory.TotalBytes
	container := hardenedTempDir(t)
	source := filepath.Join(container, "seed")
	if err = os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	descriptor := filepath.Join(source, "descriptor.json")
	metadata := []byte("{}\n")
	if err = os.WriteFile(descriptor, metadata, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "cache.tar"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	observed, reader, err := ReadStateFileInodeAnchoredObserved(work, descriptor, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err = reader.Validate(work, descriptor, 64<<10, observed, preparedSerenaHash(metadata)); err != nil {
		t.Fatal(err)
	}
	view := reader.Snapshot()
	if view.ParentIdentitySHA256 == nil || !view.Complete {
		t.Fatal("source fixture has no original closed parent observation")
	}
	seed := &privateSerenaCacheSeed{
		config:     PrivateSerenaCacheSeed{DescriptorPath: descriptor, DescriptorSHA256: preparedSerenaHash(metadata)},
		descriptor: d, inventory: inventory, parentIdentitySHA256: *view.ParentIdentitySHA256,
		readers: []*StateFileReadObservation{reader},
	}
	return a, work, seed, raw
}

func TestPrivateSerenaCacheSeedUsesOriginalAttemptAndClosesBeforeInstall(t *testing.T) {
	a, work, seed, _ := privateCacheSeedOwnedFixture(t)
	owner := a.attempt.platform.(*windowsSerenaOwnedTree)
	original := owner.root
	before, err := serenaTreeHandleIdentity(original.handle, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := &privateSerenaCacheSeedMaterialization{owner: owner, workDeadline: a.inputs.Request.WorkDeadline, cleanupDeadline: a.inputs.Request.AbsoluteCleanupDeadline}
	a.cacheMaterialization = m
	if err = m.materialize(work, seed); err != nil {
		t.Fatal(err)
	}
	if !m.success || !m.closed || m.live != 0 || m.closeOriginal() != nil {
		t.Fatal("original data handles not closed", m.closeErr)
	}
	for _, handle := range m.handles {
		if !handle.close.Attempted || !handle.close.Closed || handle.close.Error != nil {
			t.Fatal("unreceived original handle close")
		}
	}
	if owner.root != original || original.close.Attempted {
		t.Fatal("materializer replaced or closed the borrowed attempt root")
	}
	after, err := serenaTreeHandleIdentity(original.handle, nil)
	if err != nil || after.Device != before.Device || after.FileID != before.FileID {
		t.Fatal("original attempt identity changed", err)
	}
	inventory, err := a.attempt.Inventory(work)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Objects) != 3 {
		t.Fatalf("unexpected created set: %+v", inventory.Objects)
	}
	seenFile := false
	for _, object := range inventory.Objects {
		if !object.Directory {
			seenFile = true
			if object.ContentSHA256 != preparedSerenaHash([]byte("seed")) || object.Bytes != 4 || object.Links != 1 || object.Modified != uint64(1700000000123456700/100)+116444736000000000 {
				t.Fatalf("actual readback/identity/timestamp differs: %+v", object)
			}
		}
	}
	if !seenFile {
		t.Fatal("owned inventory did not receive the copied file")
	}
	if err = m.materialize(work, seed); err == nil {
		t.Fatal("one materialization object reused its original opportunity")
	}
	// An independent DELETE open must still be excluded by the original owner.
	path, err := a.attempt.WorkingDirectory(work)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ntOpenStateReadRootMode(path, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err == nil {
		t.Error("seed extraction weakened original delete exclusion")
		if closeErr := windows.CloseHandle(other); closeErr != nil {
			t.Error(closeErr)
		}
	} else if !errors.Is(err, windows.STATUS_SHARING_VIOLATION) {
		t.Fatal(err)
	}
}

func TestPrivateSerenaCacheSeedFilesystemRefusalsRetainOriginalCloses(t *testing.T) {
	for _, scenario := range []string{"existing_cache", "changed_archive", "external_archive_link", "replaced_parent", "canceled_work", "extended_work"} {
		t.Run(scenario, func(t *testing.T) {
			a, work, seed, raw := privateCacheSeedOwnedFixture(t)
			owner := a.attempt.platform.(*windowsSerenaOwnedTree)
			m := &privateSerenaCacheSeedMaterialization{owner: owner, workDeadline: a.inputs.Request.WorkDeadline, cleanupDeadline: a.inputs.Request.AbsoluteCleanupDeadline}
			a.cacheMaterialization = m
			root, err := a.attempt.WorkingDirectory(work)
			if err != nil {
				t.Fatal(err)
			}
			source := filepath.Dir(seed.config.DescriptorPath)
			switch scenario {
			case "existing_cache":
				if err = os.Mkdir(filepath.Join(root, "cache"), 0700); err != nil {
					t.Fatal(err)
				}
			case "changed_archive":
				changed := append([]byte(nil), raw...)
				changed[len(changed)-1] = 1
				if err = os.WriteFile(filepath.Join(source, "cache.tar"), changed, 0600); err != nil {
					t.Fatal(err)
				}
			case "external_archive_link":
				if err = os.Link(filepath.Join(source, "cache.tar"), filepath.Join(source, "other-link")); err != nil {
					t.Fatal(err)
				}
			case "replaced_parent":
				if err = os.Rename(source, source+"-retained"); err != nil {
					t.Fatal(err)
				}
				if err = os.Mkdir(source, 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(source, "cache.tar"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "canceled_work":
				canceled, cancel := context.WithCancel(work)
				cancel()
				work = canceled
			case "extended_work":
				later, cancel := context.WithDeadline(context.Background(), a.inputs.Request.WorkDeadline.Add(time.Second))
				defer cancel()
				work = later
			}
			if err = m.materialize(work, seed); err == nil || m.success {
				t.Fatal("refused seed reached install eligibility", err)
			}
			if err = m.closeOriginal(); err != nil {
				t.Fatal("unstarted materializer lost original closes", err)
			}
			if m.live != 0 {
				t.Fatal("read/write originals remained live")
			}
			for _, handle := range m.handles {
				if !handle.close.Attempted || !handle.close.Closed {
					t.Fatal("partial extraction lost a resource")
				}
			}
		})
	}
}

func TestPrivateSerenaCacheSeedCloseFailureIsSticky(t *testing.T) {
	root := hardenedTempDir(t)
	raw, err := ntOpenStateReadRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	m := &privateSerenaCacheSeedMaterialization{cleanupDeadline: time.Now().Add(time.Minute)}
	h := m.retain(raw)
	// Deliberately invalidate this original to exercise the actual native close
	// failure. The failed owner close must not be replaced with a later guess.
	if err = windows.CloseHandle(raw); err != nil {
		t.Fatal(err)
	}
	first := m.closeOriginal()
	if first == nil || h.close.Closed || !h.close.Attempted || m.live != 1 {
		t.Fatal("actual failed close reported success")
	}
	second := m.closeOriginal()
	if second != first || len(m.handles) != 1 || m.handles[0] != h {
		t.Fatal("failed original close was retried or lost")
	}
}
