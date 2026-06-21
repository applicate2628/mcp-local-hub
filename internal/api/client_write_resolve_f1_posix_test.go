//go:build !windows

package api

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// AF-1 F1 — intermediate-component-swap closure (POSIX leg).
//
// The handle-pinned resolved-symlink write descends to the resolved target's
// parent COMPONENT-BY-COMPONENT from the volume root ("/"), O_NOFOLLOW at
// every step. A single path-based open of the whole parent string (the
// pre-F1 shape) protected only the FINAL component; the kernel re-walked
// every INTERMEDIATE component, so an intermediate dir swapped to a symlink
// between resolve and write redirected the write. F1 closes that: descending
// one real component at a time refuses a swapped intermediate at the next
// O_NOFOLLOW open (ELOOP).
//
// These tests run on POSIX only (symlink creation needs no elevation there;
// the cross-platform descent code is identical and exercised on this leg —
// same convention as the TestA3_T* threat-model tests).

// TestF1_IntermediateComponentSwap_Refused engineers the intermediate-swap
// window deterministically via resolvedParentDescendStepHook (race-window-
// assertion discipline: NO natural race). Layout: <root>/anchor/mid/real.json.
//
// The resolved target's PARENT is <root>/anchor/mid, so within that parent
// path `mid` is the FINAL component (O_NOFOLLOW already guards it even on the
// pre-F1 single-open shape) and `anchor` is an INTERMEDIATE component (the
// pre-F1 single open re-walked it — the F1 gap). The swap therefore targets
// `anchor`: after the descent opens <root>'s leaf and BEFORE it opens
// `anchor`, the hook renames the real `anchor` aside and plants a symlink at
// `anchor` pointing at an attacker tree that mirrors anchor/mid/real.json.
// The descent's next O_NOFOLLOW open of `anchor` must REFUSE (ELOOP) and the
// write must NEVER land on the attacker tree.
//
// Swapping an INTERMEDIATE (not the parent's final component) is what makes
// this a genuine F1 regression guard: a pre-F1 build that opened the whole
// parent string once with O_NOFOLLOW would follow the swapped `anchor` and
// write the attacker file.
func TestF1_IntermediateComponentSwap_Refused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows; the cross-platform descent is exercised on POSIX")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "1") // env opt-in (F2 lane)
	_ = hubMcpStateTestHelper(t)

	// Real resolved chain: <root>/anchor/mid/real.json (all real dirs/file).
	// Build it under a hardened temp so the parent gate would otherwise pass.
	root := hardenedTempDir(t)
	anchor := filepath.Join(root, "anchor")
	mid := filepath.Join(anchor, "mid")
	if err := os.MkdirAll(mid, 0o700); err != nil {
		t.Fatalf("mkdir anchor/mid: %v", err)
	}
	real := filepath.Join(mid, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed real: %v", err)
	}

	// Attacker tree mirrors anchor/mid/real.json so a followed swap would land
	// the privileged write at attacker/mid/real.json.
	attacker := hardenedTempDir(t)
	if err := os.MkdirAll(filepath.Join(attacker, "mid"), 0o700); err != nil {
		t.Fatalf("mkdir attacker/mid: %v", err)
	}
	attackerFile := filepath.Join(attacker, "mid", "real.json")
	if err := os.WriteFile(attackerFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed attacker file: %v", err)
	}

	// The symlink the operator points at. It resolves to `real` so the
	// confirm-time resolve sees a follow-able target; the swap below targets
	// the INTERMEDIATE `anchor`, not the symlink itself.
	linkTree := hardenedTempDir(t)
	link := filepath.Join(linkTree, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// <root>'s leaf basename is the component opened immediately BEFORE
	// `anchor`. Compute it from the resolved chain so the hook fires at the
	// right step regardless of how /tmp resolves.
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("eval root: %v", err)
	}
	rootLeaf := filepath.Base(rootResolved)

	swapped := false
	resolvedParentDescendStepHook = func(openedComponent string) {
		if swapped || openedComponent != rootLeaf {
			return
		}
		swapped = true
		// Swap the not-yet-opened INTERMEDIATE `anchor` into a symlink to the
		// attacker tree. Model the co-resident attack: rename the real
		// `anchor` directory aside (it is non-empty — it holds mid/real.json)
		// and drop a symlink in its place. The descent holds <root>'s fd; its
		// next O_NOFOLLOW open of `anchor` must now refuse the planted symlink.
		asideAnchor := filepath.Join(root, "anchor.real")
		if err := os.Rename(anchor, asideAnchor); err != nil {
			t.Fatalf("rename real anchor aside for swap: %v", err)
		}
		if err := os.Symlink(attacker, anchor); err != nil {
			t.Fatalf("plant intermediate symlink at anchor: %v", err)
		}
	}
	t.Cleanup(func() { resolvedParentDescendStepHook = nil })

	err = secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil)
	if err == nil {
		t.Fatalf("F1: intermediate-component swap must REFUSE the write")
	}
	if !swapped {
		t.Fatalf("F1: descent hook never fired on %q — the descent did not pass through the expected intermediate; test wiring is wrong", rootLeaf)
	}
	// The attacker file must be UNTOUCHED — the swapped intermediate must
	// never have been followed.
	if b, _ := os.ReadFile(attackerFile); string(b) != "{}" {
		t.Errorf("F1: attacker file was written via the swapped intermediate — TOCTOU NOT closed: %q", b)
	}
}

