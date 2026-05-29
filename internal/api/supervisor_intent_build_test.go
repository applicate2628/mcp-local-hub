package api

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// TestSerenaTaskNameForWorkspace_Deterministic covers plan §D.2:
// same canonical workspace path must always produce the same task
// name. Same hash, same prefix, same byte sequence.
func TestSerenaTaskNameForWorkspace_Deterministic(t *testing.T) {
	const ws = "C:/work/alpha"
	first := SerenaTaskNameForWorkspace(ws)
	second := SerenaTaskNameForWorkspace(ws)
	if first != second {
		t.Fatalf("non-deterministic: first=%q second=%q", first, second)
	}
}

// TestSerenaTaskNameForWorkspace_CanonicalForm covers plan §D.2 task
// name shape: leading backslash + literal serena prefix + 8-hex-chars
// hash. Total byte length is len(prefix) + 8.
func TestSerenaTaskNameForWorkspace_CanonicalForm(t *testing.T) {
	const ws = "C:/work/alpha"
	got := SerenaTaskNameForWorkspace(ws)
	if !strings.HasPrefix(got, SerenaTaskNamePrefix) {
		t.Fatalf("missing canonical prefix: got=%q want prefix=%q", got, SerenaTaskNamePrefix)
	}
	suffix := strings.TrimPrefix(got, SerenaTaskNamePrefix)
	if len(suffix) != 8 {
		t.Fatalf("suffix len: got=%d want=8 (full=%q)", len(suffix), got)
	}
	// Suffix must be 8 lowercase-hex chars (WorkspaceKey produces
	// exactly that — confirms the contract holds end-to-end).
	for i, r := range suffix {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			t.Fatalf("non-hex char at position %d: %q (suffix=%q)", i, r, suffix)
		}
	}
	// And the suffix must equal WorkspaceKey(ws) — the public hash
	// helper. (Test the helper via the underlying byte sequence in
	// hashWorkspacePathForTest to cross-check.)
	if want := hashWorkspacePathForTest(ws); suffix != want {
		t.Fatalf("suffix != hashWorkspacePathForTest: got=%q want=%q", suffix, want)
	}
}

// TestSerenaTaskNameForWorkspace_NoCollisionFor100Workspaces covers
// plan §D.2 collision-resistance: 100 distinct canonical paths must
// produce 100 distinct task names. The 32-bit hash birthday bound is
// well above this set, so a collision indicates a real bug (e.g.
// truncation, encoding error). Plan §D.2.
func TestSerenaTaskNameForWorkspace_NoCollisionFor100Workspaces(t *testing.T) {
	const N = 100
	seen := make(map[string]string, N)
	for i := 0; i < N; i++ {
		// Mix path shapes so the test covers depth + length + chars.
		ws := genTestWorkspacePath(i)
		got := SerenaTaskNameForWorkspace(ws)
		if prev, dup := seen[got]; dup {
			t.Fatalf("collision: %q and %q both hashed to %q", prev, ws, got)
		}
		seen[got] = ws
	}
}

// genTestWorkspacePath builds a unique synthetic workspace path for
// the collision test. Mixes index + a couple of fixed string shapes
// so the hash inputs vary across drive letter, path depth, name
// prefix, and trailing segment.
func genTestWorkspacePath(i int) string {
	prefixes := []string{"C:/work", "D:/projects", "/home/user/code", "/srv/repos"}
	suffixes := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	prefix := prefixes[i%len(prefixes)]
	suffix := suffixes[i%len(suffixes)]
	return prefix + "/" + suffix + "-" + intToHex(i)
}

// intToHex stringifies i as 4-byte lowercase hex with leading zeros.
// Tiny helper so the generator avoids strconv in test code.
func intToHex(i int) string {
	const digits = "0123456789abcdef"
	buf := [4]byte{}
	for k := 3; k >= 0; k-- {
		buf[k] = digits[i&0xF]
		i >>= 4
	}
	return string(buf[:])
}

