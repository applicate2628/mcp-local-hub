//go:build windows

package binaryadmission

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// A successor borrows the actual composed current cohort from its original
// stage and prior. This parent owns no handle and cannot be decoded from DATA.
type canonicalEntrySuccessorRuntimeV2 struct {
	stage          *StagedWindowsCanonicalEntrySetV2
	prior          *WindowsCanonicalEntryPriorSetV2
	members        map[string]*canonicalEntryNativeFile
	stagedLauncher bool
}

func (s *StagedWindowsCanonicalEntrySetV2) SuccessorRuntimeProduct(ctx context.Context) (*WindowsCanonicalEntryRuntimeV1, error) {
	return s.successorRuntimeProduct(ctx, false)
}

// SuccessorRuntimeProductWithStagedLauncher borrows the already-published
// Product while its original candidate Launcher stays at the issued stage
// name and the canonical Launcher slot is absent.
func (s *StagedWindowsCanonicalEntrySetV2) SuccessorRuntimeProductWithStagedLauncher(ctx context.Context) (*WindowsCanonicalEntryRuntimeV1, error) {
	return s.successorRuntimeProduct(ctx, true)
}

func (s *StagedWindowsCanonicalEntrySetV2) successorRuntimeProduct(ctx context.Context, stagedLauncher bool) (*WindowsCanonicalEntryRuntimeV1, error) {
	if s == nil {
		return nil, errors.New("missing original current successor stage")
	}
	s.original.mu.Lock()
	defer s.original.mu.Unlock()
	if !s.original.transferred {
		return nil, errors.New("original current successor requires transferred custody")
	}
	if !s.priorAttempted || s.priorError != nil || s.prior == nil {
		return nil, errors.Join(s.priorError, errors.New("original current successor prior owner unavailable"))
	}
	s.prior.original.mu.Lock()
	defer s.prior.original.mu.Unlock()
	owner := &canonicalEntrySuccessorRuntimeV2{stage: s, prior: s.prior, stagedLauncher: stagedLauncher}
	members, actual, err := owner.receiveMembersLocked(ctx)
	if err != nil {
		return nil, err
	}
	owner.members = members
	return &WindowsCanonicalEntryRuntimeV1{owner: owner, member: 1, complete: true, artifact: actual.ProductArtifact}, nil
}

// PublishedSnapshot receives only detached data from the exact stage/prior
// originals. It neither transfers custody nor grants a runtime capability.
func (s *StagedWindowsCanonicalEntrySetV2) PublishedSnapshot(ctx context.Context) (WindowsCanonicalEntrySetSnapshotV2, error) {
	if s == nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.New("missing original current published stage")
	}
	s.original.mu.Lock()
	defer s.original.mu.Unlock()
	if !s.priorAttempted || s.priorError != nil || s.prior == nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.Join(s.priorError, errors.New("original current published prior owner unavailable"))
	}
	s.prior.original.mu.Lock()
	defer s.prior.original.mu.Unlock()
	owner := &canonicalEntrySuccessorRuntimeV2{stage: s, prior: s.prior}
	_, actual, err := owner.receiveMembersLocked(ctx)
	return actual, err
}

