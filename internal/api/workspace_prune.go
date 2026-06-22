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
// SAFETY POSTURE — THIS PREDICATE GATES A DEFAULT-ON DESTRUCTIVE AUTO-PRUNE.
// NEVER prune a LIVE workspace: a FALSE-POSITIVE here is unacceptable (it tears
// down a live registration of a real working directory). Missing a genuinely
// dead worktree in an EXOTIC git layout (a FALSE-NEGATIVE) is ACCEPTABLE and
// documented — the orphan row just lingers (benign). Therefore every classifier
// in this file resolves an AMBIGUOUS path to false (do not prune): any non-ENOENT
// stat error, any cross-OS / unparsable / oversized pointer, and any nearest `.git`
// pointer that is not DIRECTLY a clean worktree admin all return false. When in
// doubt, do NOT prune.
//
// NEAREST-POINTER-ONLY (no climbing past it). The classification keys on the
// workspace's NEAREST regular-file `.git` pointer and NOTHING above it. An earlier
// design (r8-r11) added a WALK-CONTINUE that, on a pointer path-classified as a
// submodule store, CLIMBED PAST it to find an OUTER dead worktree (the exotic
// "registered workspace is a submodule INSIDE a removed linked worktree" case).
// That was the ROOT CAUSE of a recurring false-POSITIVE class and has been REMOVED:
// git stores are NOT reliably distinguishable from user dirs by PATH ALONE, so a
// `git init --separate-git-dir` repo or a coincidentally-named `X.git/modules/Y`
// user dir could be mis-classified as a submodule store, the walk would climb PAST
// that LIVE nested repo's own `.git`, match a dead OUTER-worktree ancestor, and
// PRUNE the live nested repo. The false-positive tail (r9 separate-git-dir, r10
// user `worktrees`/`modules` dirs, r11 `X.git/modules/...`) is endless under
// path-alone classification, so the only sound resolution under the safety posture
// is to STOP climbing. The exotic submodule-in-removed-worktree case is now a
// DOCUMENTED FAIL-SAFE FALSE-NEGATIVE (benign lingering orphan row), the same
// posture already accepted for the submodule's-own-worktree case.
//
// It returns true ONLY when ALL of the following hold — conservative by
// construction so a live repo, a live worktree, a live submodule, an unmounted
// admin root, a cross-OS pointer, or any ambiguous-error stat can NEVER be
// misclassified:
//
//  1. the workspace directory STILL EXISTS (a definitive ENOENT on the dir is
//     the deleted-dir case, owned by WorkspaceDirDeleted — NOT here);
//  2. walking UP from the workspace dir through its ancestors, the NEAREST `.git`
//     found is a REGULAR FILE (a worktree/submodule pointer). The walk handles a
//     SUBDIR workspace inside a linked worktree (a monorepo package, or an
//     LSP/.serena marker root) whose `.git` pointer lives at the worktree ROOT (an
//     ancestor), not the subdir itself — that ANCESTOR walk lives inside
//     findNearestGitPointer and is NOT the removed walk-continue. If a `.git` is a
//     DIRECTORY it is a normal repo root → LIVE → false (stop). A non-git tree with
//     no `.git` anywhere up to the volume root → false. The NEAREST pointer is
//     DECISIVE: it is classified once and the result is final — there is no climbing
//     past it;
//  3. the pointer's `gitdir: <path>` (relative paths resolved against the
//     worktree-root dir that holds the pointer) names DIRECTLY a WORKTREE admin
//     directory of the canonical `<common-git-dir>/worktrees/<name>` shape — its
//     immediate parent dir is `worktrees` AND a real git-common-dir segment
//     (`.git`/`*.git`) sits STRICTLY ABOVE that `worktrees` parent AND no INTERIOR
//     `modules` store segment appears after the first git-common-dir segment (so a
//     `<repo>/.git/modules/<name>` submodule path, a submodule under a
//     worktrees-named dir `<repo>/.git/modules/.../worktrees/foo`, a submodule
//     INSIDE a worktree `<repo>/.git/worktrees/<wt>/modules/<sub>...`, a
//     separate-git-dir / arbitrary gitfile, AND a BOUNDARY-LESS `worktrees/<name>`
//     with NO `.git`/`*.git` ancestor — a `git init --separate-git-dir` admin dir
//     under a user folder named `worktrees`, `/worktrees/gone` — are ALL rejected
//     here, see isWorktreeAdminPath) that is ABSENT via
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
// gone), so the discriminator returns false; (c) condition 3
// (isWorktreeAdminPath) requires the NEAREST pointer's admin path to be DIRECTLY a
// WORKTREE admin path (immediate parent dir `worktrees`, a `.git`/`*.git` boundary
// STRICTLY ABOVE it, AND no INTERIOR `modules` store segment) — a `modules/<name>`
// submodule path fails the parent check, a submodule under a worktrees-named dir
// (`.git/modules/.../worktrees/foo`) carries an interior `modules`, a submodule
// INSIDE a worktree (`.git/worktrees/<wt>/modules/<sub>...`) also carries an
// interior `modules` — all rejected as worktree admin paths, so the predicate STOPS
// at the nearest pointer and returns false. The nearest pointer being a submodule
// store is a TERMINAL reject now (the walk no longer climbs past it). So an ONLINE
// submodule whose admin LEAF is merely absent (parent dir still present, e.g.
// before `git submodule update --init`) is never misread as a removed worktree, and
// a submodule registered INSIDE a removed linked worktree is a documented benign
// false-NEGATIVE (the orphan row lingers) rather than a climb-and-prune risk. Layer
// (c) is the worktree-admin-path requirement; layers (a)/(b) are the offline guard.
//
// BOUNDARY-LESS bare-repo accepted false-NEGATIVE: a submodule store (or worktree)
// under a SUFFIX-LESS bare superproject (`<bare>/worktrees/<wt>[/modules/...]`, no
// `.git`/`*.git` segment anywhere) is NOT classified as a worktree admin — with no
// real boundary it is path-indistinguishable from a coincidentally-named user
// `worktrees`/`modules` dir, so isWorktreeAdminPath returns false there and the
// predicate returns false. A dead worktree reachable only via such a boundary-less
// path lingers as a benign orphan row — the documented SAFE-direction tradeoff under
// NEVER-prune-a-live-workspace.
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

	// Condition 2+3: walk UP from the workspace dir to the NEAREST regular-file
	// `.git` pointer. A subdir workspace inside a linked worktree keeps its `.git`
	// pointer at the worktree ROOT (an ancestor), so probing only `<dir>/.git` would
	// miss it (Finding 1) — findNearestGitPointer handles that ancestor walk
	// internally and STOPS at the first normal-repo `.git` DIRECTORY (a live repo
	// boundary), an ambiguous Lstat, or the volume root.
	//
	// THE NEAREST POINTER IS DECISIVE — no climbing past it. The earlier r8-r11
	// walk-continue (re-invoke findNearestGitPointer from above a pointer
	// path-classified as a submodule store, to reach an OUTER dead worktree for the
	// exotic "registered workspace is a submodule INSIDE a removed linked worktree"
	// case) was REMOVED as the ROOT CAUSE of a recurring false-POSITIVE class. Git
	// stores are NOT reliably distinguishable from user dirs by PATH ALONE: a
	// `git init --separate-git-dir` repo, or a coincidentally-named `X.git/modules/Y`
	// user dir, can be path-classified as a submodule store; the walk would then
	// climb PAST that LIVE nested repo's own `.git`, match a dead OUTER-worktree
	// ancestor, and PRUNE the live nested repo (r9 separate-git-dir, r10 user
	// `worktrees`/`modules` dirs, r11 `X.git/modules/...` — an endless false-positive
	// tail). Under the SAFETY POSTURE (NEVER prune a LIVE workspace) the only sound
	// resolution is to STOP climbing: when the nearest `.git` is anything other than
	// a clean worktree admin pointer, return false.
	//
	// The exotic submodule-in-removed-worktree case is now a DOCUMENTED FAIL-SAFE
	// FALSE-NEGATIVE (the orphan row lingers benignly), the same posture already
	// accepted for the submodule's-own-worktree case. The COMMON case (the
	// workspace's own `.git/worktrees/<name>` admin is gone, reached with no
	// climbing) is unaffected.
	gitPath, pointerDir, ok := findNearestGitPointer(dir)
	if !ok {
		// No regular-file `.git` pointer before a normal-repo `.git` DIRECTORY, an
		// ambiguous Lstat, or the volume root → not a (dead) linked worktree.
		return false
	}
	// Parse the `gitdir:` pointer (relative paths resolved against the dir that HOLDS
	// the pointer — the worktree/submodule root — not the subdir workspace).
	adminDir, ok := parseGitWorktreePointer(gitPath, pointerDir)
	if !ok {
		// Unreadable/unparsable/cross-OS/oversized `.git` pointer → ambiguous, never
		// prune.
		return false
	}
	// The parsed admin path must be DIRECTLY a WORKTREE admin path
	// (`<common-git-dir>/worktrees/<name>`), NOT a submodule admin path
	// (`<git-dir>/...modules/<sub>...`), a separate-git-dir, or any other shape. A
	// regular-file `.git` is written by BOTH a linked worktree AND a submodule (and
	// a `git init --separate-git-dir`); only a clean worktree admin pointer is a
	// candidate for the dead-worktree signal. isWorktreeAdminPath requires the admin
	// path's immediate parent basename to be `worktrees`, a `.git`/`*.git` boundary
	// STRICTLY ABOVE it, AND no INTERIOR `modules` store segment (see its doc). Any
	// non-worktree-admin nearest pointer (submodule, separate-git-dir, boundary-less,
	// ambiguous) → STOP, return false (never prune a live/ambiguous boundary).
	if !isWorktreeAdminPath(adminDir) {
		return false
	}
	// Genuine worktree admin pointer → this is the classification target. A submodule
	// whose admin leaf is absent (e.g. before `git submodule update --init`) but
	// whose `.git/modules/` parent still exists never reaches here (its admin path is
	// rejected above), so it can never satisfy isAdminDirGenuinelyDeleted's
	// sibling-present branch.
	return isAdminDirGenuinelyDeleted(adminDir)
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
// normal and bare-with-`.git`-suffix cases). The worktree store,
// `<git-dir>/worktrees/`, never has a `modules` STORE segment in its admin path.
// So the discriminator is THREE conjuncts: (1) the admin path's immediate PARENT
// basename is `worktrees`, (2) a real git-common-dir segment (`.git`/`*.git`)
// sits STRICTLY ABOVE that `worktrees` parent, AND (3) scanning AFTER the FIRST
// git-common-dir segment, no INTERIOR `modules` segment appears.
//
// REQUIRE-A-BOUNDARY (this round — Findings 1+2 convergent root fix): conjunct (2)
// is the load-bearing addition. A `worktrees`/`modules` component is git's store
// marker ONLY when a real `.git`/`*.git` segment is its ANCESTOR. A path with NO
// git-common-dir segment (a `git init --separate-git-dir` admin dir under a user
// folder literally named `worktrees`, `/worktrees/gone`; or a SUFFIX-LESS bare
// repo, `git init --bare myrepo`) is path-indistinguishable from a coincidentally-
// named user dir, so it is REJECTED (returns false → not pruned). This REVERTS the
// prior r9 "accept boundary-less bare-repo worktrees" addition, which was the
// unsafe path: it mis-pruned a LIVE `/worktrees/gone` separate-git-dir repo
// (Finding 2). The cost is an accepted false-NEGATIVE — see the BOUNDARY-LESS note
// at the bottom of this comment.
//
// "INTERIOR" is the load-bearing refinement for conjunct (3): a `modules`
// segment is git's submodule-store marker ONLY when it has a CHILD
// (`modules/<sub>...`). A worktree literally NAMED `modules` (created by
// `git worktree add ./modules`) stores its admin dir at
// `<repo>/.git/worktrees/modules` — there `modules` is the LEAF directly under
// `worktrees`, a worktree NAME, not a store marker. Treating only interior
// `modules` as the marker accepts that legitimate worktree while still rejecting
// every real submodule store (which always nests a sub-path under `modules`).
//
// Scanning for an interior `modules` ANYWHERE after the first git-common-dir
// segment (not the earlier IMMEDIATELY-AFTER positional check) catches a submodule
// INSIDE a LINKED WORKTREE (`<repo>/.git/worktrees/<wt>/modules/<sub>...`, where
// `modules` is preceded by the worktree NAME `<wt>`, NOT a git-common-dir segment)
// and the deeper `.git/worktrees/<wt>/modules/<sub>/worktrees/foo` shape too, while
// preserving every prior accept/reject:
//
//   ACCEPTS (worktree shapes — must fire the dead-worktree signal):
//   - normal `<repo>/.git/worktrees/<name>` (`.git` boundary above `worktrees`;
//     after the first `.git`: only `worktrees/<name>`, no interior modules),
//   - bare-repo-WITH-`.git`-suffix `<repo>/main.git/worktrees/<name>` (the common
//     git dir is e.g. `main.git`, not literally `.git`; the `*.git` suffix matches
//     it — a real boundary above `worktrees`),
//   - a worktree whose owning repo merely LIVES under a user dir literally named
//     `modules` (`/home/user/modules/proj/.git/worktrees/wt`): that `modules` is
//     BEFORE the first git-common-dir segment, so the after-the-boundary scan
//     never sees it (architect adjudication, prior round),
//   - a worktree literally NAMED `modules` (`<repo>/.git/worktrees/modules`): the
//     `modules` is the LEAF, not interior → accepted (the carve-out).
//
//   REJECTS (a live submodule / live separate-git-dir repo must never be pruned):
//   - the ordinary submodule pointer `<repo>/.git/modules/<name>`,
//   - a nested submodule `<repo>/.git/modules/libs/<name>`,
//   - the r6 trap, a submodule under a dir literally named `worktrees`:
//     `<repo>/.git/modules/deps/worktrees/foo` (immediate parent `worktrees`, so
//     a parent-only check would ACCEPT it, but interior `modules` after `.git`
//     rejects it),
//   - a submodule INSIDE a linked worktree:
//     `<repo>/.git/worktrees/<wt>/modules/<sub>...` and its own worktree
//     `<repo>/.git/worktrees/<wt>/modules/<sub>/worktrees/foo` (interior
//     `modules` after the first `.git` → rejected),
//   - the Finding-2 (this round) BOUNDARY-LESS shape, a `worktrees`/`modules`
//     component with NO `.git`/`*.git` ancestor: a `git init --separate-git-dir`
//     admin dir under a user folder named `worktrees` (`/worktrees/gone`) OR a
//     submodule/worktree under a SUFFIX-LESS bare repo
//     (`<bare>/worktrees/<wt>[/modules/...]`). Conjunct (2) rejects it — the prior
//     r9 boundary-less ACCEPT mis-pruned a LIVE separate-git-dir workspace,
//   - a submodule's OWN linked worktree `<repo>/.git/modules/<sub>/worktrees/<name>`
//     (verified real-git: `gitdir: <super>/.git/modules/sub/worktrees/sub-wt`).
//     Immediate parent is `worktrees` (parent gate passes) but an interior `modules`
//     after the first `.git` → REJECTED. This is a DELIBERATE, ACCEPTED
//     false-NEGATIVE: a dead worktree-of-a-submodule lingers as a benign orphan row
//     rather than risk a false-positive, because it is NOT path-distinguishable from
//     a LIVE submodule CHECKED OUT at a dir literally named `worktrees`
//     (`<repo>/.git/modules/worktrees/<leaf>`, verified real-git
//     `gitdir: ../.git/modules/worktrees`) without filesystem probing — `worktrees`
//     is simultaneously git's store-dir name, a legal submodule checkout path, and a
//     legal worktree name. Ambiguous → do not prune.
//
// BOUNDARY-LESS accepted false-NEGATIVE: a SUFFIX-LESS bare-repo worktree
// (`<bare>/worktrees/<wt>`, no `.git`/`*.git` segment) is no longer accepted — a
// dead one lingers as a benign orphan row. It is genuinely path-indistinguishable
// from a user dir named `worktrees` without a filesystem probe (which this pure
// predicate deliberately does not do), so under NEVER-prune-a-live-workspace the
// only safe answer is to reject the ambiguous shape. A bare repo whose common dir
// name DOES end in `.git` (`<repo>.git/worktrees/<name>`) keeps the `.git`-suffix
// boundary and is still classified, so the false-negative is scoped to the
// suffix-less bare case only.
//
// Anchoring on the FIRST git-common-dir segment is correct: the
// submodule-in-a-worktree admin path carries only the OUTER `.git` (no inner
// literal `.git` segment), and the user-dir-named-`modules` accept case REQUIRES
// the scan window to start past the user-path `modules` — both satisfied by
// "first". When NO git-common-dir segment exists, conjunct (2) rejects the path
// outright (the boundary-less false-NEGATIVE above), so the interior-`modules`
// scan never runs there.
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
	// REQUIRE A BOUNDARY (this round — Findings 1+2 convergent root fix): a real
	// git-common-dir segment (`.git`/`*.git`) MUST sit STRICTLY ABOVE the
	// `worktrees` parent dir. Without it a directory literally named `worktrees`
	// (e.g. a `git init --separate-git-dir` admin dir under a user folder named
	// `worktrees`, `/worktrees/gone`) is path-indistinguishable from a real
	// linked-worktree store and would be mis-pruned as a removed worktree
	// (Finding 2). adminDir = `<...>/worktrees/<name>`, so the `worktrees` parent
	// is the SECOND-TO-LAST component; the boundary index must be < that.
	//
	// This REVERTS the prior r9 acceptance of a BOUNDARY-LESS bare-repo worktree
	// (`<bare-without-.git-suffix>/worktrees/<name>`, no git-common-dir segment):
	// such a path now returns false (NOT a worktree admin path → not pruned). That
	// is an accepted false-NEGATIVE — a boundary-less bare-repo worktree is
	// path-indistinguishable from a user dir named `worktrees`, so under the
	// NEVER-prune-a-live-workspace posture the only safe answer is to reject the
	// ambiguous shape and let the dead orphan row linger benignly. A bare repo
	// whose common dir name DOES end in `.git` (`<repo>.git/worktrees/<name>`) still
	// has the `.git`-suffix boundary and is still classified.
	comps := strings.Split(adminDir, string(filepath.Separator))
	worktreesIdx := len(comps) - 2 // the `worktrees` parent (admin = .../worktrees/<name>)
	firstGit := firstGitCommonDirIndex(comps)
	if firstGit < 0 || firstGit >= worktreesIdx {
		// No git-common-dir boundary, or the only one is AT/BELOW the `worktrees`
		// segment (never a real store layout) → not a git worktree admin path.
		return false
	}
	// Reject an INTERIOR `modules` store segment — git's submodule-store marker
	// (`<git-dir>/...modules/<sub>...`). hasInteriorModulesStoreSegment is the
	// single owner of that detection. A genuine worktree admin path (normal,
	// bare-repo-with-`.git`-suffix, worktree-named-`modules`,
	// worktree-under-a-user-dir-named-`modules`) carries NO interior modules store
	// segment → accepted; every submodule store carries one → rejected.
	return !hasInteriorModulesStoreSegment(adminDir)
}

