// internal/api/workspace_prune.go
//
// Pure structural-prune classifiers for the workspace-daemon auto-prune
// feature (Phase 1, 1b). These are the two predicates the in-GUI prune sweeper
// evaluates per workspace daemon row to decide whether a registration is
// structurally dead and should be auto-pruned. They are deliberately pure (no
// fleet, no IPC, no registry) so they are unit-testable with table cases.
//
// Both operate on the CANONICAL workspace path. Registry rows store the
// EvalSymlinks-resolved + drive-lowercased canonical form (set at register
// time: serena_auto_register.go and lsp_auto_register.go canonicalize before
// writing the row), so the predicates can rely on that normalization.

package api

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// AutoPruneWorkspacesSettingKey is the GUI-settable bool that gates the in-GUI
// workspace-daemon auto-prune sweeper (SettingsRegistry:
// daemons.auto_prune_workspaces, Default "true"). The sweeper reads it each
// tick so a toggle takes effect within ~60s with no restart.
const AutoPruneWorkspacesSettingKey = "daemons.auto_prune_workspaces"

// PruneIdleHoursSettingKey is the GUI-settable int (HOURS) that enables the
// Phase-3 idle auto-prune. "0" = off (only the structural triggers run); >0 =
// also prune a workspace whose most-recent activity is older than that many
// hours. The sweeper reads it each tick.
const PruneIdleHoursSettingKey = "daemons.prune_idle_hours"

// PruneDeadWorktreesSettingKey is the GUI-settable bool that gates the
// dead-git-worktree structural orphan signal (SettingsRegistry:
// daemons.prune_dead_worktrees, Default "true"). The sweeper reads it each tick
// so a toggle takes effect within ~60s with no restart. Default-on because the
// signal is structural and false-positive-safe by construction (see
// IsDeadGitWorktreePath), consistent with the deleted-dir signal's default-on
// posture.
const PruneDeadWorktreesSettingKey = "daemons.prune_dead_worktrees"

// gitWorktreePointerPrefix is the leading token of the single-line pointer a git
// worktree stores in its `.git` FILE: `gitdir: <path-to-admin-dir>`. A normal
// repo has a `.git` DIRECTORY (no pointer file); a linked worktree has this
// `.git` regular file pointing at its admin directory under the main repo's
// `.git/worktrees/<name>`.
const gitWorktreePointerPrefix = "gitdir:"

// maxGitPointerFileBytes caps how many bytes parseGitWorktreePointer reads from a
// `.git` pointer FILE (Finding 2, r6). A real linked-worktree / submodule `.git`
// holds a single `gitdir: <path>\n` line — a few hundred bytes at most. The file
// lives under the (possibly attacker-controlled) workspace tree, so an unbounded
// os.ReadFile on every GUI sweep / CLI prune tick would let a hostile or
// corrupt multi-GB `.git` cause repeated memory spikes / stalls. The read is
// bounded by io.LimitReader to this cap, and a result that hits the cap is
// treated as AMBIGUOUS (ok=false → never prune) — a genuine pointer never
// approaches it, so the cap can only ever reject a malformed/oversized file.
// 64 KiB matches the maxSerenaProjectYMLBytes posture for the same class of
// small, untrusted, under-workspace control files.
const maxGitPointerFileBytes = 64 * 1024

// agentWorktreeMarker is the path segment that identifies an ephemeral
// agent worktree (e.g. ".claude/worktrees/agent-<id>"). Such worktrees are
// created per agent session and are ephemeral by design, so a daemon
// registered against one is pruned IMMEDIATELY (no consecutive-tick grace
// window) once observed.
const agentWorktreeMarker = "/.claude/worktrees/agent-"

// IsAgentWorktreePath reports whether canonicalPath points inside an ephemeral
// `.claude/worktrees/agent-*` worktree. The check is a forward-slash substring
// match (filepath.ToSlash normalizes the Windows backslash separators in the
// stored canonical path) so it works identically on every GOOS. canonicalPath
// is already canonical (the registry stores EvalSymlinks-resolved,
// drive-lowercased paths), so the substring is reliable after ToSlash.
// Exported so the GUI prune sweeper (internal/gui) can classify rows.
func IsAgentWorktreePath(canonicalPath string) bool {
	if canonicalPath == "" {
		return false
	}
	return strings.Contains(filepath.ToSlash(canonicalPath), agentWorktreeMarker)
}

// WorkspaceDirDeleted reports whether the workspace directory at canonicalPath
// has been DEFINITIVELY deleted. It stats the best-effort symlink-resolved form
// of the path (resolveSymlinksBestEffort, the same resolver the cleanup
// canonicalizer uses) and returns true ONLY on a definitive ENOENT
// (os.IsNotExist). Any OTHER stat error (permission denied, transient I/O, an
// unavailable network/removable drive) returns FALSE — a transient or
// ambiguous error must NEVER be read as "deleted", because pruning a workspace
// whose directory merely momentarily failed to stat would tear down a live
// registration. The sweeper layers a 2-consecutive-ENOENT-tick grace on top of
// this to absorb a transient unmount; this predicate is the single-observation
// primitive. Exported so the GUI prune sweeper (internal/gui) can classify rows.
func WorkspaceDirDeleted(canonicalPath string) bool {
	if canonicalPath == "" {
		return false
	}
	resolved := resolveSymlinksBestEffort(canonicalPath)
	if _, err := os.Stat(resolved); err != nil {
		return os.IsNotExist(err)
	}
	return false
}

