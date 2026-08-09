// Package discovery resolves the vcpkg root directory per the discovery
// order in work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md
// ("Root discovery — never hardcode, never silently guess"):
//
//  1. Explicit parameter supplied by the caller.
//  2. VCPKG_ROOT environment variable.
//  3. `vcpkg` resolved on PATH -> its containing directory (Microsoft's own
//     documented rule: the vcpkg root is the directory containing the vcpkg
//     program; VCPKG_ROOT is the recommended convention, there is no single
//     default install path).
//  4. Manifest mode: walk up from the working directory to the nearest
//     vcpkg.json / vcpkg-configuration.json, then check for a co-located
//     `vcpkg` submodule directory containing the vcpkg binary (the common
//     "vcpkg as a git submodule" layout). A manifest alone does not name a
//     root — only a manifest directory that ALSO contains a vcpkg binary
//     is treated as a candidate; this rule never guesses.
//  5. Heuristic common locations, explicitly labelled as heuristic.
//
// Every rule reports which one fired. Two rules bound what this package is
// allowed to CONCLUDE, as opposed to merely observe:
//
//   - An EXPLICIT root is TERMINAL. When the caller names a root, that root
//     is the question. If it does not hold a vcpkg binary, the answer is
//     unknown(explicit_root_invalid) or unknown(explicit_root_unreadable) —
//     never a silent fall-through to some other installation the caller did
//     not ask about. Answering "ok, D:\other" to "is C:\wanted a vcpkg root?"
//     sends every downstream tool to analyse the wrong installation.
//   - A HEURISTIC NEVER SELECTS. Rule 5 matches a hardcoded machine-layout
//     guess; one match is not evidence that THIS is the caller's vcpkg. Every
//     heuristic outcome is unknown(heuristic_only) (one hit) or
//     unknown(multiple_candidates) (several), always listing the candidates
//     so the caller can confirm one by passing it explicitly.
//
// No candidate at all is reported plainly, with a remedy: supply root
// explicitly.
package discovery

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// Rule names which discovery step produced a candidate. Closed enum.
type Rule string

const (
	RuleExplicit  Rule = "explicit_param"
	RuleEnv       Rule = "vcpkg_root_env"
	RulePath      Rule = "path_lookup"
	RuleManifest  Rule = "manifest_walkup"
	RuleHeuristic Rule = "heuristic"
)

// Reason is populated only when Result.Status == evidence.StatusUnknown.
// Closed enum.
type Reason string

const (
	// ReasonNoneFound: no rule produced any candidate at all.
	ReasonNoneFound Reason = "no_candidates_found"
	// ReasonAmbiguous: the heuristic tier found more than one valid
	// candidate and none of the higher-precedence rules fired.
	ReasonAmbiguous Reason = "multiple_candidates"
	// ReasonExplicitRootInvalid: the caller named a root explicitly and the
	// filesystem positively reported no vcpkg binary there (the directory or
	// the binary does not exist, or one of them is the wrong kind of entry).
	// Terminal: no lower-precedence rule is consulted, because the caller
	// asked about THIS root, not about whichever installation happens to be
	// reachable some other way.
	ReasonExplicitRootInvalid Reason = "explicit_root_invalid"
	// ReasonExplicitRootUnreadable: the caller named a root explicitly and
	// the probe could not determine whether a vcpkg binary is there at all
	// (permission denied, I/O error, disconnected path). Distinct from
	// ReasonExplicitRootInvalid because the remedy differs: fix access to the
	// path, rather than correct the path.
	ReasonExplicitRootUnreadable Reason = "explicit_root_unreadable"
	ReasonExplicitRootRelative   Reason = "explicit_root_relative"
	ReasonEnvRootRelative        Reason = "env_root_relative"
	// ReasonHeuristicOnly: the ONLY thing that matched was a hardcoded
	// heuristic common location (exactly one of them). A heuristic match is a
	// candidate, never a selection: nothing about C:\vcpkg existing proves it
	// is the installation this caller means. Candidates carries it; the
	// remedy is to confirm it by passing root explicitly.
	ReasonHeuristicOnly Reason = "heuristic_only"
)