// hasInteriorModulesStoreSegment reports whether a filepath.Clean-ed git admin
// path carries an INTERIOR `modules` segment (a `modules` component with a child)
// that sits STRICTLY AFTER a real git-common-dir segment (`.git` or `*.git`).
// That interior `modules` is git's submodule-store marker
// (`<git-dir>/...modules/<sub>...`). It is the SINGLE OWNER of the
// interior-modules-store detection, consumed by isWorktreeAdminPath to REJECT a
// submodule store (a regular-file `.git` is written by both a linked worktree and a
// submodule; only the worktree case is a dead-worktree candidate).
//
// REQUIRE-A-BOUNDARY: a `modules` segment is git's submodule-store marker ONLY when
// a real git-common-dir (`.git`/`*.git`) segment is its ANCESTOR. When the path has
// NO git-common-dir segment at all (firstGit < 0) it returns FALSE — a directory
// literally named `modules` with no `.git`/`*.git` ancestor (e.g. a
// `git init --separate-git-dir` admin dir under a user dir named `modules`,
// `/mnt/modules/nested-gitdir`) is NOT a submodule store and must NOT be treated as
// one. (Either way such a boundary-less path is not a worktree admin path, so
// isWorktreeAdminPath rejects it and IsDeadGitWorktreePath returns false; this
// boundary requirement keeps the interior-`modules` detection itself honest.)
//
// Boundaries (each verified by a TestIsWorktreeAdminPath case):
//   - A `modules` segment BEFORE the first git-common-dir (a USER dir literally
//     named `modules` ABOVE the git dir, `/home/user/modules/proj/.git/...`) is
//     NOT git's marker → not counted. Anchoring the scan strictly after the first
//     git-common-dir segment skips it.
//   - A LEAF `modules` (one with NO child, e.g. a worktree literally NAMED
//     `modules` directly under `worktrees`: `<repo>/.git/worktrees/modules`) is a
//     worktree NAME, not a store marker → not counted (the `j < lastIdx`
//     interior-only bound).
//   - NO git-common-dir segment (firstGit < 0, the boundary-less bare-repo case,
//     `git init --bare myrepo`) → FALSE: with no `.git`/`*.git` ancestor a
//     `modules`/`worktrees` component is path-indistinguishable from a user dir of
//     the same name, so the conservative answer is "not a git store".
//
// adminDir is filepath.Clean-ed by parseGitWorktreePointer, so each
// separator-bounded segment is a real path component (no empty/`.` segments).
func hasInteriorModulesStoreSegment(adminDir string) bool {
	comps := strings.Split(adminDir, string(filepath.Separator))
	firstGit := firstGitCommonDirIndex(comps)
	if firstGit < 0 {
		// No real git-common-dir boundary → a `modules` here is a coincidental user
		// dir name, NOT git's submodule store. Conservative: not a submodule store.
		return false
	}
	lastIdx := len(comps) - 1
	for j := firstGit + 1; j < lastIdx; j++ { // j > firstGit ⇒ strictly below the boundary; j < lastIdx ⇒ interior (has a child)
		if comps[j] == "modules" {
			return true
		}
	}
	return false
}