// TestIsSerenaTaskName_CanonicalForm covers both branches of the
// prefix matcher: leading-backslash canonical form returns true.
func TestIsSerenaTaskName_CanonicalForm(t *testing.T) {
	const ws = "/work/alpha"
	got := SerenaTaskNameForWorkspace(ws)
	if !IsSerenaTaskName(got) {
		t.Fatalf("IsSerenaTaskName(%q) = false; want true", got)
	}
}

// TestIsSerenaTaskName_BareForm covers the bare-prefix branch: the
// pre-canonical form (no leading backslash) is also accepted.
func TestIsSerenaTaskName_BareForm(t *testing.T) {
	const bare = "mcp-local-hub-serena-abcd1234"
	if !IsSerenaTaskName(bare) {
		t.Fatalf("IsSerenaTaskName(%q) = false; want true", bare)
	}
}

// TestIsSerenaTaskName_PrefixWithoutSuffix covers the bare-prefix
// edge: the literal prefix string alone (no hash suffix) is NOT a
// serena task name.
func TestIsSerenaTaskName_PrefixWithoutSuffix(t *testing.T) {
	cases := []string{
		"mcp-local-hub-serena-",
		`\mcp-local-hub-serena-`,
	}
	for _, c := range cases {
		if IsSerenaTaskName(c) {
			t.Errorf("IsSerenaTaskName(%q) = true; want false (no hash suffix)", c)
		}
	}
}

func TestIsSerenaTaskName_RejectsNonHexSuffix(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{`\mcp-local-hub-serena-foo!bar`, false},
		{`\mcp-local-hub-serena-FOOBAR12`, false},
		{`\mcp-local-hub-serena-deadbeef`, true},
	}
	for _, c := range cases {
		if got := IsSerenaTaskName(c.name); got != c.want {
			t.Errorf("IsSerenaTaskName(%q) = %v; want %v", c.name, got, c.want)
		}
	}
}

func TestIsSerenaTaskName_RejectsWrongLengthSuffix(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{`\mcp-local-hub-serena-abc`, false},
		{`\mcp-local-hub-serena-abcdef123`, false},
		{`\mcp-local-hub-serena-abcdef12`, true},
	}
	for _, c := range cases {
		if got := IsSerenaTaskName(c.name); got != c.want {
			t.Errorf("IsSerenaTaskName(%q) = %v; want %v", c.name, got, c.want)
		}
	}
}

// TestIsSerenaTaskName_NonSerenaTaskName covers LSP-bridge task names
// (mcp-local-hub-lsp-go-* etc.) and generic non-serena names. None
// should match.
func TestIsSerenaTaskName_NonSerenaTaskName(t *testing.T) {
	cases := []string{
		"",
		"mcp-local-hub-memory-default",
		`\mcp-local-hub-memory-default`,
		"mcp-local-hub-lsp-go-abcd1234",
		"unrelated-task-name",
	}
	for _, c := range cases {
		if IsSerenaTaskName(c) {
			t.Errorf("IsSerenaTaskName(%q) = true; want false", c)
		}
	}
}

// testMcphubBinary is the placeholder mcphub binary path passed to
// BuildSupervisorDaemonsForSerena across the unit tests. Production
// callers (D.3 / install_intent) resolve via canonicalMcphubPath();
// tests just need a non-empty stable string to assert against.
const testMcphubBinary = `C:\test\bin\mcphub.exe`

// fixtureSerenaManifest returns a minimal *config.ServerManifest
// matching what Phase D.3 migration would write into
// servers/serena/manifest.yaml. Used by the fan-out tests below.
func fixtureSerenaManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:      "serena",
		Kind:      config.KindWorkspaceScoped,
		Transport: config.TransportNativeHTTP,
		Command:   "uvx",
		BaseArgs:  []string{"--from", "git+https://example.invalid/serena", "serena", "start-mcp-server"},
		Env:       map[string]string{"PYTHONUNBUFFERED": "1"},
		DaemonTemplate: &config.DaemonTemplate{
			Context: "codex",
			PortPool: &config.PortPool{
				Start: 9121,
				End:   9199,
			},
			// design §5: extra_args_template carries ONLY --project
			// ${workspace.path}. --context is APPENDED by the materializer
			// from DaemonTemplate.Context (a --context token here would
			// produce a doubled --context in the child argv).
			ExtraArgsTemplate: []string{
				"--project", "${workspace.path}",
			},
		},
	}
}