// Candidate is one discovered (or considered) vcpkg root.
type Candidate struct {
	Path string `json:"path"`
	Rule Rule   `json:"rule"`
	// Detail names the specific signal, e.g. which heuristic path template
	// matched, or which manifest file was found above it.
	Detail string `json:"detail,omitempty"`
}

// Result is the outcome of one DiscoverRoot call.
type Result struct {
	Status Status `json:"status"`
	// Reason is set only when Status == unknown.
	Reason Reason `json:"reason,omitempty"`
	// RuleFired names the winning rule when Status == ok.
	RuleFired Rule `json:"rule_fired,omitempty"`
	// Root is the resolved vcpkg root when Status == ok.
	Root string `json:"root,omitempty"`
	// Candidates lists every candidate considered. Always populated when
	// Status != ok (so the caller sees what was tried); populated with the
	// single winner when Status == ok too, for uniformity.
	Candidates []Candidate       `json:"candidates,omitempty"`
	Evidence   evidence.Evidence `json:"evidence"`
}

// Status is a local alias so callers of this package do not need to import
// the evidence package themselves just to read Result.Status.
type Status = evidence.Status

// Deps abstracts every ambient input DiscoverRoot reads (env, PATH, cwd,
// filesystem, OS) behind explicit, injectable functions — see the repo-wide
// "Determinism and ambient-input control" rule: nothing here reads os.Getenv
// or exec.LookPath directly, so tests exercise the exact same code path
// with fully controlled inputs, never the developer's real machine.
type Deps struct {
	Getenv   func(key string) string
	LookPath func(file string) (string, error)
	// EvalSymlinks resolves the executable returned by LookPath before its
	// containing directory is treated as the vcpkg root.
	EvalSymlinks func(path string) (string, error)
	Getwd        func() (string, error)
	// Stat reports whether path exists (any file type); tests can fake this
	// without touching a real filesystem.
	Stat func(path string) (os.FileInfo, error)
	// GOOS drives which heuristic path list + submodule binary name is used.
	GOOS string
	// UserHomeDir mirrors os.UserHomeDir.
	UserHomeDir func() (string, error)
}

// DefaultDeps wires Deps to the real OS. Production callers use this;
// tests build their own Deps with fake functions.
func DefaultDeps() Deps {
	return Deps{
		Getenv:       os.Getenv,
		LookPath:     lookPath,
		EvalSymlinks: filepath.EvalSymlinks,
		Getwd:        os.Getwd,
		Stat:         os.Stat,
		GOOS:         runtime.GOOS,
		UserHomeDir:  os.UserHomeDir,
	}
}

// lookPath is os/exec.LookPath, referenced indirectly so DefaultDeps stays
// a plain function-value assignment (and tests can substitute a fake).
func lookPath(file string) (string, error) {
	return exec.LookPath(file)
}

// vcpkgBinaryName returns the vcpkg executable basename for goos.
func vcpkgBinaryName(goos string) string {
	if goos == "windows" {
		return "vcpkg.exe"
	}
	return "vcpkg"
}

// probeVcpkgBinary reports whether dir contains a vcpkg executable, per the
// documented rule "the vcpkg root is the directory containing the vcpkg
// program". The result is tri-state on purpose: a Stat that FAILS (permission
// denied, I/O error) is not evidence of absence, and the explicit-root rule
// must be able to tell those two cases apart — see ReasonExplicitRootInvalid
// vs ReasonExplicitRootUnreadable.
func probeVcpkgBinary(deps Deps, dir string) (evidence.Presence, error) {
	if dir == "" {
		return evidence.PresenceAbsent, errors.New("empty directory")
	}
	info, err := deps.Stat(filepath.Join(dir, vcpkgBinaryName(deps.GOOS)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return evidence.PresenceAbsent, nil
		}
		return evidence.PresenceUnreadable, err
	}
	if !info.Mode().IsRegular() || (deps.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		return evidence.PresenceAbsent, nil
	}
	return evidence.PresenceExists, nil
}