// IsDeadGitWorktreePath reports whether canonicalPath is (or is a SUBDIR inside)
// a leftover git LINKED WORKTREE whose admin directory has been deleted — the
// workspace directory still exists on disk (so WorkspaceDirDeleted is false) but
// the worktree it represents is structurally dead because git's per-worktree
// admin dir (`<main-repo>/.git/worktrees/<name>`) is gone. This is the REAL
// incident signal: such a directory slips through BOTH the agent-worktree path
// check (its path is not under `.claude/worktrees/agent-`) AND the deleted-dir
// check (the directory still exists).
//
// It returns true ONLY when ALL of the following hold — conservative by
// construction so a live repo, a live worktree, a live submodule, an unmounted
// admin root, a cross-OS pointer, or any ambiguous-error stat can NEVER be
// misclassified:
//
//  1. the workspace directory STILL EXISTS (a definitive ENOENT on the dir is
//     the deleted-dir case, owned by WorkspaceDirDeleted — NOT here);
//  2. walking UP from the workspace dir through its ancestors, a `.git` found is
//     a REGULAR FILE (a worktree/submodule pointer). The walk handles a SUBDIR
//     workspace inside a linked worktree (a monorepo package, or an LSP/.serena
//     marker root) whose `.git` pointer lives at the worktree ROOT (an ancestor),
//     not the subdir itself. If a `.git` is a DIRECTORY it is a normal repo root
//     → LIVE → false (stop the walk). A non-git tree with no `.git` anywhere up to
//     the volume root → false. When the NEAREST regular-file pointer resolves to a
//     SUBMODULE admin path (a submodule registered as the workspace, sitting INSIDE
//     a linked worktree), the walk CONTINUES past it to find the OUTER worktree's
//     own `.git` pointer (Finding 2) — a submodule pointer is the ONE outcome the
//     walk climbs past; any ambiguous/unparsable pointer stops it false;
//  3. the pointer's `gitdir: <path>` (relative paths resolved against the
//     worktree-root dir that holds the pointer) names a WORKTREE admin directory
//     of the canonical `<common-git-dir>/worktrees/<name>` shape — its immediate
//     parent dir is `worktrees` AND no INTERIOR `modules` store segment appears
//     after the first git-common-dir segment (so a `<repo>/.git/modules/<name>`
//     submodule path, a submodule under a worktrees-named dir
//     `<repo>/.git/modules/.../worktrees/foo`, AND a submodule INSIDE a worktree
//     `<repo>/.git/worktrees/<wt>/modules/<sub>...` are ALL rejected here, see
//     isWorktreeAdminPath / Findings 1+3) that is ABSENT via
//     os.IsNotExist-ONLY, AND
//     an admin-dir ANCESTOR (the PARENT `.git/worktrees/`, or — when the LAST
//     worktree was removed and git cleaned the empty `worktrees/` — the
//     GRANDPARENT repo `.git/`) is still present (distinguishing "worktree
//     removed" from "whole admin root unmounted" — see isAdminDirGenuinelyDeleted).
//     Any OTHER stat error on the admin dir (permission, transient I/O, an
//     offline/removable mount) returns FALSE — it inherits the EXACT
//     false-positive-safe discipline of WorkspaceDirDeleted: a transient or
//     ambiguous error must NEVER read as "dead".
//
// SUBMODULE SAFETY: a git submodule ALSO stores a `.git` FILE, pointing at
// `<superproject>/.git/modules/<name>`. THREE layers keep a submodule from ever
// being pruned: (a) a LIVE submodule's admin dir is present, so the availability
// discriminator returns false; (b) a submodule on an OFFLINE mount yields a
// NON-ENOENT stat error on the admin dir (or an ENOENT whose PARENT is also
// gone), so the discriminator returns false; (c) condition 3a
// (isWorktreeAdminPath) requires the admin path to be a WORKTREE admin path
// (immediate parent dir `worktrees` AND no INTERIOR `modules` store segment) — a
// `modules/<name>` submodule path fails the parent check, a submodule under a
// worktrees-named dir (`.git/modules/.../worktrees/foo`) carries an interior
// `modules`, a submodule INSIDE a worktree (`.git/worktrees/<wt>/modules/<sub>...`)
// also carries an interior `modules`, and a BOUNDARY-LESS bare-repo submodule
// store (no `.git`/`*.git` segment, `<bare>/worktrees/<wt>/modules/deps/worktrees/foo`)
// is caught by the front-scan — all classified submodule paths. A submodule path
// is NOT a terminal reject: the walk CLIMBS PAST it (Finding 2) to look for an
// OUTER worktree pointer. For a plain submodule in a regular repo the next climb
// hits the superproject's `.git` DIRECTORY → stop the walk → false; for a
// submodule INSIDE a linked worktree the next climb finds the worktree-root `.git`
// pointer and fires ONLY when that OUTER worktree admin is genuinely deleted. So an
// ONLINE submodule whose admin LEAF is merely absent (parent dir still present,
// e.g. before `git submodule update --init`) is never misread as a removed
// worktree. Layer (c) is the Finding-1/Finding-2/Finding-3 fix; layers (a)/(b) are
// the prior offline guard.
//
// Exported so the GUI prune sweeper (internal/gui) can classify rows.
func IsDeadGitWorktreePath(canonicalPath string) bool {
	if canonicalPath == "" {
		return false
	}
	dir := resolveSymlinksBestEffort(canonicalPath)

	// Condition 1: the workspace directory must STILL EXIST. A definitive ENOENT
	// on the directory is the deleted-dir case (WorkspaceDirDeleted owns it); any
	// other stat error is ambiguous → not a dead worktree. Either way, bail.
	if _, err := os.Stat(dir); err != nil {
		return false
	}

	// Condition 2+3: walk UP from the workspace dir to the NEAREST `.git`. A subdir
	// workspace inside a linked worktree keeps its `.git` pointer at the worktree
	// ROOT (an ancestor), so probing only `<dir>/.git` would miss it (Finding 1).
	//
	// Finding 2 (this round): the nearest regular-file `.git` may be a SUBMODULE
	// pointer rather than a worktree pointer — a SUBMODULE registered as the
	// workspace sits INSIDE a linked worktree, so its own `.git` points at the
	// submodule store (`<repo>/.git/worktrees/<wt>/modules/<sub>`), which
	// isWorktreeAdminPath REJECTS (interior `modules`). The OUTER worktree's own
	// `.git` pointer (the genuine worktree admin) lives at an ANCESTOR. So when the
	// nearest pointer resolves to a submodule admin path we do NOT stop — we
	// CONTINUE the ancestor walk (re-invoking from the parent of the dir that held
	// the just-rejected pointer) to find an OUTER worktree's `.git` pointer. The
	// continue fires on EXACTLY ONE outcome: a successfully-parsed, positively-
	// classified submodule-admin pointer. Every other non-accept outcome STOPS
	// false (a git DIRECTORY, the volume root, an ambiguous Lstat, an
	// unparsable/foreign/oversized pointer): climbing past an UNCLASSIFIABLE
	// pointer could prune a workspace under a live git pointer, so ambiguity always
	// stops. (Architect-adjudicated decision table; see isWorktreeAdminPath.)
	start := dir
	for {
		gitPath, pointerDir, ok := findNearestGitPointer(start)
		if !ok {
			// No regular-file `.git` pointer before a normal-repo `.git` DIRECTORY,
			// an ambiguous Lstat, or the volume root → not a (dead) linked worktree.
			return false
		}
		// Parse the `gitdir:` pointer (relative paths resolved against the dir that
		// HOLDS the pointer — the worktree/submodule root — not the subdir workspace).
		adminDir, ok := parseGitWorktreePointer(gitPath, pointerDir)
		if !ok {
			// Unreadable/unparsable/cross-OS/oversized `.git` pointer found mid-walk
			// → ambiguous, never prune (do NOT climb past an unclassifiable pointer).
			return false
		}
		// The parsed admin path must be a WORKTREE admin path
		// (`<common-git-dir>/worktrees/<name>`), NOT a submodule admin path
		// (`<git-dir>/...modules/<sub>...`). A regular-file `.git` is written by BOTH
		// a linked worktree AND a submodule; only the worktree case is a candidate
		// for the dead-worktree signal. isWorktreeAdminPath requires the admin
		// path's immediate parent basename to be `worktrees` AND no INTERIOR
		// `modules` store segment (see its doc) — accepting normal, bare-repo, and
		// worktree-from-bare admin paths while excluding every submodule store.
		if isWorktreeAdminPath(adminDir) {
			// Genuine worktree admin pointer → this is the classification target.
			// A submodule whose admin leaf is absent (e.g. before
			// `git submodule update --init`) but whose `.git/modules/` parent still
			// exists never reaches here (its admin path is rejected above), so it
			// can never satisfy isAdminDirGenuinelyDeleted's sibling-present branch.
			return isAdminDirGenuinelyDeleted(adminDir)
		}
		// SUBMODULE admin pointer → the genuine OUTER worktree pointer (if any) is an
		// ANCESTOR. Continue the walk from the PARENT of pointerDir so the next walk
		// starts strictly ABOVE the just-rejected pointer and cannot re-find it. A
		// fixpoint at the root stops the loop (belt-and-suspenders: findNearestGitPointer
		// also terminates at the volume root).
		next := filepath.Dir(pointerDir)
		if next == pointerDir {
			return false
		}
		start = next
	}
}