// TestBuildSupervisorIntent_FansOutPerSerenaWorkspace covers plan D.2
// happy path: manifest with daemon_template + 3 registered serena
// workspaces produces 3 SupervisorDaemon entries in input order, each
// carrying the right task name + port + workspace + expanded args.
func TestBuildSupervisorIntent_FansOutPerSerenaWorkspace(t *testing.T) {
	m := fixtureSerenaManifest()
	workspaces := []WorkspaceEntry{
		{
			WorkspaceKey:  WorkspaceKey("C:/work/alpha"),
			WorkspacePath: "C:/work/alpha",
			Language:      SerenaLanguageSentinel,
			Backend:       "serena",
			Port:          9121,
		},
		{
			WorkspaceKey:  WorkspaceKey("C:/work/beta"),
			WorkspacePath: "C:/work/beta",
			Language:      SerenaLanguageSentinel,
			Backend:       "serena",
			Port:          9122,
		},
		{
			WorkspaceKey:  WorkspaceKey("C:/work/gamma"),
			WorkspacePath: "C:/work/gamma",
			Language:      SerenaLanguageSentinel,
			Backend:       "serena",
			Port:          9123,
		},
	}
	got := BuildSupervisorDaemonsForSerena(m, workspaces, "abc123hash", testMcphubBinary)
	if len(got) != 3 {
		t.Fatalf("got %d descriptors; want 3", len(got))
	}
	// Closes review feedback on cbd1b1a (bot P2): each descriptor MUST
	// carry a workspace-unique Daemon field. Collect the seen values
	// to assert pairwise distinctness across the fan-out.
	seenDaemon := map[string]int{}
	for i, d := range got {
		ws := workspaces[i]
		wantTaskName := SerenaTaskNameForWorkspace(ws.WorkspacePath)
		if d.TaskName != wantTaskName {
			t.Errorf("[%d] TaskName: got=%q want=%q", i, d.TaskName, wantTaskName)
		}
		if d.Server != "serena" {
			t.Errorf("[%d] Server: got=%q want=%q", i, d.Server, "serena")
		}
		wantDaemon := ws.WorkspaceKey
		if d.Daemon != wantDaemon {
			t.Errorf("[%d] Daemon: got=%q want=%q (per-workspace WorkspaceKey)", i, d.Daemon, wantDaemon)
		}
		seenDaemon[d.Daemon]++
		// Closes review feedback on cbd1b1a (bot P1): supervisor exec
		// path is `<mcphub binary> daemon --server <name> --workspace
		// <path>`. The supervisor uses exec.Command(d.Command,
		// d.Args...) verbatim; if we emitted the raw uvx command and
		// manifest BaseArgs/ExtraArgsTemplate, the per-workspace port
		// (ws.Port) would never bind because uvx itself has no
		// --port understanding - the mcphub daemon wrapper owns
		// port binding + health probes.
		if d.Command != testMcphubBinary {
			t.Errorf("[%d] Command: got=%q want=%q (mcphub binary)", i, d.Command, testMcphubBinary)
		}
		wantArgs := []string{
			"daemon", "serena-proxy",
			"--server", "serena",
			"--workspace", ws.WorkspacePath,
			"--port", strconv.Itoa(ws.Port),
			"--task-name", SerenaTaskNameForWorkspace(ws.WorkspacePath),
		}
		if !reflect.DeepEqual(d.Args, wantArgs) {
			t.Errorf("[%d] Args:\n got=%#v\nwant=%#v", i, d.Args, wantArgs)
		}
		if d.Workspace != ws.WorkspacePath {
			t.Errorf("[%d] Workspace: got=%q want=%q", i, d.Workspace, ws.WorkspacePath)
		}
		if d.Port != ws.Port {
			t.Errorf("[%d] Port: got=%d want=%d", i, d.Port, ws.Port)
		}
		if d.ManifestHash != "abc123hash" {
			t.Errorf("[%d] ManifestHash: got=%q want=%q", i, d.ManifestHash, "abc123hash")
		}
		if !reflect.DeepEqual(d.Env, m.Env) {
			t.Errorf("[%d] Env: got=%#v want=%#v", i, d.Env, m.Env)
		}
	}
	// Pairwise-distinctness assertion - no two workspaces may collapse
	// onto the same Daemon field. Three workspaces -> three distinct
	// Daemon values. Closes bot P2 finding on cbd1b1a.
	for d, n := range seenDaemon {
		if n != 1 {
			t.Errorf("Daemon %q appeared %d times; want exactly 1 (per-workspace uniqueness)", d, n)
		}
	}
}

