//go:build windows

package binaryadmission

import (
	"context"
	"errors"
	"strings"

	"golang.org/x/sys/windows"
)

// The live successor may combine promoted stage members and identical retained
// prior members. Its view borrows those exact objects from the same transaction;
// a matching path or digest alone cannot construct this receiver.
type canonicalEntrySuccessorRuntimeV1 struct {
	stage   *StagedWindowsCanonicalEntrySetV1
	prior   *WindowsCanonicalEntryPriorSetV1
	members [3]*canonicalEntryNativeFile
}

func (s *StagedWindowsCanonicalEntrySetV1) SuccessorRuntimeProduct(ctx context.Context) (*WindowsCanonicalEntryRuntimeV1, error) {
	if s == nil {
		return nil, errors.New("missing original successor stage")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root == nil || s.root.payloadDirectoryLink == nil {
		return nil, errors.New("original successor prior owner unavailable")
	}
	s.root.payloadDirectoryLink.mu.Lock()
	prior := s.root.payloadDirectoryLink.prior
	s.root.payloadDirectoryLink.mu.Unlock()
	if prior == nil {
		return nil, errors.New("original successor prior owner unavailable")
	}
	prior.mu.Lock()
	defer prior.mu.Unlock()
	owner := &canonicalEntrySuccessorRuntimeV1{stage: s, prior: prior}
	members, err := owner.receiveMembersLocked(ctx)
	if err != nil {
		return nil, err
	}
	owner.members = members
	// Admit the actual composed set through its retained objects. In particular,
	// an unchanged Product does not authorize unrelated launcher/identity bytes.
	checker := &WindowsCanonicalEntrySetV1{entry: members[0], product: members[1], identity: members[2]}
	if err := checker.receive(ctx, nil, ""); err != nil {
		return nil, err
	}
	return &WindowsCanonicalEntryRuntimeV1{owner: owner, member: 1, complete: true, artifact: checker.snapshot.ProductArtifact}, nil
}

// Both original owners are locked, in the stage -> prior order also used by
// payload-directory receiving. The selected objects must remain canonical and
// match the same admitted stage generation, even if a member was not replaced.
func (r *canonicalEntrySuccessorRuntimeV1) receiveMembersLocked(ctx context.Context) (members [3]*canonicalEntryNativeFile, retErr error) {
	s, p := r.stage, r.prior
	if ctx == nil {
		return members, errors.New("original successor runtime context required")
	}
	if err := ctx.Err(); err != nil {
		return members, err
	}
	if !s.ready || !s.taken || s.closed || s.root == nil || s.root.closed || !p.ready || p.closed || p.root == nil || p.root.closed {
		return members, errors.New("original successor runtime owners are not ready/open or are closed")
	}
	if s.destinations != p.destinations || s.parent == nil || p.parent == nil || s.parent.identity != p.parent.identity {
		return members, errors.New("original successor owners have different destination roots")
	}
	for i, member := range []struct{ target, hash string }{
		{s.destinations.CanonicalEntryPath, s.snapshot.CanonicalEntry.SHA256},
		{s.destinations.ProductPath, s.snapshot.Product.SHA256},
		{s.destinations.IdentityPath, s.snapshot.Identity.SHA256},
	} {
		root, parent := s.root, s.parent
		selected := s.members[i]
		if selected == nil || !strings.EqualFold(selected.path, member.target) {
			selected, root, parent = p.members[i], p.root, p.parent
			if !p.present[i] || selected == nil || !strings.EqualFold(selected.path, member.target) || p.sha256[i] != member.hash {
				return members, errors.New("original successor member is not published at its canonical target")
			}
		}
		if err := canonicalEntryVerifyOriginalMember(selected, member.hash); err != nil {
			return members, err
		}
		_, _, closeErr, err := canonicalEntryCurrentMember(root, parent, selected)
		if err := errors.Join(err, closeErr); err != nil {
			return members, err
		}
		members[i] = selected
	}
	return members, ctx.Err()
}

func (r *canonicalEntrySuccessorRuntimeV1) validateLocked(ctx context.Context, member int, complete bool) error {
	if member != 1 || !complete {
		return errors.New("original successor runtime projection changed")
	}
	members, err := r.receiveMembersLocked(ctx)
	if err != nil {
		return err
	}
	if members != r.members {
		return errors.New("original successor member custody changed")
	}
	return nil
}

func (r *canonicalEntrySuccessorRuntimeV1) validateOriginalRuntime(ctx context.Context, member int, complete bool) error {
	r.stage.mu.Lock()
	defer r.stage.mu.Unlock()
	r.prior.mu.Lock()
	defer r.prior.mu.Unlock()
	return r.validateLocked(ctx, member, complete)
}

func (r *canonicalEntrySuccessorRuntimeV1) verifyOriginalRuntimeProcess(member int, complete bool, handle uintptr) error {
	r.stage.mu.Lock()
	defer r.stage.mu.Unlock()
	r.prior.mu.Lock()
	defer r.prior.mu.Unlock()
	if err := r.validateLocked(context.Background(), member, complete); err != nil {
		return err
	}
	return verifyCanonicalEntryProductProcessImage(r.members[1], handle,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, r.stage.root.payloadDirectoryLink)
}