// isWorktreeAdminPath reports whether adminDir is a git WORKTREE admin path —
// the `<common-git-dir>/worktrees/<name>` shape git writes for a linked worktree
// — and NOT a submodule admin path. The two real shapes that share a regular-file
// `.git` pointer are:
//
//   - WORKTREE:  `<common-git-dir>/worktrees/<name>`
//   - SUBMODULE: `<git-dir>/modules/<sub>[/.../worktrees/<name>]`
//
// The `modules` path component is git's OWN internal layout marker for the
// submodule store: git stores a submodule's admin dir at
// `<git-dir>/modules/<sub-path>` (the sub-path may be multi-segment for a nested
// submodule, e.g. `<git-dir>/modules/libs/mysub`), and that `modules` segment
// always appears AFTER the git common-dir segment (a `.git` or `*.git` basename
// — `.git` itself satisfies the `*.git` suffix, so one predicate covers both the
// normal and bare cases). The worktree store, `<git-dir>/worktrees/`, never has
// a `modules` STORE segment in its admin path. So the discriminator is: the
// admin path's immediate PARENT basename is `worktrees` AND, scanning AFTER the
// FIRST git-common-dir segment, no INTERIOR `modules` segment appears. When NO
// git-common-dir segment is found at all (a BARE repo whose common dir name does
// not end in `.git`, e.g. `git init --bare myrepo`), the interior-`modules` scan
// runs from the FRONT instead (Finding 1, this round) so a boundary-less
// submodule store like `<bare>/worktrees/<wt>/modules/deps/worktrees/foo` is
// still rejected — returning true unconditionally there would mis-prune a LIVE
// submodule workspace.
//
// "INTERIOR" is the load-bearing refinement (Finding 3, this round): a `modules`
// segment is git's submodule-store marker ONLY when it has a CHILD
// (`modules/<sub>...`). A worktree literally NAMED `modules` (created by
// `git worktree add ./modules`) stores its admin dir at
// `<repo>/.git/worktrees/modules` — there `modules` is the LEAF directly under
// `worktrees`, a worktree NAME, not a store marker. Treating only interior
// `modules` as the marker accepts that legitimate worktree while still rejecting
// every real submodule store (which always nests a sub-path under `modules`).
//
// Why ANYWHERE-AFTER-THE-FIRST-git-dir and not the earlier
// IMMEDIATELY-AFTER positional check (Finding 3, this round): a submodule INSIDE
// a LINKED WORKTREE stores its admin under
// `<repo>/.git/worktrees/<wt>/modules/<sub>...` — there `modules` is preceded by
// the worktree NAME `<wt>`, NOT a git-common-dir segment, so an
// immediately-after-a-git-dir check MISSED it and a live submodule-in-a-worktree
// (admin leaf absent, parent present) was mis-pruned as a dead worktree.
// Scanning for an interior `modules` ANYWHERE after the first git-common-dir
// segment catches it (and the deeper `.git/worktrees/<wt>/modules/<sub>/worktrees/foo`
// shape too) while preserving every prior accept/reject:
//
//   ACCEPTS (worktree shapes — must fire the dead-worktree signal):
//   - normal `<repo>/.git/worktrees/<name>` (after the first `.git`: only
//     `worktrees/<name>`, no interior modules),
//   - bare-repo `<repo>/main.git/worktrees/<name>` (the common git dir is e.g.
//     `main.git`, not literally `.git`; `*.git` suffix still matches it — the
//     earlier grandparent==`.git` check WRONGLY rejected every bare-repo
//     worktree, Finding 1),
//   - a worktree whose owning repo merely LIVES under a user dir literally named
//     `modules` (`/home/user/modules/proj/.git/worktrees/wt`): that `modules` is
//     BEFORE the first git-common-dir segment, so the after-the-boundary scan
//     never sees it (architect adjudication, prior round),
//   - a worktree literally NAMED `modules` (`<repo>/.git/worktrees/modules`): the
//     `modules` is the LEAF, not interior → accepted (Finding 3 carve-out),
//   - a BARE repo whose common dir name does NOT end in `.git`
//     (`<bare>/worktrees/<wt>`, no git-common-dir segment, no interior modules):
//     the front-scan finds no interior `modules` → accepted (Finding 1, this round).
//
//   REJECTS (submodule shapes — a live submodule must never be pruned):
//   - the ordinary submodule pointer `<repo>/.git/modules/<name>`,
//   - a nested submodule `<repo>/.git/modules/libs/<name>`,
//   - the r6 trap, a submodule under a dir literally named `worktrees`:
//     `<repo>/.git/modules/deps/worktrees/foo` (immediate parent `worktrees`, so
//     a parent-only check would ACCEPT it, but interior `modules` after `.git`
//     rejects it),
//   - the Finding-3 shape, a submodule INSIDE a linked worktree:
//     `<repo>/.git/worktrees/<wt>/modules/<sub>...` and its own worktree
//     `<repo>/.git/worktrees/<wt>/modules/<sub>/worktrees/foo` (interior
//     `modules` after the first `.git` → rejected),
//   - the Finding-1 (this round) BOUNDARY-LESS shape, a submodule under a BARE
//     repo whose common dir name does NOT end in `.git` (no git-common-dir segment
//     at all): `<bare>/worktrees/<wt>/modules/deps/worktrees/foo` (immediate parent
//     `worktrees`, no `.git`/`*.git` segment). The front-scan finds the interior
//     `modules` and rejects it — a parent-only OR a return-true-when-firstGit<0
//     check would have ACCEPTED it and mis-pruned a LIVE submodule workspace.
//
// Anchoring on the FIRST git-common-dir segment is correct WHEN ONE EXISTS: the
// submodule-in-a-worktree admin path carries only the OUTER `.git` (no inner
// literal `.git` segment), and the user-dir-named-`modules` accept case REQUIRES
// the scan window to start past the user-path `modules` — both satisfied by
// "first". When NO git-common-dir segment exists (the boundary-less bare-repo
// case), the scan starts at the FRONT so an interior submodule-store `modules` is
// still caught; a boundary-less NORMAL bare-repo worktree (`<bare>/worktrees/<wt>`)
// has no interior `modules` → accepted.
//
// Any bare/other pointer (parent not `worktrees`) also returns false, so only a
// genuine linked-worktree admin path can reach the dead-worktree availability
// discriminator. adminDir is already filepath.Clean-ed by
// parseGitWorktreePointer, so filepath.Base/filepath.Dir yield each ancestor
// basename with no trailing-separator ambiguity, and the component split below
// sees clean segments.
func isWorktreeAdminPath(adminDir string) bool {
	if adminDir == "" {
		return false
	}
	worktreesDir := filepath.Dir(adminDir) // the `worktrees` dir (admin parent)
	if filepath.Base(worktreesDir) != "worktrees" {
		return false
	}
	// Reject an INTERIOR `modules` segment (one with a child) appearing anywhere
	// AFTER the first git-common-dir segment (`.git` or `*.git`) — that is git's
	// submodule-store marker (`<git-dir>/...modules/<sub>...`). A `modules`
	// segment BEFORE the first git-common-dir (a user dir named `modules` above
	// the git dir) is not git's marker; a LEAF `modules` directly under
	// `worktrees` is a worktree NAME, not a store marker — neither rejects a
	// genuine worktree. adminDir is filepath.Clean-ed, so each separator-bounded
	// segment is a real path component (no empty/`.` segments to skip).
	comps := strings.Split(adminDir, string(filepath.Separator))
	firstGit := -1
	for i, comp := range comps {
		if isGitCommonDirSegment(comp) {
			firstGit = i
			break
		}
	}
	// Finding 1 (this round): when NO git-common-dir boundary is found
	// (firstGit < 0) the interior-`modules` scan must STILL run. A BARE repo whose
	// common dir name does NOT end in `.git` (e.g. `git init --bare myrepo`, or a
	// clone without the suffix) leaves firstGit == -1, yet a submodule store still
	// nests an interior `modules` segment — e.g. a submodule under such a bare
	// worktree, `<bare>/worktrees/<wt>/modules/deps/worktrees/foo`. Returning true
	// unconditionally here would ACCEPT that boundary-less submodule store and
	// mis-prune a LIVE submodule workspace. So scan from the FRONT (start = 0) when
	// there is no boundary; otherwise scan AFTER the first git-common-dir segment
	// (start = firstGit + 1) so a user dir named `modules` ABOVE the git dir is
	// skipped. The LEAF exclusion (j < lastIdx) is preserved in BOTH windows so a
	// worktree literally NAMED `modules` directly under `worktrees` stays accepted.
	scanStart := 0
	if firstGit >= 0 {
		scanStart = firstGit + 1
	}
	lastIdx := len(comps) - 1
	for j := scanStart; j < lastIdx; j++ { // j < lastIdx ⇒ interior only (has a child)
		if comps[j] == "modules" {
			return false
		}
	}
	return true
}