// TestBuildSupervisorIntent_EnvClonedNotAliased verifies that mutating
// the descriptor Env after BuildSupervisorDaemonsForSerena does NOT
// leak into the manifest Env. The fan-out clones m.Env per descriptor
// so callers can layer per-daemon env overrides safely.
func TestBuildSupervisorIntent_EnvClonedNotAliased(t *testing.T) {
	m := fixtureSerenaManifest()
	ws := WorkspaceEntry{
		WorkspaceKey:  WorkspaceKey("C:/work/alpha"),
		WorkspacePath: "C:/work/alpha",
		Language:      SerenaLanguageSentinel,
		Port:          9121,
	}
	got := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{ws}, "", testMcphubBinary)
	if len(got) != 1 {
		t.Fatalf("got %d descriptors; want 1", len(got))
	}
	got[0].Env["NEW_VAR"] = "from-descriptor"
	if _, leaked := m.Env["NEW_VAR"]; leaked {
		t.Fatalf("descriptor Env mutation leaked into manifest.Env")
	}
}

// TestBuildSupervisorIntent_EmptyRegistryNoDescriptors covers plan D.2:
// manifest with daemon_template + 0 serena workspaces returns nil
// (not an error). Empty pool is a steady state.
func TestBuildSupervisorIntent_EmptyRegistryNoDescriptors(t *testing.T) {
	m := fixtureSerenaManifest()
	got := BuildSupervisorDaemonsForSerena(m, nil, "", testMcphubBinary)
	if got != nil {
		t.Errorf("got=%#v want=nil (empty registry)", got)
	}
	got = BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{}, "", testMcphubBinary)
	if got != nil {
		t.Errorf("got=%#v want=nil (empty slice)", got)
	}
}

// TestBuildSupervisorIntent_LegacyDaemonsListStillWorks covers plan D.2:
// manifest with legacy daemons:[] (no daemon_template) returns nil
// - the fan-out helper only handles the dynamic-pool branch. Legacy
// manifests continue to flow through the existing install path.
func TestBuildSupervisorIntent_LegacyDaemonsListStillWorks(t *testing.T) {
	m := &config.ServerManifest{
		Name:      "serena",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "uvx",
		Daemons: []config.DaemonSpec{
			{Name: "unified", Context: "codex", Port: 9121, ExtraArgs: []string{"--context", "codex"}},
		},
	}
	got := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{{
		WorkspacePath: "C:/work/alpha",
		Language:      SerenaLanguageSentinel,
		Port:          9121,
	}}, "", testMcphubBinary)
	if got != nil {
		t.Fatalf("legacy manifest must yield nil from the dynamic-pool fan-out; got=%#v", got)
	}
}

