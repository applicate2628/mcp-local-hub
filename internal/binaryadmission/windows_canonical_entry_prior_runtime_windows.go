//go:build windows

package binaryadmission

import (
	"context"
	"errors"
)

// RuntimeProduct returns nil,nil only when ALL three original slots were
// positively observed absent. The caller still must receive its existing native
// runtime-absence oracle before any stop/no-op decision. Partial sets refuse.
func (p *WindowsCanonicalEntryPriorSetV1) RuntimeProduct(ctx context.Context) (*WindowsCanonicalEntryPriorRuntimeV1, error) {
	if p == nil {
		return nil, errors.New("missing original prior owner")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ready || p.closed {
		return nil, errors.New("original prior runtime owner is not ready/open")
	}
	if ctx == nil {
		return nil, errors.New("nil original prior runtime context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !p.present[0] && !p.present[1] && !p.present[2] {
		return nil, nil
	}
	for i, present := range p.present {
		if present && (p.members[i] == nil || p.members[i].file == nil) {
			return nil, errors.New("prior runtime original member has already been disposed or is unavailable")
		}
	}
	if !p.present[0] {
		return nil, errors.New("prior runtime Product is partial or ambiguous")
	}
	entry := p.members[0]
	info, err := entry.file.Stat()
	if err != nil {
		return nil, err
	}
	header, err := readWindowsPEHeader(entry.file, info.Size())
	if err != nil {
		return nil, err
	}
	if header.characteristics&windowsPEDLLCharacteristic != 0 {
		return nil, errors.New("prior canonical entry is a DLL")
	}
	view := &WindowsCanonicalEntryPriorRuntimeV1{owner: p}
	switch header.subsystem {
	case WindowsGUISubsystem:
		metadata, err := readWindowsBuildIdentityAt(entry.file, info.Size())
		if err != nil {
			return nil, err
		}
		if metadata.Role != WindowsArtifactRoleProduct {
			return nil, errors.New("legacy prior runtime is not the admitted GUI Product role")
		}
		view.artifact = WindowsArtifact{Path: entry.path, Role: metadata.Role, Version: metadata.Version, Commit: metadata.Commit, BuildDate: metadata.BuildDate, SHA256: p.sha256[0], Machine: header.machine, Subsystem: header.subsystem}
	case WindowsCUISubsystem:
		if !p.present[1] || !p.present[2] {
			return nil, errors.New("prior canonical entry set is incomplete")
		}
		// This checker BORROWS the same three original objects; it has no files
		// to close and never replaces/duplicates the prior owner's capability.
		checker := &WindowsCanonicalEntrySetV1{entry: p.members[0], product: p.members[1], identity: p.members[2]}
		if err := checker.receive(ctx, nil, ""); err != nil {
			return nil, err
		}
		view.member = 1
		view.complete = true
		view.artifact = checker.snapshot.ProductArtifact
	default:
		return nil, errors.New("prior canonical entry has no admitted runtime role")
	}
	if err := validateCanonicalEntryRuntimeLocked(ctx, p, view.member, view.complete); err != nil {
		return nil, err
	}
	return view, nil
}

func validateCanonicalEntryRuntimeLocked(ctx context.Context, p *WindowsCanonicalEntryPriorSetV1, member int, complete bool) error {
	if p == nil || !p.ready || p.closed {
		return errors.New("borrowed prior runtime owner is unavailable/closed")
	}
	if ctx == nil {
		return errors.New("nil original prior runtime validation context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	indices := []int{member}
	if complete {
		indices = []int{0, 1, 2}
	}
	for _, index := range indices {
		if err := canonicalEntryVerifyOriginalMember(p.members[index], p.sha256[index]); err != nil {
			return err
		}
		_, _, closeErr, err := canonicalEntryCurrentMember(p.root, p.parent, p.members[index])
		if err = errors.Join(err, closeErr); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (v *WindowsCanonicalEntryPriorRuntimeV1) ValidateOriginal(ctx context.Context) error {
	if v == nil || v.owner == nil {
		return errors.New("missing borrowed prior runtime view")
	}
	return v.owner.validateOriginalRuntime(ctx, v.member, v.complete)
}

func (v *WindowsCanonicalEntryPriorRuntimeV1) VerifyOriginalProductProcessHandle(borrowedProcessHandle uintptr) error {
	if v == nil || v.owner == nil {
		return errors.New("missing borrowed prior runtime view")
	}
	return v.owner.verifyOriginalRuntimeProcess(v.member, v.complete, borrowedProcessHandle)
}

func (p *WindowsCanonicalEntryPriorSetV1) validateOriginalRuntime(ctx context.Context, member int, complete bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return validateCanonicalEntryRuntimeLocked(ctx, p, member, complete)
}

func (p *WindowsCanonicalEntryPriorSetV1) verifyOriginalRuntimeProcess(member int, complete bool, handle uintptr) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validateCanonicalEntryRuntimeLocked(context.Background(), p, member, complete); err != nil {
		return err
	}
	return verifyCanonicalEntryOriginalProductProcessHandle(p.members[member], handle, p)
}
