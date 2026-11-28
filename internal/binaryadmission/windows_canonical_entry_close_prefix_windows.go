//go:build windows

package binaryadmission

import (
	"context"
	"errors"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/windows"
)

// This plan is local to one untransferred V2 Close under the stage lock. It
// binds the complete final-name set to its retained parent; it holds no probe
// result or authority to dispose a member.
type canonicalEntryStageClosePrefixV2 struct {
	root      *WindowsCanonicalEntrySetV1
	parent    *canonicalEntryNativeFile
	component string
	before    []string
	after     []string
	prior     *canonicalEntryClosePriorParentsV2
}

func planCanonicalEntryStageClosePrefixV2(root *WindowsCanonicalEntrySetV1, parent *canonicalEntryNativeFile) *canonicalEntryStageClosePrefixV2 {
	if root == nil || root.closed || parent == nil || parent.file == nil ||
		!slices.Contains(root.files, parent.file) || root.memberDirectories == nil {
		return nil
	}

	prefix := ""
	first, last := 0, 0
	for i, relative := range root.memberFinalPaths {
		if !publisherSourcePathValid(relative) {
			return nil
		}
		component, _, nested := strings.Cut(relative, "/")
		if !nested {
			continue
		}
		if prefix == "" {
			prefix, first, last = component, i, i
		}
		if component != prefix || i != last {
			// Preserve order and use one literal nested block. Alternative
			// component spellings must go through complete native lookup.
			return nil
		}
		last++
	}
	if prefix == "" {
		return nil
	}
	return &canonicalEntryStageClosePrefixV2{
		root: root, parent: parent, component: prefix,
		before: root.memberFinalPaths[:first], after: root.memberFinalPaths[last:],
	}
}

func (p *canonicalEntryStageClosePrefixV2) observeAbsent(ctx context.Context, member *canonicalEntryNativeFile) (bool, error, error) {
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	root, parent := p.root, p.parent
	if root.closed || parent.file == nil || member == nil || member.file == nil {
		return false, nil, errors.New("original stage close prefix owner unavailable")
	}
	// Only the native parent resolver can qualify absence. A lender may
	// select a different guarded path or report its own refusal/closure.
	// Leave both the lender and any outstanding borrow to the full walk.
	index := root.memberDirectories
	index.mu.Lock()
	defer index.mu.Unlock()
	if len(root.memberDirectoryBorrows) != 0 {
		return false, nil, nil
	}
	for _, owner := range index.owners {
		// Prior acquisition registers incomplete owners before FILE_OPEN.
		// Like the ordinary resolver, only an issued native directory can
		// lend resolution. The retained record is never absence evidence.
		if owner == nil || owner.directory != nil {
			return false, nil, nil
		}
	}
	id, err := canonicalEntryNativeID(windows.Handle(parent.file.Fd()), true)
	if err != nil {
		return false, nil, err
	}
	if id != parent.identity || member.identity.volume != id.volume {
		return false, nil, errors.New("original stage close prefix parent identity or volume changed")
	}

	h, openErr := canonicalEntryOpenRelativeDirectory(parent, p.component)
	if openErr == windows.STATUS_OBJECT_NAME_NOT_FOUND && h == 0 {
		return true, nil, ctx.Err()
	}
	if openErr != nil {
		var closeErr error
		if h != 0 {
			closeErr = windows.CloseHandle(h)
		}
		return false, closeErr, openErr
	}
	if h == 0 {
		return false, nil, errors.New("original stage close prefix open returned no handle")
	}
	prefixID, idErr := canonicalEntryNativeID(h, true)
	closeErr := windows.CloseHandle(h)
	if idErr == nil && prefixID.volume != id.volume {
		idErr = errors.New("canonical entry destination parent is on another volume")
	}
	// A positive ordinary directory, including a possible namespace alias,
	// never qualifies the shortcut. The full walk owns its leaf decisions.
	return false, closeErr, errors.Join(idErr, ctx.Err())
}