// isGitCommonDirSegment reports whether a cleaned path segment is a git
// common-dir basename: the literal `.git` OR a bare-repo common dir like
// `main.git` / `proj.git`. `.git` itself ends in `.git`, so the suffix test
// covers both; a non-git segment such as `notgit` or `modules` does not.
func isGitCommonDirSegment(seg string) bool {
	return strings.HasSuffix(seg, ".git")
}

// findNearestGitPointer walks UP from dir through its ancestors and returns the
// path of the NEAREST `.git` that is a REGULAR FILE (a linked-worktree /
// submodule pointer), along with the directory that holds it (pointerDir — used
// to resolve a relative `gitdir:` target). The walk stops — with ok=false — the
// moment it finds a `.git` that is a DIRECTORY (a normal repo root: that tree is
// a LIVE repo, NOT a dead worktree), or when it climbs past the volume/filesystem
// root (filepath.Dir stops changing) without finding any `.git`.
//
// Bounding the walk on filepath.Dir's fixpoint (root) is the depth guard the
// design requires: on every OS filepath.Dir("/") == "/" and filepath.Dir(`c:\`)
// == `c:\`, so the loop terminates at the volume root. Each ancestor's `.git` is
// probed with Lstat (so a symlinked `.git` is rejected as not-a-regular-file,
// matching the prior single-dir behavior). A non-ENOENT Lstat error on an
// ancestor's `.git` (permission, transient I/O) is treated as ambiguous and
// stops the walk with ok=false — never climb past an unreadable ancestor and
// risk matching a deeper unrelated pointer.
func findNearestGitPointer(dir string) (gitPath, pointerDir string, ok bool) {
	cur := dir
	for {
		candidate := filepath.Join(cur, ".git")
		info, err := os.Lstat(candidate)
		switch {
		case err == nil && info.Mode().IsRegular():
			// Regular-file `.git` → worktree/submodule pointer. Nearest wins.
			return candidate, cur, true
		case err == nil && info.IsDir():
			// Directory `.git` → normal repo root → LIVE repo, not a dead
			// worktree. Stop the walk (and any deeper ancestor pointer is
			// irrelevant: this repo root is the owning boundary).
			return "", "", false
		case err == nil:
			// `.git` exists but is neither a regular file nor a directory
			// (symlink, device, etc.) → ambiguous → stop, never prune.
			return "", "", false
		case !os.IsNotExist(err):
			// Non-ENOENT Lstat error (permission, transient I/O) → ambiguous →
			// stop the walk; do not climb past an unreadable ancestor.
			return "", "", false
		}
		// ENOENT here → no `.git` at this ancestor → climb one level up.
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the volume/filesystem root without a `.git` → not a worktree.
			return "", "", false
		}
		cur = parent
	}
}

