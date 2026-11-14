//go:build windows

package binaryadmission

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func canonicalRecoveryMembersV1(v WindowsCanonicalRecoveryLocationsV1) [3]WindowsCanonicalRecoveryMemberV1 {
	return [3]WindowsCanonicalRecoveryMemberV1{v.Launcher, v.Product, v.Identity}
}

func validateCanonicalRecoveryInputV1(targets WindowsCanonicalRecoveryTargetsV1, locations WindowsCanonicalRecoveryLocationsV1) error {
	if !filepath.IsAbs(targets.Root) || filepath.Clean(targets.Root) != targets.Root || targets.LauncherRelative != WindowsCanonicalEntryRelativePathV1 || targets.ProductRelative != WindowsCanonicalProductRelativePathV1 || targets.IdentityRelative != WindowsCanonicalIdentityRelativePathV1 {
		return errors.New("invalid original recovery root/final projection")
	}
	for _, member := range canonicalRecoveryMembersV1(locations) {
		if member.Present {
			if !publisherSourcePathValid(member.SourceRelative) || !validLowerHex(member.ExpectedSHA256, 64, 64) {
				return errors.New("selected recovery original path/hash is missing or invalid")
			}
		} else if member.SourceRelative != "" || member.ExpectedSHA256 != "" {
			return errors.New("selected absent recovery member has unexpected path/hash")
		}
	}
	return nil
}

func receiveCanonicalRecoveryInputV1(ctx context.Context, source WindowsCanonicalRecoverySourceV1, side WindowsCanonicalRecoverySideV1, receive func(WindowsCanonicalRecoveryTargetsV1, WindowsCanonicalRecoveryLocationsV1) error) error {
	calls := 0
	var callbackErr error
	err := source.WithOriginalRecoveryInput(ctx, side, func(targets WindowsCanonicalRecoveryTargetsV1, locations WindowsCanonicalRecoveryLocationsV1) error {
		calls++
		if calls != 1 {
			callbackErr = errors.Join(callbackErr, errors.New("original recovery source invoked receiving more than once"))
			return callbackErr
		}
		callbackErr = validateCanonicalRecoveryInputV1(targets, locations)
		if callbackErr == nil {
			callbackErr = receive(targets, locations)
		}
		return callbackErr
	})
	if calls != 1 {
		return errors.Join(err, callbackErr, errors.New("original recovery input not uniquely received"))
	}
	return errors.Join(err, callbackErr)
}

