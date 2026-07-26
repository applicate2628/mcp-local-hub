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
// Every rule reports which one fired. Several candidates surviving one rule
// tier (only possible at the heuristic tier) are ALL returned so the caller
// can disambiguate — never silently picked. No candidate at all is reported
// plainly, with a remedy: supply root explicitly.
package discovery

import (
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
	Getwd    func() (string, error)
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
		Getenv:      os.Getenv,
		LookPath:    lookPath,
		Getwd:       os.Getwd,
		Stat:        os.Stat,
		GOOS:        runtime.GOOS,
		UserHomeDir: os.UserHomeDir,
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

// hasVcpkgBinary reports whether dir contains a vcpkg executable, per the
// documented rule "the vcpkg root is the directory containing the vcpkg
// program" — this is a verified fact (a Stat call), never a guess.
func hasVcpkgBinary(deps Deps, dir string) bool {
	if dir == "" {
		return false
	}
	fi, err := deps.Stat(filepath.Join(dir, vcpkgBinaryName(deps.GOOS)))
	return err == nil && !fi.IsDir()
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

	// Rule 1: explicit parameter.
	if strings.TrimSpace(explicitRoot) != "" {
		res.Evidence.AddPath(explicitRoot)
		if hasVcpkgBinary(deps, explicitRoot) {
			return Result{
				Status:     evidence.StatusOK,
				RuleFired:  RuleExplicit,
				Root:       explicitRoot,
				Candidates: []Candidate{{Path: explicitRoot, Rule: RuleExplicit}},
				Evidence:   res.Evidence,
			}
		}
		// Explicit but wrong: still surface it as a considered (invalid)
		// candidate list of exactly one — never silently fall through
		// without saying the explicit value was rejected.
		res.Candidates = append(res.Candidates, Candidate{
			Path: explicitRoot, Rule: RuleExplicit,
			Detail: "no " + vcpkgBinaryName(deps.GOOS) + " found in this directory",
		})
	}

	// Rule 2: VCPKG_ROOT env var.
	if envRoot := deps.Getenv("VCPKG_ROOT"); strings.TrimSpace(envRoot) != "" {
		res.Evidence.AddPath(envRoot)
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
			dir := filepath.Dir(p)
			res.Evidence.AddPath(p)
			return Result{
				Status:     evidence.StatusOK,
				RuleFired:  RulePath,
				Root:       dir,
				Candidates: []Candidate{{Path: dir, Rule: RulePath, Detail: "resolved from PATH: " + p}},
				Evidence:   res.Evidence,
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

	// Rule 5: heuristic common locations.
	var heuristicHits []Candidate
	for _, h := range heuristicPathsFor(deps) {
		if hasVcpkgBinary(deps, h.path) {
			heuristicHits = append(heuristicHits, Candidate{Path: h.path, Rule: RuleHeuristic, Detail: h.detail})
			res.Evidence.AddPath(h.path)
		}
	}
	switch len(heuristicHits) {
	case 0:
		// fall through to "none found"
	case 1:
		return Result{
			Status:     evidence.StatusOK,
			RuleFired:  RuleHeuristic,
			Root:       heuristicHits[0].Path,
			Candidates: heuristicHits,
			Evidence:   res.Evidence,
		}
	default:
		res.Candidates = append(res.Candidates, heuristicHits...)
		return Result{
			Status:     evidence.StatusUnknown,
			Reason:     ReasonAmbiguous,
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
			if fi, err := deps.Stat(filepath.Join(cur, m)); err == nil && !fi.IsDir() {
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