// TestF1_OutOfHomeSymlink_Succeeds is claim 7: the volume-root anchor does NOT
// reject an out-of-home resolved target. A symlink resolving to a tree OUTSIDE
// $HOME (a SEPARATE temp tree, not under the redirected HOME) still writes
// successfully through the component descent — proving the F1 walk delivers
// ONLY "no intermediate is followed through a swap", with no path-containment
// refusal. This is the same property TestA3_T4 pins; here it is asserted
// against the new descent code with an explicit out-of-home target.
func TestF1_OutOfHomeSymlink_Succeeds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "1")
	// Redirect HOME to a hardened tree that is NOT an ancestor of the resolved
	// target, so "out of home" is concrete.
	home := hardenedTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	_ = hubMcpStateTestHelper(t)

	// Resolved target lives in a SEPARATE tree, deeper than one component, so
	// the descent walks several intermediates.
	targetTree := hardenedTempDir(t)
	deep := filepath.Join(targetTree, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatalf("mkdir deep target tree: %v", err)
	}
	real := filepath.Join(deep, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed real: %v", err)
	}
	linkTree := hardenedTempDir(t)
	link := filepath.Join(linkTree, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil); err != nil {
		t.Fatalf("F1: out-of-home symlink write must SUCCEED via the component descent: %v", err)
	}
	if b, _ := os.ReadFile(real); string(b) != `{"v":1}` {
		t.Errorf("F1: out-of-home resolved target content = %q, want %q", b, `{"v":1}`)
	}
	// Owner-only (the per-file boundary still applies through the descent).
	if info, err := os.Stat(real); err != nil {
		t.Fatalf("F1: stat resolved target: %v", err)
	} else if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("F1: resolved file mode = %o, want 0600 (owner-only)", mode)
	}
}

// TestF1_RelativeSymlink_Succeeds is the Finding 1 regression guard. A
// client-config symlink reached by a RELATIVE write destination, whose target
// is ALSO relative (link.json -> real.json in the same directory), must still
// write through the component descent.
//
// Pre-fix, resolveSymlinkFinalPath returned filepath.EvalSymlinks(path)
// directly. When the input path is relative (the symlink is referenced by a
// relative destination), EvalSymlinks of a relative-target symlink returns a
// RELATIVE path, which the volume-root descent then rejected (`!filepath.IsAbs`
// → "is not absolute; cannot descend from volume root"), regressing the
// shipping opt-in/consent symlink path (the old single-open opened "." as the
// parent). The fix absolutizes the input before EvalSymlinks (the single
// resolveSymlinkFinalPath owner), so the resolved path is absolute again and
// BOTH the descent AND the PR-A full-target pin see the same absolutized path.
// FAILS pre-fix.
//
// The write destination is passed RELATIVE (bare "link.json") from inside the
// link's directory — that is the input shape that makes EvalSymlinks return a
// relative path. An absolute destination already yields an absolute resolved
// path on POSIX (the link's own dir is absolute), so the regression is only
// reachable via a relative input.
func TestF1_RelativeSymlink_Succeeds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "1") // env opt-in (F2 lane)
	_ = hubMcpStateTestHelper(t)

	// real.json and link.json live in the SAME hardened dir; the symlink
	// target is the BARE basename "real.json" (a relative target).
	dir := hardenedTempDir(t)
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed real: %v", err)
	}
	link := filepath.Join(dir, "link.json")
	// RELATIVE target: "real.json", NOT the absolute path.
	if err := os.Symlink("real.json", link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// Drive the write with a RELATIVE destination ("link.json") from inside
	// the link's directory, so resolveSymlinkFinalPath receives a relative
	// input and (pre-fix) resolves to a relative path the descent rejects.
	// Restore CWD on exit; these tests are not parallel.
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir into link dir: %v", err)
	}

	if err := secureWriteWithOperatorOptConsent("link.json", []byte(`{"v":1}`), nil); err != nil {
		t.Fatalf("F1: relative-input relative-target symlink write must SUCCEED via the component descent: %v", err)
	}
	if b, _ := os.ReadFile(real); string(b) != `{"v":1}` {
		t.Errorf("F1: relative-symlink resolved target content = %q, want %q", b, `{"v":1}`)
	}
	if info, err := os.Stat(real); err != nil {
		t.Fatalf("F1: stat resolved target: %v", err)
	} else if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("F1: resolved file mode = %o, want 0600 (owner-only)", mode)
	}
}