// isAdminDirGenuinelyDeleted reports whether the worktree admin dir at adminDir
// is GENUINELY deleted (a `git worktree remove` outcome) rather than merely
// UNAVAILABLE because its whole admin ROOT is offline/unmounted (Finding 2).
//
// A `git worktree remove` deletes the `worktrees/<name>` subdir. When OTHER
// worktrees remain it leaves the parent `.git/worktrees/` present; when it
// removes the LAST/only worktree, git ALSO deletes the now-empty
// `.git/worktrees/` directory — so the parent alone cannot distinguish
// "last worktree removed" (repo online) from "mount offline" (whole repo gone).
// The GRANDPARENT — the main repo's `.git/` (= filepath.Dir of `.git/worktrees/`)
// — is the discriminator: present means the repo/mount is online and the empty
// `worktrees/` was cleaned, absent means the whole admin root is gone. So,
// walking up the gitdir chain (`.git/worktrees/<name>` → `.git/worktrees/`
// → `.git/`):
//
//   - adminDir Stat succeeds → admin dir present → LIVE worktree → false.
//   - adminDir Stat is a NON-ENOENT error (permission, transient I/O, offline
//     mount) → ambiguous → false (inherit WorkspaceDirDeleted's discipline).
//   - adminDir Stat is ENOENT → the worktree subdir is gone. Refine via the
//     ancestor chain:
//     1. PARENT `.git/worktrees/` EXISTS (Stat ok) → a worktree was removed and
//     siblings remain → DEAD.
//     2. PARENT is ENOENT but GRANDPARENT `.git/` EXISTS (Stat ok) → the LAST
//     worktree was removed and git cleaned the empty `worktrees/`; the
//     repo/mount is present → DEAD (the last-worktree case Finding-2's
//     parent-only check missed).
//     3. PARENT non-ENOENT error, OR grandparent ENOENT / non-ENOENT error, OR
//     a degenerate root with no parent/grandparent to corroborate against →
//     the admin ROOT is gone/unavailable → ambiguous → false.
//
// os.IsNotExist-ONLY at every level: any NON-ENOENT stat error (permission,
// transient I/O, offline mount) is ambiguous and returns false. The grandparent
// `.git/` presence is what separates "last worktree removed" (repo online) from
// "mount offline" (repo absent) WITHOUT weakening the offline-mount protection.
func isAdminDirGenuinelyDeleted(adminDir string) bool {
	if _, err := os.Stat(adminDir); err == nil {
		// Admin dir present → live worktree (or live submodule) → not dead.
		return false
	} else if !os.IsNotExist(err) {
		// Non-ENOENT (permission, transient I/O, offline mount) → ambiguous.
		return false
	}
	// adminDir is ENOENT. Confirm an ancestor is present before declaring DEAD —
	// otherwise the whole admin root may merely be unmounted (Finding 2).
	parent := filepath.Dir(adminDir)
	if parent == adminDir {
		// Degenerate: adminDir is itself a root. No ancestor to corroborate
		// against → treat as ambiguous (never prune).
		return false
	}
	if _, err := os.Stat(parent); err == nil {
		// Parent `.git/worktrees/` present but the `<name>` subdir is ENOENT →
		// a worktree was removed while siblings remain → genuinely DEAD.
		return true
	} else if !os.IsNotExist(err) {
		// Parent inaccessible (permission, transient I/O, offline mount) →
		// ambiguous → never prune.
		return false
	}
	// Parent is ALSO ENOENT. This is EITHER the last/only worktree removed (git
	// cleaned the now-empty `.git/worktrees/`, so the repo's `.git/` survives) OR
	// the whole admin root unmounted (everything gone). The GRANDPARENT `.git/`
	// is the discriminator.
	grandparent := filepath.Dir(parent)
	if grandparent == parent {
		// Degenerate: parent is itself a root. No grandparent to corroborate
		// against → ambiguous (never prune).
		return false
	}
	if _, err := os.Stat(grandparent); err != nil {
		// Grandparent `.git/` absent OR inaccessible (ENOENT, permission, offline
		// mount) → the whole admin root is gone/unavailable → ambiguous.
		return false
	}
	// Grandparent `.git/` present → the repo/mount is online and the empty
	// `.git/worktrees/` was cleaned because the LAST worktree was removed →
	// genuinely a removed worktree → DEAD.
	return true
}

