//go:build windows

package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSecureCreateParentDirAnywhere_WindowsStrictAllowsBroadenedAncestor pins bot
// PR #420 r17 finding B1: the volume-root-anchored config-lock parent creator
// must DACL-gate ONLY the deepest existing prefix, NOT every system-owned ancestor
// from the volume root down. A broadened ANCESTOR (the Windows analog of C:\Users
// granting Authenticated Users) ABOVE the deepest existing prefix must NOT make a
// fresh strict-mode create fail — only the deepest existing prefix (the dir the
// creator does not create and must trust) is gated.
//
// Layout: <hardened-tmp>/broad (broadened ACE) / deep (HARDENED allowlist) is the
// deepest existing prefix; the creator makes fresh children below `deep`. The
// broad ancestor must be skipped by the gate, the hardened deep prefix verified,
// and the create succeed.
func TestSecureCreateParentDirAnywhere_WindowsStrictAllowsBroadenedAncestor(t *testing.T) {
	root := hardenedTempDir(t)

	broad := filepath.Join(root, "broad")
	if err := os.Mkdir(broad, 0o700); err != nil {
		t.Fatalf("mkdir broad ancestor: %v", err)
	}
	synthesizeDirWithInheritableAuthUsersReadACE(t, broad) // broaden the ANCESTOR

	deep := filepath.Join(broad, "deep")
	if err := os.Mkdir(deep, 0o700); err != nil {
		t.Fatalf("mkdir deep prefix: %v", err)
	}
	// Make `deep` allowlist-conforming so it passes the strict DACL gate; it is the
	// deepest existing prefix (the only dir the gate should verify). PROTECTED so it
	// does not inherit the broadened ACE from `broad`.
	entries, err := allowlistExplicitAccess()
	if err != nil {
		t.Fatalf("allowlistExplicitAccess: %v", err)
	}
	applyProtectedDACLFromEntries(t, deep, entries)

	// Fresh chain to create BELOW the hardened deepest existing prefix.
	dir := filepath.Join(deep, "config", "mimocode")

	// skipParentGate=false → strict gate ENABLED. With the B1 fix it fires ONCE on
	// `deep` (allowlist-conforming → passes), and NEVER on the broadened `broad`
	// ancestor, so the create must SUCCEED.
	if err := secureCreateParentDirAnywhereImpl(dir, false); err != nil {
		t.Fatalf("strict-mode create must SUCCEED with a broadened ANCESTOR above the deepest existing prefix (B1), got %v", err)
	}
	if st, statErr := os.Lstat(dir); statErr != nil || !st.IsDir() {
		t.Fatalf("dir %s not created as a real directory: %v", dir, statErr)
	}
}

// TestSecureCreateParentDirAnywhere_WindowsStrictRefusesBroadenedDeepestPrefix
// confirms the B1 fix still ENFORCES the real security check: when the DEEPEST
// EXISTING PREFIX (not a higher ancestor) is broadened, the strict gate refuses
// with ErrSecureWriteParentInsecure.
func TestSecureCreateParentDirAnywhere_WindowsStrictRefusesBroadenedDeepestPrefix(t *testing.T) {
	root := hardenedTempDir(t)

	// The deepest existing prefix itself is broadened.
	deep := filepath.Join(root, "deep-broadened")
	if err := os.Mkdir(deep, 0o700); err != nil {
		t.Fatalf("mkdir deep prefix: %v", err)
	}
	synthesizeDirWithInheritableAuthUsersReadACE(t, deep)

	dir := filepath.Join(deep, "config", "mimocode") // fresh children below the broadened prefix
	err := secureCreateParentDirAnywhereImpl(dir, false)
	if err == nil {
		t.Fatalf("strict-mode create must REFUSE a broadened deepest existing prefix, got nil")
	}
	if !errors.Is(err, ErrSecureWriteParentInsecure) {
		t.Fatalf("expected ErrSecureWriteParentInsecure for a broadened deepest existing prefix, got %v", err)
	}
	// The fresh child must NOT have been created.
	if _, statErr := os.Stat(filepath.Join(deep, "config")); statErr == nil {
		t.Fatalf("refused create still made the child dir %s", filepath.Join(deep, "config"))
	}
}