// TestF1_ExecuteOnlyAncestor_Succeeds is the Finding 2 regression guard. A
// resolved target below an EXECUTE-ONLY ancestor (0111 — no directory READ)
// with a writable final parent must still write: ordinary path traversal and
// the old single parent open only ever needed SEARCH/EXECUTE on ancestors.
//
// Pre-fix, the descent opened every intermediate with O_RDONLY
// (openExistingRealDirAt), requiring directory READ, so an execute-only
// ancestor failed EACCES BEFORE the final parent opened, breaking legitimate
// opted-in symlink configs. The fix opens intermediate ancestors search-only
// (O_PATH on Linux), keeping the final parent on the normal read fd. FAILS
// pre-fix on Linux (on darwin, the preview-tier read-gate fallback would still
// EACCES, so the assertion is Linux-scoped).
func TestF1_ExecuteOnlyAncestor_Succeeds(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Finding 2 search-only (O_PATH) ancestor traversal is a Linux capability; darwin keeps the preview-tier read-gate fallback")
	}
	// Root (UID 0) bypasses POSIX directory READ permission checks
	// (DAC_READ_SEARCH), so chmod 0111 does NOT make the ancestor unreadable
	// to root — the sanity check below (os.ReadDir must EACCES) would itself
	// fail before the secure-write path is exercised, turning a real test into
	// a false failure. Skip when euid==0; the path is still exercised as a
	// non-root user (CI runs the api package unprivileged).
	if os.Geteuid() == 0 {
		t.Skip("execute-only ancestor is unenforceable as root (UID 0 bypasses POSIX directory READ checks); run as non-root to exercise the search-only descent")
	}
	t.Setenv(RequireSingleUserHomeEnv, "")
	t.Setenv(AllowClientConfigSymlinkEnv, "1")
	_ = hubMcpStateTestHelper(t)

	// Chain: <root>/exec-only/leaf/real.json. `leaf` (the FINAL parent) is
	// writable 0700; `exec-only` (an INTERMEDIATE ancestor) is execute-only
	// 0111 — traversable but NOT readable. A read-gating descent EACCESes
	// here before reaching `leaf`.
	root := hardenedTempDir(t)
	execOnly := filepath.Join(root, "exec-only")
	leaf := filepath.Join(execOnly, "leaf")
	if err := os.MkdirAll(leaf, 0o700); err != nil {
		t.Fatalf("mkdir exec-only/leaf: %v", err)
	}
	real := filepath.Join(leaf, "real.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed real: %v", err)
	}

	linkTree := hardenedTempDir(t)
	link := filepath.Join(linkTree, "link.json")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// Clamp the ancestor to execute-only (0111) AFTER seeding so the seed
	// writes above succeeded. Restore to 0700 in cleanup so t.TempDir's
	// RemoveAll can descend (it needs write+execute on the dir to unlink
	// children).
	if err := os.Chmod(execOnly, 0o111); err != nil {
		t.Fatalf("chmod exec-only to 0111: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(execOnly, 0o700) })

	// Sanity: prove the ancestor is genuinely NOT readable (so the test
	// would actually EACCES on a read-gating descent — not a no-op).
	if _, err := os.ReadDir(execOnly); err == nil {
		t.Fatalf("test wiring: exec-only ancestor is still readable; the read-gate regression would not be exercised")
	}

	if err := secureWriteWithOperatorOptConsent(link, []byte(`{"v":1}`), nil); err != nil {
		t.Fatalf("F1/Finding-2: write below an execute-only ancestor must SUCCEED via search-only descent: %v", err)
	}
	if b, _ := os.ReadFile(real); string(b) != `{"v":1}` {
		t.Errorf("F1/Finding-2: resolved target content = %q, want %q", b, `{"v":1}`)
	}
	if info, err := os.Stat(real); err != nil {
		t.Fatalf("F1/Finding-2: stat resolved target: %v", err)
	} else if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("F1/Finding-2: resolved file mode = %o, want 0600 (owner-only)", mode)
	}
}
