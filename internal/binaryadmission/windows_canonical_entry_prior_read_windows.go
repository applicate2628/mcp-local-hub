//go:build windows

package binaryadmission

import (
	"context"
	"errors"
	"path"
	"strings"
)

// ReadOriginal reads one actually owned prior file by its stable final member
// key. Known presence, expected digest and acquired bytes remain evidence on
// failure; none of those outputs qualifies a failed read or grants authority.
func (p *WindowsCanonicalEntryPriorSetV2) ReadOriginal(ctx context.Context, member string) (present bool, body []byte, expectedSHA256 string, err error) {
	if p == nil {
		return false, nil, "", errors.New("original current prior owner required")
	}
	c := &p.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if !publisherSourcePathValid(member) {
		return false, nil, "", errors.New("exact canonical prior member key required")
	}
	m, owned := c.members[member]
	absence, recordedAbsent := p.observation.Absences[member]
	if owned {
		present = true
		if m != nil {
			expectedSHA256 = m.evidence.File.SHA256
		}
	}
	if !owned && !recordedAbsent {
		return false, nil, "", errors.New("unissued original current prior member")
	}
	if owned && recordedAbsent {
		return present, nil, expectedSHA256, errors.New("original current prior member has conflicting presence and absence")
	}
	if ctx == nil {
		return present, nil, expectedSHA256, errors.New("original current prior read context required")
	}
	if e := ctx.Err(); e != nil {
		return present, nil, expectedSHA256, e
	}
	if c.parent == nil || c.parent.file == nil {
		return present, nil, expectedSHA256, errors.Join(c.acquisitionError, c.closeError, c.probeCloseError, errors.New("original current prior parent is unavailable"))
	}
	if owned && (m == nil || m.original == nil || m.original.file == nil || m.cleanup != nil && m.cleanup.Applied) {
		return present, nil, expectedSHA256, errors.Join(c.acquisitionError, c.closeError, c.probeCloseError, errors.New("original current prior member is unavailable or disposed"))
	}
	if owned && (m.evidence.Member != member || !validLowerHex(expectedSHA256, 64, 64)) {
		return present, nil, expectedSHA256, errors.New("original current prior member evidence is incomplete or mismatched")
	}
	// The locked core assumes every issued member has a record. Refuse a
	// degraded map before invoking it, including damage to a different member.
	for _, key := range c.keys {
		if c.members[key] == nil {
			return present, nil, expectedSHA256, errors.New("original current prior member map is incomplete")
		}
	}
	if e := c.verify(ctx); e != nil {
		return present, nil, expectedSHA256, errors.Join(e, c.closeError, c.probeCloseError, ctx.Err())
	}
	// Preserve the whole-prior postcheck even when reading or qualification
	// fails. Never recursively call a public method that takes this same mutex.
	defer func() { err = errors.Join(err, c.verify(ctx), ctx.Err()) }()
	if !owned {
		parentRelative := absence.ParentRelative
		wantParent := parentRelative
		if wantParent == "" {
			wantParent = "."
		}
		if !publisherSourcePathValid(absence.MissingRelative) || parentRelative != "" && !publisherSourcePathValid(parentRelative) || path.Dir(absence.MissingRelative) != wantParent || member != absence.MissingRelative && !strings.HasPrefix(member, absence.MissingRelative+"/") {
			return false, nil, "", errors.New("original current prior absence record is invalid")
		}
		// This is the recorded original absence, not a pathname retry or a
		// reservation. Match its nearest original parent through retained custody.
		if parentRelative == "" {
			id, e := canonicalPayloadDirectoryIdentity(c.parent)
			if e != nil || id != absence.ParentIdentity {
				return false, nil, "", errors.Join(e, errors.New("original current prior absence parent changed"))
			}
		} else {
			if c.directories == nil {
				return false, nil, "", errors.New("original current prior absence directory owner unavailable")
			}
			c.directories.mu.Lock()
			d := c.directories.owners[parentRelative]
			c.directories.mu.Unlock()
			if d == nil {
				return false, nil, "", errors.New("original current prior absence parent was not acquired")
			}
			snapshot, e := d.SnapshotForEntryMembersV2()
			if e != nil || snapshot.Root != c.parent.path || snapshot.RelativePath != parentRelative || snapshot.DirectoryIdentity != absence.ParentIdentity {
				return false, nil, "", errors.Join(e, errors.New("original current prior absence parent evidence changed"))
			}
		}
		return false, nil, "", nil
	}
	body, readErr := canonicalEntryHeldBytes(ctx, m.original.file)
	var extentErr, digestErr error
	if int64(len(body)) != m.evidence.Size {
		extentErr = errors.New("original current prior read extent differs from retained member")
	}
	if canonicalEntrySHA256(body) != expectedSHA256 {
		digestErr = errors.New("original current prior read digest differs from retained member")
	}
	return true, body, expectedSHA256, errors.Join(readErr, extentErr, digestErr)
}