// Probe the nested branch in its original visit order. The first absence
// only selects that branch for another observation at the disposition boundary.
func (p *canonicalEntryStageClosePrefixV2) refuseFinals(ctx context.Context, member *canonicalEntryNativeFile) (bool, error, error) {
	if p.prior != nil {
		closeErr, err := canonicalEntryRefuseFinalPaths(p.root, p.parent, member, p.root.memberFinalPaths, p.prior)
		return false, closeErr, err
	}
	if closeErr, err := canonicalEntryRefuseFinalPaths(p.root, p.parent, member, p.before, nil); closeErr != nil || err != nil {
		return false, closeErr, err
	}
	missing, closeErr, err := p.observeAbsent(ctx, member)
	if closeErr != nil || err != nil {
		return false, closeErr, err
	}
	if !missing {
		closeErr, err = canonicalEntryRefuseFinalObject(p.root, p.parent, member)
		return false, closeErr, err
	}
	closeErr, err = canonicalEntryRefuseFinalPaths(p.root, p.parent, member, p.after, nil)
	return closeErr == nil && err == nil, closeErr, err
}

// Consume a deferred prefix only after the exact staged current-name check.
// A positive or no longer eligible prefix forces a fresh complete walk and
// another current-name check before the caller can perform disposition.
func (p *canonicalEntryStageClosePrefixV2) confirmCurrentMember(ctx context.Context, member *canonicalEntryNativeFile) (*canonicalEntryNativeFile, string, error, error) {
	directory, leaf, closeErr, err := canonicalEntryCurrentMember(p.root, p.parent, member)
	if closeErr != nil || err != nil {
		return directory, leaf, closeErr, err
	}
	missing, closeErr, err := p.observeAbsent(ctx, member)
	if closeErr != nil || err != nil || missing {
		return directory, leaf, closeErr, err
	}
	if closeErr, err := canonicalEntryRefuseFinalObject(p.root, p.parent, member); closeErr != nil || err != nil {
		return nil, "", closeErr, err
	}
	return canonicalEntryCurrentMember(p.root, p.parent, member)
}

// A Close-only map of issued directory borrows. It never stores a missing
// name or a final-file probe. The stage bank releases the original borrows.
type canonicalEntryClosePriorParentsV2 struct {
	bank   *WindowsCanonicalEntrySetV1
	anchor *canonicalEntryClosePriorParentV2
	byPath map[string]*canonicalEntryClosePriorParentV2
}

type canonicalEntryClosePriorParentV2 struct {
	parent   *canonicalEntryClosePriorParentV2
	original *canonicalEntryNativeFile
	identity WindowsCanonicalEntryDirectoryIdentityV1
}

func planCanonicalEntryClosePriorParentsV2(bank *WindowsCanonicalEntrySetV1, anchor *canonicalEntryNativeFile) *canonicalEntryClosePriorParentsV2 {
	if bank == nil || bank.closed || bank.memberDirectories == nil || len(bank.memberFinalPaths) == 0 ||
		anchor == nil || anchor.file == nil || !slices.Contains(bank.files, anchor.file) {
		return nil
	}
	for _, relative := range bank.memberFinalPaths {
		if !publisherSourcePathValid(relative) {
			return nil
		}
	}
	handle := windows.Handle(anchor.file.Fd())
	var serial uint32
	var filesystem [windows.MAX_PATH]uint16
	if err := windows.GetVolumeInformationByHandle(handle, nil, 0, &serial, nil, nil, &filesystem[0], uint32(len(filesystem))); err != nil ||
		windows.UTF16ToString(filesystem[:]) != "NTFS" || serial != anchor.identity.volume ||
		!canonicalEntryClosePrimaryNameV2(anchor) {
		return nil
	}
	identity, err := canonicalPayloadDirectoryIdentity(anchor)
	if err != nil || identity.VolumeSerialNumber != serial ||
		identity.FileIndexHigh != anchor.identity.high || identity.FileIndexLow != anchor.identity.low {
		return nil
	}
	return &canonicalEntryClosePriorParentsV2{bank: bank,
		anchor: &canonicalEntryClosePriorParentV2{original: anchor, identity: identity},
		byPath: make(map[string]*canonicalEntryClosePriorParentV2)}
}

