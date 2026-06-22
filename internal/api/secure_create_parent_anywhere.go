// internal/api/secure_create_parent_anywhere.go
//
// SecureCreateParentDirForConfigLock (declared in client_write_init.go) is the
// production swap target for clients.SecureCreateParentDir — the parent-dir
// creation the shared withConfigLock chokepoint
// (internal/clients/config_lock.go) performs before opening its advisory flock.
//
// This file holds the PLATFORM-NEUTRAL package doc; the OS-specific descent is
// in secure_create_parent_anywhere_{posix,windows}.go. Both descend the FULL
// path of `dir` from the TRUSTED VOLUME ROOT (POSIX "/"; Windows drive root
// "C:\" or UNC share root) component-by-component, fd/handle-relative, refusing
// to follow any symlink/reparse-point at EVERY component — existing prefix OR
// missing tail. They reuse the SAME single-owner per-component create-or-open
// step as the home-bounded G17 creator (mkdirOrOpenRealDirAt on POSIX;
// mkdirOrVerifyRealDirWindows on Windows), so the symlink/reparse-refusing walk
// is NOT re-implemented here — only the trust anchor differs (the volume root vs
// the user home).
//
// Why descend from the VOLUME ROOT, not the "nearest existing ancestor" (bot PR
// #420 finding 1 RESIDUAL F1): an earlier revision selected the nearest existing
// ancestor and RE-OPENED it by its full ABSOLUTE PATH with O_NOFOLLOW /
// FILE_FLAG_OPEN_REPARSE_POINT. Those flags refuse ONLY the TRAILING component;
// the kernel/object-manager FOLLOWS every INTERMEDIATE component of an
// absolute-path open. So for an outside-home target ".../a/b/c" where the
// existing-prefix component "a" is a symlink into attacker space and ".../a/b"
// exists, re-opening anchor ".../a/b" by absolute path FOLLOWED "a" → the anchor
// handle became the attacker's "b" → the missing "c" was created fd-relative
// under the redirected handle and token-bearing config was published OUTSIDE the
// intended path. That is the SAME vulnerability CLASS as the original finding-1
// P1 (symlink-redirected privileged write), relocated from the final component
// to an anchor INTERMEDIATE. Descending from the volume root and opening each
// component O_NOFOLLOW-relative to the previously-held handle refuses an
// intermediate symlink at ANY position, closing the residual. (The home-bounded
// G17 creator never had this because its anchor is os.UserHomeDir() — a fixed
// trusted root — and only the relative TAIL is attacker-influenceable.)
//
// Why a separate creator from SecureCreateClientConfigParentDir (G17): that one
// refuses any path OUTSIDE the user home (a blast-radius bound for the GUI Init
// affordance). withConfigLock is SHARED across every client adapter and a
// legitimate write target can live outside the home (MiMoCode's
// $MIMOCODE_HOME/config or $XDG_CONFIG_HOME/mimocode — see
// internal/clients/mimocode.go resolveMimoCodeGlobalDir). So this creator drops
// the home-containment refusal while KEEPING the load-bearing security property:
// refuse a symlink/reparse-point component, create each real component fresh
// owner-only, descend fd/handle-relative from the trusted volume root
// (TOCTOU-safe). (bot PR #420 finding 1 + its residual F1.)

package api