// parseGitWorktreePointer reads the `.git` FILE at gitPath (via the SIZE-CAPPED
// readGitPointerFile — Finding 2, r6: a regular-file Lstat check plus a
// maxGitPointerFileBytes-bounded read, so a hostile/corrupt oversized `.git`
// under the workspace can never trigger an unbounded read) and returns the
// absolute path of the admin directory named by its single `gitdir: <path>`
// line. A relative pointer is resolved against workspaceDir (git writes a
// relative pointer for a worktree created with `--relative-paths`, and an
// absolute one otherwise). It returns ok=false on any read error, a non-regular
// or oversized `.git` file, a missing `gitdir:` line, an empty target, a
// FOREIGN-ABSOLUTE target (Finding 3, r2), OR a FOREIGN-RELATIVE target (a
// Windows relative gitdir seen on POSIX — Finding 3, r5) — every such case makes
// the caller treat the path as NOT a dead worktree (ambiguous → safe).
//
// FOREIGN-ABSOLUTE (cross-OS) gitdir handling: a worktree created by
// Git-for-Windows writes `gitdir: C:/...` (or a `\\srv\share` UNC); a worktree
// created on POSIX writes `gitdir: /...`. filepath.IsAbs is OS-SPECIFIC — on
// POSIX it returns FALSE for `C:/...` and `\\...`, on Windows it returns FALSE
// for `/...` — so a naive `if !IsAbs { join under workspace }` would JOIN a
// foreign-absolute pointer UNDER the workspace as if relative, fabricate a path
// that does not exist, stat ENOENT, and prune a LIVE workspace. Translating a
// path across OSes is fragile, so a foreign-absolute target is rejected as
// AMBIGUOUS (ok=false): the worktree was created on the other OS and we cannot
// reliably resolve its admin dir → never prune. A NATIVE absolute path (IsAbs
// true) and a NATIVE relative path (joined against workspaceDir) keep working.
//
// FOREIGN-RELATIVE (cross-OS) gitdir handling (Finding 3, r5): the foreign reject
// above only catches ABSOLUTE cross-OS pointers. A Windows worktree created with
// `--relative-paths` writes a backslash RELATIVE gitdir like
// `..\main\.git\worktrees\live` — not absolute, so it slips past the
// foreign-absolute reject. On POSIX a backslash is an ordinary filename byte, so
// joining it under the workspace fabricates a single one-backslash filename whose
// Stat is ENOENT and whose parent is the LIVE workspace → false-positive prune of
// a cross-OS worktree. isForeignRelativePath rejects such a target (ambiguous).
// On Windows both `\` and `/` are native relative separators, so nothing relative
// is foreign there.
func parseGitWorktreePointer(gitPath, workspaceDir string) (adminDir string, ok bool) {
	data, ok := readGitPointerFile(gitPath)
	if !ok {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, gitWorktreePointerPrefix) {
			continue
		}
		target := strings.TrimSpace(strings.TrimPrefix(line, gitWorktreePointerPrefix))
		if target == "" {
			return "", false
		}
		if filepath.IsAbs(target) {
			// Native absolute (drive-letter/UNC on Windows, `/...` on POSIX).
			return filepath.Clean(target), true
		}
		if isForeignAbsolutePath(target) {
			// A path the CURRENT OS does not consider absolute but the OTHER OS
			// would (Windows drive-letter/UNC seen on POSIX, or a POSIX `/...`
			// seen on Windows). Cross-OS worktree → cannot resolve safely →
			// ambiguous, never prune.
			return "", false
		}
		if isForeignRelativePath(target) {
			// A RELATIVE target authored on the OTHER OS (Finding 3): a Windows
			// relative gitdir like `..\main\.git\worktrees\live` is not absolute,
			// so it skips the foreign-absolute reject above, yet on POSIX its
			// backslashes are ordinary filename bytes — joining it under the
			// workspace yields a single synthetic one-backslash filename whose
			// Stat is ENOENT and whose filepath.Dir is the (existing) workspace,
			// fabricating a "dead admin dir" against a LIVE cross-OS worktree.
			// Reject it as ambiguous → never prune.
			return "", false
		}
		// Genuinely native-relative → resolve against the pointer's dir.
		return filepath.Clean(filepath.Join(workspaceDir, target)), true
	}
	return "", false
}