// hasVcpkgBinary is the boolean view used by the lower-precedence rules,
// where an unreadable candidate and an absent one are handled identically
// (both simply fail to win that tier and fall through to the next one).
// The explicit-root rule deliberately does NOT use this — it needs the
// distinction, and it is terminal.
func hasVcpkgBinary(deps Deps, dir string) bool {
	p, _ := probeVcpkgBinary(deps, dir)
	return p == evidence.PresenceExists
}

// manifestFiles are the two manifest markers named by the design doc, most
// specific first (a vcpkg.json is stronger project-local evidence than a
// bare vcpkg-configuration.json, but either is accepted).
var manifestFiles = []string{"vcpkg.json", "vcpkg-configuration.json"}

// heuristicPathsFor returns the design doc's labelled heuristic locations
// for goos, with %VAR%/~ expansion applied via deps.
func heuristicPathsFor(deps Deps) []struct{ path, detail string } {
	var out []struct{ path, detail string }
	add := func(p, detail string) {
		if p == "" {
			return
		}
		out = append(out, struct{ path, detail string }{p, detail})
	}
	if deps.GOOS == "windows" {
		add(`C:\vcpkg`, `C:\vcpkg`)
		add(`C:\opt\vcpkg`, `C:\opt\vcpkg`)
		add(`C:\dev\vcpkg`, `C:\dev\vcpkg`)
		if home, err := deps.UserHomeDir(); err == nil && home != "" {
			add(filepath.Join(home, "vcpkg"), `%USERPROFILE%\vcpkg`)
		}
		add(`D:\vcpkg`, `D:\vcpkg`)
		// %ProgramFiles%\Microsoft Visual Studio\*\*\VC\vcpkg — two glob
		// levels (VS release year/version, then edition).
		pf := deps.Getenv("ProgramFiles")
		if pf == "" {
			pf = `C:\Program Files`
		}
		pattern := filepath.Join(pf, "Microsoft Visual Studio", "*", "*", "VC", "vcpkg")
		if matches, err := filepath.Glob(pattern); err == nil {
			for _, m := range matches {
				add(m, `%ProgramFiles%\Microsoft Visual Studio\*\*\VC\vcpkg`)
			}
		}
	} else {
		add("/opt/vcpkg", "/opt/vcpkg")
		if home, err := deps.UserHomeDir(); err == nil && home != "" {
			add(filepath.Join(home, "vcpkg"), "~/vcpkg")
		}
	}
	return out
}