// TestSecureCreateParentDirAnywhere_WindowsStrictGatesDeepPrefixAfterAnchorClosed
// pins the bot PR #420 r18 HIGH finding: the strict DACL gate must fire on the
// DEEPEST EXISTING PREFIX even though the volume-root anchor handle has been
// CLOSED many descent iterations earlier (the loop closes the held parent on each
// step). The prior code decided "is this the volume root? → skip the gate" by raw
// handle-value equality `curHandle != anchorHandle`; because Windows recycles
// handle values after CloseHandle, a real deep prefix's handle could alias the
// now-closed anchor value and be MISCLASSIFIED as the root → gate skipped → a
// broadened deepest-existing-prefix would slip through. The fix keys the root-skip
// on a POSITION flag (curIsAnchor), not a handle value.
//
// This layout makes the deepest existing prefix several levels below the volume
// root AND below the (already deep) hardened temp dir, so the anchor handle is
// closed long before the gate iteration — exactly the window where a recycled
// handle value could alias the anchor. The deepest existing prefix is BROADENED,
// so the gate MUST refuse regardless of any handle-value coincidence. (The alias
// itself is nondeterministic and cannot be forced; this test asserts the
// position-based decision always gates the deepest prefix, which is the property
// the fix guarantees deterministically.)
func TestSecureCreateParentDirAnywhere_WindowsStrictGatesDeepPrefixAfterAnchorClosed(t *testing.T) {
	root := hardenedTempDir(t)

	// A multi-level existing chain below the hardened temp root. Each level is
	// owner-only/hardened EXCEPT the final one, which is the broadened deepest
	// existing prefix the gate must catch. The depth guarantees the anchor handle
	// is closed many iterations before the gated prefix is reached.
	a := filepath.Join(root, "a")
	b := filepath.Join(a, "b")
	c := filepath.Join(b, "c")
	deep := filepath.Join(c, "deepest-existing-broadened")
	for _, d := range []string{a, b, c, deep} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// Broaden ONLY the deepest existing prefix; the gate must fire on it.
	synthesizeDirWithInheritableAuthUsersReadACE(t, deep)

	// Fresh children to create BELOW the broadened deepest existing prefix.
	dir := filepath.Join(deep, "config", "mimocode")
	err := secureCreateParentDirAnywhereImpl(dir, false)
	if err == nil {
		t.Fatalf("strict-mode create must REFUSE a broadened deepest existing prefix reached after the anchor handle was closed, got nil")
	}
	if !errors.Is(err, ErrSecureWriteParentInsecure) {
		t.Fatalf("expected ErrSecureWriteParentInsecure for the broadened deep prefix, got %v", err)
	}
	// No child created under the rejected prefix (no-child-on-refusal guarantee).
	if _, statErr := os.Stat(filepath.Join(deep, "config")); statErr == nil {
		t.Fatalf("refused create still made the child dir %s", filepath.Join(deep, "config"))
	}
}

// TestSecureCreateParentDirAnywhere_WindowsStrictGatesDirItselfWhenAllExist pins
// the trailing all-exist gate after the r18 HIGH fix: when EVERY component of
// `dir` already exists (nothing created), the gate must fire on `dir` itself (the
// deepest existing prefix in that case). With the fix the trailing gate is plain
// `if !gated` — the anchor can never be the held handle here because `parts` is
// non-empty so the loop swapped at least once. A broadened already-existing `dir`
// must be refused.
func TestSecureCreateParentDirAnywhere_WindowsStrictGatesDirItselfWhenAllExist(t *testing.T) {
	root := hardenedTempDir(t)
	a := filepath.Join(root, "a")
	dir := filepath.Join(a, "b") // dir fully exists; b is broadened
	for _, d := range []string{a, dir} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	synthesizeDirWithInheritableAuthUsersReadACE(t, dir) // broaden the all-exist target itself

	err := secureCreateParentDirAnywhereImpl(dir, false)
	if err == nil {
		t.Fatalf("strict-mode create must REFUSE an all-exist broadened `dir` itself, got nil")
	}
	if !errors.Is(err, ErrSecureWriteParentInsecure) {
		t.Fatalf("expected ErrSecureWriteParentInsecure for the all-exist broadened dir, got %v", err)
	}
}

// TestSecureCreateParentDirAnywhere_WindowsRelaxLaneSkipsGate confirms the relax
// lane (skipParentGate=true) bypasses the DACL gate entirely (created dirs still
// owner-only), so a broadened ancestor never blocks the default (non-strict) lane.
func TestSecureCreateParentDirAnywhere_WindowsRelaxLaneSkipsGate(t *testing.T) {
	root := hardenedTempDir(t)
	broad := filepath.Join(root, "broad")
	if err := os.Mkdir(broad, 0o700); err != nil {
		t.Fatalf("mkdir broad: %v", err)
	}
	synthesizeDirWithInheritableAuthUsersReadACE(t, broad)

	dir := filepath.Join(broad, "config", "mimocode")
	if err := secureCreateParentDirAnywhereImpl(dir, true); err != nil {
		t.Fatalf("relax lane must skip the DACL gate and succeed, got %v", err)
	}
	if st, statErr := os.Lstat(dir); statErr != nil || !st.IsDir() {
		t.Fatalf("relax-lane create did not produce a real dir: %v", statErr)
	}
}