// TestBuildSupervisorIntent_RejectsContextInTemplate covers the duplicate-context
// defense-in-depth gate (bot PR #246 r2 P2): a daemon_template manifest whose
// extra_args_template already carries --context must yield nil — not a descriptor
// with a doubled --context (buildSerenaChildArgs appends the authoritative one).
// config.ServerManifest.Validate rejects this shape up front, but
// InstallParsedManifest accepts a PRE-PARSED manifest, so the fan-out guards too.
func TestBuildSupervisorIntent_RejectsContextInTemplate(t *testing.T) {
	m := fixtureSerenaManifest()
	m.DaemonTemplate.ExtraArgsTemplate = append([]string{"--context", "codex"}, m.DaemonTemplate.ExtraArgsTemplate...)
	got := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{{
		WorkspacePath: "C:/work/alpha",
		Language:      SerenaLanguageSentinel,
		Port:          9121,
	}}, "", testMcphubBinary)
	if got != nil {
		t.Fatalf("a --context-in-template manifest must yield nil from the fan-out (defense-in-depth); got=%#v", got)
	}
}

// TestBuildSupervisorIntent_AddingWorkspaceAddsDescriptor covers plan
// D.2 incremental-add: start with 2 workspaces, register a 3rd, and
// rebuild the fan-out. Result has exactly 3 descriptors.
func TestBuildSupervisorIntent_AddingWorkspaceAddsDescriptor(t *testing.T) {
	m := fixtureSerenaManifest()
	beforeAdd := []WorkspaceEntry{
		{WorkspacePath: "C:/work/alpha", Language: SerenaLanguageSentinel, Port: 9121},
		{WorkspacePath: "C:/work/beta", Language: SerenaLanguageSentinel, Port: 9122},
	}
	got := BuildSupervisorDaemonsForSerena(m, beforeAdd, "", testMcphubBinary)
	if len(got) != 2 {
		t.Fatalf("before-add: got %d descriptors; want 2", len(got))
	}

	afterAdd := append(beforeAdd, WorkspaceEntry{
		WorkspacePath: "C:/work/gamma",
		Language:      SerenaLanguageSentinel,
		Port:          9123,
	})
	got = BuildSupervisorDaemonsForSerena(m, afterAdd, "", testMcphubBinary)
	if len(got) != 3 {
		t.Fatalf("after-add: got %d descriptors; want 3", len(got))
	}
	want := SerenaTaskNameForWorkspace("C:/work/gamma")
	if got[2].TaskName != want {
		t.Errorf("after-add[2].TaskName: got=%q want=%q", got[2].TaskName, want)
	}
}

// TestBuildSupervisorIntent_RemovingWorkspaceRemovesDescriptor covers
// plan D.2 incremental-remove: start with 3, unregister 1, rebuild.
// Result has exactly 2 descriptors and the unregistered workspace
// task name is absent.
func TestBuildSupervisorIntent_RemovingWorkspaceRemovesDescriptor(t *testing.T) {
	m := fixtureSerenaManifest()
	beforeRemove := []WorkspaceEntry{
		{WorkspacePath: "C:/work/alpha", Language: SerenaLanguageSentinel, Port: 9121},
		{WorkspacePath: "C:/work/beta", Language: SerenaLanguageSentinel, Port: 9122},
		{WorkspacePath: "C:/work/gamma", Language: SerenaLanguageSentinel, Port: 9123},
	}
	got := BuildSupervisorDaemonsForSerena(m, beforeRemove, "", testMcphubBinary)
	if len(got) != 3 {
		t.Fatalf("before-remove: got %d descriptors; want 3", len(got))
	}

	afterRemove := []WorkspaceEntry{
		beforeRemove[0],
		beforeRemove[2],
	}
	got = BuildSupervisorDaemonsForSerena(m, afterRemove, "", testMcphubBinary)
	if len(got) != 2 {
		t.Fatalf("after-remove: got %d descriptors; want 2", len(got))
	}
	betaTaskName := SerenaTaskNameForWorkspace("C:/work/beta")
	for _, d := range got {
		if d.TaskName == betaTaskName {
			t.Fatalf("after-remove still contains beta task name %q", betaTaskName)
		}
	}
}

