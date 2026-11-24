//go:build windows

package binaryadmission

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

func newCanonicalEntryMembersV2() canonicalEntryMembersV2 {
	dirs := &canonicalEntryMemberDirectoriesV2{owners: make(map[string]*WindowsCanonicalEntryPayloadDirectoryV1)}
	return canonicalEntryMembersV2{root: &WindowsCanonicalEntrySetV1{memberDirectories: dirs}, members: make(map[string]*canonicalEntryOwnedMemberV2), directories: dirs}
}

func canonicalMemberFileIdentityV2(file *canonicalEntryNativeFile) (WindowsCanonicalEntryFileIdentityV2, int64, uint32, error) {
	if file == nil || file.file == nil {
		return WindowsCanonicalEntryFileIdentityV2{}, 0, 0, errors.New("original member file unavailable")
	}
	id, err := canonicalEntryNativeID(windows.Handle(file.file.Fd()), false)
	if err != nil {
		return WindowsCanonicalEntryFileIdentityV2{}, 0, 0, err
	}
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(windows.Handle(file.file.Fd()), &info); err != nil {
		return WindowsCanonicalEntryFileIdentityV2{}, 0, 0, err
	}
	stat, err := file.file.Stat()
	if err != nil {
		return WindowsCanonicalEntryFileIdentityV2{}, 0, 0, err
	}
	return WindowsCanonicalEntryFileIdentityV2{id.volume, id.high, id.low, uint64(info.CreationTime.HighDateTime)<<32 | uint64(info.CreationTime.LowDateTime)}, stat.Size(), info.FileAttributes & 7, nil
}

func (c *canonicalEntryMembersV2) receiveMember(ctx context.Context, key string, original *canonicalEntryNativeFile, expectedHash string) (WindowsCanonicalEntryMemberEvidenceV2, error) {
	return c.receiveMemberRead(ctx, key, original, expectedHash, nil)
}

func (c *canonicalEntryMembersV2) receiveMemberRead(ctx context.Context, key string, original *canonicalEntryNativeFile, expectedHash string, scope *canonicalEntryRecoveryReadScopeV2) (WindowsCanonicalEntryMemberEvidenceV2, error) {
	if scope != nil {
		if err := scope.validateBank(ctx, c); err != nil {
			return WindowsCanonicalEntryMemberEvidenceV2{}, err
		}
	}
	id, size, attrs, err := canonicalMemberFileIdentityV2(original)
	if err != nil {
		return WindowsCanonicalEntryMemberEvidenceV2{}, err
	}
	if err = canonicalEntryVerifyOriginalMember(original, expectedHash); err != nil {
		return WindowsCanonicalEntryMemberEvidenceV2{}, err
	}
	relative, err := canonicalEntryMemberRelative(c.parent, original)
	if err != nil {
		return WindowsCanonicalEntryMemberEvidenceV2{}, err
	}
	parent, _, closeErr, err := canonicalEntryCurrentMemberRead(ctx, c.root, c.parent, original, scope)
	if err = errors.Join(err, closeErr); err != nil {
		return WindowsCanonicalEntryMemberEvidenceV2{}, err
	}
	parentID, err := canonicalPayloadDirectoryIdentity(parent)
	if err != nil {
		return WindowsCanonicalEntryMemberEvidenceV2{}, err
	}
	if err = ctx.Err(); err != nil {
		return WindowsCanonicalEntryMemberEvidenceV2{}, err
	}
	return WindowsCanonicalEntryMemberEvidenceV2{key, WindowsCanonicalEntryFileV1{Path: relative, SHA256: expectedHash}, id, parentID, size, attrs}, nil
}

func canonicalMemberPlanV2(snapshot WindowsCanonicalEntrySetSnapshotV2) ([]string, map[string]RuntimeObjectV1, error) {
	if err := ValidateWindowsCanonicalEntrySetSnapshotV2(snapshot); err != nil {
		return nil, nil, err
	}
	objects := make(map[string]RuntimeObjectV1)
	objects[snapshot.CanonicalEntryRelativePath] = RuntimeObjectV1{Path: snapshot.CanonicalEntryRelativePath, Kind: "file", SHA256: snapshot.CanonicalEntry.SHA256}
	for _, object := range snapshot.BundleInventory {
		if object.Kind == "file" {
			object.Path = path.Join(snapshot.BundleRelativePath, object.Path)
			objects[object.Path] = object
		}
	}
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, objects, nil
}

// A member file qualifies against the authenticated snapshot plan before its
// retained native identity becomes the basis for later currentness checks.
func canonicalMemberMatchesPlanV2(observed WindowsCanonicalEntryMemberEvidenceV2, expected RuntimeObjectV1) bool {
	return expected.Path != "" && observed.Member == expected.Path && observed.File.Path == expected.Path &&
		observed.File.SHA256 == expected.SHA256 &&
		(expected.Size == nil || observed.Size == *expected.Size && observed.Attributes == expected.Attributes)
}

// Directories are a separate authenticated projection: file keys remain file
// keys, while empty declared leaves and every required ancestor retain custody.
func canonicalMemberDirectoryPlanV2(snapshot WindowsCanonicalEntrySetSnapshotV2) ([]string, error) {
	if err := ValidateWindowsCanonicalEntrySetSnapshotV2(snapshot); err != nil {
		return nil, err
	}
	seen := map[string]bool{snapshot.BundleRelativePath: true}
	for _, object := range snapshot.BundleInventory {
		key := path.Join(snapshot.BundleRelativePath, object.Path)
		if object.Kind == "directory" {
			seen[key] = true
		}
		for parent := path.Dir(key); parent != "."; parent = path.Dir(parent) {
			seen[parent] = true
		}
	}
	ordered := make([]string, 0, len(seen))
	for key := range seen {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := strings.Count(ordered[i], "/"), strings.Count(ordered[j], "/")
		if a != b {
			return a < b
		}
		return ordered[i] < ordered[j]
	})
	return ordered, nil
}

// Optional only for the source's synchronous read-only operation. Public source
// and native effect contracts continue to perform their ordinary full checks.
type canonicalEntryRecoveryReadScopeSourceV2 interface {
	WithCanonicalRecoveryReadScope(context.Context, func(func(context.Context) error) error) error
}

// Cleanup may receive its exact current projection inside the same protected
// read window. Standalone input receiving remains the fallback for old sources.
type canonicalEntryRecoveryCleanupReadSourceV2 interface {
	WithCanonicalRecoveryCleanupReadScope(context.Context, WindowsCanonicalRecoverySideV1, WindowsCanonicalEntryRecoveryPurposeV2, func(func(context.Context) error, WindowsCanonicalEntryMembersRecoveryInputV2) error) error
}

type canonicalEntryRecoveryReadAcceptanceV2 func(func(context.Context) error, *WindowsCanonicalEntryMembersRecoveryInputV2) error

type canonicalEntryRecoveryReadScopeV2 struct {
	bank   *canonicalEntryMembersV2
	source WindowsCanonicalRecoverySourceV2
	check  func(context.Context) error
}

func sameCanonicalRecoverySourceV2(a, b WindowsCanonicalRecoverySourceV2) bool {
	return a != nil && b != nil && reflect.TypeOf(a) == reflect.TypeOf(b) && reflect.ValueOf(a).Comparable() && reflect.ValueOf(b).Comparable() && a == b
}

func (s *canonicalEntryRecoveryReadScopeV2) validateBank(ctx context.Context, bank *canonicalEntryMembersV2) error {
	if s == nil || bank == nil || s.bank != bank || !sameCanonicalRecoverySourceV2(s.source, bank.source) || s.check == nil {
		return errors.New("read-only recovery scope belongs to another source/bank")
	}
	return s.check(ctx)
}

func (s *canonicalEntryRecoveryReadScopeV2) validateDirectory(ctx context.Context, source WindowsCanonicalRecoverySourceV2) error {
	if source != nil && !sameCanonicalRecoverySourceV2(s.source, source) {
		return errors.New("read-only directory belongs to another recovery source")
	}
	return s.validateBank(ctx, s.bank)
}

func (c *canonicalEntryMembersV2) withRecoveryReadScope(ctx context.Context, body func(*canonicalEntryRecoveryReadScopeV2) error) error {
	provider, ok := c.source.(canonicalEntryRecoveryReadScopeSourceV2)
	if !ok {
		return body(nil)
	}
	return c.receiveRecoveryReadScope(ctx, func(accept canonicalEntryRecoveryReadAcceptanceV2) error {
		return provider.WithCanonicalRecoveryReadScope(ctx, func(check func(context.Context) error) error {
			return accept(check, nil)
		})
	}, func(scope *canonicalEntryRecoveryReadScopeV2, _ *WindowsCanonicalEntryMembersRecoveryInputV2) error {
		return body(scope)
	})
}

func (c *canonicalEntryMembersV2) receiveRecoveryReadScope(ctx context.Context,
	invoke func(canonicalEntryRecoveryReadAcceptanceV2) error,
	body func(*canonicalEntryRecoveryReadScopeV2, *WindowsCanonicalEntryMembersRecoveryInputV2) error) error {
	source, root, directories := c.source, c.root, c.directories
	var mu sync.Mutex
	var bodyDone sync.WaitGroup
	active, calls, running := true, 0, false
	defer func() {
		mu.Lock()
		active = false
		mu.Unlock()
	}()
	var bodyErr error
	providerErr := invoke(func(check func(context.Context) error, input *WindowsCanonicalEntryMembersRecoveryInputV2) error {
		mu.Lock()
		if !active {
			mu.Unlock()
			return errors.New("late read-only recovery body refused")
		}
		calls++
		if calls != 1 || check == nil {
			bodyErr = errors.Join(bodyErr, errors.New("read-only recovery body/check must be unique and present"))
			err := bodyErr
			mu.Unlock()
			return err
		}
		running = true
		bodyDone.Add(1)
		mu.Unlock()
		defer func() {
			mu.Lock()
			running = false
			mu.Unlock()
			bodyDone.Done()
		}()
		scope := &canonicalEntryRecoveryReadScopeV2{bank: c, source: source}
		scope.check = func(useCtx context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			if !active || useCtx == nil || ctx == nil || c.closed || c.root != root || root == nil || root.closed || c.directories != directories || !sameCanonicalRecoverySourceV2(c.source, source) {
				err := errors.New("read-only recovery operation is no longer live")
				if active {
					bodyErr = errors.Join(bodyErr, err)
				}
				return err
			}
			if err := errors.Join(ctx.Err(), useCtx.Err()); err != nil {
				bodyErr = errors.Join(bodyErr, err)
				return err
			}
			err := check(useCtx)
			bodyErr = errors.Join(bodyErr, err)
			return bodyErr
		}
		err := body(scope, input)
		mu.Lock()
		bodyErr = errors.Join(bodyErr, err)
		mu.Unlock()
		return err
	})
	mu.Lock()
	active = false
	asynchronous := running
	mu.Unlock()
	bodyDone.Wait()
	mu.Lock()
	err := errors.Join(providerErr, bodyErr)
	if ctx == nil || c.closed || c.root != root || root == nil || root.closed || c.directories != directories || !sameCanonicalRecoverySourceV2(c.source, source) {
		err = errors.Join(err, errors.New("read-only recovery bank changed before closing"))
	} else {
		err = errors.Join(err, ctx.Err())
	}
	if calls != 1 || asynchronous {
		err = errors.Join(err, errors.New("read-only recovery body was not uniquely synchronous"))
	}
	mu.Unlock()
	if err != nil {
		c.ready = false
		c.acquisitionError = errors.Join(c.acquisitionError, err)
	}
	return err
}