// readGitPointerFile reads a `.git` pointer FILE with a SIZE CAP (Finding 2, r6).
// It returns the file's bytes and ok=true only when the file is a regular file
// whose size is at or under maxGitPointerFileBytes; otherwise it returns
// ok=false so the caller treats the path as AMBIGUOUS and never prunes. The
// bound matters because the `.git` file lives under the (possibly
// attacker-controlled) workspace tree and is read on EVERY GUI sweep and CLI
// prune/dry-run tick; an unbounded os.ReadFile of a hostile multi-GB file would
// cause repeated memory spikes / stalls.
//
// The Lstat+IsRegular pre-check is preserved (a symlinked / FIFO / device `.git`
// is refused, matching findNearestGitPointer's regular-file requirement). The
// AUTHORITATIVE bound is the io.LimitReader(maxGitPointerFileBytes+1) read plus
// the len-check below: it reads one byte past the cap and rejects a result that
// exceeded it, so it is robust even if the file grew between the Lstat and the
// open. A genuine `gitdir: <path>\n` pointer is a few hundred bytes and never
// approaches the cap, so the cap can only ever reject a malformed/oversized
// file, never a real worktree/submodule pointer.
func readGitPointerFile(gitPath string) ([]byte, bool) {
	fi, err := os.Lstat(gitPath)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	// Cheap fast-path: reject an obviously-oversized file before opening. The
	// authoritative bound is the LimitReader+len check below.
	if fi.Size() > maxGitPointerFileBytes {
		return nil, false
	}
	f, err := os.Open(gitPath)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	// Re-verify regular-file on the OPEN handle (close the Lstat→Open swap window)
	// before the bounded read.
	if openedFI, serr := f.Stat(); serr != nil || !openedFI.Mode().IsRegular() {
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxGitPointerFileBytes+1))
	if err != nil {
		return nil, false
	}
	if int64(len(data)) > maxGitPointerFileBytes {
		// Oversized → ambiguous → never prune.
		return nil, false
	}
	return data, true
}

// isForeignAbsolutePath reports whether target is absolute on the OTHER OS but
// not the current one — i.e. filepath.IsAbs(target) already returned false on
// THIS GOOS, yet the path is clearly an absolute path authored on the opposite
// platform. Callers use this to REJECT (treat as ambiguous), never to resolve.
//
//   - On non-Windows (POSIX/WSL): a Windows drive-letter root (`C:\` or `C:/`)
//     or a UNC root (`\\server\share`).
//   - On Windows: a POSIX-absolute root (`/foo`). Note `\foo` is intentionally
//     NOT treated as foreign on Windows — it is a native (drive-relative) path
//     that Windows IsAbs already classifies as relative, and git never emits it
//     as a gitdir.
func isForeignAbsolutePath(target string) bool {
	if target == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		// POSIX-absolute `/...` seen on Windows (but not `\...`, which is a
		// native Windows drive-relative path, nor `\\...` UNC which Windows
		// IsAbs already accepts as absolute).
		return target[0] == '/'
	}
	// Non-Windows: Windows drive-letter root `^[A-Za-z]:[\\/]`.
	if len(target) >= 3 &&
		((target[0] >= 'A' && target[0] <= 'Z') || (target[0] >= 'a' && target[0] <= 'z')) &&
		target[1] == ':' &&
		(target[2] == '\\' || target[2] == '/') {
		return true
	}
	// Non-Windows: UNC root `^\\` (two leading backslashes).
	if len(target) >= 2 && target[0] == '\\' && target[1] == '\\' {
		return true
	}
	return false
}

// isForeignRelativePath reports whether a RELATIVE target (filepath.IsAbs and
// isForeignAbsolutePath both already returned false) was authored on the OTHER
// OS and so must NOT be relative-joined under the workspace (Finding 3).
// Callers use this to REJECT (treat as ambiguous), never to resolve.
//
//   - On non-Windows (POSIX/WSL): a target containing a BACKSLASH is a Windows
//     relative path (`..\main\.git\worktrees\live`). On POSIX a backslash is an
//     ordinary filename byte, so joining it would fabricate a single
//     one-backslash filename → synthetic ENOENT → false-positive prune. Reject.
//   - On Windows: a relative target is native — a backslash is the native
//     separator and a forward slash is ALSO a valid Windows separator — so
//     nothing relative is "foreign" there. Return false (resolve normally).
//
// A target with NEITHER separator (a bare `name`) is never foreign on either OS.
func isForeignRelativePath(target string) bool {
	if runtime.GOOS == "windows" {
		// On Windows both `\` and `/` are native relative separators.
		return false
	}
	// Non-Windows: a backslash means a Windows relative path → foreign.
	return strings.ContainsRune(target, '\\')
}

// WorkspaceOrphanReason names WHY a registered workspace classifies as an
// orphan eligible for auto-prune. The four values mirror the four SHIPPED
// detection signals; the type makes the reason a typed value the sweeper logs
// and the `mcphub workspace prune` command surfaces, instead of a bare string
// duplicated across call sites.
type WorkspaceOrphanReason string