// TestBuildSupervisorIntent_RejectsKindGlobal verifies the kind-gate:
// a manifest with daemon_template but kind=global returns nil. The
// validator at internal/config/manifest.go:306-307 already rejects this
// combination at parse time; the fan-out enforces it again as defense
// in depth for in-memory constructions.
func TestBuildSupervisorIntent_RejectsKindGlobal(t *testing.T) {
	m := fixtureSerenaManifest()
	m.Kind = config.KindGlobal
	got := BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{
		{WorkspacePath: "C:/work/alpha", Language: SerenaLanguageSentinel, Port: 9121},
	}, "", testMcphubBinary)
	if got != nil {
		t.Fatalf("kind=global must be rejected by fan-out; got=%#v", got)
	}
}

// TestBuildSupervisorIntent_NilManifestReturnsNil verifies nil-manifest
// safety. The caller may pass nil if the manifest load failed; the
// fan-out must not panic.
func TestBuildSupervisorIntent_NilManifestReturnsNil(t *testing.T) {
	got := BuildSupervisorDaemonsForSerena(nil, []WorkspaceEntry{
		{WorkspacePath: "C:/work/alpha", Language: SerenaLanguageSentinel, Port: 9121},
	}, "", testMcphubBinary)
	if got != nil {
		t.Fatalf("nil manifest must yield nil; got=%#v", got)
	}
}

// TestBuildSupervisorIntent_SkipsNonSentinelRows ensures hand-built
// slices containing LSP rows are filtered out - only sentinel rows
// produce a serena descriptor. Defense in depth (canonical entry is
// SerenaEntries which already filters).
func TestBuildSupervisorIntent_SkipsNonSentinelRows(t *testing.T) {
	m := fixtureSerenaManifest()
	workspaces := []WorkspaceEntry{
		{WorkspacePath: "C:/work/alpha", Language: SerenaLanguageSentinel, Port: 9121},
		{WorkspacePath: "C:/work/alpha", Language: "go", Port: 9201},
		{WorkspacePath: "C:/work/beta", Language: SerenaLanguageSentinel, Port: 9122},
	}
	got := BuildSupervisorDaemonsForSerena(m, workspaces, "", testMcphubBinary)
	if len(got) != 2 {
		t.Fatalf("got %d descriptors; want 2 (LSP row filtered out)", len(got))
	}
}

// TestBuildSupervisorIntent_SkipsRowsWithEmptyWorkspacePath verifies
// the defensive guard against the hash-of-empty-string collision: a
// row with WorkspacePath empty is skipped rather than producing a
// task name with the deterministic hash of the empty string.
func TestBuildSupervisorIntent_SkipsRowsWithEmptyWorkspacePath(t *testing.T) {
	m := fixtureSerenaManifest()
	workspaces := []WorkspaceEntry{
		{WorkspacePath: "C:/work/alpha", Language: SerenaLanguageSentinel, Port: 9121},
		{WorkspacePath: "", Language: SerenaLanguageSentinel, Port: 9122},
		{WorkspacePath: "C:/work/gamma", Language: SerenaLanguageSentinel, Port: 9123},
	}
	got := BuildSupervisorDaemonsForSerena(m, workspaces, "", testMcphubBinary)
	if len(got) != 2 {
		t.Fatalf("got %d descriptors; want 2 (empty-path row skipped)", len(got))
	}
}

// TestBuildSupervisorIntent_TaskNameMatchesRegisteredEntry verifies
// the canonical leading-backslash supervisor-intent task name pairs
// with the bare-form task name persisted by workspace_cmd register
// modulo the canonicalIntentTaskKey prepend. This protects against a
// future refactor where the two diverge silently.
func TestBuildSupervisorIntent_TaskNameMatchesRegisteredEntry(t *testing.T) {
	const ws = "C:/work/alpha"
	canonical := SerenaTaskNameForWorkspace(ws)
	bare := canonicalIntentTaskKey("mcp-local-hub-serena-" + WorkspaceKey(ws))
	if canonical != bare {
		t.Fatalf("supervisor-intent vs register task-name shape diverged:\n  fan-out:   %q\n  register+canon: %q", canonical, bare)
	}
}

