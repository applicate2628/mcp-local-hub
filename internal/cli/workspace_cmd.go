// Package cli — `mcphub workspace {register, unregister, list, set-default,
// bootstrap}` subcommands for the dynamic-pool serena flow (Phases B.2 +
// B.3 of docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md).
//
// Distinct from the existing `mcphub workspaces` (plural) command, which
// lists every (workspace, language) tuple in the registry across all
// backends. The singular `workspace` command group is the serena-specific
// operator surface for one-workspace-one-daemon dynamic-pool entries
// (Language == api.SerenaLanguageSentinel).
//
// The set-default flag is persisted in a sidecar file
// (`<state-dir>/default-workspace.txt`) carrying one canonical workspace
// path. This is intentionally a separate file rather than a field on
// WorkspaceEntry so the change stays inside the Phase B.2 file boundary
// (`internal/cli/workspace_cmd.go` only) and avoids registry schema
// churn — Phase F is the consumer and can promote it to a registry
// field if needed when the routing seam lands.
package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// defaultWorkspaceFilename is the sidecar file alongside workspaces.yaml
// that records the operator-selected default serena workspace by its
// canonical path. Absent file = no default. Empty file = no default.
// Phase F (no-path-args routing) consumes this.
const defaultWorkspaceFilename = "default-workspace.txt"

// loadSerenaManifestForCLI is the test-injectable manifest loader. The
// production form goes through the embed-first manifest pipeline
// (`api.NewAPI().ManifestGet`); the override seam below lets tests
// hand-shape a manifest (e.g. with a daemon_template.port_pool block
// that production serena/manifest.yaml does not yet declare in v0.5.x
// because Phase D.1 is a separate phase).
var loadSerenaManifestForCLI = loadSerenaManifestFromDisk

// loadSerenaManifestFromDisk is the production manifest loader. It uses
// the same MCPHUB_MANIFEST_DIR_OVERRIDE seam the api package honors, so
// tests that set the override env get hermetic manifests.
func loadSerenaManifestFromDisk() (*config.ServerManifest, error) {
	a := api.NewAPI()
	data, err := a.ManifestGet("serena")
	if err != nil {
		return nil, fmt.Errorf("load serena manifest: %w", err)
	}
	m, err := config.ParseManifest(strings.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse serena manifest: %w", err)
	}
	return m, nil
}

// serenaPortPool resolves the port pool to allocate serena workspace
// daemons from. Phase D.1 introduces `daemon_template.port_pool` on the
// serena manifest; until that lands the resolver fails closed with an
// explicit operator-actionable message instead of silently allocating
// from the wrong (top-level) pool.
func serenaPortPool(m *config.ServerManifest) (config.PortPool, error) {
	if m.DaemonTemplate != nil && m.DaemonTemplate.PortPool != nil {
		return *m.DaemonTemplate.PortPool, nil
	}
	return config.PortPool{}, fmt.Errorf(
		"serena manifest does not declare daemon_template.port_pool — " +
			"Phase D.1 manifest migration is required before `mcphub workspace register` " +
			"can allocate ports for serena daemons")
}

// newWorkspaceCmd builds the `mcphub workspace` parent command. The
// subcommands wire B.1 registry primitives (`PutSerena` / `SerenaEntries`
// / `RemoveByBackend` / `AllocateSerenaPort`) into the operator surface.
func newWorkspaceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "workspace",
		Short: "Manage serena dynamic-pool workspaces (one daemon per workspace)",
		Long: `Group of subcommands for the serena dynamic-pool architecture (Phase B
of the v0.5.x serena-supervisor unified plan). Each registered workspace
gets its own long-lived serena daemon bootstrapped on --project <abs-path>
with languages snapshotted from .serena/project.yml at register time.

Distinct from the existing ` + "`mcphub workspaces`" + ` (plural) command, which
enumerates every (workspace, language) tuple across all backends. The
singular ` + "`mcphub workspace`" + ` group manages only the serena rows.

Subcommands:
  bootstrap     Initialize .serena/project.yml from a directory survey
  register      Register a workspace + allocate a serena port
  unregister    Remove a workspace from the registry
  list          List registered serena workspaces
  set-default   Mark a workspace as default for no-path-args routing
`,
	}
	c.AddCommand(newWorkspaceRegisterCmd())
	c.AddCommand(newWorkspaceUnregisterCmd())
	c.AddCommand(newWorkspaceListCmd())
	c.AddCommand(newWorkspaceSetDefaultCmd())
	c.AddCommand(newWorkspaceBootstrapCmd())
	return c
}