// Lock order is stage -> prior -> existing shared directory owner, matching
// current staging/prior receiving. No member is reopened or duplicated here.
func (r *canonicalEntrySuccessorRuntimeV2) receiveMembersLocked(ctx context.Context) (map[string]*canonicalEntryNativeFile, WindowsCanonicalEntrySetSnapshotV2, error) {
	if ctx == nil {
		return nil, WindowsCanonicalEntrySetSnapshotV2{}, errors.New("original current successor context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	s, p := r.stage, r.prior
	if s == nil || p == nil {
		return nil, WindowsCanonicalEntrySetSnapshotV2{}, errors.New("original current successor owners required")
	}
	c, pc := &s.original, &p.original
	if !c.ready || c.closed || c.root == nil || c.root.closed || !pc.ready || pc.closed || pc.root == nil || pc.root.closed {
		return nil, WindowsCanonicalEntrySetSnapshotV2{}, errors.New("original current successor owners are not ready/open or are closed")
	}
	if s.prior != p || c.targets != pc.targets || c.parent == nil || pc.parent == nil || c.parent.identity != pc.parent.identity || c.directories == nil || c.directories != pc.directories {
		return nil, WindowsCanonicalEntrySetSnapshotV2{}, errors.New("original current successor owners disagree on target/root/directory custody")
	}
	for _, source := range []WindowsCanonicalRecoverySourceV2{c.source, pc.source} {
		if source != nil {
			if err := source.ValidateOriginalRecovery(ctx); err != nil {
				return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
			}
		}
	}
	keys, plan, err := canonicalMemberPlanV2(c.snapshot)
	if err != nil {
		return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	members := make(map[string]*canonicalEntryNativeFile, len(c.snapshot.BundleInventory)+1)
	actualFiles := make(map[string]WindowsCanonicalEntryMemberEvidenceV2, len(keys))
	seen := make(map[canonicalEntryNativeIdentity]bool, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
		}
		target := filepath.Join(c.parent.path, filepath.FromSlash(key))
		selected, owner := c.members[key], c
		launcher := r.stagedLauncher && key == c.snapshot.CanonicalEntryRelativePath
		if launcher {
			relative := s.launcherStageRelative
			if !publisherSourcePathValid(relative) || path.Base(relative) != relative ||
				strings.EqualFold(relative, key) || selected == nil || selected.evidence.File.Path != relative {
				return nil, WindowsCanonicalEntrySetSnapshotV2{}, errors.New("original Launcher is not at its issued stage name")
			}
			target = filepath.Join(c.parent.path, relative)
		} else if selected == nil || selected.original == nil || !strings.EqualFold(selected.original.path, target) {
			if r.stagedLauncher && key == c.snapshot.ProductRelativePath {
				return nil, WindowsCanonicalEntrySetSnapshotV2{}, errors.New("original staged Product is not at its canonical target")
			}
			selected, owner = pc.members[key], pc
		}
		if selected == nil || selected.original == nil || selected.original.file == nil || !strings.EqualFold(selected.original.path, target) || selected.evidence.File.SHA256 != plan[key].SHA256 {
			return nil, WindowsCanonicalEntrySetSnapshotV2{}, errors.New("original current successor member is not published at its canonical target")
		}
		if owner == c {
			seal := selected.original.readOnlyGuard
			if seal == nil {
				return nil, WindowsCanonicalEntrySetSnapshotV2{}, errors.New("original current published member requires its live stage seal")
			}
			identity, err := canonicalEntryNativeID(windows.Handle(seal.Fd()), false)
			if err != nil || identity != selected.original.identity {
				return nil, WindowsCanonicalEntrySetSnapshotV2{}, errors.Join(err, errors.New("original current published member seal changed"))
			}
		}
		if err := owner.verifyEffectMember(ctx, key); err != nil {
			return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
		}
		fact := selected.evidence
		if launcher {
			// The original name was received above. Compare this detached
			// fact against the logical member key in the authenticated plan.
			fact.File.Path = key
		}
		if !canonicalMemberMatchesPlanV2(fact, plan[key]) {
			return nil, WindowsCanonicalEntrySetSnapshotV2{}, errors.New("original current successor member differs from authenticated source extent/attributes")
		}
		if seen[selected.original.identity] {
			return nil, WindowsCanonicalEntrySetSnapshotV2{}, errors.New("original current successor members alias one object")
		}
		seen[selected.original.identity] = true
		members[key] = selected.original
		actualFiles[key] = fact
	}
	// Directory records are borrowed from the same already-issued lenders. The
	// expected inventory supplies membership, never native directory authority.
	objects := make(map[string]*canonicalEntryNativeFile, len(c.snapshot.BundleInventory))
	inventory := RuntimeInventoryV1{Schema: RuntimeInventorySchemaV1, Target: "win32-x64", Objects: make([]RuntimeObjectV1, 0, len(c.snapshot.BundleInventory))}
	for _, object := range c.snapshot.BundleInventory {
		key := path.Join(c.snapshot.BundleRelativePath, object.Path)
		if object.Kind == "file" {
			fact := actualFiles[key]
			n := fact.Size
			inventory.Objects = append(inventory.Objects, RuntimeObjectV1{Path: object.Path, Kind: "file", Size: &n, SHA256: fact.File.SHA256, Attributes: fact.Attributes})
			objects[object.Path] = members[key]
			continue
		}
		original, attributes, err := receiveCanonicalSuccessorDirectoryV2(ctx, c, key)
		if err != nil {
			return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
		}
		members[key] = original
		objects[object.Path] = original
		inventory.Objects = append(inventory.Objects, RuntimeObjectV1{Path: object.Path, Kind: "directory", Attributes: attributes})
	}
	bundle, _, err := receiveCanonicalSuccessorDirectoryV2(ctx, c, c.snapshot.BundleRelativePath)
	if err != nil {
		return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	members[c.snapshot.BundleRelativePath] = bundle
	registry, err := CompiledWindowsProductRegistry()
	if err != nil {
		return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	base, _, comments, err := receiveCanonicalEntryFilesWithRegistry(ctx, [3]*os.File{members[c.snapshot.CanonicalEntryRelativePath].file, members[c.snapshot.ProductRelativePath].file, members[c.snapshot.IdentityRelativePath].file}, c.targets.entryTargets(), nil, "", registry)
	if err != nil {
		return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	manifest, err := readHeldCanonicalRuntimeIndex(ctx, members[c.snapshot.RuntimePayload.Path].file)
	if err != nil {
		return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	actual, err := currentCanonicalEntrySnapshot(base, comments.entry, manifest, inventory)
	if err != nil {
		return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	if !compareSnapshotV2(actual, c.snapshot) {
		return nil, WindowsCanonicalEntrySetSnapshotV2{}, errors.New("original composed successor differs from complete admitted current generation")
	}
	payload, err := ParseRuntimePayload(manifest)
	if err != nil {
		return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	for _, component := range payload.Components {
		if err := canonicalEntryVerifyDirectoryMembers(ctx, c.root, objects, objects[component.Root], component.Root); err != nil {
			return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
		}
	}
	if r.stagedLauncher {
		if err := canonicalEntryRequireAbsent(c.parent, c.snapshot.CanonicalEntryRelativePath); err != nil {
			return nil, WindowsCanonicalEntrySetSnapshotV2{}, err
		}
	}
	return members, actual, ctx.Err()
}

func receiveCanonicalSuccessorDirectoryV2(ctx context.Context, c *canonicalEntryMembersV2, key string) (*canonicalEntryNativeFile, uint32, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	c.directories.mu.Lock()
	d := c.directories.owners[key]
	c.directories.mu.Unlock()
	if d == nil {
		return nil, 0, errors.New("original current successor directory lender unavailable")
	}
	if err := c.borrowDirectoryRead(ctx, key, d, nil); err != nil {
		return nil, 0, err
	}
	snapshot, err := d.SnapshotForEntryMembersV2()
	if err != nil {
		return nil, 0, err
	}
	if snapshot.Root != c.parent.path || snapshot.RelativePath != key || d.directory == nil || d.directory.file == nil {
		return nil, 0, errors.New("original current successor directory custody changed")
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(d.directory.file.Fd()), &info); err != nil {
		return nil, 0, err
	}
	return d.directory, info.FileAttributes & 7, ctx.Err()
}

func (r *canonicalEntrySuccessorRuntimeV2) validateLocked(ctx context.Context, member int, complete bool) error {
	if !r.stage.original.transferred {
		return errors.New("original current successor requires transferred custody")
	}
	if member != 1 || !complete {
		return errors.New("original current successor runtime projection changed")
	}
	members, _, err := r.receiveMembersLocked(ctx)
	if err != nil {
		return err
	}
	if len(members) != len(r.members) {
		return errors.New("original current successor member custody changed")
	}
	for key, original := range members {
		if original != r.members[key] {
			return errors.New("original current successor member custody changed")
		}
	}
	return nil
}

func (r *canonicalEntrySuccessorRuntimeV2) validateOriginalRuntime(ctx context.Context, member int, complete bool) error {
	r.stage.original.mu.Lock()
	defer r.stage.original.mu.Unlock()
	r.prior.original.mu.Lock()
	defer r.prior.original.mu.Unlock()
	return r.validateLocked(ctx, member, complete)
}

func (r *canonicalEntrySuccessorRuntimeV2) verifyOriginalRuntimeProcess(member int, complete bool, handle uintptr) error {
	r.stage.original.mu.Lock()
	defer r.stage.original.mu.Unlock()
	r.prior.original.mu.Lock()
	defer r.prior.original.mu.Unlock()
	if err := r.validateLocked(context.Background(), member, complete); err != nil {
		return err
	}
	return verifyCanonicalEntryProductProcessImageInNamespace(r.members[r.stage.original.snapshot.ProductRelativePath], handle, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, r.stage.original.directories)
}