// TestBuildSupervisorIntent_WindowsBackslashPath verifies byte-blind
// path handling: a workspace path with native Windows backslashes
// flows through to the descriptor's --workspace arg and Workspace
// field unchanged. The fan-out treats WorkspacePath as an opaque
// string; it does NOT normalize separators.
func TestBuildSupervisorIntent_WindowsBackslashPath(t *testing.T) {
	m := fixtureSerenaManifest()
	const backslashPath = `C:\Users\dev\repos\alpha`
	workspaces := []WorkspaceEntry{
		{WorkspacePath: backslashPath, Language: SerenaLanguageSentinel, Port: 9121},
	}
	got := BuildSupervisorDaemonsForSerena(m, workspaces, "", testMcphubBinary)
	if len(got) != 1 {
		t.Fatalf("expected 1 descriptor, got %d", len(got))
	}
	// --workspace and the path must appear as adjacent argv tokens
	// with the backslashes preserved verbatim (no separator
	// normalization).
	wantArgs := []string{"daemon", "serena-proxy", "--server", "serena", "--workspace", backslashPath, "--port", "9121", "--task-name", SerenaTaskNameForWorkspace(backslashPath)}
	if !reflect.DeepEqual(got[0].Args, wantArgs) {
		t.Fatalf("backslash path mangled in Args:\n got=%#v\nwant=%#v", got[0].Args, wantArgs)
	}
	if got[0].Workspace != backslashPath {
		t.Fatalf("Workspace field must preserve backslashes; got=%q want=%q", got[0].Workspace, backslashPath)
	}
}

// TestBuildSupervisorIntent_UnicodePath verifies unicode characters
// in workspace paths flow through unchanged. WorkspaceKey hashes the
// UTF-8 bytes; the fan-out passes the path verbatim into the
// --workspace argv token and the Workspace field.
func TestBuildSupervisorIntent_UnicodePath(t *testing.T) {
	m := fixtureSerenaManifest()
	const unicodePath = "C:/work/проект-альфа"
	workspaces := []WorkspaceEntry{
		{WorkspacePath: unicodePath, Language: SerenaLanguageSentinel, Port: 9121},
	}
	got := BuildSupervisorDaemonsForSerena(m, workspaces, "", testMcphubBinary)
	if len(got) != 1 {
		t.Fatalf("expected 1 descriptor, got %d", len(got))
	}
	if got[0].Workspace != unicodePath {
		t.Fatalf("unicode workspace path mangled; got=%q want=%q", got[0].Workspace, unicodePath)
	}
	wantArgs := []string{"daemon", "serena-proxy", "--server", "serena", "--workspace", unicodePath, "--port", "9121", "--task-name", SerenaTaskNameForWorkspace(unicodePath)}
	if !reflect.DeepEqual(got[0].Args, wantArgs) {
		t.Fatalf("unicode path mangled in Args:\n got=%#v\nwant=%#v", got[0].Args, wantArgs)
	}
}

// TestBuildSupervisorIntent_NilManifestEnvProducesNilDescriptorEnv
// verifies cloneStringMap's nil-safety: when the manifest has no env
// block (m.Env == nil), each descriptor's Env field is nil too
// (not an empty map). This matches the supervisor IPC contract where
// "no env" is canonically encoded as absent rather than empty.
func TestBuildSupervisorIntent_NilManifestEnvProducesNilDescriptorEnv(t *testing.T) {
	m := fixtureSerenaManifest()
	m.Env = nil
	workspaces := []WorkspaceEntry{
		{WorkspacePath: "C:/work/alpha", Language: SerenaLanguageSentinel, Port: 9121},
		{WorkspacePath: "C:/work/beta", Language: SerenaLanguageSentinel, Port: 9122},
	}
	got := BuildSupervisorDaemonsForSerena(m, workspaces, "", testMcphubBinary)
	if len(got) != 2 {
		t.Fatalf("expected 2 descriptors, got %d", len(got))
	}
	for i, d := range got {
		if d.Env != nil {
			t.Fatalf("descriptor[%d].Env must be nil when m.Env==nil; got=%#v", i, d.Env)
		}
	}
}