// newWorkspaceRegisterCmd builds `mcphub workspace register <path>
// [--default] [--languages cpp,typescript,markdown]`.
func newWorkspaceRegisterCmd() *cobra.Command {
	var setDefault bool
	var languagesFlag string
	c := &cobra.Command{
		Use:   "register <path>",
		Short: "Register a workspace for serena dynamic-pool routing",
		Long: `Allocate a serena port from the manifest's daemon_template.port_pool
and persist the workspace as a serena (sentinel) row in workspaces.yaml.

Behavior:
  - Reads .serena/project.yml under <path> for the languages snapshot.
  - --languages <list> overrides the .serena/project.yml read.
  - If .serena/project.yml is missing AND --languages is empty, the
    command refuses with an explicit guidance to run
    ` + "`mcphub workspace bootstrap <path>`" + ` first (B.3).
  - --default marks this workspace as the default for no-path-args routing
    (Phase F). Replaces any prior default. The marker lives in a sidecar
    file next to workspaces.yaml.
  - A second register against the same workspace_key is rejected; use
    ` + "`mcphub workspace unregister`" + ` first if you intend to re-register.

Examples:
  mcphub workspace register D:\dev\PaperPane
  mcphub workspace register D:\dev\PaperPane --default
  mcphub workspace register D:\dev\PaperPane --languages cpp,typescript,markdown
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceRegister(cmd, args[0], setDefault, languagesFlag)
		},
	}
	c.Flags().BoolVar(&setDefault, "default", false,
		"mark this workspace as the default for no-path-args routing (Phase F)")
	c.Flags().StringVar(&languagesFlag, "languages", "",
		"comma-separated language list (overrides .serena/project.yml)")
	return c
}

func runWorkspaceRegister(cmd *cobra.Command, rawPath string, setDefault bool, languagesFlag string) error {
	canonical, err := api.CanonicalWorkspacePath(rawPath)
	if err != nil {
		return err
	}
	wsKey := api.WorkspaceKey(canonical)

	// 1. Resolve languages: flag overrides, otherwise read .serena/project.yml.
	var languages []string
	if languagesFlag != "" {
		languages = splitAndTrim(languagesFlag, ",")
	} else {
		langs, err := readSerenaProjectLanguages(canonical)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf(".serena/project.yml not found in %s — "+
					"run `mcphub workspace bootstrap %s` first, "+
					"or pass --languages explicitly", canonical, canonical)
			}
			return fmt.Errorf("read .serena/project.yml: %w", err)
		}
		languages = langs
	}
	if len(languages) == 0 {
		return fmt.Errorf("no languages resolved for workspace %s "+
			"(empty .serena/project.yml or --languages= flag)", canonical)
	}
	sort.Strings(languages)

	// 2. Resolve serena manifest's port pool. Phase D.1 introduces
	// daemon_template.port_pool; until that lands this fails closed.
	m, err := loadSerenaManifestForCLI()
	if err != nil {
		return err
	}
	pool, err := serenaPortPool(m)
	if err != nil {
		return err
	}

	// 3. Acquire registry flock and load.
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return err
	}
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return err
	}

	// 4. Reject duplicate registration. B.1 default-unregister-LSP-only
	// semantics make it ambiguous to silently re-register a serena row
	// (it would clobber the Languages snapshot + RegisteredAt timestamp
	// from a prior `register --default` invocation).
	if _, exists := reg.GetSerena(wsKey); exists {
		return fmt.Errorf("workspace %s (key %s) is already registered for serena — "+
			"run `mcphub workspace unregister %s --backend serena` first if you intend to re-register",
			canonical, wsKey, canonical)
	}

	// 5. Allocate port from the serena pool.
	port, err := reg.AllocateSerenaPort(pool)
	if err != nil {
		return err
	}

	// 6. Build entry and PutSerena. Task name follows the serena-per-
	// workspace convention used by Phase D.2 (`mcp-local-hub-serena-<key>`).
	entry := api.WorkspaceEntry{
		WorkspaceKey:  wsKey,
		WorkspacePath: canonical,
		Language:      api.SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          port,
		TaskName:      fmt.Sprintf("mcp-local-hub-serena-%s", wsKey),
		RegisteredAt:  time.Now().UTC(),
		RegisteredVia: "manual",
		Languages:     languages,
	}
	if err := reg.PutSerena(entry); err != nil {
		return err
	}
	if err := reg.Save(); err != nil {
		return err
	}

	// 7. Optionally write the default marker. Errors here are non-fatal
	// — registration already succeeded; the default is a UX nicety.
	if setDefault {
		if err := writeDefaultWorkspace(filepath.Dir(regPath), canonical); err != nil {
			fmt.Fprintf(cmd.OutOrStderr(),
				"warning: workspace registered but failed to write default marker: %v\n", err)
		}
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"Registered serena workspace %s (key %s)\n  port: %d\n  task: %s\n  languages: %s\n",
		canonical, wsKey, entry.Port, entry.TaskName, strings.Join(languages, ", "))
	if setDefault {
		fmt.Fprintln(cmd.OutOrStdout(), "  default: yes")
	}
	return nil
}

// newWorkspaceUnregisterCmd builds `mcphub workspace unregister <path>
// [--backend serena|all|<name>]`.
func newWorkspaceUnregisterCmd() *cobra.Command {
	var backend string
	c := &cobra.Command{
		Use:   "unregister <path>",
		Short: "Remove a workspace from the registry",
		Long: `Drop registry rows for <path> via B.1's RemoveByBackend semantics.

--backend handling:
  (omitted)       remove only LSP rows; the serena (sentinel) row stays.
                  This matches the B.1 v5 default that lets operators
                  disable LSP routing while keeping the long-lived
                  serena daemon registered.
  --backend serena   remove only the serena (sentinel) row; LSP rows stay.
  --backend all      remove every row for <path>.
  --backend NAME     remove only LSP rows whose Backend or Language field
                     equals NAME (e.g. "mcp-language-server" / "go" /
                     "gopls-mcp"). Sentinel rows are NOT included.

The .serena/ directory on disk is never touched — disk state survives
unregister so re-registering later replays the same languages snapshot.

Examples:
  mcphub workspace unregister D:\dev\PaperPane
  mcphub workspace unregister D:\dev\PaperPane --backend serena
  mcphub workspace unregister D:\dev\PaperPane --backend all
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceUnregister(cmd, args[0], backend)
		},
	}
	c.Flags().StringVar(&backend, "backend", "",
		"backend filter: empty (LSP-only), serena, all, backend name, or LSP language name")
	return c
}

func runWorkspaceUnregister(cmd *cobra.Command, rawPath, backend string) error {
	// Use the existence-tolerant variant so an operator can unregister a
	// workspace whose directory has since been deleted or moved.
	canonical, err := api.CanonicalWorkspacePathForCleanup(rawPath)
	if err != nil {
		return err
	}
	wsKey := api.WorkspaceKey(canonical)

	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return err
	}
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return err
	}

	removed := reg.RemoveByBackend(wsKey, backend)
	if removed == 0 {
		return fmt.Errorf("no registry rows match workspace %s (key %s) with --backend=%q",
			canonical, wsKey, backend)
	}
	if err := reg.Save(); err != nil {
		return err
	}

	// If the default marker pointed at this workspace AND we removed the
	// serena row (or --backend all), clear the marker. Otherwise stale
	// default would route Phase F to a workspace that no longer exists.
	if backend == "all" || backend == "serena" {
		_ = clearDefaultIfMatches(filepath.Dir(regPath), canonical)
	}

	fmt.Fprintf(cmd.OutOrStdout(),
		"Removed %d registry row(s) for workspace %s (key %s) with --backend=%q\n",
		removed, canonical, wsKey, backend)
	return nil
}

// newWorkspaceListCmd builds `mcphub workspace list [--json]`.
func newWorkspaceListCmd() *cobra.Command {
	var jsonOut bool
	c := &cobra.Command{
		Use:   "list",
		Short: "List registered serena workspaces",
		Long: `Enumerate every serena (sentinel) row in the registry. Default output
is a human-readable table; --json emits the full WorkspaceEntry array
plus the per-row "default" flag derived from the sidecar marker.

Columns: WORKSPACE | LANGUAGES | DEFAULT | PORT | LAST_SPAWN

LAST_SPAWN is the LastMaterializedAt timestamp (set by Phase D's
supervisor reconciler when the daemon is first spawned). Until Phase D
lands, the column reads "-" for freshly-registered workspaces.
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceList(cmd, jsonOut)
		},
	}
	c.Flags().BoolVar(&jsonOut, "json", false, "machine-readable JSON output")
	return c
}

// workspaceListJSONRow is the JSON shape returned by `workspace list --json`.
// It embeds the full WorkspaceEntry verbatim plus a synthesized "default" flag.
type workspaceListJSONRow struct {
	api.WorkspaceEntry
	Default bool `json:"default"`
}

func runWorkspaceList(cmd *cobra.Command, jsonOut bool) error {
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return err
	}
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return err
	}
	defaultPath, _ := readDefaultWorkspace(filepath.Dir(regPath))

	entries := reg.SerenaEntries()
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].WorkspacePath < entries[j].WorkspacePath
	})

	if jsonOut {
		rows := make([]workspaceListJSONRow, 0, len(entries))
		for _, e := range entries {
			rows = append(rows, workspaceListJSONRow{
				WorkspaceEntry: e,
				Default:        e.WorkspacePath == defaultPath,
			})
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	return printWorkspaceTable(cmd.OutOrStdout(), entries, defaultPath)
}

// workspaceTablePathWidth is the column width for the WORKSPACE column
// in `mcphub workspace list` output. Wide enough for typical
// project paths (deep nested temp dirs in CI tests can exceed this and
// will be truncated via the shared truncate helper).
const workspaceTablePathWidth = 80

// printWorkspaceTable renders the column-aligned table form. Extracted so
// tests can exercise the layout independently of cobra dispatch.
func printWorkspaceTable(w io.Writer, entries []api.WorkspaceEntry, defaultPath string) error {
	fmt.Fprintf(w, "%-*s %-30s %-7s %-6s %-12s\n",
		workspaceTablePathWidth,
		"WORKSPACE", "LANGUAGES", "DEFAULT", "PORT", "LAST_SPAWN")
	for _, e := range entries {
		def := ""
		if e.WorkspacePath == defaultPath {
			def = "*"
		}
		fmt.Fprintf(w, "%-*s %-30s %-7s %-6d %-12s\n",
			workspaceTablePathWidth,
			truncate(e.WorkspacePath, workspaceTablePathWidth),
			truncate(strings.Join(e.Languages, ","), 30),
			def,
			e.Port,
			formatLastSpawn(e.LastMaterializedAt))
	}
	return nil
}

// formatLastSpawn renders the LastMaterializedAt timestamp. Zero = "-".
func formatLastSpawn(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02")
}

// newWorkspaceSetDefaultCmd builds `mcphub workspace set-default <path>`.
func newWorkspaceSetDefaultCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "set-default <path>",
		Short: "Mark a registered serena workspace as the default for no-path-args routing",
		Long: `Persist the canonical path of <path> as the operator-selected default
for Phase F's no-path-args routing fallback. The marker lives in a
sidecar file (` + defaultWorkspaceFilename + `) next to workspaces.yaml.

The workspace MUST already be registered via ` + "`mcphub workspace register`" + `;
the command refuses an unknown workspace_key with an explicit error.

To clear the default, pass an empty string (` + "`mcphub workspace set-default ''`" + `)
or unregister the workspace via ` + "`mcphub workspace unregister --backend serena|all`" + `,
which clears the marker as a side effect.
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceSetDefault(cmd, args[0])
		},
	}
	return c
}

func runWorkspaceSetDefault(cmd *cobra.Command, rawPath string) error {
	regPath, err := api.DefaultRegistryPath()
	if err != nil {
		return err
	}
	stateDir := filepath.Dir(regPath)
	reg := api.NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	// Empty string clears the marker.
	if strings.TrimSpace(rawPath) == "" {
		if err := writeDefaultWorkspace(stateDir, ""); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Cleared default workspace.")
		return nil
	}

	canonical, err := api.CanonicalWorkspacePath(rawPath)
	if err != nil {
		return err
	}
	wsKey := api.WorkspaceKey(canonical)

	if err := reg.Load(); err != nil {
		return err
	}
	if _, ok := reg.GetSerena(wsKey); !ok {
		return fmt.Errorf("workspace %s (key %s) is not registered for serena — "+
			"run `mcphub workspace register %s` first",
			canonical, wsKey, canonical)
	}
	if err := writeDefaultWorkspace(stateDir, canonical); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Set default serena workspace: %s\n", canonical)
	return nil
}

// newWorkspaceBootstrapCmd builds `mcphub workspace bootstrap <path>
// [--force]` per Phase B.3.
func newWorkspaceBootstrapCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "bootstrap <path>",
		Short: "Initialize .serena/project.yml from a file-extension survey",
		Long: `Survey <path> (depth-bounded at 5, gitignore-aware, hardcoded skip
list for node_modules/target/dist/.git) and write a .serena/project.yml
with a detected languages list, read_only=false, and excluded_dirs.

Detection map (matches language names accepted by upstream serena):
  .cpp .hpp .cc .cxx .h     -> cpp
  .go                       -> go
  .ts .tsx                  -> typescript
  .js .jsx                  -> javascript
  .py                       -> python
  .rs                       -> rust
  .md                       -> markdown
  .css                      -> vscode-css
  .html .htm                -> vscode-html
  .f90 .f95 .f03 .f         -> fortran

--force overwrites an existing .serena/project.yml. Without --force, the
command refuses to clobber.
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkspaceBootstrap(cmd, args[0], force)
		},
	}
	c.Flags().BoolVar(&force, "force", false, "overwrite an existing .serena/project.yml")
	return c
}

func runWorkspaceBootstrap(cmd *cobra.Command, rawPath string, force bool) error {
	canonical, err := api.CanonicalWorkspacePath(rawPath)
	if err != nil {
		return err
	}
	projectYmlPath := filepath.Join(canonical, ".serena", "project.yml")
	if !force {
		if _, err := os.Stat(projectYmlPath); err == nil {
			return fmt.Errorf("%s already exists; pass --force to overwrite", projectYmlPath)
		}
	}

	languages, err := surveyLanguages(canonical, 5)
	if err != nil {
		return fmt.Errorf("survey languages: %w", err)
	}
	sort.Strings(languages)

	// Write .serena/project.yml. Use a stable schema matching upstream
	// serena's expectations (languages, read_only, excluded_dirs).
	doc := map[string]any{
		"languages":     languages,
		"read_only":     false,
		"excluded_dirs": []string{"node_modules", "target", "dist", ".git"},
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal project.yml: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(projectYmlPath), 0o700); err != nil {
		return fmt.Errorf("mkdir .serena: %w", err)
	}
	if err := os.WriteFile(projectYmlPath, body, 0o600); err != nil {
		return fmt.Errorf("write project.yml: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n  languages: %s\n",
		projectYmlPath, strings.Join(languages, ", "))
	return nil
}

// ------------------------------------------------------------------
// File-extension survey for Phase B.3 bootstrap
// ------------------------------------------------------------------

// alwaysSkipDirs is the hardcoded skip list applied regardless of
// .gitignore content. Plan §B.3 acceptance criterion.
var alwaysSkipDirs = map[string]bool{
	"node_modules": true,
	"target":       true,
	"dist":         true,
	".git":         true,
}

// extensionToLanguage maps file extensions (lower-case, with leading dot)
// to the language identifier accepted by upstream serena.
var extensionToLanguage = map[string]string{
	".cpp": "cpp", ".hpp": "cpp", ".cc": "cpp", ".cxx": "cpp", ".h": "cpp",
	".go": "go",
	".ts": "typescript", ".tsx": "typescript",
	".js": "javascript", ".jsx": "javascript",
	".py":   "python",
	".rs":   "rust",
	".md":   "markdown",
	".css":  "vscode-css",
	".html": "vscode-html", ".htm": "vscode-html",
	".f90": "fortran", ".f95": "fortran", ".f03": "fortran", ".f": "fortran",
}

// surveyLanguages walks <root> bounded at maxDepth, gitignore-aware,
// and returns a deterministic-order list of unique languages it found.
// Returns an empty slice if no recognized extensions found.
func surveyLanguages(root string, maxDepth int) ([]string, error) {
	if maxDepth <= 0 {
		maxDepth = 5
	}
	rootIgnore := readGitignoreDirs(filepath.Join(root, ".gitignore"))
	ignoreByDir := map[string]map[string]bool{
		root: rootIgnore,
	}

	seen := map[string]bool{}
	rootDepth := strings.Count(filepath.ToSlash(root), "/")

	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// Tolerate per-entry permission errors so a single unreadable
			// subdir does not abort the entire survey.
			return nil
		}
		// Depth calculation: count the number of separators relative to
		// root. filepath.Walk gives us absolute-ish paths; converting to
		// slash form makes this OS-portable.
		curDepth := strings.Count(filepath.ToSlash(path), "/") - rootDepth
		if info.IsDir() {
			name := info.Name()
			if path != root {
				if alwaysSkipDirs[name] {
					return filepath.SkipDir
				}
				parentIgnore := ignoreByDir[filepath.Dir(path)]
				if parentIgnore[name] {
					return filepath.SkipDir
				}
				subIgnore := cloneGitignoreDirs(parentIgnore)
				for k := range readGitignoreDirs(filepath.Join(path, ".gitignore")) {
					subIgnore[k] = true
				}
				ignoreByDir[path] = subIgnore
			}
			if path == root {
				// Root rules are seeded before the walk so they apply to
				// immediate child directories without affecting siblings
				// of nested .gitignore files.
				ignoreByDir[path] = cloneGitignoreDirs(rootIgnore)
			}
			if curDepth >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		// File: classify by extension.
		ext := strings.ToLower(filepath.Ext(info.Name()))
		if lang, ok := extensionToLanguage[ext]; ok {
			seen[lang] = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	langs := make([]string, 0, len(seen))
	for l := range seen {
		langs = append(langs, l)
	}
	return langs, nil
}

func cloneGitignoreDirs(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// readGitignoreDirs reads a .gitignore file and returns the set of
// directory-name patterns it lists. Empty lines, comments, negation
// (`!`) entries, root-anchored entries, path entries, and glob entries
// are ignored. This is a deliberate simplification: Phase B.3 only needs
// "skip this directory name" behavior, not full gitignore parsing.
func readGitignoreDirs(path string) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		if strings.HasPrefix(line, "/") {
			continue
		}
		line = strings.TrimRight(line, "/")
		if line == "" {
			continue
		}
		if strings.ContainsAny(line, "/\\") {
			continue
		}
		if strings.ContainsAny(line, "*?[") {
			continue
		}
		out[line] = true
	}
	return out
}

// ------------------------------------------------------------------
// .serena/project.yml read helper for `workspace register`
// ------------------------------------------------------------------

// serenaProjectYml is the minimal struct we need from .serena/project.yml.
// We only consume the languages list; other serena fields are preserved
// verbatim on disk (we never rewrite project.yml from register).
type serenaProjectYml struct {
	Languages []string `yaml:"languages"`
}

func readSerenaProjectLanguages(canonical string) ([]string, error) {
	path := filepath.Join(canonical, ".serena", "project.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc serenaProjectYml
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc.Languages, nil
}

// ------------------------------------------------------------------
// default-workspace.txt sidecar marker
// ------------------------------------------------------------------

// writeDefaultWorkspace persists the canonical default workspace path
// (or an empty string to clear). Atomic rename so a crash mid-write
// cannot leave a truncated file.
func writeDefaultWorkspace(stateDir, canonical string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(stateDir, defaultWorkspaceFilename)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(canonical), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// readDefaultWorkspace returns the persisted default workspace path, or
// the empty string when the marker file is absent or empty.
func readDefaultWorkspace(stateDir string) (string, error) {
	path := filepath.Join(stateDir, defaultWorkspaceFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// clearDefaultIfMatches removes the marker when its stored value equals
// canonical. Returns nil if the marker is absent OR points elsewhere.
// Side effect during `workspace unregister --backend serena|all` so a
// stale default cannot survive the workspace it pointed at.
func clearDefaultIfMatches(stateDir, canonical string) error {
	got, err := readDefaultWorkspace(stateDir)
	if err != nil {
		return err
	}
	if got != canonical {
		return nil
	}
	return writeDefaultWorkspace(stateDir, "")
}

// ------------------------------------------------------------------
// small helpers
// ------------------------------------------------------------------

func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// (truncate lives in cleanup.go; same package, reused here.)