func AcquireWindowsCanonicalEntryForRecoveryV1(ctx context.Context, source WindowsCanonicalRecoverySourceV1, side WindowsCanonicalRecoverySideV1) (*WindowsCanonicalEntryRecoverySetV1, error) {
	r := &WindowsCanonicalEntryRecoverySetV1{source: source, side: side, original: &WindowsCanonicalEntryPriorSetV1{root: &WindowsCanonicalEntrySetV1{}}}
	if ctx == nil || source == nil {
		return r, errors.New("original recovery source/context required")
	}
	if side != WindowsCanonicalRecoveryPriorV1 && side != WindowsCanonicalRecoverySuccessorV1 {
		return r, errors.New("invalid original recovery side")
	}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	if err := source.ValidateOriginalRecovery(ctx); err != nil {
		return r, err
	}
	if provider, ok := source.(canonicalEntryPayloadDirectorySourceV1); ok {
		directory, err := provider.OriginalPayloadDirectory(ctx)
		if err != nil {
			return r, err
		}
		r.original.root.payloadDirectoryLink = &canonicalEntryPayloadDirectoryLinkV1{owner: directory}
	}
	err := receiveCanonicalRecoveryInputV1(ctx, source, side, func(targets WindowsCanonicalRecoveryTargetsV1, locations WindowsCanonicalRecoveryLocationsV1) error {
		r.targets = targets
		r.locations = locations
		p := r.original
		var err error
		p.parent, err = p.root.openAbsolute(targets.Root, true)
		if err != nil {
			return err
		}
		p.destinations = WindowsCanonicalEntrySetTargetsV1{filepath.Join(targets.Root, filepath.FromSlash(targets.LauncherRelative)), filepath.Join(targets.Root, filepath.FromSlash(targets.ProductRelative)), filepath.Join(targets.Root, filepath.FromSlash(targets.IdentityRelative))}
		for i, input := range canonicalRecoveryMembersV1(locations) {
			if err = ctx.Err(); err != nil {
				return err
			}
			if !input.Present {
				continue
			} // Selected-side absence, never global vacancy.
			member, missing, acquireErr := p.acquireOriginalMember(input.SourceRelative)
			if acquireErr != nil {
				return acquireErr
			}
			if member == nil || missing != "" {
				return fmt.Errorf("selected original recovery member is missing: %s", input.SourceRelative)
			}
			p.members[i] = member
			p.present[i] = true
			p.sha256[i], err = canonicalEntryHashFile(ctx, member.file)
			if err != nil {
				return err
			}
			if p.sha256[i] != input.ExpectedSHA256 {
				return errors.New("selected original recovery bytes disagree with the original journal classification")
			}
			for _, prior := range p.members[:i] {
				if prior != nil && prior.identity == member.identity {
					return errors.New("selected original recovery members alias one native object")
				}
			}
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	other := WindowsCanonicalRecoveryPriorV1
	if side == other {
		other = WindowsCanonicalRecoverySuccessorV1
	}
	err = receiveCanonicalRecoveryInputV1(ctx, source, other, func(targets WindowsCanonicalRecoveryTargetsV1, locations WindowsCanonicalRecoveryLocationsV1) error {
		if targets != r.targets {
			return errors.New("original recovery sides have different target roots/projections")
		}
		for _, input := range canonicalRecoveryMembersV1(locations) {
			if !input.Present {
				continue
			}
			directory, leaf, err := canonicalEntryRelativeParent(r.original.root, r.original.parent, input.SourceRelative)
			if err != nil {
				return err
			}
			h, err := canonicalEntryOpenNative(windows.Handle(directory.file.Fd()), leaf, false, windows.FILE_READ_DATA, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
			if err != nil {
				return err
			}
			probe := &WindowsCanonicalEntrySetV1{}
			member, receiveErr := probe.retain(h, filepath.Join(directory.path, leaf), false)
			if receiveErr == nil {
				for _, selected := range r.original.members {
					if selected != nil && selected.identity == member.identity {
						receiveErr = errors.New("original recovery sides alias one native object")
						break
					}
				}
			}
			if receiveErr == nil {
				hash, hashErr := canonicalEntryHashFile(ctx, member.file)
				receiveErr = hashErr
				if hashErr == nil && hash != input.ExpectedSHA256 {
					receiveErr = errors.New("opposite recovery side changed from the original journal classification")
				}
			}
			if err = errors.Join(receiveErr, probe.Close()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	if err = source.ValidateOriginalRecovery(ctx); err != nil {
		return r, err
	}
	r.original.ready = true
	r.ready = true
	return r, nil
}

func (r *WindowsCanonicalEntryRecoverySetV1) validateLocked(ctx context.Context) error {
	if r == nil || !r.ready || r.closed || r.source == nil {
		return errors.New("original recovery owner is not ready/open")
	}
	if ctx == nil {
		return errors.New("original recovery operation context required")
	}
	return r.source.ValidateOriginalRecovery(ctx)
}

func (r *WindowsCanonicalEntryRecoverySetV1) ReadOriginal(member string) (bool, []byte, string, error) {
	if r == nil {
		return false, nil, "", errors.New("missing original recovery owner")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.validateLocked(context.Background()); err != nil {
		return false, nil, "", err
	}
	index, err := canonicalEntryMemberIndex(member)
	if err != nil {
		return false, nil, "", err
	}
	if !canonicalRecoveryMembersV1(r.locations)[index].Present {
		return false, nil, "", nil
	}
	return r.original.ReadOriginal(member)
}

func (r *WindowsCanonicalEntryRecoverySetV1) MoveOriginal(ctx context.Context, member, destinationRelative string) (WindowsCanonicalEntryMoveResultV1, error) {
	if r == nil {
		err := errors.New("missing original recovery owner")
		return WindowsCanonicalEntryMoveResultV1{Member: member, DestinationRelative: destinationRelative, Error: err}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.validateLocked(ctx); err != nil {
		return WindowsCanonicalEntryMoveResultV1{Member: member, DestinationRelative: destinationRelative, Error: err, OriginalOwnerPending: !r.closed}, err
	}
	return r.original.MoveOriginal(ctx, member, destinationRelative)
}

func (r *WindowsCanonicalEntryRecoverySetV1) DiscardOriginal(ctx context.Context, member, expectedRelative string) (WindowsCanonicalEntryMoveResultV1, error) {
	if r == nil {
		err := errors.New("missing original recovery owner")
		return WindowsCanonicalEntryMoveResultV1{Member: member, SourceRelative: expectedRelative, Error: err}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.validateLocked(ctx); err != nil {
		return WindowsCanonicalEntryMoveResultV1{Member: member, SourceRelative: expectedRelative, Error: err, OriginalOwnerPending: !r.closed}, err
	}
	index, err := canonicalEntryMemberIndex(member)
	if err != nil {
		return WindowsCanonicalEntryMoveResultV1{Member: member, Error: err, OriginalOwnerPending: true}, err
	}
	if !canonicalRecoveryMembersV1(r.locations)[index].Present {
		return WindowsCanonicalEntryMoveResultV1{Member: member, OriginalOwnerPending: true}, nil
	}
	return r.original.DiscardOriginal(ctx, member, expectedRelative)
}

func (r *WindowsCanonicalEntryRecoverySetV1) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.closeError
	}
	r.closed = true
	r.closeError = r.original.Close()
	return r.closeError
}

// A nil view means selected-side absence only. The caller must independently
// receive original GUI/Supervisor/process/port absence before skipping stop.
func (r *WindowsCanonicalEntryRecoverySetV1) RuntimeProduct(ctx context.Context) (*WindowsCanonicalEntryPriorRuntimeV1, error) {
	if r == nil {
		return nil, errors.New("missing original recovery owner")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.validateLocked(ctx); err != nil {
		return nil, err
	}
	view, err := r.original.RuntimeProduct(ctx)
	if err != nil || view == nil {
		return view, err
	}
	view.owner = r
	return view, nil
}

func (r *WindowsCanonicalEntryRecoverySetV1) validateOriginalRuntime(ctx context.Context, member int, complete bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.validateLocked(ctx); err != nil {
		return err
	}
	return r.original.validateOriginalRuntime(ctx, member, complete)
}

func (r *WindowsCanonicalEntryRecoverySetV1) verifyOriginalRuntimeProcess(member int, complete bool, handle uintptr) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.validateLocked(context.Background()); err != nil {
		return err
	}
	return r.original.verifyOriginalRuntimeProcess(member, complete, handle)
}