// GetFinalPathNameByHandle with zero flags queries the normalized DOS name
// from the original handle. Only an exact local primary name qualifies.
func canonicalEntryClosePrimaryNameV2(original *canonicalEntryNativeFile) bool {
	if volume := filepath.VolumeName(original.path); len(volume) != 2 || volume[1] != ':' {
		return false
	}
	handle := windows.Handle(original.file.Fd())
	var buffer [windows.MAX_PATH]uint16
	name := buffer[:]
	n, err := windows.GetFinalPathNameByHandle(handle, &name[0], uint32(len(name)), 0)
	if err == nil && n > uint32(len(name)) {
		name = make([]uint16, n)
		n, err = windows.GetFinalPathNameByHandle(handle, &name[0], uint32(len(name)), 0)
	}
	return err == nil && n != 0 && n < uint32(len(name)) &&
		windows.UTF16ToString(name[:n]) == "\\\\?\\"+original.path
}

func (p *canonicalEntryClosePriorParentsV2) finalParent(relative string) (*canonicalEntryNativeFile, string, error) {
	dir, leaf := path.Split(relative)
	if dir == "" {
		return nil, "", nil
	}
	parent, err := p.heldParent(strings.TrimSuffix(dir, "/"))
	if parent == nil || err != nil {
		return nil, "", err
	}
	return parent.original, leaf, nil
}

func (p *canonicalEntryClosePriorParentsV2) heldParent(relative string) (*canonicalEntryClosePriorParentV2, error) {
	if relative == "." {
		return p.anchor, p.anchor.check()
	}
	if held := p.byPath[relative]; held != nil {
		return held, held.check()
	}
	above, err := p.heldParent(path.Dir(relative))
	if err != nil || above == nil {
		return nil, err
	}
	index := p.bank.memberDirectories
	index.mu.Lock()
	lender := index.owners[relative]
	index.mu.Unlock()
	if lender == nil {
		return nil, nil
	}

	component := path.Base(relative)
	original, matched, err := canonicalEntryBorrowMemberDirectoryV2(p.bank, above.original, component)
	if err != nil || !matched {
		return nil, err
	}
	lender.mu.Lock()
	defer lender.mu.Unlock()
	snapshot := lender.memberSnapshot
	// Only the exact already-existing current lender qualifies. Its guard
	// opened the directory with traverse/list access and without share-delete,
	// and the issued borrow interlocks lender Disposition and Close.
	if snapshot == nil || snapshot.Created || lender.memberSource != nil || lender.source != nil ||
		!lender.ready || lender.closed || lender.guard == nil || lender.guard.closed ||
		original != lender.directory || !p.bank.memberDirectoryBorrows[lender] ||
		!slices.Contains(lender.guard.files, original.file) ||
		snapshot.Root != p.anchor.original.path || snapshot.RelativePath != relative ||
		snapshot.ParentIdentity != above.identity || original.path != filepath.Join(above.original.path, component) ||
		original.identity.volume != p.anchor.original.identity.volume ||
		!canonicalEntryClosePrimaryNameV2(original) {
		return nil, nil
	}
	held := &canonicalEntryClosePriorParentV2{parent: above, original: original, identity: snapshot.DirectoryIdentity}
	if err := held.check(); err != nil {
		return nil, err
	}
	p.byPath[relative] = held
	return held, nil
}

// A held name binds the directory object. Its metadata can change even
// between two leaf probes in the same discard.
func (p *canonicalEntryClosePriorParentV2) check() error {
	if p.parent != nil {
		if err := p.parent.check(); err != nil {
			return err
		}
	}
	identity, err := canonicalPayloadDirectoryIdentity(p.original)
	if err != nil {
		return err
	}
	if identity != p.identity {
		return errors.New("held prior final ancestor identity or creation changed")
	}
	return nil
}