// firstGitCommonDirIndex returns the index of the FIRST git-common-dir segment
// (`.git` or `*.git`) in the cleaned path components, or -1 when none is present.
// It is the SINGLE OWNER of "where is the git-common-dir boundary" so
// isWorktreeAdminPath (require a boundary strictly above `worktrees`) and
// hasInteriorModulesStoreSegment (require a boundary strictly above the interior
// `modules`) cannot drift on the boundary definition.
func firstGitCommonDirIndex(comps []string) int {
	for i, comp := range comps {
		if isGitCommonDirSegment(comp) {
			return i
		}
	}
	return -1
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
		// FOREIGN reject runs BEFORE the native-absolute acceptance (Finding 3, r10).
		// A forward-slash UNC pointer authored by Git-for-Windows (`gitdir:
		// //server/share/...`) is classified ABSOLUTE by POSIX filepath.IsAbs (a
		// leading `/`), so an `IsAbs`-first ordering would ACCEPT it as a native
		// POSIX path, filepath.Clean it to a LOCAL `/server/share/...`, and — if any
		// matching local parent exists while the real UNC leaf is offline — stat the
		// local leaf ENOENT and PRUNE the LIVE UNC worktree. isForeignAbsolutePath
		// now detects that `//`-prefixed forward-slash UNC on non-Windows, so the
		// foreign reject must be consulted FIRST. The drive-letter (`C:/...`) and
		// backslash-UNC (`\\...`) cases are IsAbs=false on POSIX and were already
		// caught here; only the forward-slash-UNC case needed the reordering because
		// it is the sole foreign-absolute shape POSIX IsAbs mistakes for native.
		if isForeignAbsolutePath(target) {
			// A path the CURRENT OS does not consider NATIVELY absolute but the OTHER
			// OS would (Windows drive-letter/UNC seen on POSIX, or a POSIX `/...`
			// seen on Windows). Cross-OS worktree → cannot resolve safely →
			// ambiguous, never prune.
			return "", false
		}
		if filepath.IsAbs(target) {
			// Native absolute (drive-letter/UNC on Windows, single-slash `/...` on
			// POSIX). The foreign-UNC `//...` case was already rejected above, so a
			// path reaching here is genuinely native-absolute.
			return filepath.Clean(target), true
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

// isForeignAbsolutePath reports whether target is absolute on the OTHER OS, OR is
// a UNC network root authored on Windows — a path that the current OS cannot
// resolve to a real local admin directory. Callers use this to REJECT (treat as
// ambiguous), never to resolve.
//
//   - On non-Windows (POSIX/WSL): a Windows drive-letter root (`C:\` or `C:/`),
//     a backslash-UNC root (`\\server\share`), OR a forward-slash-UNC root
//     (`//server/share`). The drive-letter and backslash-UNC shapes are
//     filepath.IsAbs=FALSE on POSIX, so the caller's IsAbs-first ordering used to
//     skip past them harmlessly; this function caught them. The forward-slash-UNC
//     shape is the Finding-3 (r10) addition: Git-for-Windows writes a UNC gitdir
//     with FORWARD slashes (`gitdir: //server/share/...`), and POSIX
//     filepath.IsAbs classifies a leading `/` as ABSOLUTE — so `//server/share`
//     is IsAbs=TRUE on POSIX and would be mistaken for a native single-slash
//     absolute path. A `//` (two-or-more leading forward slashes) prefix is never
//     a genuine native POSIX gitdir (git writes single-slash `/...`); it is always
//     a Windows-authored UNC. Detecting it here (and consulting this reject BEFORE
//     the native-IsAbs acceptance in parseGitWorktreePointer) keeps a LIVE UNC
//     worktree from being mis-resolved to a local `/server/share/...` and pruned.
//     A SINGLE leading slash (`/foo`) is a real POSIX absolute → NOT foreign.
//   - On Windows: a POSIX-absolute root (`/foo`). Note `\foo` is intentionally
//     NOT treated as foreign on Windows — it is a native (drive-relative) path
//     that Windows IsAbs already classifies as relative, and git never emits it
//     as a gitdir. A native Windows UNC (`\\server\share` or `//server/share`) is
//     already filepath.IsAbs=TRUE on Windows and resolves natively, so it must NOT
//     be treated as foreign there.
func isForeignAbsolutePath(target string) bool {
	if target == "" {
		return false
	}
	if runtime.GOOS == "windows" {
		// POSIX-absolute `/...` seen on Windows (but not `\...`, which is a
		// native Windows drive-relative path, nor `\\...` / `//...` UNC which
		// Windows IsAbs already accepts as absolute and resolves natively). A UNC
		// uses two leading separators; a single leading `/` is the foreign
		// POSIX-absolute shape.
		return len(target) >= 1 && target[0] == '/' &&
			!(len(target) >= 2 && (target[1] == '/' || target[1] == '\\'))
	}
	// Non-Windows: Windows drive-letter root `^[A-Za-z]:[\\/]`.
	if len(target) >= 3 &&
		((target[0] >= 'A' && target[0] <= 'Z') || (target[0] >= 'a' && target[0] <= 'z')) &&
		target[1] == ':' &&
		(target[2] == '\\' || target[2] == '/') {
		return true
	}
	// Non-Windows: UNC root with two-or-more leading separators — either backslash
	// (`\\server\share`, IsAbs=false on POSIX) or forward-slash
	// (`//server/share`, the Finding-3 r10 case that POSIX IsAbs mistakes for a
	// native single-slash absolute). A SINGLE leading `/` is a real POSIX absolute
	// → not foreign; only a DOUBLE leading separator is the foreign UNC shape.
	if len(target) >= 2 &&
		(target[0] == '\\' || target[0] == '/') &&
		(target[1] == '\\' || target[1] == '/') {
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