// DiscoverRoot resolves the vcpkg root per the discovery order documented
// on this package. explicitRoot is the caller-supplied parameter (rule 1);
// pass "" when the caller did not supply one.
func DiscoverRoot(explicitRoot string, deps Deps) Result {
	var res Result

	// Rule 1: explicit parameter. TERMINAL — see the package doc comment.
	// When the caller names a root, that root IS the question being asked;
	// resolving some other installation instead would answer a question
	// nobody asked and silently redirect every downstream tool.
	if strings.TrimSpace(explicitRoot) != "" {
		res.Evidence.AddPath(explicitRoot)
		if !filepath.IsAbs(explicitRoot) {
			return Result{Status: evidence.StatusUnknown, Reason: ReasonExplicitRootRelative, Candidates: []Candidate{{Path: explicitRoot, Rule: RuleExplicit, Detail: "explicit root must be absolute"}}, Evidence: res.Evidence}
		}
		// A caller-supplied root names a directory. Probe that object before
		// constructing a child executable path so a regular file is diagnosed as
		// invalid input, not as an unreadable synthetic file/child path.
		if rootInfo, rootErr := deps.Stat(explicitRoot); rootErr == nil && !rootInfo.IsDir() {
			return Result{
				Status: evidence.StatusUnknown,
				Reason: ReasonExplicitRootInvalid,
				Candidates: []Candidate{{
					Path: explicitRoot, Rule: RuleExplicit, Detail: "explicit root is not a directory",
				}},
				Evidence: res.Evidence,
			}
		}
		presence, probeErr := probeVcpkgBinary(deps, explicitRoot)
		if presence == evidence.PresenceExists {
			return Result{
				Status:     evidence.StatusOK,
				RuleFired:  RuleExplicit,
				Root:       explicitRoot,
				Candidates: []Candidate{{Path: explicitRoot, Rule: RuleExplicit}},
				Evidence:   res.Evidence,
			}
		}
		reason := ReasonExplicitRootInvalid
		detail := "no " + vcpkgBinaryName(deps.GOOS) + " found in this directory"
		if presence == evidence.PresenceUnreadable {
			reason = ReasonExplicitRootUnreadable
			detail = "could not determine whether " + vcpkgBinaryName(deps.GOOS) +
				" is present in this directory"
		}
		if probeErr != nil {
			detail += ": " + probeErr.Error()
		}
		return Result{
			Status: evidence.StatusUnknown,
			Reason: reason,
			Candidates: []Candidate{{
				Path: explicitRoot, Rule: RuleExplicit, Detail: detail,
			}},
			Evidence: res.Evidence,
		}
	}

	// Rule 2: VCPKG_ROOT env var.
	if envRoot := deps.Getenv("VCPKG_ROOT"); strings.TrimSpace(envRoot) != "" {
		res.Evidence.AddPath(envRoot)
		if !filepath.IsAbs(envRoot) {
			return Result{
				Status: evidence.StatusUnknown,
				Reason: ReasonEnvRootRelative,
				Candidates: []Candidate{{
					Path: envRoot, Rule: RuleEnv, Detail: "VCPKG_ROOT must be absolute",
				}},
				Evidence: res.Evidence,
			}
		}
		if hasVcpkgBinary(deps, envRoot) {
			return Result{
				Status:     evidence.StatusOK,
				RuleFired:  RuleEnv,
				Root:       envRoot,
				Candidates: []Candidate{{Path: envRoot, Rule: RuleEnv}},
				Evidence:   res.Evidence,
			}
		}
		res.Candidates = append(res.Candidates, Candidate{
			Path: envRoot, Rule: RuleEnv,
			Detail: "VCPKG_ROOT is set but no " + vcpkgBinaryName(deps.GOOS) + " found there",
		})
	}

	// Rule 3: vcpkg resolved on PATH -> containing directory.
	if deps.LookPath != nil {
		if p, err := deps.LookPath(strings.TrimSuffix(vcpkgBinaryName(deps.GOOS), ".exe")); err == nil && p != "" {
			res.Evidence.AddPath(p)
			resolvedPath := p
			if deps.EvalSymlinks != nil {
				resolved, resolveErr := deps.EvalSymlinks(p)
				if resolveErr != nil {
					res.Candidates = append(res.Candidates, Candidate{Path: p, Rule: RulePath, Detail: "PATH executable target could not be resolved: " + resolveErr.Error()})
					resolvedPath = ""
				} else {
					resolvedPath = resolved
					if resolved != p {
						res.Evidence.AddPath(resolved)
					}
				}
			}
			if resolvedPath != "" && !filepath.IsAbs(resolvedPath) {
				if deps.Getwd == nil {
					res.Candidates = append(res.Candidates, Candidate{Path: resolvedPath, Rule: RulePath, Detail: "PATH executable target is relative and cwd is unavailable"})
					resolvedPath = ""
				} else if cwd, cwdErr := deps.Getwd(); cwdErr != nil || !filepath.IsAbs(cwd) {
					res.Candidates = append(res.Candidates, Candidate{Path: resolvedPath, Rule: RulePath, Detail: "PATH executable target could not be made absolute"})
					resolvedPath = ""
				} else {
					resolvedPath = filepath.Clean(filepath.Join(cwd, resolvedPath))
					res.Evidence.AddPath(resolvedPath)
				}
			}
			dir := filepath.Dir(resolvedPath)
			if resolvedPath != "" && hasVcpkgBinary(deps, dir) {
				return Result{
					Status:     evidence.StatusOK,
					RuleFired:  RulePath,
					Root:       dir,
					Candidates: []Candidate{{Path: dir, Rule: RulePath, Detail: "resolved from PATH: " + p}},
					Evidence:   res.Evidence,
				}
			}
		}
	}

	// Rule 4: manifest walk-up, then a co-located vcpkg/ submodule binary.
	if deps.Getwd != nil {
		if cwd, err := deps.Getwd(); err == nil && cwd != "" {
			if manifestDir, manifestFile, ok := walkUpForManifest(deps, cwd); ok {
				res.Evidence.AddPath(filepath.Join(manifestDir, manifestFile))
				candidateRoot := filepath.Join(manifestDir, "vcpkg")
				if hasVcpkgBinary(deps, candidateRoot) {
					return Result{
						Status:    evidence.StatusOK,
						RuleFired: RuleManifest,
						Root:      candidateRoot,
						Candidates: []Candidate{{
							Path: candidateRoot, Rule: RuleManifest,
							Detail: "co-located with manifest " + manifestFile + " found at " + manifestDir,
						}},
						Evidence: res.Evidence,
					}
				}
				// Manifest found but no co-located vcpkg/ binary — informational
				// only, never guessed as a root.
				res.Candidates = append(res.Candidates, Candidate{
					Path: manifestDir, Rule: RuleManifest,
					Detail: "manifest " + manifestFile + " found, but no vcpkg binary in " + candidateRoot,
				})
			}
		}
	}

	// Rule 5: heuristic common locations. A hit here is a CANDIDATE, never a
	// selection — the paths are hardcoded machine-layout guesses, so an
	// unrelated C:\vcpkg on the box says nothing about which installation
	// this caller means. Both outcomes below are therefore unknown, and both
	// list every candidate so the caller can confirm one by passing it as an
	// explicit root (which rule 1 then verifies and treats as terminal).
	var heuristicHits []Candidate
	for _, h := range heuristicPathsFor(deps) {
		if hasVcpkgBinary(deps, h.path) {
			heuristicHits = append(heuristicHits, Candidate{Path: h.path, Rule: RuleHeuristic, Detail: h.detail})
			res.Evidence.AddPath(h.path)
		}
	}
	if len(heuristicHits) > 0 {
		reason := ReasonHeuristicOnly
		if len(heuristicHits) > 1 {
			reason = ReasonAmbiguous
		}
		res.Candidates = append(res.Candidates, heuristicHits...)
		return Result{
			Status:     evidence.StatusUnknown,
			Reason:     reason,
			Candidates: res.Candidates,
			Evidence:   res.Evidence,
		}
	}

	// No rule produced a usable candidate at all.
	res.Status = evidence.StatusUnknown
	res.Reason = ReasonNoneFound
	return res
}

// walkUpForManifest walks up from start looking for one of manifestFiles.
// Returns the directory it was found in and which filename matched.
func walkUpForManifest(deps Deps, start string) (dir, file string, ok bool) {
	cur := start
	for i := 0; i < 64; i++ { // bounded: never walk unboundedly on a pathological tree
		for _, m := range manifestFiles {
			if fi, err := deps.Stat(filepath.Join(cur, m)); err == nil && fi.Mode().IsRegular() {
				return cur, m, true
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return "", "", false
}