func (c *canonicalEntryMembersV2) verify(ctx context.Context) error {
	if ctx == nil {
		return errors.New("member context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.ready {
		return errors.Join(c.acquisitionError, errors.New("original member owner is not ready/open"))
	}
	return c.withRecoveryReadScope(ctx, func(scope *canonicalEntryRecoveryReadScopeV2) error {
		return c.verifyRead(ctx, scope)
	})
}

func (c *canonicalEntryMembersV2) verifyRead(ctx context.Context, scope *canonicalEntryRecoveryReadScopeV2) error {
	if ctx == nil {
		return errors.New("member context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.closed || c.root == nil || c.root.closed {
		return errors.Join(c.acquisitionError, errors.New("original member owner is not ready/open"))
	}
	if scope != nil {
		if err := scope.validateBank(ctx, c); err != nil {
			return err
		}
	} else if c.source != nil {
		if err := c.source.ValidateOriginalRecovery(ctx); err != nil {
			return err
		}
	}
	for _, key := range c.keys {
		m := c.members[key]
		if m.cleanup != nil && m.cleanup.Applied && m.cleanup.Readback {
			continue
		}
		actual, err := c.receiveMemberRead(ctx, key, m.original, m.evidence.File.SHA256, scope)
		if err != nil {
			return err
		}
		if actual != m.evidence {
			return errors.New("original member identity, parent, extent, attributes or bytes changed")
		}
	}
	return ctx.Err()
}

func (c *canonicalEntryMembersV2) verifyEffectMember(ctx context.Context, key string) error {
	if ctx == nil {
		return errors.New("member context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !c.ready || c.closed || c.root == nil || c.root.closed {
		return errors.Join(c.acquisitionError, errors.New("original member owner is not ready/open"))
	}
	if c.source != nil {
		if err := c.source.ValidateOriginalRecovery(ctx); err != nil {
			return err
		}
	}
	m := c.members[key]
	if m == nil {
		return errors.New("unissued member key")
	}
	actual, err := c.receiveMember(ctx, key, m.original, m.evidence.File.SHA256)
	if err != nil {
		return err
	}
	if actual != m.evidence {
		return errors.New("original effect member evidence changed")
	}
	return nil
}

// observeOriginalMember requires the caller to hold c.mu. The returned issued
// evidence has just been compared with a fresh receiving of this original;
// unrelated members are left to their own effects and whole-bank barriers.
func (c *canonicalEntryMembersV2) observeOriginalMember(ctx context.Context, key string) (WindowsCanonicalEntryMemberEvidenceV2, error) {
	if err := c.verifyEffectMember(ctx, key); err != nil {
		return WindowsCanonicalEntryMemberEvidenceV2{}, err
	}
	return c.members[key].evidence, nil
}

func (c *canonicalEntryMembersV2) disposition() WindowsCanonicalEntryMemberDispositionV2 {
	pending := false
	for key, m := range c.members {
		if _, retained := c.recoveryRetained[key]; retained {
			continue
		}
		if m.cleanup == nil || !m.cleanup.Applied || !m.cleanup.Readback {
			pending = true
		}
	}
	originalPending := c.probeCloseError != nil || c.root != nil && (!c.root.closed || c.root.closeError != nil)
	for _, d := range c.directoryOwners() {
		d.mu.Lock()
		originalPending = originalPending || !d.closed || d.closeError != nil
		if d.memberSnapshot != nil && d.memberSnapshot.Created && (d.disposition == nil || !d.disposition.Readback) {
			pending = true
		}
		d.mu.Unlock()
	}
	return WindowsCanonicalEntryMemberDispositionV2{Transferred: c.transferred, CloseEntered: c.closed, OriginalOwnerPending: originalPending, DispositionPending: pending, Moves: append([]WindowsCanonicalEntryMoveResultV1(nil), c.moves...), Cleanup: append([]WindowsCanonicalEntryMoveResultV1(nil), c.cleanup...), CloseError: c.closeError}
}

func (c *canonicalEntryMembersV2) directoryOwners() []*WindowsCanonicalEntryPayloadDirectoryV1 {
	if c.directories == nil {
		return nil
	}
	c.directories.mu.Lock()
	defer c.directories.mu.Unlock()
	names := make([]string, 0, len(c.directories.owners))
	for name := range c.directories.owners {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := strings.Count(names[i], "/"), strings.Count(names[j], "/")
		if a != b {
			return a > b
		}
		return names[i] > names[j]
	})
	out := make([]*WindowsCanonicalEntryPayloadDirectoryV1, 0, len(names))
	for _, name := range names {
		out = append(out, c.directories.owners[name])
	}
	return out
}

func (c *canonicalEntryMembersV2) close() error {
	if c.closed {
		return c.closeError
	}
	c.closed = true
	if c.root != nil {
		c.closeError = errors.Join(c.closeError, c.root.Close())
	}
	c.closeError = errors.Join(c.closeError, c.probeCloseError)
	return c.closeError
}

type canonicalEntryStageSelectionV2 uint8

const (
	canonicalEntryStageFull canonicalEntryStageSelectionV2 = iota
	canonicalEntryStageRetained
	canonicalEntryStageUpgrade
)

func (o *WindowsCanonicalEntrySetV2) Stage(ctx context.Context, canonicalEntryPath string) (*StagedWindowsCanonicalEntrySetV2, error) {
	return o.stage(ctx, canonicalEntryPath, canonicalEntryStageFull)
}

// StageRetainingEqualRuntime qualifies the original installed current bundle
// before creating any staged file. The prior owner is read-only for this
// preparation. Close the stage on every exit, including a partially received
// pair. It closes its prior first and its shared directory lenders last.
func (o *WindowsCanonicalEntrySetV2) StageRetainingEqualRuntime(ctx context.Context, canonicalEntryPath string) (*StagedWindowsCanonicalEntrySetV2, *WindowsCanonicalEntryPriorSetV2, error) {
	stage, err := o.stage(ctx, canonicalEntryPath, canonicalEntryStageRetained)
	return stage, stage.prior, err
}

// StageForUpgrade selects the staging mode from the admitted original target.
// Close the stage on every exit. It owns its returned prior and directory
// lenders in both modes, including a partially received pair.
func (o *WindowsCanonicalEntrySetV2) StageForUpgrade(ctx context.Context, canonicalEntryPath string) (*StagedWindowsCanonicalEntrySetV2, *WindowsCanonicalEntryPriorSetV2, error) {
	stage, err := o.stage(ctx, canonicalEntryPath, canonicalEntryStageUpgrade)
	return stage, stage.prior, err
}

func (o *WindowsCanonicalEntrySetV2) stage(ctx context.Context, canonicalEntryPath string, selection canonicalEntryStageSelectionV2) (*StagedWindowsCanonicalEntrySetV2, error) {
	s := &StagedWindowsCanonicalEntrySetV2{
		original: newCanonicalEntryMembersV2(), ownsPrior: selection != canonicalEntryStageFull,
		retainsRuntime: selection == canonicalEntryStageRetained,
	}
	c := &s.original
	if o == nil || ctx == nil {
		return s, errors.New("source owner and stage context required")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if err := o.verifyLocked(); err != nil {
		return s, err
	}
	var err error
	c.targets, err = CanonicalEntryTargetsV2(canonicalEntryPath)
	if err != nil {
		return s, err
	}
	c.parent, err = c.root.openAbsolute(filepath.Dir(canonicalEntryPath), true)
	if err != nil {
		return s, err
	}
	c.snapshot = cloneCanonicalEntrySnapshotV2(o.snapshot)
	c.snapshot.CanonicalEntry.Path, c.snapshot.Product.Path, c.snapshot.Identity.Path = c.targets.CanonicalEntryPath, c.targets.ProductPath, c.targets.IdentityPath
	c.snapshot.ProductArtifact.Path = c.targets.ProductPath
	keys, plan, err := canonicalMemberPlanV2(c.snapshot)
	if err != nil {
		return s, err
	}
	c.root.memberFinalPaths = append([]string(nil), keys...)
	if s.ownsPrior {
		prior, err := s.acquireOriginalPriorSet(ctx, keys, nil)
		if err != nil {
			return s, err
		}
		if selection == canonicalEntryStageUpgrade {
			switch prior.observation.BeforeKind {
			case WindowsCanonicalEntryBeforeBundleV2:
				s.retainsRuntime = true
			case WindowsCanonicalEntryBeforeAbsentV2, WindowsCanonicalEntryBeforeRootProductV2, WindowsCanonicalEntryBeforeLegacyV2:
				// The full loop consumes the same admitted prior.
			default:
				return s, errors.New("upgrade original prior kind is unavailable")
			}
		}
	}
	if s.retainsRuntime {
		if err := verifyRetainedCanonicalRuntime(ctx, s.prior, c.snapshot); err != nil {
			return s, err
		}
		keys, err = o.changedCanonicalStageKeys(s.prior, keys, plan)
		if err != nil {
			return s, err
		}
	} else {
		c.keys = keys
	}
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return s, err
	}
	stem := ".mcphub-entry-members-" + hex.EncodeToString(token[:])
	for ordinal, key := range keys {
		if err = ctx.Err(); err != nil {
			return s, err
		}
		source := o.guard.entry
		if key != o.snapshot.CanonicalEntryRelativePath {
			source = o.objects[strings.TrimPrefix(key, o.snapshot.BundleRelativePath+"/")]
		}
		if source == nil || source.file == nil {
			return s, errors.New("authenticated member has no retained source")
		}
		leaf := fmt.Sprintf("%s-%d.stage", stem, ordinal)
		name := filepath.Join(c.parent.path, leaf)
		attributes := plan[key].Attributes
		if key == o.snapshot.CanonicalEntryRelativePath {
			_, _, sourceAttributes, e := canonicalMemberFileIdentityV2(source)
			if e != nil {
				return s, e
			}
			attributes = sourceAttributes
		}
		if attributes == 0 {
			attributes = windows.FILE_ATTRIBUTE_NORMAL
		}
		h, e := canonicalEntryOpenNativeAttributes(windows.Handle(c.parent.file.Fd()), leaf, false, windows.FILE_READ_DATA|windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_CREATE, attributes)
		if e != nil {
			if s.retainsRuntime {
				return s, fmt.Errorf("create selective stage member: %w", e)
			}
			return s, e
		}
		// FILE_CREATE already selected this object. Retain its physical duty
		// even if wrapping or receiving native identity fails before evidence.
		m := &canonicalEntryOwnedMemberV2{original: &canonicalEntryNativeFile{path: name}, evidence: WindowsCanonicalEntryMemberEvidenceV2{Member: key, File: WindowsCanonicalEntryFileV1{Path: leaf}}}
		c.members[key] = m
		if s.retainsRuntime {
			c.keys = append(c.keys, key)
		}
		guard, e := c.root.retain(h, name, false)
		if guard != nil {
			m.original = guard
		}
		if e != nil {
			return s, e
		}
		emptyHash, e := canonicalEntryHashFile(context.Background(), guard.file)
		if e != nil {
			return s, e
		}
		m.evidence, e = c.receiveMember(context.Background(), key, guard, emptyHash)
		if e != nil {
			return s, e
		}
		h, e = canonicalEntryOpenNative(windows.Handle(c.parent.file.Fd()), leaf, false, windows.FILE_WRITE_DATA, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
		if e != nil {
			return s, e
		}
		out := os.NewFile(uintptr(h), name)
		if out == nil {
			return s, errors.Join(errors.New("member stage writer unavailable"), windows.CloseHandle(h))
		}
		created := &canonicalEntryNativeFile{file: out, path: name, identity: guard.identity}
		c.root.files = append(c.root.files, out)
		writerID, e := canonicalEntryNativeID(h, false)
		if e != nil || writerID != guard.identity {
			return s, errors.Join(e, errors.New("member writer differs from original creation object"), canonicalEntryCloseOriginalMember(c.root, created))
		}

		info, statErr := source.file.Stat()
		var copyErr error
		if statErr == nil {
			_, copyErr = io.Copy(out, &canonicalEntryContextReader{ctx: ctx, reader: io.NewSectionReader(source.file, 0, info.Size())})
		}
		syncErr := out.Sync()
		hash, hashErr := canonicalEntryHashFile(context.Background(), guard.file)
		m.evidence.File = WindowsCanonicalEntryFileV1{Path: leaf, SHA256: hash}
		m.evidence.Member = key
		actual, receiveErr := c.receiveMember(context.Background(), key, guard, hash)
		if receiveErr == nil {
			m.evidence = actual
		}
		closeErr := canonicalEntryCloseOriginalMember(c.root, created)
		sealErr := canonicalEntrySealOriginalStage(c.root, c.parent, guard)
		if err = errors.Join(statErr, copyErr, syncErr, hashErr, receiveErr, closeErr, sealErr); err != nil {
			return s, err
		}
		object := plan[key]
		if hash != object.SHA256 || object.Size != nil && actual.Size != *object.Size {
			return s, errors.New("actual member stage differs from authenticated source")
		}
		// Preserve the portable attributes described by the authenticated plan.
		if key == o.snapshot.CanonicalEntryRelativePath {
			s.launcherStageRelative = leaf
		} else if actual.Attributes != object.Attributes {
			return s, errors.New("created member attributes differ from authenticated source")
		}
	}
	if err = o.verifyLocked(); err != nil {
		return s, err
	}
	c.ready = true
	if s.ownsPrior && !s.retainsRuntime {
		s.prior.readOnly = false
	}
	return s, nil
}

// Component content-ID directories own their descriptor, inventory and
// runtime root. Other files in the bundle are independently staged if changed.
func canonicalRuntimeObjects(snapshot WindowsCanonicalEntrySetSnapshotV2, payload RuntimePayloadV1) map[string]RuntimeObjectV1 {
	objects := make(map[string]RuntimeObjectV1)
	for _, object := range snapshot.BundleInventory {
		for _, component := range payload.Components {
			parent := path.Dir(component.Root)
			if object.Path == parent || strings.HasPrefix(object.Path, parent+"/") {
				objects[object.Path] = object
				break
			}
		}
	}
	return objects
}

func verifyRetainedCanonicalRuntime(ctx context.Context, prior *WindowsCanonicalEntryPriorSetV2, expected WindowsCanonicalEntrySetSnapshotV2) error {
	if prior.observation.BeforeKind != WindowsCanonicalEntryBeforeBundleV2 {
		return errors.New("retained runtime requires an original current installed bundle")
	}
	pc := &prior.original
	if !bytes.Equal(pc.snapshot.RuntimePayloadBytes, expected.RuntimePayloadBytes) {
		return errors.New("installed runtime components differ from the candidate")
	}
	payload, err := ParseRuntimePayload(expected.RuntimePayloadBytes)
	if err != nil {
		return err
	}
	want := canonicalRuntimeObjects(expected, payload)
	installed := canonicalRuntimeObjects(pc.snapshot, payload)
	if !reflect.DeepEqual(installed, want) {
		return errors.New("original installed runtime inventory differs from the candidate")
	}

	return verifyCanonicalRuntimeObjects(ctx, pc, expected.BundleRelativePath, payload, want, nil)
}

func verifyCanonicalRuntimeObjects(ctx context.Context, pc *canonicalEntryMembersV2, bundle string, payload RuntimePayloadV1, want map[string]RuntimeObjectV1, scope *canonicalEntryRecoveryReadScopeV2) error {
	objects := make(map[string]*canonicalEntryNativeFile, len(want))
	for name, object := range want {
		key := path.Join(bundle, name)
		if object.Kind == "directory" {
			directory, attrs, err := receiveCanonicalSuccessorDirectoryReadV2(ctx, pc, key, scope)
			if err != nil {
				return err
			}
			if attrs != object.Attributes {
				return errors.New("original retained runtime directory attributes changed")
			}
			objects[name] = directory
			continue
		}
		member := pc.members[key]
		object.Path = key
		if member == nil || !canonicalMemberMatchesPlanV2(member.evidence, object) {
			return errors.New("original retained runtime file differs from the candidate")
		}
		objects[name] = member.original
	}
	for _, component := range payload.Components {
		root := path.Dir(component.Root)
		original := objects[root]
		if original == nil {
			return errors.New("original retained runtime component directory unavailable")
		}
		if err := canonicalEntryVerifyDirectoryMembers(ctx, pc.root, objects, original, root); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (o *WindowsCanonicalEntrySetV2) changedCanonicalStageKeys(prior *WindowsCanonicalEntryPriorSetV2, keys []string, plan map[string]RuntimeObjectV1) ([]string, error) {
	// The launcher has no extent/attributes in the bundle inventory because
	// it sits outside the bundle. Receive those from the held candidate.
	_, size, attrs, err := canonicalMemberFileIdentityV2(o.guard.entry)
	if err != nil {
		return nil, err
	}
	var changed []string
	for _, key := range keys {
		object := plan[key]
		if key == o.snapshot.CanonicalEntryRelativePath {
			object.Size, object.Attributes = &size, attrs
		}
		if member := prior.original.members[key]; member == nil || !canonicalMemberMatchesPlanV2(member.evidence, object) {
			changed = append(changed, key)
		}
	}
	return changed, nil
}

func (s *StagedWindowsCanonicalEntrySetV2) Snapshot() (WindowsCanonicalEntrySetSnapshotV2, error) {
	if s == nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.New("staged member owner required")
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready || c.closed {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.New("staged member owner is not ready/open")
	}
	return cloneCanonicalEntrySnapshotV2(c.snapshot), nil
}
func (s *StagedWindowsCanonicalEntrySetV2) DestinationTargets() (WindowsCanonicalEntrySetTargetsV2, error) {
	if s == nil {
		return WindowsCanonicalEntrySetTargetsV2{}, errors.New("staged member owner required")
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready || c.closed {
		return WindowsCanonicalEntrySetTargetsV2{}, errors.New("staged member owner is not ready/open")
	}
	return c.targets, nil
}
func (s *StagedWindowsCanonicalEntrySetV2) Members() ([]WindowsCanonicalEntryMemberEvidenceV2, error) {
	if s == nil {
		return nil, errors.New("staged member owner required")
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready || c.closed {
		return nil, errors.New("staged member owner is not ready/open")
	}
	out := make([]WindowsCanonicalEntryMemberEvidenceV2, 0, len(c.keys))
	for _, key := range c.keys {
		out = append(out, c.members[key].evidence)
	}
	return out, nil
}
func (s *StagedWindowsCanonicalEntrySetV2) VerifyUnchanged(ctx context.Context) error {
	if s == nil {
		return errors.New("staged member owner required")
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.verify(ctx)
}

// RetainedMembersWitness rechecks the exact native originals omitted from
// a selective stage. The digest covers the canonical JSON array of existing
// member evidence records, ordered by their exact relative member key.
func (s *StagedWindowsCanonicalEntrySetV2) RetainedMembersWitness(ctx context.Context) (WindowsCanonicalEntryRetainedMembersV2, error) {
	var witness WindowsCanonicalEntryRetainedMembersV2
	if s == nil {
		return witness, errors.New("selective runtime stage required")
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if !s.retainsRuntime || s.prior == nil || s.priorError != nil {
		return witness, errors.Join(s.priorError, errors.New("original retained runtime prior required"))
	}
	pc := &s.prior.original
	pc.mu.Lock()
	defer pc.mu.Unlock()
	return s.retainedMembersWitnessLocked(ctx)
}

// The caller holds the selective stage and original prior mutexes in that order.
func (s *StagedWindowsCanonicalEntrySetV2) retainedMembersWitnessLocked(ctx context.Context) (WindowsCanonicalEntryRetainedMembersV2, error) {
	var witness WindowsCanonicalEntryRetainedMembersV2
	c, pc := &s.original, &s.prior.original
	if err := c.verify(ctx); err != nil {
		return witness, err
	}
	if !pc.ready || pc.closed || pc.root == nil || pc.root.closed ||
		c.targets != pc.targets || c.parent == nil || pc.parent == nil ||
		c.parent.identity != pc.parent.identity || c.directories == nil || c.directories != pc.directories {
		return witness, errors.New("original retained runtime prior custody changed")
	}
	if err := verifyRetainedCanonicalRuntime(ctx, s.prior, c.snapshot); err != nil {
		return witness, err
	}
	payload, err := ParseRuntimePayload(c.snapshot.RuntimePayloadBytes)
	if err != nil {
		return witness, err
	}
	components := canonicalRuntimeObjects(c.snapshot, payload)
	keys, plan, err := canonicalMemberPlanV2(c.snapshot)
	if err != nil {
		return witness, err
	}
	var omitted []WindowsCanonicalEntryMemberEvidenceV2
	singletons := []string{}
	for _, key := range keys {
		if c.members[key] != nil {
			continue
		}
		member, err := pc.observeOriginalMember(ctx, key)
		if err != nil {
			return witness, err
		}
		if !canonicalMemberMatchesPlanV2(member, plan[key]) {
			return witness, errors.New("retained original differs from the candidate plan")
		}
		omitted = append(omitted, member)
		relative, inBundle := strings.CutPrefix(key, c.snapshot.BundleRelativePath+"/")
		if _, inComponent := components[relative]; !inBundle || !inComponent {
			singletons = append(singletons, key)
		}
	}
	digest, err := retainedCanonicalMembersSHA256(ctx, omitted)
	if err != nil {
		return witness, err
	}
	return WindowsCanonicalEntryRetainedMembersV2{MembersSHA256: digest, Singletons: singletons}, nil
}

// Both callers supply original evidence in exact member-key order.
func retainedCanonicalMembersSHA256(ctx context.Context, omitted []WindowsCanonicalEntryMemberEvidenceV2) (string, error) {
	if len(omitted) == 0 {
		return "", errors.New("selective transaction has no retained originals")
	}
	raw, err := CanonicalRuntimeJSON(omitted)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return RuntimeDigest(raw), nil
}

// ObserveOriginalMember freshly verifies only the selected issued original.
// MoveOriginal independently revalidates it after the caller persists intent.
func (s *StagedWindowsCanonicalEntrySetV2) ObserveOriginalMember(ctx context.Context, key string) (WindowsCanonicalEntryMemberEvidenceV2, error) {
	if s == nil {
		return WindowsCanonicalEntryMemberEvidenceV2{}, errors.New("staged member owner required")
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.observeOriginalMember(ctx, key)
}

func (s *StagedWindowsCanonicalEntrySetV2) TakeForTransaction(ctx context.Context, entry string) (WindowsCanonicalEntrySetSnapshotV2, error) {
	if s == nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.New("staged member owner required")
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.retainsRuntime {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.New("retained runtime stage cannot transfer to the full-member transaction")
	}
	if c.transferred {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.New("member custody already transferred")
	}
	if err := c.verify(ctx); err != nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	if entry != c.targets.CanonicalEntryPath {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.New("member transfer destination mismatch")
	}
	c.transferred = true // Cancellation after this latch belongs to the transaction.
	return cloneCanonicalEntrySnapshotV2(c.snapshot), nil
}

// TakeRetainingForTransaction transfers the selective stage to the existing
// Product transaction. Its caller receives the first V3 write/readback and
// each exact move or discard intent before entering the corresponding effect.
func (s *StagedWindowsCanonicalEntrySetV2) TakeRetainingForTransaction(ctx context.Context, entry string, expected WindowsCanonicalEntryRetainedMembersV2) (WindowsCanonicalEntrySetSnapshotV2, error) {
	if s == nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.New("selective stage required")
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if !s.retainsRuntime || s.prior == nil || s.priorError != nil || c.transferred || entry != c.targets.CanonicalEntryPath {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.Join(s.priorError, errors.New("selective original custody or target is unavailable"))
	}
	pc := &s.prior.original
	pc.mu.Lock()
	defer pc.mu.Unlock()
	witness, err := s.retainedMembersWitnessLocked(ctx)
	if err != nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	if !reflect.DeepEqual(witness, expected) {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.New("current selective original witness differs")
	}
	if len(c.keys) == 0 {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.New("selective transfer has no changed originals")
	}
	changed := make(map[string]bool, len(c.keys))
	for _, key := range c.keys {
		if _, err := pc.observeOriginalMember(ctx, key); err != nil {
			return WindowsCanonicalEntrySetSnapshotV2{}, err
		}
		changed[key] = true
	}
	if err := ctx.Err(); err != nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	pc.warmChanged = changed
	c.transferred = true
	return cloneCanonicalEntrySnapshotV2(c.snapshot), nil
}

func (c *canonicalEntryMembersV2) move(ctx context.Context, key, destination string) (result WindowsCanonicalEntryMoveResultV1, retErr error) {
	defer func() { result.Error = errors.Join(retErr, result.CloseError); retErr = result.Error }()
	if _, retained := c.recoveryRetained[key]; retained {
		return WindowsCanonicalEntryMoveResultV1{Member: key, DestinationRelative: destination}, errors.New("retained runtime member is read-only")
	}
	m := c.members[key]
	if m == nil {
		return WindowsCanonicalEntryMoveResultV1{Member: key, DestinationRelative: destination}, errors.New("unissued member key")
	}
	if err := c.verifyEffectMember(ctx, key); err != nil {
		return WindowsCanonicalEntryMoveResultV1{Member: key, DestinationRelative: destination, Error: err, OriginalOwnerPending: !c.closed}, err
	}
	// A member may select its own final key or an admitted transaction aside;
	// it cannot replace another protected final member.
	for _, final := range c.root.memberFinalPaths {
		if strings.EqualFold(final, destination) && final != key {
			return WindowsCanonicalEntryMoveResultV1{Member: key}, errors.New("member destination selects another final member")
		}
	}
	if err := c.authorizeRecoveryEffect(ctx, key, destination, false); err != nil {
		return WindowsCanonicalEntryMoveResultV1{Member: key, DestinationRelative: destination, Error: err, OriginalOwnerPending: !c.closed}, err
	}
	r, err := canonicalEntryMoveOriginal(ctx, c.root, c.parent, m.original, key, destination, m.evidence.File.SHA256)
	if r.Applied {
		actual, e := c.receiveMember(context.Background(), key, m.original, m.evidence.File.SHA256)
		if e == nil {
			m.evidence = actual
		}
		err = errors.Join(err, e)
		r.Error = err
		if e != nil {
			r.Readback = false
		}
	}
	c.moves = append(c.moves, r)
	return r, err
}
func (c *canonicalEntryMembersV2) discard(ctx context.Context, key, relative string) (result WindowsCanonicalEntryMoveResultV1, retErr error) {
	defer func() { result.Error = errors.Join(retErr, result.CloseError); retErr = result.Error }()
	if _, retained := c.recoveryRetained[key]; retained {
		return WindowsCanonicalEntryMoveResultV1{Member: key, SourceRelative: relative}, errors.New("retained runtime member is read-only")
	}
	m := c.members[key]
	if m == nil {
		return WindowsCanonicalEntryMoveResultV1{Member: key}, errors.New("unissued member key")
	}
	if m.cleanup != nil && m.cleanup.Applied {
		return *m.cleanup, m.cleanup.Error
	}
	if err := c.verifyEffectMember(ctx, key); err != nil {
		return WindowsCanonicalEntryMoveResultV1{Member: key, Error: err}, err
	}
	if err := c.authorizeRecoveryEffect(ctx, key, relative, true); err != nil {
		return WindowsCanonicalEntryMoveResultV1{Member: key, SourceRelative: relative, Error: err, OriginalOwnerPending: !c.closed}, err
	}
	r, err := canonicalEntryDiscardOriginal(ctx, c.root, c.parent, m.original, key, relative, m.evidence.File.SHA256)
	c.cleanup = append(c.cleanup, r)
	if r.Applied {
		copy := r
		m.cleanup = &copy
	}
	return r, err
}
func (s *StagedWindowsCanonicalEntrySetV2) MoveOriginal(ctx context.Context, key, dst string) (WindowsCanonicalEntryMoveResultV1, error) {
	if s == nil {
		return WindowsCanonicalEntryMoveResultV1{}, errors.New("stage required")
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.transferred {
		return WindowsCanonicalEntryMoveResultV1{Member: key}, errors.New("move requires transferred custody")
	}
	return c.move(ctx, key, dst)
}
func (s *StagedWindowsCanonicalEntrySetV2) DiscardOriginal(ctx context.Context, key, relative string) (WindowsCanonicalEntryMoveResultV1, error) {
	if s == nil {
		return WindowsCanonicalEntryMoveResultV1{}, errors.New("stage required")
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.transferred {
		return WindowsCanonicalEntryMoveResultV1{Member: key}, errors.New("transaction discard requires transferred custody")
	}
	return c.discard(ctx, key, relative)
}
func (s *StagedWindowsCanonicalEntrySetV2) Disposition() WindowsCanonicalEntryMemberDispositionV2 {
	if s == nil {
		return WindowsCanonicalEntryMemberDispositionV2{}
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disposition()
}

// Directories returns the native bank's actual deepest-first lender union.
// It is passive and remains available for partial/closed member owners.
func (s *StagedWindowsCanonicalEntrySetV2) Directories() []*WindowsCanonicalEntryPayloadDirectoryV1 {
	if s == nil {
		return nil
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.directoryOwners()
}

func (s *StagedWindowsCanonicalEntrySetV2) Close() error {
	if s == nil {
		return nil
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	s.closeMembersLocked()
	if s.ownsPrior && !s.directoryCloseEntered {
		s.directoryCloseEntered = true
		for _, directory := range c.directoryOwners() {
			c.closeError = errors.Join(c.closeError, directory.Close())
		}
	}
	return c.closeError
}

// CloseMembers settles the file banks without closing the directory lenders.
// The existing transaction may then dispose its eligible original directories
// before the final Close. Untransferred stage files keep their normal cleanup.
func (s *StagedWindowsCanonicalEntrySetV2) CloseMembers() error {
	if s == nil {
		return nil
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	return s.closeMembersLocked()
}

func (s *StagedWindowsCanonicalEntrySetV2) closeMembersLocked() error {
	c := &s.original
	if c.closed {
		return c.closeError
	}
	if s.ownsPrior && s.prior != nil {
		c.closeError = errors.Join(c.closeError, s.prior.Close())
	}
	if !c.transferred {
		prefix := planCanonicalEntryStageClosePrefixV2(c.root, c.parent)
		if s.prior != nil && s.priorError == nil {
			prior := s.prior
			prior.original.mu.Lock()
			if prior.observation.BeforeKind == WindowsCanonicalEntryBeforeBundleV2 &&
				prior.original.closed && prior.original.closeError == nil {
				if parents := planCanonicalEntryClosePriorParentsV2(c.root, c.parent); parents != nil {
					prefix = &canonicalEntryStageClosePrefixV2{root: c.root, parent: c.parent, prior: parents}
				}
			}
			prior.original.mu.Unlock()
		}
		for _, key := range c.keys {
			m := c.members[key]
			if m == nil {
				continue
			}
			if m.evidence.File.SHA256 == "" {
				c.closeError = errors.Join(c.closeError, errors.New("produced member preimage unavailable; retained disposition pending"))
				continue
			}
			if e := canonicalEntrySealOriginalStage(c.root, c.parent, m.original); e != nil {
				c.closeError = errors.Join(c.closeError, e)
				continue
			}
			r, e := canonicalEntryDiscardOriginalWithClosePrefix(context.Background(), c.root, c.parent, m.original, key, m.evidence.File.Path, m.evidence.File.SHA256, prefix)
			c.cleanup = append(c.cleanup, r)
			if r.Applied {
				copy := r
				m.cleanup = &copy
			}
			c.closeError = errors.Join(c.closeError, e)
		}
	}
	return c.close()
}

func (d *WindowsCanonicalEntryPayloadDirectoryV1) SnapshotForEntryMembersV2() (WindowsCanonicalEntryMemberDirectorySnapshotV2, error) {
	return d.snapshotForEntryMembersRead(context.Background(), nil)
}

func (d *WindowsCanonicalEntryPayloadDirectoryV1) snapshotForEntryMembersRead(ctx context.Context, scope *canonicalEntryRecoveryReadScopeV2) (WindowsCanonicalEntryMemberDirectorySnapshotV2, error) {
	if d == nil {
		return WindowsCanonicalEntryMemberDirectorySnapshotV2{}, errors.New("member directory required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.memberSnapshot == nil {
		return WindowsCanonicalEntryMemberDirectorySnapshotV2{}, errors.New("historical directory cannot emit current member evidence")
	}
	if err := d.validateReadLocked(ctx, scope); err != nil {
		return *d.memberSnapshot, err
	}
	return *d.memberSnapshot, nil
}

func canonicalEntryBorrowMemberDirectoryV2(root *WindowsCanonicalEntrySetV1, parent *canonicalEntryNativeFile, component string) (*canonicalEntryNativeFile, bool, error) {
	return canonicalEntryBorrowMemberDirectoryReadV2(context.Background(), root, parent, component, nil)
}

func canonicalEntryBorrowMemberDirectoryReadV2(ctx context.Context, root *WindowsCanonicalEntrySetV1, parent *canonicalEntryNativeFile, component string, scope *canonicalEntryRecoveryReadScopeV2) (*canonicalEntryNativeFile, bool, error) {
	index := root.memberDirectories
	index.mu.Lock()
	defer index.mu.Unlock()
	name := filepath.Join(parent.path, component)
	var found *WindowsCanonicalEntryPayloadDirectoryV1
	for _, d := range index.owners {
		if d.directory != nil && strings.EqualFold(d.directory.path, name) {
			if found != nil && found != d {
				return nil, false, errors.New("ambiguous original directory owner")
			}
			found = d
		}
	}
	if found == nil {
		return nil, false, nil
	}
	d := found
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.validateReadLocked(ctx, scope); err != nil {
		return nil, false, err
	}
	actualParent, err := canonicalPayloadDirectoryIdentity(parent)
	if err != nil {
		return nil, false, err
	}
	if actualParent != d.memberSnapshot.ParentIdentity {
		return nil, false, errors.New("member directory original parent changed")
	}
	h, err := canonicalEntryOpenNative(windows.Handle(parent.file.Fd()), component, true, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
	if err != nil {
		return nil, false, err
	}
	id, e := canonicalPayloadDirectoryHandleIdentity(h)
	closeErr := windows.CloseHandle(h)
	if err = errors.Join(e, closeErr); err != nil {
		return nil, false, err
	}
	if id != d.memberSnapshot.DirectoryIdentity {
		return nil, false, errors.New("member directory name selects a different original")
	}
	if root.memberDirectoryBorrows == nil {
		root.memberDirectoryBorrows = make(map[*WindowsCanonicalEntryPayloadDirectoryV1]bool)
	}
	if !root.memberDirectoryBorrows[d] {
		if d.borrowers == ^uint64(0) {
			return nil, false, errors.New("member directory borrower overflow")
		}
		d.borrowers++
		root.memberDirectoryBorrows[d] = true
	}
	return d.directory, true, nil
}
func (o *WindowsCanonicalEntrySetV1) releaseMemberDirectoryBorrowsV2() error {
	for d := range o.memberDirectoryBorrows {
		d.mu.Lock()
		if d.borrowers == 0 {
			d.mu.Unlock()
			return errors.New("member directory borrow settlement unavailable")
		}
		d.borrowers--
		delete(o.memberDirectoryBorrows, d)
		d.mu.Unlock()
	}
	return nil
}

// Registration is owned by the existing lender validation path, including
// cached and terminal directories. The bank retains this borrow until Close.
func (c *canonicalEntryMembersV2) borrowDirectoryRead(ctx context.Context, relative string, d *WindowsCanonicalEntryPayloadDirectoryV1, scope *canonicalEntryRecoveryReadScopeV2) error {
	if d == nil || d.memberSnapshot == nil || d.parent == nil || c.parent == nil || d.memberSnapshot.Root != c.parent.path || d.memberSnapshot.RelativePath != relative {
		return errors.New("original member directory belongs to another root or path")
	}
	original, matched, err := canonicalEntryBorrowMemberDirectoryReadV2(ctx, c.root, d.parent, path.Base(relative), scope)
	if err != nil || !matched || original != d.directory {
		return errors.Join(err, errors.New("original member directory registered borrow unavailable"))
	}
	return ctx.Err()
}

// Walk each exact native parent. A negative is evidence of the first missing
// ancestor, never an inference from the plan or the directory-owner map.
func (c *canonicalEntryMembersV2) observeDirectory(ctx context.Context, relative string) (*WindowsCanonicalEntryMemberAbsenceV2, error) {
	parent, parentRelative := c.parent, ""
	parts := strings.Split(relative, "/")
	for i := range parts {
		key := strings.Join(parts[:i+1], "/")
		d, err := c.directory(ctx, key, false, nil)
		if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
			identity, e := canonicalPayloadDirectoryIdentity(parent)
			return &WindowsCanonicalEntryMemberAbsenceV2{parentRelative, identity, key}, e
		}
		if err != nil {
			return nil, err
		}
		parent, parentRelative = d.directory, key
	}
	return nil, ctx.Err()
}

// Only the immutable authenticated member plan and the current fenced source
// call this helper. Missing recorded prior parents are never created here.
func (c *canonicalEntryMembersV2) directory(ctx context.Context, relative string, create bool, expected *WindowsCanonicalEntryMemberDirectorySnapshotV2) (*WindowsCanonicalEntryPayloadDirectoryV1, error) {
	return c.directoryRead(ctx, relative, create, expected, nil)
}

func (c *canonicalEntryMembersV2) directoryRead(ctx context.Context, relative string, create bool, expected *WindowsCanonicalEntryMemberDirectorySnapshotV2, scope *canonicalEntryRecoveryReadScopeV2) (*WindowsCanonicalEntryPayloadDirectoryV1, error) {
	if scope != nil {
		if create {
			return nil, errors.New("read-only recovery cannot create a directory")
		}
		if err := scope.validateBank(ctx, c); err != nil {
			return nil, err
		}
	}
	if ctx == nil || !publisherSourcePathValid(relative) {
		return nil, errors.New("confined member directory input required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.directories.mu.Lock()
	existing := c.directories.owners[relative]
	c.directories.mu.Unlock()
	if existing != nil {
		snapshot, err := existing.snapshotForEntryMembersRead(ctx, scope)
		if expected != nil && snapshot != *expected {
			return existing, errors.Join(err, errors.New("recorded member directory disagrees with original"))
		}
		if err == nil {
			err = c.borrowDirectoryRead(ctx, relative, existing, scope)
		}
		return existing, err
	}
	d := &WindowsCanonicalEntryPayloadDirectoryV1{guard: &WindowsCanonicalEntrySetV1{memberDirectories: c.directories}, memberSource: c.source}
	d.memberSnapshot = &WindowsCanonicalEntryMemberDirectorySnapshotV2{Schema: WindowsCanonicalEntryMemberDirectorySchemaV2, Root: c.parent.path, RelativePath: relative}
	c.directories.mu.Lock()
	c.directories.owners[relative] = d
	c.directories.mu.Unlock()
	var err error
	d.parent, err = d.guard.openAbsolute(c.parent.path, true)
	if err != nil {
		d.acquisitionError = err
		return d, err
	}
	rootID, e := canonicalPayloadDirectoryIdentity(d.parent)
	if e != nil {
		d.acquisitionError = e
		return d, e
	}
	expectedRoot, e := canonicalPayloadDirectoryIdentity(c.parent)
	if e != nil || rootID != expectedRoot {
		err = errors.Join(e, errors.New("member directory original root changed"))
		d.acquisitionError = err
		return d, err
	}
	if path.Dir(relative) != "." {
		parent, _, err := canonicalEntryRelativeParentRead(ctx, d.guard, d.parent, relative, scope)
		if err != nil {
			d.acquisitionError = err
			return d, err
		}
		d.parent = parent
	}
	parentID, err := canonicalPayloadDirectoryIdentity(d.parent)
	if err != nil {
		d.acquisitionError = err
		return d, err
	}
	d.memberSnapshot.ParentIdentity = parentID
	access := uint32(windows.FILE_TRAVERSE | windows.FILE_LIST_DIRECTORY)
	disposition := uint32(windows.FILE_OPEN)
	if create {
		access |= windows.DELETE
		disposition = windows.FILE_CREATE
	}
	if expected != nil {
		if expected.Schema != WindowsCanonicalEntryMemberDirectorySchemaV2 || expected.Root != c.parent.path || expected.RelativePath != relative || expected.ParentIdentity != parentID {
			err = errors.New("invalid current original directory recovery evidence")
			d.acquisitionError = err
			return d, err
		}
		if expected.Created {
			access |= windows.DELETE
		}
	}
	h, err := canonicalEntryOpenNative(windows.Handle(d.parent.file.Fd()), path.Base(relative), true, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, disposition)
	if err != nil {
		d.acquisitionError = err
		return d, err
	}
	d.memberSnapshot.Created = create
	d.directory, err = d.guard.retain(h, filepath.Join(c.parent.path, filepath.FromSlash(relative)), true)
	if err != nil {
		d.acquisitionError = err
		return d, err
	}
	d.memberSnapshot.Created = create
	d.memberSnapshot.DirectoryIdentity, err = canonicalPayloadDirectoryIdentity(d.directory)
	if err == nil && d.memberSnapshot.DirectoryIdentity.VolumeSerialNumber != parentID.VolumeSerialNumber {
		err = errors.New("member directory crosses root volume")
	}
	if err == nil && expected != nil {
		d.memberSnapshot.Created = expected.Created
		if *d.memberSnapshot != *expected {
			err = errors.New("current recovery directory identity or creation differs")
		}
	}
	d.acquisitionError = err
	if err == nil {
		c.directories.mu.Lock()
		for name, other := range c.directories.owners {
			if name != relative && other.ready && other.memberSnapshot != nil && other.memberSnapshot.DirectoryIdentity == d.memberSnapshot.DirectoryIdentity {
				err = errors.New("declared member directories alias one original")
				break
			}
		}
		c.directories.mu.Unlock()
		d.acquisitionError = err
	}
	d.ready = err == nil
	if err == nil {
		err = c.borrowDirectoryRead(ctx, relative, d, scope)
	}
	return d, err
}

func (c *canonicalEntryMembersV2) openMember(ctx context.Context, key, relative string) (*canonicalEntryOwnedMemberV2, *WindowsCanonicalEntryMemberAbsenceV2, error) {
	return c.openMemberRead(ctx, key, relative, nil)
}

func (c *canonicalEntryMembersV2) openMemberRead(ctx context.Context, key, relative string, scope *canonicalEntryRecoveryReadScopeV2) (*canonicalEntryOwnedMemberV2, *WindowsCanonicalEntryMemberAbsenceV2, error) {
	return c.openMemberWithAccess(ctx, key, relative, windows.FILE_READ_DATA|windows.DELETE, scope)
}

func (c *canonicalEntryMembersV2) openMemberWithAccess(ctx context.Context, key, relative string, access uint32, scope *canonicalEntryRecoveryReadScopeV2) (*canonicalEntryOwnedMemberV2, *WindowsCanonicalEntryMemberAbsenceV2, error) {
	if scope != nil {
		if err := scope.validateBank(ctx, c); err != nil {
			return nil, nil, err
		}
	}
	if ctx == nil || !publisherSourcePathValid(key) || !publisherSourcePathValid(relative) {
		return nil, nil, errors.New("exact member location required")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	parts := strings.Split(relative, "/")
	parent := c.parent
	parentRelative := ""
	for n, component := range parts[:len(parts)-1] {
		dirRelative := strings.Join(parts[:n+1], "/")
		d, err := c.directoryRead(ctx, dirRelative, false, nil, scope)
		if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
			id, e := canonicalPayloadDirectoryIdentity(parent)
			return nil, &WindowsCanonicalEntryMemberAbsenceV2{parentRelative, id, dirRelative}, e
		}
		if err != nil {
			return nil, nil, err
		}
		borrowed, matched, err := canonicalEntryBorrowMemberDirectoryReadV2(ctx, c.root, parent, component, scope)
		if err != nil || !matched {
			return nil, nil, errors.Join(err, errors.New("original member parent borrow missing"))
		}
		parent = borrowed
		parentRelative = dirRelative
		_ = d
	}
	h, err := canonicalEntryOpenNative(windows.Handle(parent.file.Fd()), parts[len(parts)-1], false, access, windows.FILE_SHARE_READ, windows.FILE_OPEN)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
		id, e := canonicalPayloadDirectoryIdentity(parent)
		return nil, &WindowsCanonicalEntryMemberAbsenceV2{parentRelative, id, relative}, e
	}
	if err != nil {
		return nil, nil, err
	}
	file, err := c.root.retain(h, filepath.Join(c.parent.path, filepath.FromSlash(relative)), false)
	if err != nil {
		return nil, nil, err
	}
	m := &canonicalEntryOwnedMemberV2{original: file}
	c.members[key] = m
	var hash string
	if key == c.snapshot.RuntimePayload.Path {
		index, readErr := readHeldCanonicalRuntimeIndex(ctx, file.file)
		err = readErr
		hash = RuntimeDigest(index)
	} else {
		hash, err = canonicalEntryHashFile(ctx, file.file)
	}
	if err != nil {
		return m, nil, err
	}
	m.evidence, err = c.receiveMemberRead(ctx, key, file, hash, scope)
	return m, nil, err
}

func (s *StagedWindowsCanonicalEntrySetV2) EnsureOriginalMemberDirectories(ctx context.Context) (out []*WindowsCanonicalEntryPayloadDirectoryV1, retErr error) {
	if s == nil {
		return nil, errors.New("stage required")
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() {
		owners := c.directoryOwners()
		out = make([]*WindowsCanonicalEntryPayloadDirectoryV1, 0, len(owners))
		for i := len(owners) - 1; i >= 0; i-- {
			out = append(out, owners[i])
		}
	}()
	if err := c.verify(ctx); err != nil {
		return nil, err
	}
	if !s.priorAttempted || s.priorError != nil || s.prior == nil {
		return nil, errors.Join(s.priorError, errors.New("original prior observation required before directory creation"))
	}
	ordered, err := canonicalMemberDirectoryPlanV2(c.snapshot)
	if err != nil {
		return out, err
	}
	for _, relative := range ordered {
		c.directories.mu.Lock()
		d := c.directories.owners[relative]
		c.directories.mu.Unlock()
		if d != nil && d.ready {
			_, err := c.directory(ctx, relative, false, nil)
			out = append(out, d)
			if err != nil {
				return out, err
			}
			continue
		}
		// Creation requires a pre-acquired negative lookup covering this exact
		// missing path; later occupants are refused by native FILE_CREATE.
		observed := false
		for _, a := range s.prior.observation.Absences {
			if a.MissingRelative == relative || strings.HasPrefix(relative, a.MissingRelative+"/") {
				observed = true
				break
			}
		}
		if !observed {
			return out, errors.New("member directory lacks original absence evidence")
		}
		if d != nil { // A failed negative lookup acquired no original directory.
			if d.directory != nil {
				return out, errors.New("partial member directory ownership unresolved")
			}
			if err := d.Close(); err != nil {
				return out, err
			}
			c.directories.mu.Lock()
			delete(c.directories.owners, relative)
			c.directories.mu.Unlock()
		}
		var err error
		d, err = c.directory(ctx, relative, true, nil)
		out = append(out, d)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// The existing runtime parser authenticates metadata before it can select any
// descendants. Only declared generations enter this installed inventory.
func canonicalDeclaredBundleInventoryV2(ctx context.Context, bundlePath string, identity WindowsProductIdentityV2, manifest []byte, product, identityFile *canonicalEntryNativeFile) (inventory RuntimeInventoryV1, retErr error) {
	root, err := os.OpenRoot(bundlePath)
	if err != nil {
		return inventory, err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	inspection, err := inspectInstalledRuntimePayload(ctx, root, identity.RuntimePayloadPath, identity.RuntimePayloadSHA256, manifest)
	if err != nil {
		return inventory, err
	}
	objects := map[string]RuntimeObjectV1{}
	parents := map[string]bool{}
	for _, file := range inspection.Files {
		info, e := root.Lstat(file.Path)
		if e != nil || runtimeReparse(info) || !info.Mode().IsRegular() {
			return inventory, errors.Join(e, errors.New("declared installed file is not ordinary"))
		}
		size := file.Size
		objects[file.Path] = RuntimeObjectV1{Path: file.Path, Kind: "file", Size: &size, SHA256: file.SHA256, Attributes: runtimeAttributes(info)}
	}
	for _, original := range []*canonicalEntryNativeFile{product, identityFile} {
		_, size, attrs, e := canonicalMemberFileIdentityV2(original)
		if e != nil {
			return inventory, e
		}
		hash, e := canonicalEntryHashFile(ctx, original.file)
		if e != nil {
			return inventory, e
		}
		name := filepath.Base(original.path)
		objects[name] = RuntimeObjectV1{Path: name, Kind: "file", Size: &size, SHA256: hash, Attributes: attrs}
	}
	for _, component := range inspection.Components {
		parents[component.Component.Root] = true
		for _, object := range component.Inventory.Objects {
			if object.Kind == "directory" {
				name := path.Join(component.Component.Root, object.Path)
				parents[name] = true
				objects[name] = RuntimeObjectV1{Path: name, Kind: "directory", Attributes: object.Attributes}
			}
		}
	}
	for name := range objects {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			parents[parent] = true
		}
	}
	for name := range parents {
		info, e := root.Lstat(name)
		if e != nil || runtimeReparse(info) || !info.IsDir() {
			return inventory, errors.Join(e, errors.New("declared installed parent is not ordinary"))
		}
		attrs := runtimeAttributes(info)
		if expected, ok := objects[name]; ok && expected.Attributes != attrs {
			return inventory, errors.New("declared installed directory attributes changed")
		}
		objects[name] = RuntimeObjectV1{Path: name, Kind: "directory", Attributes: attrs}
	}
	inventory = RuntimeInventoryV1{Schema: RuntimeInventorySchemaV1, Target: inspection.Payload.Target, Objects: make([]RuntimeObjectV1, 0, len(objects))}
	for _, object := range objects {
		inventory.Objects = append(inventory.Objects, object)
	}
	sort.Slice(inventory.Objects, func(i, j int) bool { return inventory.Objects[i].Path < inventory.Objects[j].Path })
	raw, err := CanonicalRuntimeJSON(inventory)
	if err == nil {
		_, err = ParseRuntimeInventory(raw)
	}
	return inventory, err
}

func canonicalEntryPriorProbeKeysV2(incoming []string, prior map[string]*canonicalEntryOwnedMemberV2, kind WindowsCanonicalEntryBeforeKindV2) ([]string, error) {
	switch kind {
	case WindowsCanonicalEntryBeforeAbsentV2, WindowsCanonicalEntryBeforeRootProductV2, WindowsCanonicalEntryBeforeLegacyV2, WindowsCanonicalEntryBeforeBundleV2:
	default:
		return nil, errors.New("original prior kind is not classified")
	}
	seen := make(map[string]bool, len(incoming)+len(prior))
	for _, key := range incoming {
		seen[key] = true
	}
	for key := range prior {
		seen[key] = true
	}
	// A current bundle authenticates its adjacent identity. Its unrelated root
	// V1 metadata neighbor is neither a prior member nor a missing target.
	if kind != WindowsCanonicalEntryBeforeBundleV2 {
		seen[WindowsCanonicalIdentityRelativePathV1] = true
	}
	keys := make([]string, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *StagedWindowsCanonicalEntrySetV2) AcquireOriginalPriorSet(ctx context.Context) (*WindowsCanonicalEntryPriorSetV2, error) {
	if s == nil {
		return nil, errors.New("stage required")
	}
	c := &s.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.retainsRuntime {
		return nil, errors.New("selective stage requires its original prior handoff")
	}
	if s.priorAttempted {
		return s.prior, s.priorError
	}
	return s.acquireOriginalPriorSet(ctx, c.keys, c.verify(ctx))
}

// The caller owns c.mu, or the unexposed Stage while composing its native
// destination. incoming is the complete candidate namespace in both modes.
func (s *StagedWindowsCanonicalEntrySetV2) acquireOriginalPriorSet(ctx context.Context, incoming []string, stageErr error) (*WindowsCanonicalEntryPriorSetV2, error) {
	c := &s.original
	s.priorAttempted = true
	p := &WindowsCanonicalEntryPriorSetV2{original: newCanonicalEntryMembersV2(), readOnly: s.ownsPrior}
	s.prior = p
	pc := &p.original
	pc.targets = c.targets
	pc.snapshot = cloneCanonicalEntrySnapshotV2(c.snapshot)
	pc.directories = c.directories
	pc.root.memberDirectories = c.directories
	s.priorError = func() error {
		if stageErr != nil {
			return stageErr
		}
		var err error
		pc.parent, err = pc.root.openAbsolute(c.parent.path, true)
		if err != nil {
			return err
		}
		if pc.parent.identity != c.parent.identity {
			return errors.New("prior root differs from stage root")
		}
		p.observation.Absences = make(map[string]WindowsCanonicalEntryMemberAbsenceV2)
		entry, absence, err := pc.openMember(ctx, WindowsCanonicalEntryRelativePathV1, WindowsCanonicalEntryRelativePathV1)
		if err != nil {
			return err
		}
		if absence != nil {
			p.observation.BeforeKind = WindowsCanonicalEntryBeforeAbsentV2
			p.observation.Absences[WindowsCanonicalEntryRelativePathV1] = *absence
		} else {
			info, err := entry.original.file.Stat()
			if err != nil {
				return err
			}
			header, err := readWindowsPEHeader(entry.original.file, info.Size())
			if err != nil {
				return err
			}
			switch header.subsystem {
			case WindowsGUISubsystem:
				metadata, err := readWindowsBuildIdentityAt(entry.original.file, info.Size())
				if err != nil {
					return err
				}
				if metadata.Role != WindowsArtifactRoleProduct || header.characteristics&windowsPEDLLCharacteristic != 0 {
					return errors.New("root prior is not an admitted original Product")
				}
				comments, err := parseWindowsBuildCommentsComposition(metadata.Comments)
				if err != nil {
					return err
				}
				if comments.runtimeSHA256 != "" {
					return errors.New("current runtime Product cannot enter the manifestless legacy root mode")
				}
				p.observation.BeforeKind = WindowsCanonicalEntryBeforeRootProductV2
				p.runtimeMember = WindowsCanonicalEntryRelativePathV1
				p.artifact = WindowsArtifact{Path: entry.original.path, Role: metadata.Role, Version: metadata.Version, Commit: metadata.Commit, BuildDate: metadata.BuildDate, SHA256: entry.evidence.File.SHA256, Machine: header.machine, Subsystem: header.subsystem}
			case WindowsCUISubsystem:
				product, productAbsence, e := pc.openMember(ctx, WindowsCanonicalProductRelativePathV1, WindowsCanonicalProductRelativePathV1)
				if e != nil || productAbsence != nil {
					return errors.Join(e, errors.New("prior launcher has no original Product"))
				}
				productInfo, e := product.original.file.Stat()
				if e != nil {
					return e
				}
				metadata, e := readWindowsBuildIdentityAt(product.original.file, productInfo.Size())
				if e != nil {
					return e
				}
				comments, e := parseWindowsBuildCommentsComposition(metadata.Comments)
				if e != nil {
					return e
				}
				current := comments.runtimeSHA256 != ""
				identityRelative := WindowsCanonicalIdentityRelativePathV1
				if current {
					identityRelative = c.snapshot.IdentityRelativePath
				}
				identityMember, missing, e := pc.openMember(ctx, identityRelative, identityRelative)
				if e != nil || missing != nil {
					return errors.Join(e, errors.New("prior launcher has no complete identity in its declared layout"))
				}
				p.runtimeMember = WindowsCanonicalProductRelativePathV1
				if current {
					registry, e := CompiledWindowsProductRegistry()
					if e != nil {
						return e
					}
					targets := WindowsCanonicalEntrySetTargetsV1{CanonicalEntryPath: entry.original.path, ProductPath: product.original.path, IdentityPath: identityMember.original.path}
					base, identity, currentComments, e := receiveCanonicalEntryFilesWithRegistry(ctx, [3]*os.File{entry.original.file, product.original.file, identityMember.original.file}, targets, nil, "", registry)
					if e != nil {
						return e
					}
					p.artifact = base.ProductArtifact
					indexKey := path.Join(c.snapshot.BundleRelativePath, identity.RuntimePayloadPath)
					index, missing, e := pc.openMember(ctx, indexKey, indexKey)
					if e != nil || missing != nil {
						return errors.Join(e, errors.New("current prior runtime index is missing"))
					}
					manifest, e := readHeldCanonicalRuntimeIndex(ctx, index.original.file)
					if e != nil {
						return e
					}
					inventory, e := canonicalDeclaredBundleInventoryV2(ctx, c.targets.BundlePath, identity, manifest, product.original, identityMember.original)
					if e != nil {
						return e
					}
					priorSnapshot, e := currentCanonicalEntrySnapshot(base, currentComments.entry, manifest, inventory)
					if e != nil {
						return e
					}
					p.observation.BeforeKind = WindowsCanonicalEntryBeforeBundleV2
					p.observation.Inventory = &inventory
					priorKeys, priorPlan, e := canonicalMemberPlanV2(priorSnapshot)
					if e != nil {
						return e
					}
					for _, key := range priorKeys {
						member := pc.members[key]
						if member == nil {
							var missing *WindowsCanonicalEntryMemberAbsenceV2
							member, missing, e = pc.openMember(ctx, key, key)
							if e != nil || missing != nil {
								return errors.Join(e, errors.New("declared prior member missing"))
							}
						}
						if member == nil || !canonicalMemberMatchesPlanV2(member.evidence, priorPlan[key]) {
							return fmt.Errorf("original current prior member %q differs from authenticated snapshot plan", key)
						}
					}
					priorDirectories, e := canonicalMemberDirectoryPlanV2(priorSnapshot)
					if e != nil {
						return e
					}
					for _, relative := range priorDirectories {
						missing, e := pc.observeDirectory(ctx, relative)
						if e != nil {
							return e
						}
						if missing != nil {
							return errors.New("declared prior directory missing")
						}
					}
					pc.snapshot = priorSnapshot
				} else {
					legacy := WindowsCanonicalEntrySetV1{entry: entry.original, product: product.original, identity: identityMember.original}
					if e := legacy.receive(ctx, nil, ""); e != nil {
						return e
					}
					if e := validateLegacyCanonicalEntryBuild(comments, legacy.snapshot); e != nil {
						return e
					}
					p.artifact = legacy.snapshot.ProductArtifact
					p.observation.BeforeKind = WindowsCanonicalEntryBeforeLegacyV2
				}
			default:
				return errors.New("prior entry has no admitted Product/launcher role")
			}
		}
		keys, err := canonicalEntryPriorProbeKeysV2(incoming, pc.members, p.observation.BeforeKind)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if pc.members[key] != nil {
				continue
			}
			_, missing, e := pc.openMember(ctx, key, key)
			if e != nil {
				return e
			}
			if missing == nil {
				return errors.New("new selected leaf is occupied outside authenticated prior membership")
			}
			p.observation.Absences[key] = *missing
		}
		incomingDirectories, err := canonicalMemberDirectoryPlanV2(c.snapshot)
		if err != nil {
			return err
		}
		for _, relative := range incomingDirectories {
			missing, err := pc.observeDirectory(ctx, relative)
			if err != nil {
				return err
			}
			if missing != nil {
				p.observation.Absences[relative] = *missing
			}
		}
		for key := range pc.members {
			pc.keys = append(pc.keys, key)
		}
		sort.Strings(pc.keys)
		pc.root.memberFinalPaths = append([]string(nil), keys...)
		c.root.memberFinalPaths = append([]string(nil), keys...)
		if err := pc.verifyRead(ctx, nil); err != nil {
			return err
		}
		pc.ready = true
		return nil
	}()
	pc.acquisitionError = s.priorError
	return p, s.priorError
}

func (p *WindowsCanonicalEntryPriorSetV2) Observation(ctx context.Context) (WindowsCanonicalEntryPriorObservationV2, error) {
	if p == nil {
		return WindowsCanonicalEntryPriorObservationV2{}, errors.New("prior required")
	}
	c := &p.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx == nil {
		return WindowsCanonicalEntryPriorObservationV2{}, errors.New("member context required")
	}
	if err := ctx.Err(); err != nil {
		return WindowsCanonicalEntryPriorObservationV2{}, err
	}
	if !c.ready {
		return WindowsCanonicalEntryPriorObservationV2{}, errors.Join(c.acquisitionError, errors.New("original member owner is not ready/open"))
	}
	var result WindowsCanonicalEntryPriorObservationV2
	err := c.withRecoveryReadScope(ctx, func(scope *canonicalEntryRecoveryReadScopeV2) error {
		var err error
		result, err = p.observationRead(ctx, scope)
		return err
	})
	return result, err
}

func (p *WindowsCanonicalEntryPriorSetV2) observationRead(ctx context.Context, scope *canonicalEntryRecoveryReadScopeV2) (WindowsCanonicalEntryPriorObservationV2, error) {
	c := &p.original
	if err := c.verifyRead(ctx, scope); err != nil {
		return WindowsCanonicalEntryPriorObservationV2{}, err
	}

	v := p.observation
	v.Members = make([]WindowsCanonicalEntryMemberEvidenceV2, 0, len(c.keys))
	for _, key := range c.keys {
		v.Members = append(v.Members, c.members[key].evidence)
	}
	v.Absences = make(map[string]WindowsCanonicalEntryMemberAbsenceV2, len(p.observation.Absences))
	for key, a := range p.observation.Absences {
		v.Absences[key] = a
	}
	if v.Inventory != nil {
		copy := *v.Inventory
		copy.Objects = append([]RuntimeObjectV1(nil), copy.Objects...)
		for n := range copy.Objects {
			if copy.Objects[n].Size != nil {
				size := *copy.Objects[n].Size
				copy.Objects[n].Size = &size
			}
		}
		v.Inventory = &copy
	}
	c.directories.mu.Lock()
	names := make([]string, 0, len(c.directories.owners))
	for name, d := range c.directories.owners {
		if d.ready {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := strings.Count(names[i], "/"), strings.Count(names[j], "/")
		if a != b {
			return a < b
		}
		return names[i] < names[j]
	})
	owners := make([]*WindowsCanonicalEntryPayloadDirectoryV1, 0, len(names))
	for _, name := range names {
		owners = append(owners, c.directories.owners[name])
	}
	c.directories.mu.Unlock()
	for _, d := range owners {
		snapshot, err := d.snapshotForEntryMembersRead(ctx, scope)
		if err != nil {
			return v, err
		}
		v.Directories = append(v.Directories, snapshot)
		if snapshot.RelativePath == c.snapshot.BundleRelativePath {
			v.BundleDirectory = &WindowsCanonicalEntryPayloadDirectorySnapshotV1{Schema: WindowsCanonicalEntryPayloadDirectorySchemaV1, Root: snapshot.Root, RelativePath: snapshot.RelativePath, Created: false, ParentIdentity: snapshot.ParentIdentity, DirectoryIdentity: snapshot.DirectoryIdentity}
		}
	}
	return v, nil
}
func (p *WindowsCanonicalEntryPriorSetV2) VerifyUnchanged(ctx context.Context) error {
	if p == nil {
		return errors.New("prior required")
	}
	c := &p.original
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.verify(ctx)
}

// ObserveOriginalMember freshly verifies only the selected issued original.
// Observation and VerifyUnchanged retain their whole-bank contracts.
func (p *WindowsCanonicalEntryPriorSetV2) ObserveOriginalMember(ctx context.Context, key string) (WindowsCanonicalEntryMemberEvidenceV2, error) {
	if p == nil {
		return WindowsCanonicalEntryMemberEvidenceV2{}, errors.New("prior required")
	}
	c := &p.original
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.observeOriginalMember(ctx, key)
}

func (p *WindowsCanonicalEntryPriorSetV2) MoveOriginal(ctx context.Context, key, dst string) (WindowsCanonicalEntryMoveResultV1, error) {
	if p == nil {
		return WindowsCanonicalEntryMoveResultV1{}, errors.New("prior required")
	}
	c := &p.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.readOnly && !c.warmChanged[key] {
		return WindowsCanonicalEntryMoveResultV1{Member: key}, errors.New("retained runtime prior is read-only")
	}
	return c.move(ctx, key, dst)
}
func (p *WindowsCanonicalEntryPriorSetV2) DiscardOriginal(ctx context.Context, key, relative string) (WindowsCanonicalEntryMoveResultV1, error) {
	if p == nil {
		return WindowsCanonicalEntryMoveResultV1{}, errors.New("prior required")
	}
	c := &p.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.readOnly && !c.warmChanged[key] {
		return WindowsCanonicalEntryMoveResultV1{Member: key}, errors.New("retained runtime prior is read-only")
	}
	return c.discard(ctx, key, relative)
}
func (p *WindowsCanonicalEntryPriorSetV2) Disposition() WindowsCanonicalEntryMemberDispositionV2 {
	if p == nil {
		return WindowsCanonicalEntryMemberDispositionV2{}
	}
	c := &p.original
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disposition()
}
func (p *WindowsCanonicalEntryPriorSetV2) Close() error {
	if p == nil {
		return nil
	}
	c := &p.original
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.close()
}

func (p *WindowsCanonicalEntryPriorSetV2) RuntimeProduct(ctx context.Context) (*WindowsCanonicalEntryPriorRuntimeV1, error) {
	if p == nil {
		return nil, errors.New("prior required")
	}
	c := &p.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.verify(ctx); err != nil {
		return nil, err
	}
	if p.observation.BeforeKind == WindowsCanonicalEntryBeforeAbsentV2 {
		return nil, nil
	}
	if c.source != nil {
		if err := p.receiveRecoveredRuntime(ctx); err != nil {
			return nil, err
		}
	}
	m := c.members[p.runtimeMember]
	if m == nil {
		return nil, errors.New("prior original Product member unavailable")
	}
	artifact := p.artifact
	artifact.Path = m.original.path
	return &WindowsCanonicalEntryPriorRuntimeV1{owner: p, artifact: artifact}, nil
}
func (p *WindowsCanonicalEntryPriorSetV2) validateOriginalRuntime(ctx context.Context, _ int, _ bool) error {
	return p.VerifyUnchanged(ctx)
}
func (p *WindowsCanonicalEntryPriorSetV2) verifyOriginalRuntimeProcess(_ int, _ bool, handle uintptr) error {
	c := &p.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.verify(context.Background()); err != nil {
		return err
	}
	m := c.members[p.runtimeMember]
	if m == nil {
		return errors.New("prior original Product unavailable")
	}
	if _, retained := c.recoveryRetained[p.runtimeMember]; retained {
		return verifyCanonicalEntryProductProcessImageInNamespace(m.original, handle, windows.FILE_SHARE_READ, nil, c.directories)
	}
	// The existing image verifier borrows the actual retained DELETE object;
	// this local view acquires, duplicates and closes no original resource.
	borrow := &WindowsCanonicalEntryPriorSetV1{root: c.root, ready: true, members: [3]*canonicalEntryNativeFile{m.original}}
	return verifyCanonicalEntryOriginalProductProcessHandle(m.original, handle, borrow)
}

type canonicalEntryRecoveryOriginalV2 struct {
	member    string
	prior     bool
	original  WindowsCanonicalEntryMemberEvidenceV2
	locations map[string]bool
	cleanup   *WindowsCanonicalEntryMemberCleanupRecordV2
}

type canonicalEntryRecoveryLineageV2 struct {
	root        WindowsCanonicalEntryDirectoryIdentityV1
	directories map[string]WindowsCanonicalEntryMemberDirectoryRecordV2
	originals   []*canonicalEntryRecoveryOriginalV2
	plans       map[string]WindowsCanonicalEntryMemberRecoveryPlanV2
	retained    map[string]RuntimeObjectV1
	purpose     WindowsCanonicalEntryRecoveryPurposeV2
}

type canonicalEntryRecoveryMatchV2 struct {
	original *canonicalEntryRecoveryOriginalV2
	current  *WindowsCanonicalEntryMemberEvidenceV2
}

func canonicalRecoveryPurposeV2(p WindowsCanonicalEntryRecoveryPurposeV2) bool {
	return p == WindowsCanonicalRecoveryRollbackV2 || p == WindowsCanonicalRecoveryCommittedForwardV2 || p == WindowsCanonicalRecoveryRollbackCleanupV2
}

func canonicalRecoveryCleanupPurposeV2(p WindowsCanonicalEntryRecoveryPurposeV2) bool {
	return p == WindowsCanonicalRecoveryCommittedForwardV2 || p == WindowsCanonicalRecoveryRollbackCleanupV2
}

func canonicalRecoveryIssuedLocationsV2(plan WindowsCanonicalEntryMemberRecoveryPlanV2, prior bool, original WindowsCanonicalEntryMemberEvidenceV2) []string {
	out := []string{original.File.Path, plan.Member}
	aside := plan.SuccessorAsideRelative
	if prior {
		aside = plan.PriorAsideRelative
	}
	if aside != nil {
		out = append(out, *aside)
	}
	return out
}

func (l *canonicalEntryRecoveryLineageV2) issuedImagePaths(targets WindowsCanonicalEntrySetTargetsV2) []string {
	root := filepath.Dir(targets.CanonicalEntryPath)
	out := []string{}
	add := func(relative string) {
		name := filepath.Join(root, filepath.FromSlash(relative))
		for _, prior := range out {
			if strings.EqualFold(prior, name) {
				return
			}
		}
		out = append(out, name)
	}
	for _, original := range l.originals {
		for _, relative := range canonicalRecoveryIssuedLocationsV2(l.plans[original.member], original.prior, original.original) {
			add(relative)
		}
	}
	for relative := range l.retained {
		if strings.EqualFold(filepath.Ext(relative), ".exe") {
			add(relative)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i]), strings.ToLower(out[j])
		if a != b {
			return a < b
		}
		return out[i] < out[j]
	})
	return out
}

func canonicalRecoveryEvidenceValidV2(member string, e *WindowsCanonicalEntryMemberEvidenceV2) bool {
	return e != nil && e.Member == member && publisherSourcePathValid(e.File.Path) && validLowerHex(e.File.SHA256, 64, 64) && e.Size >= 0 && e.Attributes & ^uint32(7) == 0
}

func canonicalRecoveryRetainedPlanV2(input WindowsCanonicalEntryMembersRecoveryInputV2) (map[string]RuntimeObjectV1, error) {
	if input.Retained == nil {
		return nil, nil
	}
	if input.BeforeKind != WindowsCanonicalEntryBeforeBundleV2 || !validLowerHex(input.Retained.MembersSHA256, 64, 64) || input.Retained.Singletons == nil {
		return nil, errors.New("native retained recovery witness is invalid")
	}
	payload, err := ParseRuntimePayload(input.Expected.RuntimePayloadBytes)
	if err != nil {
		return nil, err
	}
	retained := make(map[string]RuntimeObjectV1)
	for _, object := range canonicalRuntimeObjects(input.Expected, payload) {
		if object.Kind == "file" {
			object.Path = path.Join(input.Expected.BundleRelativePath, object.Path)
			retained[object.Path] = object
		}
	}
	_, plan, err := canonicalMemberPlanV2(input.Expected)
	if err != nil {
		return nil, err
	}
	last := ""
	for _, key := range input.Retained.Singletons {
		if key <= last || key != input.Expected.RuntimePayload.Path && key != input.Expected.IdentityRelativePath &&
			key != input.Expected.CanonicalEntryRelativePath && key != input.Expected.ProductRelativePath {
			return nil, errors.New("native retained singleton selection is not exact")
		}
		last = key
		object, ok := plan[key]
		if !ok || object.Kind != "file" {
			return nil, errors.New("native retained singleton is not a candidate file")
		}
		retained[key] = object
	}
	if len(retained) == 0 {
		return nil, errors.New("native retained recovery plan is empty")
	}
	return retained, nil
}

func canonicalRecoveryRetainedDirectoryV2(input WindowsCanonicalEntryMembersRecoveryInputV2, relative string) bool {
	if input.Retained == nil {
		return false
	}
	bundle := input.Expected.BundleRelativePath
	payload, err := ParseRuntimePayload(input.Expected.RuntimePayloadBytes)
	if err != nil {
		return true
	}
	relative = strings.ToLower(relative)
	for _, component := range payload.Components {
		root := strings.ToLower(path.Join(bundle, path.Dir(component.Root)))
		if relative == root || strings.HasPrefix(relative, root+"/") || strings.HasPrefix(root, relative+"/") {
			return true
		}
	}
	for _, key := range input.Retained.Singletons {
		if strings.HasPrefix(strings.ToLower(key), relative+"/") {
			return true
		}
	}
	return false
}

// This is lineage validation, not snapshot/build admission. The public receiver
// separately authenticates/crosschecks the complete current cohort before it
// calls this native-original classifier.
func buildCanonicalRecoveryLineageV2(input WindowsCanonicalEntryMembersRecoveryInputV2) (*canonicalEntryRecoveryLineageV2, error) {
	l := &canonicalEntryRecoveryLineageV2{directories: make(map[string]WindowsCanonicalEntryMemberDirectoryRecordV2), plans: make(map[string]WindowsCanonicalEntryMemberRecoveryPlanV2), purpose: input.Purpose}
	transaction, err := hex.DecodeString(input.TransactionID)
	if err != nil || len(transaction) != 16 || !canonicalRecoveryPurposeV2(input.Purpose) || len(input.Members) == 0 {
		return l, errors.New("current recovery transaction/purpose/original plan invalid")
	}
	l.retained, err = canonicalRecoveryRetainedPlanV2(input)
	if err != nil {
		return l, err
	}
	retainedNames := make(map[string]bool, len(l.retained))
	for key := range l.retained {
		retainedNames[strings.ToLower(key)] = true
	}
	rootSet := false
	setRoot := func(id WindowsCanonicalEntryDirectoryIdentityV1) error {
		if rootSet && l.root != id {
			return errors.New("original recovery root identities disagree")
		}
		l.root = id
		rootSet = true
		return nil
	}
	root := filepath.Dir(input.Targets.CanonicalEntryPath)
	seenIDs := map[WindowsCanonicalEntryFileIdentityV2]bool{}
	seenKeys := map[string]bool{}
	previous := ""
	for _, plan := range input.Members {
		if !publisherSourcePathValid(plan.Member) || plan.Member <= previous || seenKeys[strings.ToLower(plan.Member)] || plan.Prior == nil && plan.Staged == nil {
			return l, errors.New("current original member plan ordering/shape invalid")
		}
		if retainedNames[strings.ToLower(plan.Member)] {
			return l, errors.New("retained member entered effect lineage")
		}
		for _, original := range []*WindowsCanonicalEntryMemberEvidenceV2{plan.Prior, plan.Staged} {
			if original != nil && retainedNames[strings.ToLower(original.File.Path)] {
				return l, errors.New("effect original aliases a retained member")
			}
		}
		for _, aside := range []*string{plan.PriorAsideRelative, plan.SuccessorAsideRelative} {
			if aside != nil && retainedNames[strings.ToLower(*aside)] {
				return l, errors.New("effect aside aliases a retained member")
			}
		}
		previous = plan.Member
		seenKeys[strings.ToLower(plan.Member)] = true
		l.plans[plan.Member] = plan
		if plan.Prior == nil {
			if plan.PriorAbsence == nil || !publisherSourcePathValid(plan.PriorAbsence.MissingRelative) || plan.PriorAbsence.ParentRelative != "" && !publisherSourcePathValid(plan.PriorAbsence.ParentRelative) || path.Dir(plan.PriorAbsence.MissingRelative) != func() string {
				if plan.PriorAbsence.ParentRelative == "" {
					return "."
				}
				return plan.PriorAbsence.ParentRelative
			}() {
				return l, errors.New("original prior absence evidence invalid")
			}
			if plan.Member != plan.PriorAbsence.MissingRelative && !strings.HasPrefix(plan.Member, plan.PriorAbsence.MissingRelative+"/") {
				return l, errors.New("original prior absence is not on its selected member ancestry")
			}
		} else if plan.PriorAbsence != nil {
			return l, errors.New("present original prior has absence evidence")
		}
		for _, aside := range []*string{plan.PriorAsideRelative, plan.SuccessorAsideRelative} {
			if aside != nil && (!publisherSourcePathValid(*aside) || path.Dir(*aside) != "." || strings.EqualFold(*aside, plan.Member)) {
				return l, errors.New("original aside is not an issued distinct flat root location")
			}
		}
		for _, side := range []struct {
			prior bool
			e     *WindowsCanonicalEntryMemberEvidenceV2
			aside *string
		}{{true, plan.Prior, plan.PriorAsideRelative}, {false, plan.Staged, plan.SuccessorAsideRelative}} {
			if side.e == nil {
				continue
			}
			if !canonicalRecoveryEvidenceValidV2(plan.Member, side.e) || seenIDs[side.e.NativeIdentity] {
				return l, errors.New("original file evidence invalid or aliases another original")
			}
			seenIDs[side.e.NativeIdentity] = true
			if !side.prior && path.Dir(side.e.File.Path) != "." {
				return l, errors.New("original staged file is not flat under the retained root")
			}
			if path.Dir(side.e.File.Path) == "." {
				if err := setRoot(side.e.ParentIdentity); err != nil {
					return l, err
				}
			}
			o := &canonicalEntryRecoveryOriginalV2{member: plan.Member, prior: side.prior, original: *side.e, locations: map[string]bool{side.e.File.Path: true}}
			l.originals = append(l.originals, o)
		}
	}
	for _, record := range input.Directories {
		d := record.Original
		if d.Schema != WindowsCanonicalEntryMemberDirectorySchemaV2 || d.Root != root || !publisherSourcePathValid(d.RelativePath) {
			return l, errors.New("original recovery directory projection invalid")
		}
		for name := range l.directories {
			if strings.EqualFold(name, d.RelativePath) {
				return l, errors.New("duplicate/aliased original recovery directory path")
			}
		}
		if path.Dir(d.RelativePath) == "." {
			if err := setRoot(d.ParentIdentity); err != nil {
				return l, err
			}
		} else {
			parent, ok := l.directories[path.Dir(d.RelativePath)]
			if !ok || parent.Original.DirectoryIdentity != d.ParentIdentity {
				return l, errors.New("original recovery directory parent chain missing or changed")
			}
		}
		switch record.State {
		case "retained":
			if d.Created {
				return l, errors.New("created directory cannot be a retained preimage")
			}
		case "created_identity_received":
			if !d.Created {
				return l, errors.New("directory creation record has no original creation")
			}
		case "dispose_intent", "absence_received":
			if !d.Created || !canonicalRecoveryCleanupPurposeV2(input.Purpose) || canonicalRecoveryRetainedDirectoryV2(input, d.RelativePath) {
				return l, errors.New("directory disposal lacks creation/cleanup purpose")
			}
		default:
			return l, errors.New("unknown original directory state")
		}
		if record.Disposition != nil {
			r := record.Disposition
			if r.SourceRelative != d.RelativePath || r.Readback && !r.Applied || r.Applied && !r.Attempted {
				return l, errors.New("original directory native result is inconsistent")
			}
		}
		for _, prior := range l.directories {
			if prior.Original.DirectoryIdentity == d.DirectoryIdentity {
				return l, errors.New("original recovery directories alias one native object")
			}
		}
		l.directories[d.RelativePath] = record
	}
	if !rootSet {
		return l, errors.New("original recovery root evidence missing")
	}
	parentID := func(relative string) (WindowsCanonicalEntryDirectoryIdentityV1, error) {
		if path.Dir(relative) == "." {
			return l.root, nil
		}
		d, ok := l.directories[path.Dir(relative)]
		if !ok {
			return WindowsCanonicalEntryDirectoryIdentityV1{}, errors.New("original member parent was not recorded")
		}
		return d.Original.DirectoryIdentity, nil
	}
	for _, original := range l.originals {
		expectedParent, err := parentID(original.original.File.Path)
		if err != nil || expectedParent != original.original.ParentIdentity || original.original.NativeIdentity.VolumeSerialNumber != l.root.VolumeSerialNumber {
			return l, errors.Join(err, errors.New("original file parent/volume evidence disagrees with recorded chain"))
		}
	}
	for _, plan := range input.Members {
		if plan.PriorAbsence != nil {
			a := plan.PriorAbsence
			id := l.root
			if a.ParentRelative != "" {
				d, ok := l.directories[a.ParentRelative]
				if !ok {
					return l, errors.New("original absence parent was not recorded")
				}
				id = d.Original.DirectoryIdentity
			}
			if a.ParentIdentity != id {
				return l, errors.New("original absence parent identity changed")
			}
		}
	}
	find := func(member string, prior bool) *canonicalEntryRecoveryOriginalV2 {
		for _, o := range l.originals {
			if o.member == member && o.prior == prior {
				return o
			}
		}
		return nil
	}
	for _, move := range input.Moves {
		o := find(move.Member, move.Prior)
		if o == nil || !publisherSourcePathValid(move.DestinationRelative) || move.SourceRelative != "" && !publisherSourcePathValid(move.SourceRelative) || move.SourceRelative == move.DestinationRelative || move.Readback && !move.Applied || move.Applied && !move.Attempted {
			return l, errors.New("recorded original move shape invalid")
		}
		var plan WindowsCanonicalEntryMemberRecoveryPlanV2
		for _, p := range input.Members {
			if p.Member == move.Member {
				plan = p
				break
			}
		}
		issued := canonicalRecoveryIssuedLocationsV2(plan, o.prior, o.original)
		allowed := func(name string) bool {
			for _, legal := range issued {
				if name == legal {
					return true
				}
			}
			return false
		}
		if !allowed(move.DestinationRelative) || move.SourceRelative != "" && !allowed(move.SourceRelative) {
			return l, errors.New("move endpoint is outside its issued original lineage")
		}
		if move.SourceRelative != "" && !o.locations[move.SourceRelative] {
			return l, errors.New("move source contradicts ordered original lineage")
		}
		if move.Applied {
			o.locations = map[string]bool{move.DestinationRelative: true}
		} else {
			if move.SourceRelative != "" {
				o.locations = map[string]bool{move.SourceRelative: true}
			}
			if !move.NativeStatusKnown || move.NativeStatus == 0 {
				o.locations[move.DestinationRelative] = true
			}
		}
	}
	for _, row := range input.Cleanup {
		prior := row.Side == "prior"
		if !prior && row.Side != "unused-successor" {
			return l, errors.New("unknown original cleanup side")
		}
		o := find(row.Member, prior)
		if o == nil || row.TransactionID != input.TransactionID || !canonicalRecoveryCleanupPurposeV2(row.Purpose) || row.Purpose != input.Purpose || row.Purpose == WindowsCanonicalRecoveryRollbackCleanupV2 && prior || row.ExpectedSHA256 != o.original.File.SHA256 || !o.locations[row.SourceRelative] || row.SourceRelative == o.member || o.cleanup != nil {
			return l, errors.New("cleanup is outside the original side/current lineage/purpose")
		}
		if row.State != "retained" && row.State != "discard_intent" && row.State != "absence_received" {
			return l, errors.New("unknown original cleanup state")
		}
		copy := row
		o.cleanup = &copy
	}
	return l, nil
}

func (l *canonicalEntryRecoveryLineageV2) canonicalRequired(member string) bool {
	plan, ok := l.plans[member]
	if !ok {
		return false
	}
	switch l.purpose {
	case WindowsCanonicalRecoveryRollbackV2:
		return true
	case WindowsCanonicalRecoveryCommittedForwardV2:
		return plan.Staged != nil
	case WindowsCanonicalRecoveryRollbackCleanupV2:
		return plan.Prior != nil
	}
	return false
}

func canonicalRecoveryParentIdentityV2(l *canonicalEntryRecoveryLineageV2, relative string) (WindowsCanonicalEntryDirectoryIdentityV1, error) {
	if path.Dir(relative) == "." {
		return l.root, nil
	}
	record, ok := l.directories[path.Dir(relative)]
	if !ok {
		return WindowsCanonicalEntryDirectoryIdentityV1{}, errors.New("candidate parent is outside original directory lineage")
	}
	return record.Original.DirectoryIdentity, nil
}

func canonicalRecoveryDirectoryAbsentV2(record WindowsCanonicalEntryMemberDirectoryRecordV2) bool {
	return record.Original.Created && (record.State == "dispose_intent" || record.State == "absence_received")
}

// A missing descendant is observed only from the nearest still-original
// parent. Every removed ancestor must have its own authorized disposal row.
func (c *canonicalEntryMembersV2) recoveryParent(ctx context.Context, l *canonicalEntryRecoveryLineageV2, relative string) (*canonicalEntryNativeFile, string, bool, error) {
	return c.recoveryParentRead(ctx, l, relative, nil, false)
}

func (c *canonicalEntryMembersV2) recoveryParentRead(ctx context.Context, l *canonicalEntryRecoveryLineageV2, relative string, scope *canonicalEntryRecoveryReadScopeV2, requireHeld bool) (*canonicalEntryNativeFile, string, bool, error) {
	if scope != nil {
		if err := scope.validateBank(ctx, c); err != nil {
			return nil, "", false, err
		}
	}
	parts := strings.Split(relative, "/")
	current := c.parent
	for i, part := range parts[:len(parts)-1] {
		if err := ctx.Err(); err != nil {
			return nil, "", false, err
		}
		name := strings.Join(parts[:i+1], "/")
		record, ok := l.directories[name]
		if !ok {
			return nil, "", false, errors.New("candidate ancestor was not recorded")
		}
		parentID, err := canonicalPayloadDirectoryIdentity(current)
		if err != nil || parentID != record.Original.ParentIdentity {
			return nil, "", false, errors.Join(err, errors.New("candidate original parent identity changed"))
		}
		borrowed, matched, err := canonicalEntryBorrowMemberDirectoryReadV2(ctx, c.root, current, part, scope)
		if err != nil {
			return nil, "", false, err
		}
		if matched {
			current = borrowed
			continue
		}
		h, err := canonicalEntryOpenNative(windows.Handle(current.file.Fd()), part, true, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN)
		if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
			if !canonicalRecoveryDirectoryAbsentV2(record) {
				return nil, "", false, errors.New("required original directory is missing without disposal lineage")
			}
			for j := i + 1; j < len(parts)-1; j++ {
				child, ok := l.directories[strings.Join(parts[:j+1], "/")]
				if !ok || !canonicalRecoveryDirectoryAbsentV2(child) {
					return nil, "", false, errors.New("missing ancestor does not authorize another directory's absence")
				}
			}
			return nil, parts[len(parts)-1], true, nil
		}
		if err != nil {
			return nil, "", false, fmt.Errorf("open original recovery parent %q: %w", name, err)
		}
		if requireHeld {
			closeErr := windows.CloseHandle(h)
			c.probeCloseError = errors.Join(c.probeCloseError, closeErr)
			return nil, "", false, errors.Join(fmt.Errorf("cleanup slot ancestor %q is present without issued custody", name), closeErr)
		}
		next, err := c.root.retain(h, filepath.Join(current.path, part), true)
		if err != nil {
			return nil, "", false, err
		}
		id, err := canonicalPayloadDirectoryIdentity(next)
		if err != nil || id != record.Original.DirectoryIdentity {
			return nil, "", false, errors.Join(err, errors.New("candidate directory identity/creation changed"))
		}
		current = next
	}
	return current, parts[len(parts)-1], false, nil
}

func (c *canonicalEntryMembersV2) probeRecoveryLocation(ctx context.Context, l *canonicalEntryRecoveryLineageV2, relative string) (evidence *WindowsCanonicalEntryMemberEvidenceV2, retErr error) {
	return c.probeRecoveryLocationRead(ctx, l, relative, nil)
}

func (c *canonicalEntryMembersV2) probeRecoveryLocationRead(ctx context.Context, l *canonicalEntryRecoveryLineageV2, relative string, scope *canonicalEntryRecoveryReadScopeV2) (evidence *WindowsCanonicalEntryMemberEvidenceV2, retErr error) {
	parent, leaf, parentAbsent, err := c.recoveryParentRead(ctx, l, relative, scope, false)
	if err != nil {
		return nil, err
	}
	if parentAbsent {
		return nil, nil
	}
	expectedParent, err := canonicalRecoveryParentIdentityV2(l, relative)
	if err != nil {
		return nil, err
	}
	actualParent, err := canonicalPayloadDirectoryIdentity(parent)
	if err != nil || actualParent != expectedParent {
		return nil, errors.Join(err, errors.New("candidate parent differs from original recorded identity"))
	}
	_, canonical := l.plans[relative]
	optionalFinal := canonical && canonicalRecoveryCleanupPurposeV2(l.purpose) && !l.canonicalRequired(relative)
	// Identify a candidate before reading its contents. An irrelevant foreign
	// final neighbor is never adopted or read as the selected original.
	h, err := canonicalEntryOpenNative(windows.Handle(parent.file.Fd()), leaf, false, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
		return nil, nil
	}
	if optionalFinal && errors.Is(err, windows.STATUS_FILE_IS_A_DIRECTORY) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open recovery candidate %q: %w", relative, err)
	}
	file := os.NewFile(uintptr(h), filepath.Join(parent.path, leaf))
	if file == nil {
		return nil, errors.Join(errors.New("recovery metadata probe wrapper unavailable"), windows.CloseHandle(h))
	}
	c.root.files = append(c.root.files, file)
	probe := &canonicalEntryNativeFile{file: file, path: filepath.Join(parent.path, leaf)}
	defer func() {
		closeErr := canonicalEntryCloseOriginalMember(c.root, probe)
		c.probeCloseError = errors.Join(c.probeCloseError, closeErr)
		retErr = errors.Join(retErr, closeErr)
	}()
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &info); err != nil {
		return nil, err
	}
	id := WindowsCanonicalEntryFileIdentityV2{info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow, uint64(info.CreationTime.HighDateTime)<<32 | uint64(info.CreationTime.LowDateTime)}
	var original *canonicalEntryRecoveryOriginalV2
	for _, candidate := range l.originals {
		if candidate.original.NativeIdentity == id {
			original = candidate
			break
		}
	}
	if original == nil {
		if optionalFinal {
			return nil, nil
		}
		return nil, fmt.Errorf("recovery location %q is occupied by a different original file identity %+v", relative, id)
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		return nil, fmt.Errorf("located original %q is linked, reparse or wrong kind", original.member)
	}
	readable, err := canonicalEntryOpenNative(windows.Handle(parent.file.Fd()), leaf, false, windows.FILE_READ_DATA, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
	if err != nil {
		return nil, fmt.Errorf("read located recovery original %q: %w", relative, err)
	}
	held, err := c.root.retain(readable, filepath.Join(parent.path, leaf), false)
	if err != nil {
		return nil, err
	}
	defer func() {
		closeErr := canonicalEntryCloseOriginalMember(c.root, held)
		c.probeCloseError = errors.Join(c.probeCloseError, closeErr)
		retErr = errors.Join(retErr, closeErr)
	}()
	actualID, size, attrs, err := canonicalMemberFileIdentityV2(held)
	if err != nil {
		return nil, err
	}
	if actualID != id {
		return nil, errors.New("recovery candidate identity/creation changed between metadata and content receiving")
	}
	hash, err := canonicalEntryHashFile(ctx, held.file)
	if err != nil {
		return nil, err
	}
	return &WindowsCanonicalEntryMemberEvidenceV2{File: WindowsCanonicalEntryFileV1{Path: relative, SHA256: hash}, NativeIdentity: actualID, ParentIdentity: actualParent, Size: size, Attributes: attrs}, nil
}

func (c *canonicalEntryMembersV2) discoverRecoveryMembers(ctx context.Context, l *canonicalEntryRecoveryLineageV2) ([]canonicalEntryRecoveryMatchV2, error) {
	return c.discoverRecoveryMembersRead(ctx, l, nil)
}

func (c *canonicalEntryMembersV2) discoverRecoveryMembersRead(ctx context.Context, l *canonicalEntryRecoveryLineageV2, scope *canonicalEntryRecoveryReadScopeV2) ([]canonicalEntryRecoveryMatchV2, error) {
	if scope != nil {
		if err := scope.validateBank(ctx, c); err != nil {
			return nil, err
		}
	}
	paths := map[string]bool{}
	for _, original := range l.originals {
		for location := range original.locations {
			paths[location] = true
		}
	}
	for member := range l.plans {
		paths[member] = true
	}
	ordered := make([]string, 0, len(paths))
	for location := range paths {
		ordered = append(ordered, location)
	}
	sort.Strings(ordered)
	matched := make(map[*canonicalEntryRecoveryOriginalV2]*WindowsCanonicalEntryMemberEvidenceV2)
	for _, location := range ordered {
		actual, err := c.probeRecoveryLocationRead(ctx, l, location, scope)
		if err != nil {
			return nil, err
		}
		if plan, canonical := l.plans[location]; canonical && l.canonicalRequired(location) && canonicalRecoveryCleanupPurposeV2(l.purpose) {
			if actual == nil {
				return nil, fmt.Errorf("selected current canonical slot %q is absent during cleanup", location)
			}
			if l.purpose == WindowsCanonicalRecoveryCommittedForwardV2 {
				if plan.Staged == nil || actual.File.SHA256 != plan.Staged.File.SHA256 || actual.Size != plan.Staged.Size || location != WindowsCanonicalEntryRelativePathV1 && actual.Attributes != plan.Staged.Attributes {
					return nil, fmt.Errorf("canonical slot %q does not contain the selected committed cohort", location)
				}
			} else if plan.Prior == nil || actual.NativeIdentity != plan.Prior.NativeIdentity {
				return nil, fmt.Errorf("canonical slot %q does not contain its restored original prior", location)
			}
		}
		if actual == nil {
			continue
		}
		var original *canonicalEntryRecoveryOriginalV2
		for _, candidate := range l.originals {
			if candidate.original.NativeIdentity == actual.NativeIdentity {
				original = candidate
				break
			}
		}
		if original == nil {
			return nil, fmt.Errorf("recovery location %q is occupied by a different original file identity %+v", location, actual.NativeIdentity)
		}
		actual.Member = original.member
		if original.prior == (c.recoverySide == WindowsCanonicalRecoveryPriorV1) {
			c.captures = append(c.captures, *actual)
		}
		if !original.locations[location] {
			return nil, fmt.Errorf("located original %q contradicts ordered lineage at %q", original.member, location)
		}
		if actual.File.SHA256 != original.original.File.SHA256 || actual.Size != original.original.Size || actual.Attributes != original.original.Attributes {
			return nil, fmt.Errorf("located original %q at %q has changed bytes/extent/attributes", original.member, location)
		}
		if matched[original] != nil {
			return nil, fmt.Errorf("original %q has more than one admissible current location", original.member)
		}
		copy := *actual
		matched[original] = &copy
	}
	results := make([]canonicalEntryRecoveryMatchV2, 0, len(l.originals))
	for _, original := range l.originals {
		actual := matched[original]
		if actual == nil && (original.cleanup == nil || original.cleanup.State != "discard_intent" && original.cleanup.State != "absence_received") {
			return nil, fmt.Errorf("required original %q (%t prior) is absent without its exact cleanup intent", original.member, original.prior)
		}
		if actual != nil && original.cleanup != nil && original.cleanup.State == "absence_received" {
			return nil, fmt.Errorf("original %q is present despite its received cleanup absence", original.member)
		}
		if actual != nil && original.cleanup != nil && original.cleanup.State == "discard_intent" && actual.File.Path != original.cleanup.SourceRelative {
			return nil, fmt.Errorf("original %q remains at %q outside its exact discard intent", original.member, actual.File.Path)
		}
		results = append(results, canonicalEntryRecoveryMatchV2{original, actual})
	}
	return results, nil
}

func (c *canonicalEntryMembersV2) acquireRecoveryMatches(ctx context.Context, matches []canonicalEntryRecoveryMatchV2) error {
	return c.acquireRecoveryMatchesRead(ctx, matches, nil)
}

func (c *canonicalEntryMembersV2) acquireRecoveryMatchesRead(ctx context.Context, matches []canonicalEntryRecoveryMatchV2, scope *canonicalEntryRecoveryReadScopeV2) error {
	if scope != nil {
		if err := scope.validateBank(ctx, c); err != nil {
			return err
		}
	}
	for _, match := range matches {
		if match.original.prior != (c.recoverySide == WindowsCanonicalRecoveryPriorV1) || match.current == nil {
			continue
		}
		m, missing, err := c.openMemberRead(ctx, match.original.member, match.current.File.Path, scope)
		if err != nil {
			return fmt.Errorf("retain classified original %q: %w", match.original.member, err)
		}
		if missing != nil {
			return fmt.Errorf("classified original %q disappeared before retained acquisition", match.original.member)
		}
		c.keys = append(c.keys, match.original.member)
		if m.evidence != *match.current {
			return fmt.Errorf("classified original %q changed identity/creation/parent/bytes before retained acquisition", match.original.member)
		}
	}
	sort.Strings(c.keys)
	return nil
}

func validateCanonicalRecoveryCurrentCohortV2(input WindowsCanonicalEntryMembersRecoveryInputV2) error {
	want, err := CanonicalEntryTargetsV2(input.Targets.CanonicalEntryPath)
	if err != nil || want != input.Targets {
		return errors.Join(err, errors.New("current recovery target projection invalid"))
	}
	keys, plan, err := canonicalMemberPlanV2(input.Expected)
	if err != nil {
		return err
	}
	staged := map[string]bool{}
	prior := map[string]bool{}
	retained, err := canonicalRecoveryRetainedPlanV2(input)
	if err != nil {
		return err
	}
	for key := range retained {
		prior[key], staged[key] = true, true
	}
	for _, row := range input.Members {
		if row.Staged != nil {
			object, ok := plan[row.Member]
			if !ok || row.Staged.File.SHA256 != object.SHA256 || object.Size != nil && row.Staged.Size != *object.Size || row.Member != input.Expected.CanonicalEntryRelativePath && row.Staged.Attributes != object.Attributes {
				return errors.New("staged original does not match authenticated current cohort")
			}
			staged[row.Member] = true
		}
		if row.Prior != nil {
			prior[row.Member] = true
		}
	}
	if len(staged) != len(keys) {
		return errors.New("original staged plan is not the complete authenticated cohort")
	}
	switch input.BeforeKind {
	case WindowsCanonicalEntryBeforeAbsentV2:
		if len(prior) != 0 {
			return errors.New("absent prior kind has original prior files")
		}
	case WindowsCanonicalEntryBeforeRootProductV2:
		if len(prior) != 1 || !prior[WindowsCanonicalEntryRelativePathV1] {
			return errors.New("root-only prior is not the exact original root Product")
		}
	case WindowsCanonicalEntryBeforeLegacyV2:
		if len(prior) != 3 || !prior[WindowsCanonicalEntryRelativePathV1] || !prior[WindowsCanonicalProductRelativePathV1] || !prior[WindowsCanonicalIdentityRelativePathV1] {
			return errors.New("legacy prior is not the exact three-file cohort")
		}
	case WindowsCanonicalEntryBeforeBundleV2:
		for _, key := range []string{WindowsCanonicalEntryRelativePathV1, WindowsCanonicalProductRelativePathV1, input.Expected.IdentityRelativePath, input.Expected.RuntimePayload.Path} {
			if !prior[key] {
				return errors.New("current prior lacks its authenticated control members")
			}
		}
	default:
		return errors.New("current recovery prior kind invalid")
	}
	return nil
}

func canonicalRecoveryDirectoryRequiredV2(input WindowsCanonicalEntryMembersRecoveryInputV2, relative string) bool {
	if canonicalRecoveryRetainedDirectoryV2(input, relative) {
		return true
	}
	if input.Purpose == WindowsCanonicalRecoveryCommittedForwardV2 {
		for _, object := range input.Expected.BundleInventory {
			if object.Kind == "directory" && path.Join(input.Expected.BundleRelativePath, object.Path) == relative {
				return true
			}
		}
		return relative == input.Expected.BundleRelativePath
	}
	if input.Purpose == WindowsCanonicalRecoveryRollbackCleanupV2 {
		for _, plan := range input.Members {
			if plan.Prior == nil {
				continue
			}
			for parent := path.Dir(plan.Member); parent != "."; parent = path.Dir(parent) {
				if parent == relative {
					return true
				}
			}
		}
	}
	return false
}

func (c *canonicalEntryMembersV2) probeRecoveryDirectory(ctx context.Context, l *canonicalEntryRecoveryLineageV2, record WindowsCanonicalEntryMemberDirectoryRecordV2) (present bool, retErr error) {
	return c.probeRecoveryDirectoryRead(ctx, l, record, nil)
}

func (c *canonicalEntryMembersV2) probeRecoveryDirectoryRead(ctx context.Context, l *canonicalEntryRecoveryLineageV2, record WindowsCanonicalEntryMemberDirectoryRecordV2, scope *canonicalEntryRecoveryReadScopeV2) (present bool, retErr error) {
	parent, leaf, parentAbsent, err := c.recoveryParentRead(ctx, l, record.Original.RelativePath, scope, false)
	if err != nil {
		return false, err
	}
	if parentAbsent {
		return false, nil
	}
	actualParent, err := canonicalPayloadDirectoryIdentity(parent)
	if err != nil || actualParent != record.Original.ParentIdentity {
		return false, errors.Join(err, errors.New("original recovery directory parent changed"))
	}
	h, err := canonicalEntryOpenNative(windows.Handle(parent.file.Fd()), leaf, true, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe original recovery directory %q: %w", record.Original.RelativePath, err)
	}
	file, err := c.root.retain(h, filepath.Join(parent.path, leaf), true)
	if err != nil {
		return false, err
	}
	defer func() {
		closeErr := canonicalEntryCloseOriginalMember(c.root, file)
		c.probeCloseError = errors.Join(c.probeCloseError, closeErr)
		retErr = errors.Join(retErr, closeErr)
	}()
	id, err := canonicalPayloadDirectoryIdentity(file)
	if err != nil {
		return true, err
	}
	if id != record.Original.DirectoryIdentity {
		return true, errors.New("located recovery directory has different identity/creation")
	}
	return true, nil
}

func (r *WindowsCanonicalEntryRecoverySetV2) receiveRecoveryDirectories(ctx context.Context, input WindowsCanonicalEntryMembersRecoveryInputV2, l *canonicalEntryRecoveryLineageV2) error {
	return r.receiveRecoveryDirectoriesRead(ctx, input, l, nil)
}

func (r *WindowsCanonicalEntryRecoverySetV2) receiveRecoveryDirectoriesRead(ctx context.Context, input WindowsCanonicalEntryMembersRecoveryInputV2, l *canonicalEntryRecoveryLineageV2, scope *canonicalEntryRecoveryReadScopeV2) error {
	c := &r.original.original
	if scope != nil {
		if err := scope.validateBank(ctx, c); err != nil {
			return err
		}
	}
	for _, d := range r.lenders {
		if d == nil {
			return errors.New("nil original directory lender")
		}
		d.mu.Lock()
		if d.memberSnapshot == nil {
			d.mu.Unlock()
			return errors.New("directory lender is not a current native-issued member owner")
		}
		snapshot := *d.memberSnapshot
		record, ok := l.directories[snapshot.RelativePath]
		if !ok || record.Original != snapshot {
			d.mu.Unlock()
			return errors.New("directory lender disagrees with original current journal record")
		}
		if record.State == "absence_received" && d.closed && d.disposition != nil && d.disposition.Applied && d.disposition.Readback {
			d.mu.Unlock()
			continue
		}
		if !d.ready || d.closed || d.acquisitionError != nil {
			d.mu.Unlock()
			return errors.New("original directory lender is partial, closed or pending")
		}
		if d.memberTransactionID != "" && d.memberTransactionID != input.TransactionID {
			d.mu.Unlock()
			return errors.New("original directory lender belongs to a different transaction")
		}
		if err := d.validateReadLocked(ctx, scope); err != nil {
			d.mu.Unlock()
			return err
		}
		d.memberSource = c.source
		d.memberTransactionID = input.TransactionID
		d.memberRecoverySide = r.side
		d.mu.Unlock()
		c.directories.mu.Lock()
		if existing := c.directories.owners[snapshot.RelativePath]; existing != nil && existing != d {
			c.directories.mu.Unlock()
			return errors.New("more than one native lender for the same original directory")
		}
		c.directories.owners[snapshot.RelativePath] = d
		c.directories.mu.Unlock()
	}
	for _, record := range input.Directories {
		present, err := c.probeRecoveryDirectoryRead(ctx, l, record, scope)
		if err != nil {
			return err
		}
		if !present {
			if !canonicalRecoveryDirectoryAbsentV2(record) || canonicalRecoveryDirectoryRequiredV2(input, record.Original.RelativePath) {
				return errors.New("required recovery directory is missing without qualified original disposal lineage")
			}
			continue
		}
		if record.State == "absence_received" {
			return errors.New("original directory is present despite received absence")
		}
		d, err := c.directoryRead(ctx, record.Original.RelativePath, false, &record.Original, scope)
		if d != nil {
			d.mu.Lock()
			d.memberTransactionID = input.TransactionID
			d.memberRecoverySide = r.side
			d.mu.Unlock()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *WindowsCanonicalEntryRecoverySetV2) Directories() []*WindowsCanonicalEntryPayloadDirectoryV1 {
	if r == nil {
		return nil
	}
	out := make([]*WindowsCanonicalEntryPayloadDirectoryV1, 0, len(r.lenders))
	seen := map[*WindowsCanonicalEntryPayloadDirectoryV1]bool{}
	for _, d := range r.lenders {
		if d != nil && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if r.original != nil {
		c := &r.original.original
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, d := range c.directoryOwners() {
			if d != nil && !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}

// IssuedImagePaths is observation data from the original validated plans on
// both sides. It confers no file/process authority and tracks no move progress.
func (r *WindowsCanonicalEntryRecoverySetV2) IssuedImagePaths(ctx context.Context) ([]string, error) {
	if r == nil || r.original == nil || ctx == nil {
		return nil, errors.New("original current recovery owner/context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c := &r.original.original
	c.mu.Lock()
	if !c.ready || c.closed || c.root == nil || c.root.closed || c.source == nil || c.recoveryInput == nil {
		err := errors.Join(c.acquisitionError, errors.New("current recovery view is not ready/open"))
		c.mu.Unlock()
		return nil, err
	}
	original := cloneCanonicalEntryMembersRecoveryInputV2(*c.recoveryInput)
	source, side := c.source, c.recoverySide
	c.mu.Unlock()
	// Receive callbacks outside native metadata synchronization, then recheck
	// this same original lifetime. Source owns its existing before/after checks.
	fresh, err := receiveCanonicalEntryMembersRecoveryInputV2(ctx, source, side)
	if err != nil {
		return nil, err
	}
	if !canonicalRecoveryOriginalBindingStableV2(original, fresh) {
		return nil, errors.New("original recovery evidence/history changed before issued-image projection")
	}
	lineage, err := buildCanonicalRecoveryLineageV2(fresh)
	if err != nil {
		return nil, err
	}
	paths := lineage.issuedImagePaths(fresh.Targets)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready || c.closed || c.root == nil || c.root.closed || c.recoveryInput == nil {
		return nil, errors.New("current recovery lifetime closed during issued-image projection")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return paths, nil
}

func (r *WindowsCanonicalEntryRecoverySetV2) Members(ctx context.Context) ([]WindowsCanonicalEntryMemberEvidenceV2, error) {
	if r == nil || r.original == nil {
		return nil, errors.New("current recovery owner required")
	}
	c := &r.original.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready || c.closed {
		return c.partialRecoveryCaptures(), errors.Join(c.acquisitionError, errors.New("current recovery view is not ready/open"))
	}
	if err := c.verify(ctx); err != nil {
		return c.partialRecoveryCaptures(), err
	}
	out := make([]WindowsCanonicalEntryMemberEvidenceV2, 0, len(c.keys))
	for _, key := range c.keys {
		m := c.members[key]
		if m.cleanup != nil && m.cleanup.Applied && m.cleanup.Readback {
			continue
		}
		out = append(out, m.evidence)
	}
	return out, nil
}

// withCleanupRead binds observation to the same open bank and the current
// cleanup phase. A completed inverse may advance its original bank to cleanup.
func (r *WindowsCanonicalEntryRecoverySetV2) withCleanupRead(ctx context.Context, side WindowsCanonicalRecoverySideV1, purpose WindowsCanonicalEntryRecoveryPurposeV2, read func(*canonicalEntryMembersV2, WindowsCanonicalEntryMembersRecoveryInputV2, *canonicalEntryRecoveryReadScopeV2) error) error {
	if r == nil || r.original == nil || ctx == nil {
		return errors.New("current recovery owner/context required")
	}
	c := &r.original.original
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready || c.closed || c.root == nil || c.root.closed || c.parent == nil || c.parent.file == nil || c.source == nil || c.recoveryInput == nil {
		return errors.Join(c.acquisitionError, errors.New("current recovery view is not ready/open"))
	}
	if side != c.recoverySide || !canonicalRecoveryCleanupPurposeV2(purpose) || side == WindowsCanonicalRecoveryPriorV1 && purpose != WindowsCanonicalRecoveryCommittedForwardV2 {
		return errors.New("current cleanup side/purpose does not select this recovery owner")
	}
	acquired := c.recoveryInput
	if acquired.Purpose != purpose && !(acquired.Purpose == WindowsCanonicalRecoveryRollbackV2 && purpose == WindowsCanonicalRecoveryRollbackCleanupV2) {
		return errors.New("current cleanup purpose does not follow the acquired recovery purpose")
	}
	receive := func(fresh WindowsCanonicalEntryMembersRecoveryInputV2, scope *canonicalEntryRecoveryReadScopeV2) error {
		if fresh.Purpose != purpose || !canonicalRecoveryOriginalBindingStableV2(*acquired, fresh) {
			return errors.New("current cleanup journal no longer binds the acquired recovery owner")
		}
		err := read(c, fresh, scope)
		if scope == nil {
			err = errors.Join(err, c.source.ValidateOriginalRecovery(ctx))
		}
		return errors.Join(err, ctx.Err())
	}
	if provider, ok := c.source.(canonicalEntryRecoveryCleanupReadSourceV2); ok {
		return c.receiveRecoveryReadScope(ctx, func(accept canonicalEntryRecoveryReadAcceptanceV2) error {
			return provider.WithCanonicalRecoveryCleanupReadScope(ctx, side, purpose, func(check func(context.Context) error, input WindowsCanonicalEntryMembersRecoveryInputV2) error {
				return accept(check, &input)
			})
		}, func(scope *canonicalEntryRecoveryReadScopeV2, input *WindowsCanonicalEntryMembersRecoveryInputV2) error {
			if err := scope.validateBank(ctx, c); err != nil {
				return err
			}
			return receive(cloneCanonicalEntryMembersRecoveryInputV2(*input), scope)
		})
	}
	return c.withRecoveryReadScope(ctx, func(scope *canonicalEntryRecoveryReadScopeV2) error {
		var check func(context.Context) error
		if scope != nil {
			check = func(ctx context.Context) error { return scope.validateBank(ctx, c) }
		}
		fresh, err := receiveCanonicalEntryMembersRecoveryInputReadV2(ctx, c.source, side, check)
		if err != nil {
			return err
		}
		return receive(fresh, scope)
	})
}

func (c *canonicalEntryMembersV2) verifyCleanupAbsence(input WindowsCanonicalEntryMembersRecoveryInputV2, expected WindowsCanonicalEntryMemberEvidenceV2, row WindowsCanonicalEntryMemberCleanupRecordV2) error {
	side := "unused-successor"
	if c.recoverySide == WindowsCanonicalRecoveryPriorV1 {
		side = "prior"
	}
	if row.TransactionID != input.TransactionID || row.Purpose != input.Purpose || row.Side != side ||
		row.Member != expected.Member || row.ExpectedSHA256 != expected.File.SHA256 ||
		(row.State != "discard_intent" && row.State != "absence_received") ||
		!publisherSourcePathValid(row.SourceRelative) || canonicalEntryIsFinalRelative(row.SourceRelative) {
		return errors.New("exact current cleanup absence record not received")
	}
	for _, final := range c.root.memberFinalPaths {
		if strings.EqualFold(final, row.SourceRelative) {
			return errors.New("cleanup absence record selects a protected final")
		}
	}
	if m := c.members[row.Member]; m != nil {
		r := m.cleanup
		if r != nil && r.Applied && r.Readback && r.Member == row.Member && r.SourceRelative == row.SourceRelative &&
			m.evidence.File.Path == row.SourceRelative && m.evidence.NativeIdentity == expected.NativeIdentity &&
			m.evidence.File.SHA256 == expected.File.SHA256 && m.evidence.Size == expected.Size && m.evidence.Attributes == expected.Attributes {
			return nil
		}
		return errors.New("original member has no matching applied cleanup readback")
	}
	if acquired, ok := c.recoveryAbsences[row.Member]; ok && (acquired.State == "discard_intent" || row.State == "absence_received") {
		acquired.State = row.State
		if acquired == row {
			return nil
		}
	}
	return errors.New("original member has no matching acquired cleanup absence")
}

// ObserveCleanupMember re-receives only the exact issued cleanup original.
// A successful absence requires that original's acquired witness or applied
// native readback and its current durable cleanup row.
func (r *WindowsCanonicalEntryRecoverySetV2) ObserveCleanupMember(ctx context.Context, side WindowsCanonicalRecoverySideV1, purpose WindowsCanonicalEntryRecoveryPurposeV2, expected WindowsCanonicalEntryMemberEvidenceV2) (WindowsCanonicalEntryMemberEvidenceV2, bool, error) {
	return r.receiveCleanupMember(ctx, side, purpose, expected, nil)
}

// CopyRollbackCleanupOriginal streams one issued, non-final successor original
// from the held bank. Proven absences do not write. The destination may contain
// partial bytes on error and may be accepted only after successful readback.
func (r *WindowsCanonicalEntryRecoverySetV2) CopyRollbackCleanupOriginal(ctx context.Context, expected WindowsCanonicalEntryMemberEvidenceV2, destination io.Writer) (WindowsCanonicalEntryMemberEvidenceV2, bool, error) {
	if destination == nil {
		return WindowsCanonicalEntryMemberEvidenceV2{}, false, errors.New("rollback cleanup archive destination required")
	}
	return r.receiveCleanupMember(ctx, WindowsCanonicalRecoverySuccessorV1, WindowsCanonicalRecoveryRollbackCleanupV2, expected, destination)
}

func (r *WindowsCanonicalEntryRecoverySetV2) receiveCleanupMember(ctx context.Context, side WindowsCanonicalRecoverySideV1, purpose WindowsCanonicalEntryRecoveryPurposeV2, expected WindowsCanonicalEntryMemberEvidenceV2, destination io.Writer) (WindowsCanonicalEntryMemberEvidenceV2, bool, error) {
	var observation WindowsCanonicalEntryMemberEvidenceV2
	err := r.withCleanupRead(ctx, side, purpose, func(c *canonicalEntryMembersV2, input WindowsCanonicalEntryMembersRecoveryInputV2, scope *canonicalEntryRecoveryReadScopeV2) error {
		if destination != nil && c.recoveryInput.Purpose != WindowsCanonicalRecoveryRollbackCleanupV2 {
			return errors.New("rollback cleanup archive requires a bank acquired for cleanup")
		}
		i := sort.Search(len(input.Members), func(i int) bool { return input.Members[i].Member >= expected.Member })
		if i == len(input.Members) || input.Members[i].Member != expected.Member {
			return errors.New("unissued cleanup member key")
		}
		issued := input.Members[i].Staged
		cleanupSide := "unused-successor"
		if side == WindowsCanonicalRecoveryPriorV1 {
			issued = input.Members[i].Prior
			cleanupSide = "prior"
		}
		if issued == nil || *issued != expected {
			return errors.New("cleanup member does not match its issued original")
		}
		m := c.members[expected.Member]
		if m == nil || m.cleanup != nil {
			var selected *WindowsCanonicalEntryMemberCleanupRecordV2
			for i := range input.Cleanup {
				row := &input.Cleanup[i]
				if row.Member == expected.Member && row.Side == cleanupSide {
					if selected != nil {
						return errors.New("cleanup member has more than one current absence record")
					}
					selected = row
				}
			}
			if selected == nil {
				return errors.New("missing current original has no durable cleanup intent")
			}
			return c.verifyCleanupAbsence(input, expected, *selected)
		}
		actual, err := c.receiveMemberRead(ctx, expected.Member, m.original, expected.File.SHA256, scope)
		if err != nil {
			return err
		}
		if actual != m.evidence || actual.NativeIdentity != expected.NativeIdentity || actual.Size != expected.Size || actual.Attributes != expected.Attributes {
			return errors.New("original cleanup member identity, parent, extent, attributes or bytes changed")
		}
		if destination != nil {
			if canonicalEntryIsFinalRelative(actual.File.Path) || slices.ContainsFunc(c.root.memberFinalPaths, func(final string) bool {
				return strings.EqualFold(final, actual.File.Path)
			}) {
				return errors.New("rollback cleanup archive cannot read a protected final")
			}
			hash := sha256.New()
			reader := &canonicalEntryContextReader{ctx: ctx, reader: io.NewSectionReader(m.original.file, 0, actual.Size)}
			n, err := io.Copy(io.MultiWriter(destination, hash), reader)
			if err != nil {
				return err
			}
			if n != actual.Size || hex.EncodeToString(hash.Sum(nil)) != expected.File.SHA256 {
				return errors.New("rollback cleanup original bytes changed during archive copy")
			}
			after, err := c.receiveMemberRead(ctx, expected.Member, m.original, expected.File.SHA256, scope)
			if err != nil {
				return err
			}
			if after != actual {
				return errors.New("rollback cleanup original changed after archive copy")
			}
		}
		observation = actual
		return nil
	})
	if err != nil {
		return WindowsCanonicalEntryMemberEvidenceV2{}, false, err
	}
	return observation, observation.Member != "", nil
}

// VerifyCleanupAbsences checks the exact disposed slots through the held
// namespace before the caller closes this bank and settles the journal.
func (r *WindowsCanonicalEntryRecoverySetV2) VerifyCleanupAbsences(ctx context.Context, side WindowsCanonicalRecoverySideV1, purpose WindowsCanonicalEntryRecoveryPurposeV2) error {
	return r.withCleanupRead(ctx, side, purpose, func(c *canonicalEntryMembersV2, input WindowsCanonicalEntryMembersRecoveryInputV2, scope *canonicalEntryRecoveryReadScopeV2) error {
		if !reflect.DeepEqual(c.recoveryInput.Directories, input.Directories) {
			return errors.New("cleanup slot directory lineage changed after acquisition")
		}
		lineage, err := buildCanonicalRecoveryLineageV2(input)
		if err != nil {
			return err
		}
		for _, original := range lineage.originals {
			if original.prior != (side == WindowsCanonicalRecoveryPriorV1) {
				continue
			}
			row := original.cleanup
			if row == nil {
				m := c.members[original.member]
				if !original.prior && purpose == WindowsCanonicalRecoveryCommittedForwardV2 &&
					m != nil && m.cleanup == nil && m.evidence.File.Path == original.member && m.evidence.NativeIdentity == original.original.NativeIdentity {
					continue
				}
				return fmt.Errorf("cleanup member %q lacks a durable absence", original.member)
			}
			if row.State != "absence_received" {
				return fmt.Errorf("cleanup slot %q lacks durable absence readback", row.SourceRelative)
			}
			if err := c.verifyCleanupAbsence(input, original.original, *row); err != nil {
				return fmt.Errorf("cleanup slot %q: %w", row.SourceRelative, err)
			}
			if err := c.verifyCleanupSlotAbsent(ctx, lineage, row.SourceRelative, scope); err != nil {
				return fmt.Errorf("cleanup slot %q: %w", row.SourceRelative, err)
			}
		}
		return nil
	})
}

func (c *canonicalEntryMembersV2) verifyCleanupSlotAbsent(ctx context.Context, lineage *canonicalEntryRecoveryLineageV2, relative string, scope *canonicalEntryRecoveryReadScopeV2) error {
	parent, leaf, parentAbsent, err := c.recoveryParentRead(ctx, lineage, relative, scope, true)
	if err != nil || parentAbsent {
		return err
	}
	expected, err := canonicalRecoveryParentIdentityV2(lineage, relative)
	if err != nil {
		return err
	}
	actual, err := canonicalPayloadDirectoryIdentity(parent)
	if err != nil || actual != expected {
		return errors.Join(err, errors.New("cleanup slot parent differs from original recorded identity"))
	}
	h, err := canonicalEntryOpenNative(windows.Handle(parent.file.Fd()), leaf, false, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return err
	}
	closeErr := windows.CloseHandle(h)
	c.probeCloseError = errors.Join(c.probeCloseError, closeErr)
	return errors.Join(errors.New("cleanup slot is occupied after absence readback"), closeErr)
}

func (c *canonicalEntryMembersV2) partialRecoveryCaptures() []WindowsCanonicalEntryMemberEvidenceV2 {
	byMember := map[string]WindowsCanonicalEntryMemberEvidenceV2{}
	for _, capture := range c.captures {
		byMember[capture.Member] = capture
	}
	if c.recoveryInput != nil {
		for _, plan := range c.recoveryInput.Members {
			original := plan.Staged
			if c.recoverySide == WindowsCanonicalRecoveryPriorV1 {
				original = plan.Prior
			}
			if original == nil {
				continue
			}
			if member := c.members[plan.Member]; member != nil && member.evidence.NativeIdentity == original.NativeIdentity {
				byMember[plan.Member] = member.evidence
			}
		}
	}
	keys := make([]string, 0, len(byMember))
	for key := range byMember {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]WindowsCanonicalEntryMemberEvidenceV2, 0, len(keys))
	for _, key := range keys {
		out = append(out, byMember[key])
	}
	return out
}

func canonicalRecoveryOriginalBindingStableV2(original, current WindowsCanonicalEntryMembersRecoveryInputV2) bool {
	if !reflect.DeepEqual(original.Retained, current.Retained) {
		return false
	}
	if original.Targets != current.Targets || original.TransactionID != current.TransactionID || original.BeforeKind != current.BeforeKind || !reflect.DeepEqual(original.Expected, current.Expected) || !reflect.DeepEqual(original.Members, current.Members) || len(original.Directories) != len(current.Directories) || len(current.Moves) < len(original.Moves) {
		return false
	}
	for i := range original.Directories {
		if original.Directories[i].Original != current.Directories[i].Original {
			return false
		}
	}
	// Preserve historical outcomes and unknown rows. New actual attempts append.
	return reflect.DeepEqual(original.Moves, current.Moves[:len(original.Moves)])
}

func (c *canonicalEntryMembersV2) authorizeRecoveryEffect(ctx context.Context, key, relative string, discard bool) error {
	if c.source == nil {
		return nil
	}
	if c.recoveryInput == nil {
		return errors.New("current recovery original lineage unavailable")
	}
	if !discard && canonicalRecoveryCleanupPurposeV2(c.recoveryInput.Purpose) {
		return errors.New("cleanup-only recovery view cannot apply or restore a member")
	}
	fresh, err := receiveCanonicalEntryMembersRecoveryInputV2(ctx, c.source, c.recoverySide)
	if err != nil {
		return err
	}
	if !canonicalRecoveryOriginalBindingStableV2(*c.recoveryInput, fresh) {
		return errors.New("original recovery evidence/history changed before effect")
	}
	if !discard && canonicalRecoveryCleanupPurposeV2(fresh.Purpose) {
		return errors.New("current cleanup purpose cannot apply or restore a member")
	}
	if _, err = buildCanonicalRecoveryLineageV2(fresh); err != nil {
		return err
	}
	prior := c.recoverySide == WindowsCanonicalRecoveryPriorV1
	m := c.members[key]
	if m == nil {
		return errors.New("unissued recovery member")
	}
	if discard {
		if !canonicalRecoveryCleanupPurposeV2(fresh.Purpose) {
			return errors.New("current recovery purpose does not authorize file cleanup")
		}
		side := "unused-successor"
		if prior {
			side = "prior"
		}
		for _, row := range fresh.Cleanup {
			if row.Member == key && row.Side == side && row.State == "discard_intent" && row.SourceRelative == relative && row.ExpectedSHA256 == m.evidence.File.SHA256 && row.TransactionID == fresh.TransactionID && row.Purpose == fresh.Purpose {
				return nil
			}
		}
		return errors.New("exact current member discard intent not received")
	}
	for i := len(fresh.Moves) - 1; i >= 0; i-- {
		row := fresh.Moves[i]
		if row.Member != key || row.Prior != prior {
			continue
		}
		if i < len(c.recoveryInput.Moves) || row.SourceRelative != m.evidence.File.Path || row.DestinationRelative != relative || row.Attempted || row.Applied || row.Readback || row.NativeStatusKnown || row.Error != nil || row.CloseError != nil {
			return errors.New("new exact current member move intent not received")
		}
		return nil
	}
	return errors.New("current member move has no persisted intent")
}

func (d *WindowsCanonicalEntryPayloadDirectoryV1) authorizeMemberDirectoryDispose(ctx context.Context) error {
	if d.memberSource == nil {
		return nil
	}
	if d.memberSnapshot == nil || d.memberTransactionID == "" {
		return errors.New("cold directory has no original transaction/source binding")
	}
	input, err := receiveCanonicalEntryMembersRecoveryInputV2(ctx, d.memberSource, d.memberRecoverySide)
	if err != nil {
		return err
	}
	if input.TransactionID != d.memberTransactionID || filepath.Dir(input.Targets.CanonicalEntryPath) != d.memberSnapshot.Root || !canonicalRecoveryCleanupPurposeV2(input.Purpose) {
		return errors.New("cold directory cleanup transaction/purpose changed")
	}
	if _, err = buildCanonicalRecoveryLineageV2(input); err != nil {
		return err
	}
	for _, record := range input.Directories {
		if record.Original.RelativePath != d.memberSnapshot.RelativePath {
			continue
		}
		if record.Original != *d.memberSnapshot || !record.Original.Created || record.State != "dispose_intent" || canonicalRecoveryDirectoryRequiredV2(input, record.Original.RelativePath) {
			return errors.New("original directory is not eligible under its current exact disposal intent")
		}
		return nil
	}
	return errors.New("cold directory current original record missing")
}

func AcquireWindowsCanonicalEntryForRecoveryV2(ctx context.Context, source WindowsCanonicalRecoverySourceV2, side WindowsCanonicalRecoverySideV1) (r *WindowsCanonicalEntryRecoverySetV2, retErr error) {
	p := &WindowsCanonicalEntryPriorSetV2{original: newCanonicalEntryMembersV2()}
	r = &WindowsCanonicalEntryRecoverySetV2{original: p, source: source, side: side}
	c := &p.original
	c.source = source
	c.transferred = true
	c.recoverySide = side
	defer func() {
		c.acquisitionError = retErr
		if retErr != nil {
			c.ready = false
			c.recoveryAbsences = nil
		}
	}()
	if ctx == nil || source == nil {
		return r, errors.New("original current recovery source/context required")
	}
	// Retain every available original lender even if its provider also returns
	// cancellation/currentness failure. Directories remains a passive closer.
	lenders, lenderErr := source.OriginalMemberDirectories(ctx)
	r.lenders = append([]*WindowsCanonicalEntryPayloadDirectoryV1(nil), lenders...)
	if side != WindowsCanonicalRecoveryPriorV1 && side != WindowsCanonicalRecoverySuccessorV1 {
		return r, errors.Join(lenderErr, errors.New("current recovery side invalid"))
	}
	if lenderErr != nil {
		return r, lenderErr
	}
	retErr = c.withRecoveryReadScope(ctx, func(scope *canonicalEntryRecoveryReadScopeV2) error {
		return r.acquireRecoveryRead(ctx, scope)
	})
	if retErr == nil {
		c.ready = true
	}
	return r, retErr
}

func (r *WindowsCanonicalEntryRecoverySetV2) acquireRecoveryRead(ctx context.Context, scope *canonicalEntryRecoveryReadScopeV2) error {
	p := r.original
	c := &r.original.original
	var check func(context.Context) error
	if scope != nil {
		check = func(ctx context.Context) error { return scope.validateBank(ctx, c) }
	}
	input, err := receiveCanonicalEntryMembersRecoveryInputReadV2(ctx, r.source, r.side, check)
	if err != nil {
		return err
	}
	if err = validateCanonicalRecoveryCurrentCohortV2(input); err != nil {
		return err
	}
	lineage, err := buildCanonicalRecoveryLineageV2(input)
	if err != nil {
		return err
	}
	c.targets = input.Targets
	c.snapshot = cloneCanonicalEntrySnapshotV2(input.Expected)
	c.recoveryInput = &input
	p.observation.BeforeKind = input.BeforeKind
	if r.side == WindowsCanonicalRecoverySuccessorV1 {
		p.observation.BeforeKind = WindowsCanonicalEntryBeforeBundleV2
	}
	c.parent, err = c.root.openAbsolute(filepath.Dir(input.Targets.CanonicalEntryPath), true)
	if err != nil {
		return err
	}
	rootID, err := canonicalPayloadDirectoryIdentity(c.parent)
	if err != nil || rootID != lineage.root {
		return errors.Join(err, errors.New("current recovery original root identity/creation changed"))
	}
	for _, plan := range input.Members {
		c.root.memberFinalPaths = append(c.root.memberFinalPaths, plan.Member)
	}
	for key := range lineage.retained {
		c.root.memberFinalPaths = append(c.root.memberFinalPaths, key)
	}
	if err = r.receiveRecoveryDirectoriesRead(ctx, input, lineage, scope); err != nil {
		return err
	}
	matches, err := c.discoverRecoveryMembersRead(ctx, lineage, scope)
	if err != nil {
		return err
	}
	fresh, err := receiveCanonicalEntryMembersRecoveryInputReadV2(ctx, r.source, r.side, check)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(input, fresh) {
		return errors.New("original recovery journal changed after candidate discovery")
	}
	if err = c.acquireRecoveryMatchesRead(ctx, matches, scope); err != nil {
		return err
	}
	if len(lineage.retained) != 0 {
		if err := c.receiveRecoveryRetainedMembers(ctx, input, lineage.retained, scope); err != nil {
			return err
		}
	}
	finalMatches, err := c.discoverRecoveryMembersRead(ctx, lineage, scope)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(matches, finalMatches) {
		return errors.New("original recovery classification changed before ready receiving")
	}
	fresh, err = receiveCanonicalEntryMembersRecoveryInputReadV2(ctx, r.source, r.side, check)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(input, fresh) {
		return errors.New("original recovery journal changed before ready receiving")
	}
	if err = c.verifyRead(ctx, scope); err != nil {
		return err
	}
	// This index survives only when the enclosing synchronous read scope also
	// closes successfully and the acquired owner becomes ready.
	for _, match := range finalMatches {
		if match.original.prior == (r.side == WindowsCanonicalRecoveryPriorV1) && match.current == nil && match.original.cleanup != nil {
			if c.recoveryAbsences == nil {
				c.recoveryAbsences = make(map[string]WindowsCanonicalEntryMemberCleanupRecordV2)
			}
			c.recoveryAbsences[match.original.member] = *match.original.cleanup
		}
	}
	return nil
}

// The original journal still owns changed effects. This bank only borrows
// the omitted originals at their witnessed canonical names, without DELETE.
func (c *canonicalEntryMembersV2) receiveRecoveryRetainedMembers(ctx context.Context, input WindowsCanonicalEntryMembersRecoveryInputV2, retained map[string]RuntimeObjectV1, scope *canonicalEntryRecoveryReadScopeV2) error {
	keys := make([]string, 0, len(retained))
	for key := range retained {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	members := make([]WindowsCanonicalEntryMemberEvidenceV2, 0, len(keys))
	for _, key := range keys {
		member, missing, err := c.openMemberWithAccess(ctx, key, key, windows.FILE_READ_DATA, scope)
		if err != nil {
			return err
		}
		if missing != nil || member == nil || !canonicalMemberMatchesPlanV2(member.evidence, retained[key]) {
			return errors.New("canonical retained original missing or differs from the authenticated plan")
		}
		c.keys = append(c.keys, key)
		members = append(members, member.evidence)
	}
	digest, err := retainedCanonicalMembersSHA256(ctx, members)
	if err != nil {
		return err
	}
	if digest != input.Retained.MembersSHA256 {
		return errors.New("canonical retained original identity witness changed")
	}
	payload, err := ParseRuntimePayload(c.snapshot.RuntimePayloadBytes)
	if err != nil {
		return err
	}
	if err := verifyCanonicalRuntimeObjects(ctx, c, c.snapshot.BundleRelativePath, payload, canonicalRuntimeObjects(c.snapshot, payload), scope); err != nil {
		return err
	}
	sort.Strings(c.keys)
	c.recoveryRetained = retained
	return nil
}

func (p *WindowsCanonicalEntryPriorSetV2) receiveRecoveredRuntime(ctx context.Context) error {
	c := &p.original
	if p.observation.BeforeKind == WindowsCanonicalEntryBeforeAbsentV2 {
		return nil
	}
	key := WindowsCanonicalProductRelativePathV1
	if p.observation.BeforeKind == WindowsCanonicalEntryBeforeRootProductV2 {
		key = WindowsCanonicalEntryRelativePathV1
	}
	m := c.members[key]
	if m == nil {
		return errors.New("recovery original Product required")
	}
	info, err := m.original.file.Stat()
	if err != nil {
		return err
	}
	header, err := readWindowsPEHeader(m.original.file, info.Size())
	if err != nil {
		return err
	}
	metadata, err := readWindowsBuildIdentityAt(m.original.file, info.Size())
	if err != nil {
		return err
	}
	if metadata.Role != WindowsArtifactRoleProduct || header.subsystem != WindowsGUISubsystem || header.characteristics&windowsPEDLLCharacteristic != 0 {
		return errors.New("recovered runtime is not admitted Product")
	}
	p.runtimeMember = key
	p.artifact = WindowsArtifact{Path: m.original.path, Role: metadata.Role, Version: metadata.Version, Commit: metadata.Commit, BuildDate: metadata.BuildDate, SHA256: m.evidence.File.SHA256, Machine: header.machine, Subsystem: header.subsystem}
	return ctx.Err()
}
func (r *WindowsCanonicalEntryRecoverySetV2) VerifyUnchanged(ctx context.Context) error {
	if r == nil || r.original == nil {
		return errors.New("current recovery owner required")
	}
	return r.original.VerifyUnchanged(ctx)
}
func (r *WindowsCanonicalEntryRecoverySetV2) RuntimeProduct(ctx context.Context) (*WindowsCanonicalEntryPriorRuntimeV1, error) {
	if r == nil || r.original == nil {
		return nil, errors.New("current recovery owner required")
	}
	return r.original.RuntimeProduct(ctx)
}
func (r *WindowsCanonicalEntryRecoverySetV2) MoveOriginal(ctx context.Context, key, dst string) (WindowsCanonicalEntryMoveResultV1, error) {
	if r == nil || r.original == nil {
		return WindowsCanonicalEntryMoveResultV1{}, errors.New("current recovery owner required")
	}
	return r.original.MoveOriginal(ctx, key, dst)
}
func (r *WindowsCanonicalEntryRecoverySetV2) DiscardOriginal(ctx context.Context, key, relative string) (WindowsCanonicalEntryMoveResultV1, error) {
	if r == nil || r.original == nil {
		return WindowsCanonicalEntryMoveResultV1{}, errors.New("current recovery owner required")
	}
	return r.original.DiscardOriginal(ctx, key, relative)
}
func (r *WindowsCanonicalEntryRecoverySetV2) Disposition() WindowsCanonicalEntryMemberDispositionV2 {
	if r == nil || r.original == nil {
		return WindowsCanonicalEntryMemberDispositionV2{}
	}
	result := r.original.Disposition()
	for _, d := range r.Directories() {
		d.mu.Lock()
		result.OriginalOwnerPending = result.OriginalOwnerPending || !d.closed || d.closeError != nil
		result.CloseError = errors.Join(result.CloseError, d.closeError)
		if d.memberSnapshot != nil && d.memberSnapshot.Created && (d.disposition == nil || !d.disposition.Readback) {
			result.DispositionPending = true
		}
		d.mu.Unlock()
	}
	return result
}
func (r *WindowsCanonicalEntryRecoverySetV2) Close() error {
	if r == nil || r.original == nil {
		return nil
	}
	return r.original.Close()
}