const (
	// OrphanReasonAgentWorktree — the workspace lives inside an ephemeral
	// `.claude/worktrees/agent-*` worktree (IsAgentWorktreePath). Highest
	// priority: ephemeral by design, pruned with no grace window.
	OrphanReasonAgentWorktree WorkspaceOrphanReason = "agent-worktree"
	// OrphanReasonDeletedDir — the workspace directory is DEFINITIVELY gone
	// (WorkspaceDirDeleted, os.IsNotExist-only). ClassifyWorkspaceOrphan returns
	// this on a single ENOENT observation; the GUI sweeper still layers its own
	// 2-consecutive-tick grace on top before acting (the grace is stateful
	// per-sweeper-instance and stays in the sweeper, NOT here).
	OrphanReasonDeletedDir WorkspaceOrphanReason = "deleted-dir"
	// OrphanReasonDeadWorktree — the workspace directory STILL EXISTS but it is a
	// leftover git LINKED WORKTREE whose admin directory has been deleted
	// (IsDeadGitWorktreePath, os.IsNotExist-only on the admin dir). This is the
	// REAL incident signal: such a dir slips through BOTH agent-worktree (path
	// not under `.claude/worktrees/agent-`) and deleted-dir (dir still present).
	// Gated by opts.PruneDeadWorktrees. Like deleted-dir, the GUI sweeper layers
	// its 2-consecutive-tick ENOENT grace on top before acting.
	OrphanReasonDeadWorktree WorkspaceOrphanReason = "dead-worktree"
	// OrphanReasonIdle — the workspace exists and is not an agent worktree, but
	// its most-recent activity (LastToolsCallAt) is older than the idle
	// threshold. Only fires when opts.IdleThreshold > 0 AND LastToolsCallAt is
	// non-zero (a zero timestamp = no activity signal → never idle-pruned).
	OrphanReasonIdle WorkspaceOrphanReason = "idle"
)

// ClassifyOpts carries the per-workspace inputs ClassifyWorkspaceOrphan needs
// beyond the path itself: the idle threshold for THIS run, the workspace's
// most-recent activity timestamp, and the wall-clock "now" the idle comparison
// is anchored against.
type ClassifyOpts struct {
	// PruneDeadWorktrees gates the dead-git-worktree structural signal
	// (IsDeadGitWorktreePath). The caller reads daemons.prune_dead_worktrees once
	// per tick and threads the resolved bool here (mirroring how IdleThreshold is
	// resolved from daemons.prune_idle_hours). When false the dead-worktree check
	// is skipped entirely.
	PruneDeadWorktrees bool
	// IdleThreshold enables the idle signal when > 0. Zero (or negative)
	// disables idle classification — only the two structural signals run.
	IdleThreshold time.Duration
	// LastToolsCallAt is the most-recent activity timestamp across the
	// workspace's rows. A zero value means "no activity signal observed" and
	// NEVER idle-prunes (require structural evidence, not wall-clock-since-
	// register).
	LastToolsCallAt time.Time
	// Now anchors the idle comparison. The caller supplies it (the sweeper's
	// tick time, or time.Now() for the CLI) so classification stays
	// deterministic and testable. A zero Now disables the idle signal (no
	// meaningful comparison is possible).
	Now time.Time
}

// ClassifyWorkspaceOrphan is the SINGLE owner of the orphan-classification
// predicate for the workspace-daemon auto-prune feature. It evaluates the four
// SHIPPED detection signals against ONE workspace path in a fixed priority
// order and returns the matching reason plus true, or ("", false) when the
// workspace is healthy.
//
// Priority (highest first — identical to the order the GUI sweeper's inlined
// switch used before this extraction):
//  1. agent-worktree (IsAgentWorktreePath) — ephemeral by design.
//  2. deleted-dir (WorkspaceDirDeleted) — directory definitively gone. This is
//     the SINGLE-OBSERVATION verdict; the GUI sweeper still applies its
//     2-consecutive-ENOENT-tick grace before acting (the grace is stateful
//     per-sweeper-instance and is NOT this pure predicate's concern).
//  3. dead-worktree (opts.PruneDeadWorktrees && IsDeadGitWorktreePath) — the
//     directory STILL EXISTS but it is a leftover git linked worktree whose
//     admin dir is gone. Ranked AFTER deleted-dir so a directory that is BOTH a
//     (former) worktree AND now deleted classifies as deleted-dir (the
//     WorkspaceDirDeleted ENOENT fires first; IsDeadGitWorktreePath requires the
//     dir to still exist, so the two are mutually exclusive by construction —
//     the ordering is belt-and-suspenders). The GUI sweeper shares its
//     deleted-dir 2-tick ENOENT grace with this verdict.
//  4. idle (opts.IdleThreshold > 0 && LastToolsCallAt non-zero &&
//     Now.Sub(LastToolsCallAt) > IdleThreshold) — present, non-worktree, but
//     stale.
//
// path is the CANONICAL workspace path (the registry stores the
// EvalSymlinks-resolved, drive-lowercased form), the same form every structural
// predicate already relies on. Pure: no fleet, no IPC, no registry — it only
// stats the path (via the existing predicates) and compares timestamps, so it
// is unit-testable with table cases.
func ClassifyWorkspaceOrphan(path string, opts ClassifyOpts) (WorkspaceOrphanReason, bool) {
	switch {
	case IsAgentWorktreePath(path):
		return OrphanReasonAgentWorktree, true
	case WorkspaceDirDeleted(path):
		return OrphanReasonDeletedDir, true
	case opts.PruneDeadWorktrees && IsDeadGitWorktreePath(path):
		return OrphanReasonDeadWorktree, true
	case opts.IdleThreshold > 0 &&
		!opts.LastToolsCallAt.IsZero() &&
		!opts.Now.IsZero() &&
		opts.Now.Sub(opts.LastToolsCallAt) > opts.IdleThreshold:
		return OrphanReasonIdle, true
	default:
		return "", false
	}
}
