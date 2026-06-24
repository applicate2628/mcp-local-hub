package config

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// baseStdioManifest is the minimal valid stdio-bridge manifest the D-2/D-3
// tests start from. Each test mutates only the vendored/availability fields so
// the gate behavior is isolated from unrelated schema rules.
func baseStdioManifest() *ServerManifest {
	return &ServerManifest{
		Name:      "vend",
		Kind:      KindGlobal,
		Transport: TransportStdioBridge,
		Command:   "bash",
		Daemons:   []DaemonSpec{{Name: "default", Port: 9999}},
	}
}

// --- D-2 Gate A: vendored_source pin presence + enum -------------------------

func TestValidate_VendoredSource_PinnedRefAccepted(t *testing.T) {
	cases := []struct {
		name string
		ref  string
	}{
		{"full-40-hex-sha", "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"},
		{"short-sha", "a1b2c3d"},
		{"semver-tag", "v1.2.3"},
		{"annotated-tag", "release-2026-06"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := baseStdioManifest()
			m.VendoredSource = &VendoredSource{Repo: "https://github.com/x/y", PinnedRef: tc.ref}
			if err := m.Validate(); err != nil {
				t.Fatalf("Validate rejected pinned_ref %q: %v", tc.ref, err)
			}
		})
	}
}

func TestValidate_VendoredSource_EmptyPinnedRefRejected(t *testing.T) {
	for _, ref := range []string{"", "   ", "\t"} {
		m := baseStdioManifest()
		m.VendoredSource = &VendoredSource{Repo: "https://github.com/x/y", PinnedRef: ref}
		err := m.Validate()
		if err == nil {
			t.Fatalf("Validate accepted empty pinned_ref %q; want reject", ref)
		}
		if !strings.Contains(err.Error(), "requires a non-empty pinned_ref") {
			t.Fatalf("error %q missing 'requires a non-empty pinned_ref'", err)
		}
	}
}

func TestValidate_VendoredSource_MovingBranchRejected(t *testing.T) {
	for _, ref := range []string{"main", "master", "HEAD", "head", "Latest", "trunk", "develop", "dev"} {
		m := baseStdioManifest()
		m.VendoredSource = &VendoredSource{PinnedRef: ref}
		err := m.Validate()
		if err == nil {
			t.Fatalf("Validate accepted moving ref %q; want reject", ref)
		}
		if !strings.Contains(err.Error(), "requires a non-empty pinned_ref") {
			t.Fatalf("error %q missing pin-required message for moving ref %q", err, ref)
		}
	}
}

func TestValidate_VendoredSource_LicenseStatusEnum(t *testing.T) {
	for _, ls := range []string{"", LicenseStatusConfirmed, LicenseStatusPending, LicenseStatusUnknown} {
		m := baseStdioManifest()
		m.VendoredSource = &VendoredSource{PinnedRef: "v1.0.0", LicenseStatus: ls}
		if err := m.Validate(); err != nil {
			t.Fatalf("Validate rejected license_status %q: %v", ls, err)
		}
	}
	m := baseStdioManifest()
	m.VendoredSource = &VendoredSource{PinnedRef: "v1.0.0", LicenseStatus: "GPL-but-typo"}
	err := m.Validate()
	if err == nil {
		t.Fatalf("Validate accepted bogus license_status; want reject")
	}
	if !strings.Contains(err.Error(), "license_status") {
		t.Fatalf("error %q missing license_status", err)
	}
}

// --- D-3 Gate A: availability enum + install_probe cross-field ---------------

func TestValidate_Availability_Enum(t *testing.T) {
	for _, av := range []string{"", AvailabilityReady} {
		m := baseStdioManifest()
		m.Availability = av
		if err := m.Validate(); err != nil {
			t.Fatalf("Validate rejected availability %q: %v", av, err)
		}
	}
	// watch/disabled require a probe, so pair them with one to isolate the enum.
	for _, av := range []string{AvailabilityWatch, AvailabilityDisabledUntilProbe} {
		m := baseStdioManifest()
		m.Availability = av
		m.InstallProbe = &AvailabilityProbe{Binaries: []string{"matlab"}}
		if err := m.Validate(); err != nil {
			t.Fatalf("Validate rejected availability %q with a probe: %v", av, err)
		}
	}
	m := baseStdioManifest()
	m.Availability = "bogus"
	if err := m.Validate(); err == nil {
		t.Fatalf("Validate accepted bogus availability; want reject")
	}
}

func TestValidate_InstallProbeOnReadyRowRejected(t *testing.T) {
	for _, av := range []string{"", AvailabilityReady} {
		m := baseStdioManifest()
		m.Availability = av
		m.InstallProbe = &AvailabilityProbe{Binaries: []string{"matlab"}}
		err := m.Validate()
		if err == nil {
			t.Fatalf("Validate accepted install_probe on availability=%q; want reject", av)
		}
		if !strings.Contains(err.Error(), "only meaningful with availability=watch") {
			t.Fatalf("error %q missing dead-config message", err)
		}
	}
}

func TestValidate_WatchRowNeedsNonEmptyProbe(t *testing.T) {
	// nil probe.
	m := baseStdioManifest()
	m.Availability = AvailabilityWatch
	if err := m.Validate(); err == nil {
		t.Fatalf("Validate accepted watch row with nil probe; want reject")
	}
	// both-empty-lists probe.
	m2 := baseStdioManifest()
	m2.Availability = AvailabilityDisabledUntilProbe
	m2.InstallProbe = &AvailabilityProbe{}
	err := m2.Validate()
	if err == nil {
		t.Fatalf("Validate accepted watch row with empty probe; want reject")
	}
	if !strings.Contains(err.Error(), "requires a non-empty install_probe") {
		t.Fatalf("error %q missing non-empty-probe message", err)
	}
}

// --- additive / byte-identical guarantees ------------------------------------

// TestParseManifest_VendoredAvailabilityRoundTrips proves the new struct fields
// EXIST: with dec.KnownFields(true), a manifest carrying vendored_source +
// availability + install_probe would be REJECTED at parse if the fields were
// missing. It parses cleanly here, proving the schema additivity.
func TestParseManifest_VendoredAvailabilityRoundTrips(t *testing.T) {
	yamlDoc := `
name: vend
kind: global
transport: stdio-bridge
command: bash
daemons:
  - name: default
    port: 9999
vendored_source:
  repo: "https://github.com/puran-water/mathcad-mcp"
  pinned_ref: "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"
  install_cmd: "uv pip install ."
  run_cmd: "python -m mathcad_mcp"
  license_status: "confirmed"
availability: "watch"
install_probe:
  binaries: ["matlab"]
  files: []
`
	m, err := ParseManifest(strings.NewReader(yamlDoc))
	if err != nil {
		t.Fatalf("ParseManifest rejected vendored+availability manifest: %v", err)
	}
	if m.VendoredSource == nil || m.VendoredSource.PinnedRef != "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2" {
		t.Fatalf("vendored_source not parsed: %#v", m.VendoredSource)
	}
	if m.Availability != AvailabilityWatch {
		t.Fatalf("availability = %q, want watch", m.Availability)
	}
	if m.InstallProbe == nil || len(m.InstallProbe.Binaries) != 1 || m.InstallProbe.Binaries[0] != "matlab" {
		t.Fatalf("install_probe not parsed: %#v", m.InstallProbe)
	}
}

// TestServerManifest_OmitemptyByteIdentical proves a manifest WITHOUT the new
// fields re-marshals without any of the new keys — the additive byte-identical
// guarantee for existing manifests.
func TestServerManifest_OmitemptyByteIdentical(t *testing.T) {
	m := baseStdioManifest()
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(m); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_ = enc.Close()
	out := buf.String()
	for _, key := range []string{"vendored_source", "availability", "install_probe"} {
		if strings.Contains(out, key) {
			t.Fatalf("re-marshaled manifest contains new key %q though it was unset:\n%s", key, out)
		}
	}
}

// TestParseManifest_UnknownFieldStillRejected guards that KnownFields(true) is
// still in force — an unrelated unknown key must reject, proving the new fields
// were added to the struct (not silently tolerated via a loosened decoder).
func TestParseManifest_UnknownFieldStillRejected(t *testing.T) {
	yamlDoc := `
name: vend
kind: global
transport: stdio-bridge
command: bash
daemons:
  - name: default
    port: 9999
totally_unknown_key: oops
`
	if _, err := ParseManifest(strings.NewReader(yamlDoc)); err == nil {
		t.Fatalf("ParseManifest accepted an unknown key; KnownFields(true) not in force")
	}
}

// TestValidate_VendoredSource_BranchQualifiedMovingRefRejected is the FINDING-4
// (D-2) regression: ANY branch-qualified ref (refs/heads/*, refs/remotes/*) is a
// MOVING branch regardless of the bare name — a feature branch like
// refs/heads/release-2026 or refs/remotes/origin/feature/foo is NOT an immutable
// pin. The prior revision stripped the prefix and only rejected the hard-coded
// movingGitRefs names, so a branch-qualified ref with a non-listed name slipped
// past and was accepted as a pin. Only refs/tags/<tag> and a bare SHA/tag are
// immutable and stay ACCEPTED.
func TestValidate_VendoredSource_BranchQualifiedMovingRefRejected(t *testing.T) {
	rejected := []string{
		"refs/heads/main",
		"refs/heads/master",
		"refs/heads/HEAD",
		"refs/remotes/origin/main",
		"refs/remotes/upstream/develop",
		"refs/heads/release-2026",         // non-listed branch name — still a moving branch (codex catalog-r2 P2)
		"refs/heads/feature-x",            // a feature branch is not an immutable pin
		"refs/remotes/origin/feature/foo", // remote-tracking feature branch — moving
		"refs/remotes/origin/feature-y",   // ditto, remote-tracking
		"refs/heads/",                     // degenerate branch-prefix-only ref → empty bare (codex catalog-v3 P2)
		"refs/remotes/origin/",            // degenerate remote-prefix-only ref → empty bare
		"origin/main",                     // <remote>/<branch> shorthand — the r3 gap (bot catalog-r3 P2)
		"upstream/develop",                // ditto — any non-tag slash form is moving
		"feature/x",                       // bare slash non-tag — not an immutable pin
	}
	for _, ref := range rejected {
		t.Run("reject/"+ref, func(t *testing.T) {
			m := baseStdioManifest()
			m.VendoredSource = &VendoredSource{PinnedRef: ref}
			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate accepted branch-qualified moving ref %q; want reject", ref)
			}
			if !strings.Contains(err.Error(), "requires a non-empty pinned_ref") {
				t.Fatalf("error %q missing pin-required message for %q", err, ref)
			}
		})
	}
	accepted := []string{
		"refs/tags/v1",              // immutable tag — fully-qualified
		"refs/tags/release-2026-06", // immutable tag
		"refs/tags/release/2026",    // slash tag under refs/tags is immutable (bot catalog-r3 P2)
		"v1.2.3",                    // bare tag — immutable, allowed
		"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", // 40-hex SHA — immutable, allowed
		"a1b2c3d", // 7-hex short SHA — immutable, allowed
	}
	for _, ref := range accepted {
		t.Run("accept/"+ref, func(t *testing.T) {
			m := baseStdioManifest()
			m.VendoredSource = &VendoredSource{PinnedRef: ref}
			if err := m.Validate(); err != nil {
				t.Fatalf("Validate rejected %q; want accept: %v", ref, err)
			}
		})
	}
}

// TestIsMovingGitRef pins the COMPLETE INVERTIBLE immutability rule the D-2 pin
// gate depends on (bot catalog-r3 P2 — replacing the prior enumerate-bad
// BareGitBranchRef normalizer that let "<remote>/<branch>" shorthand like
// "origin/main" slip through as a pin). A ref is IMMUTABLE (IsMovingGitRef ==
// false) iff it is a 7..40-hex SHA, a "refs/tags/" tag (slash tag OK), or a bare
// non-moving tag name; everything else — including any non-tag slash form — is
// MOVING (== true).
func TestIsMovingGitRef(t *testing.T) {
	immutable := []string{
		"a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2", // 40-hex SHA
		"a1b2c3d",                // 7-hex short SHA
		"refs/tags/v1",           // fully-qualified tag
		"refs/tags/release/2026", // slash tag under refs/tags is immutable
		"v1.2.3",                 // bare tag name
		"  refs/tags/v1  ",       // trimmed
	}
	for _, ref := range immutable {
		if IsMovingGitRef(ref) {
			t.Errorf("IsMovingGitRef(%q) = true, want false (immutable pin)", ref)
		}
	}
	moving := []string{
		"main", "master", "HEAD", "develop", // bare well-known moving branches
		"refs/heads/main",
		"refs/heads/release-2026",
		"refs/heads/",
		"refs/remotes/origin/master",
		"refs/remotes/origin/",
		"origin/main",      // <remote>/<branch> shorthand — the r3 gap
		"upstream/develop", // ditto
		"feature/x",        // bare slash non-tag
		"",                 // empty
		"   ",              // whitespace
		// CHANGE D (finding 2 r5): a degenerate "refs/tags/" with an EMPTY or
		// blank tag suffix carries no tag and is NOT an immutable pin → moving.
		"refs/tags/",
		"refs/tags/   ",
	}
	for _, ref := range moving {
		if !IsMovingGitRef(ref) {
			t.Errorf("IsMovingGitRef(%q) = false, want true (moving / non-immutable)", ref)
		}
	}
}

// TestIsPathShaped is the CHANGE-B lexical taxonomy: a token is path-shaped iff
// it contains a slash/backslash, OR has a drive-letter prefix, OR begins with
// '~'. PLATFORM-NEUTRAL — the verdicts below are IDENTICAL on every GOOS (the
// point of a lexical predicate, not filepath.*). A bare command name is NOT
// path-shaped.
func TestIsPathShaped(t *testing.T) {
	pathShaped := []string{
		"/net/slow/tool", // POSIX absolute
		`C:\tools\x.exe`, // Windows drive-letter + backslash
		`\\host\share\x`, // UNC
		"./tool",         // dot-relative
		"bin/tool",       // relative with slash
		"~/tool",         // home-dir reference
		"C:tools",        // drive-letter prefix, no separator
		"d:rel",          // lowercase drive letter
		`sub\tool`,       // backslash only
		"a/b",            // bare slash
	}
	for _, tok := range pathShaped {
		if !IsPathShaped(tok) {
			t.Errorf("IsPathShaped(%q) = false, want true (path-shaped)", tok)
		}
	}
	bare := []string{
		"matlab", "go", "node", "uvx", // bare command names
		"tool.exe", // an extension is not a path
		"",         // empty is not path-shaped
		"1:thing",  // not a drive letter (digit) — bare
		":x",       // leading colon, no letter — bare
	}
	for _, tok := range bare {
		if IsPathShaped(tok) {
			t.Errorf("IsPathShaped(%q) = true, want false (bare command name)", tok)
		}
	}
}

// TestIsAbsolutePathShape is the CHANGE-C lexical absolute-path taxonomy: a token
// is absolute-path-shaped iff it has a drive-letter ABSOLUTE prefix ("<letter>:\\"
// or "<letter>:/") OR begins with '/' OR '\\' (incl UNC). GOOS-INDEPENDENT —
// "C:\\marker" AND "/opt/marker" AND "\\\\host\\share" are all absolute on EVERY
// build platform; relative + '~' forms are not. This is the cross-platform-registry
// property: filepath.IsAbs would split these by host OS.
//
// codex r6 finding 1: a bare "C:marker" / "d:rel" (drive letter + ':' WITHOUT a
// separator) is Windows DRIVE-RELATIVE — it resolves against the CWD on that
// drive, so os.Stat depends on the process working directory. It is therefore NOT
// absolute (it falls to the files[] relative-path reject), while a drive prefix
// FOLLOWED by a separator ("C:\\m" / "C:/m") IS absolute.
func TestIsAbsolutePathShape(t *testing.T) {
	absolute := []string{
		"/opt/marker",    // POSIX absolute
		`C:\marker`,      // Windows drive absolute (backslash separator)
		"C:/marker",      // drive + forward slash
		`C:\m`,           // short drive + backslash
		"C:/m",           // short drive + forward slash
		`d:\rel`,         // lowercase drive + backslash
		`\\host\share\m`, // UNC
		`\marker`,        // leading backslash (Windows root-relative absolute shape)
	}
	for _, tok := range absolute {
		if !IsAbsolutePathShape(tok) {
			t.Errorf("IsAbsolutePathShape(%q) = false, want true (absolute-path-shaped)", tok)
		}
	}
	notAbsolute := []string{
		"./marker", // dot-relative
		"marker",   // bare relative
		"~/marker", // home reference is NOT absolute
		"sub/marker",
		"",         // empty
		"1:thing",  // digit, not a drive letter
		"C:marker", // drive-RELATIVE (no separator) — resolves against CWD on that drive
		"d:rel",    // lowercase drive-relative — NOT absolute
		"C:",       // bare drive prefix, no separator
	}
	for _, tok := range notAbsolute {
		if IsAbsolutePathShape(tok) {
			t.Errorf("IsAbsolutePathShape(%q) = true, want false (not absolute)", tok)
		}
	}
}

// TestValidate_InstallProbe_PathShapedBinaryRejected is the CHANGE-B1 (finding 3
// r5) regression: a binaries[] entry that is a PATH (slash/drive/UNC/~) is a
// category error — binaries are exec.LookPath'd (PATH-searched), so a path-shaped
// token silently never resolves. Reject it at validate, naming the offending
// value. The architect section-E binaries[] table.
func TestValidate_InstallProbe_PathShapedBinaryRejected(t *testing.T) {
	reject := []struct {
		name string
		bin  string
	}{
		{"posix-abs", "/net/slow/tool"},
		{"windows-drive", `C:\tools\x.exe`},
		{"unc", `\\host\share\x`},
		{"dot-relative", "./tool"},
		{"relative-slash", "bin/tool"},
		{"home-ref", "~/tool"},
	}
	for _, tc := range reject {
		t.Run(tc.name, func(t *testing.T) {
			m := baseStdioManifest()
			m.Availability = AvailabilityWatch
			m.InstallProbe = &AvailabilityProbe{Binaries: []string{tc.bin}}
			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a path-shaped binary %q; want reject", tc.bin)
			}
			if !strings.Contains(err.Error(), "is a path, not a PATH-searchable name") {
				t.Fatalf("error %q missing the path-not-a-name message", err)
			}
		})
	}
	// A BARE binary name is accepted (matlab → browse-eval), proving the gate does
	// not over-reject. Pair with a host-absolute file so the files[] rule is met.
	m := baseStdioManifest()
	m.Availability = AvailabilityWatch
	m.InstallProbe = &AvailabilityProbe{Binaries: []string{"matlab"}, Files: []string{filepath.Join(t.TempDir(), "marker")}}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate rejected a bare binary + absolute file probe: %v", err)
	}
}

// TestValidate_InstallProbe_CrossPlatformFilePathAccepted is the CHANGE-C
// (finding 4 r5) regression: a files[] entry that is absolute on SOME host OS
// (drive-letter, leading '/', or UNC) must be ACCEPTED at parse/validate on ANY
// build platform — IsAbsolutePathShape replaces filepath.IsAbs so a
// cross-platform registry declaring "C:\\marker" or "/opt/marker" parses
// identically on linux AND windows builds. Relative + '~' forms still reject
// (CWD-protection). "/opt/.." is accepted lexically (os.Stat resolves it at
// install — we deliberately do NOT ban ".."). The architect section-E files[]
// table.
func TestValidate_InstallProbe_CrossPlatformFilePathAccepted(t *testing.T) {
	accept := []string{
		"/opt/marker",         // POSIX absolute — accepted on a windows build too
		`C:\marker`,           // Windows absolute — accepted on a linux build too
		`\\host\share\marker`, // UNC
		"/opt/..",             // lexically absolute; os.Stat resolves at install (no ..-ban)
		// Tier-1 glob enhancement: a files[] entry MAY carry glob metacharacters
		// (`*`/`?`/`[`) for a version-agnostic SHARED-catalog probe. It is still an
		// absolute path (with wildcards), so it must pass IsAbsolutePathShape; the
		// metacharacters are NOT rejected (the runtime fileProbeMatches expands them
		// via filepath.Glob). These are the two real first-batch catalog probes.
		`C:\ProgramData\Ableton\Live * Suite\Program\Ableton Live * Suite.exe`, // ableton (Live 11/12)
		`C:\Program Files*\Microsoft Office\root\Office1?\EXCEL.EXE`,           // excel (64-bit + (x86) C2R)
		"/opt/app-*/bin/server",                                                // POSIX glob, also cross-platform
	}
	for _, f := range accept {
		t.Run("accept-"+f, func(t *testing.T) {
			m := baseStdioManifest()
			m.Availability = AvailabilityWatch
			m.InstallProbe = &AvailabilityProbe{Files: []string{f}}
			if err := m.Validate(); err != nil {
				t.Fatalf("Validate rejected a host-absolute file probe %q on this build platform: %v", f, err)
			}
		})
	}
	reject := []struct {
		name string
		file string
	}{
		{"dot-relative", "./marker"},
		{"bare-relative", "marker"},
		{"home-ref", "~/marker"},
		{"nested-relative", "sub/marker"},
		// codex r6 finding 1: a drive-letter prefix WITHOUT a separator is Windows
		// drive-RELATIVE (resolves against the CWD on that drive), so os.Stat would
		// depend on the process working directory — reject it like any relative path.
		{"drive-relative", "C:marker"},
		{"drive-relative-lower", "d:rel"},
	}
	for _, tc := range reject {
		t.Run("reject-"+tc.name, func(t *testing.T) {
			m := baseStdioManifest()
			m.Availability = AvailabilityWatch
			m.InstallProbe = &AvailabilityProbe{Files: []string{tc.file}}
			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a non-absolute file probe %q; want reject", tc.file)
			}
			if !strings.Contains(err.Error(), "must be an absolute path") {
				t.Fatalf("error %q missing the absolute-path message", err)
			}
		})
	}
}

// TestValidate_InstallProbe_EmptyProbeValueRejected is the FINDING-4 (D-3 A7)
// regression: a probe whose binaries[] or files[] carries a blank token passes
// the slice-length check yet can never satisfy the runtime probe (it looks up an
// empty name) → a permanently disabled row. Reject it up front.
func TestValidate_InstallProbe_EmptyProbeValueRejected(t *testing.T) {
	cases := []struct {
		name  string
		probe *AvailabilityProbe
		want  string
	}{
		{"empty-binary", &AvailabilityProbe{Binaries: []string{""}}, "binaries[0] is empty"},
		{"whitespace-binary", &AvailabilityProbe{Binaries: []string{"   "}}, "binaries[0] is empty"},
		{"empty-file", &AvailabilityProbe{Files: []string{""}}, "files[0] is empty"},
		{"second-binary-blank", &AvailabilityProbe{Binaries: []string{"matlab", "\t"}}, "binaries[1] is empty"},
		{"good-bin-empty-file", &AvailabilityProbe{Binaries: []string{"matlab"}, Files: []string{""}}, "files[0] is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := baseStdioManifest()
			m.Availability = AvailabilityWatch
			m.InstallProbe = tc.probe
			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate accepted probe with a blank value (%s); want reject", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err, tc.want)
			}
		})
	}
	// A fully-populated probe still passes (no false positive). The file probe
	// uses a HOST-ABSOLUTE path (t.TempDir is absolute on every OS — a hardcoded
	// POSIX "/opt/..." is NOT absolute under filepath.IsAbs on Windows).
	m := baseStdioManifest()
	m.Availability = AvailabilityWatch
	m.InstallProbe = &AvailabilityProbe{Binaries: []string{"matlab"}, Files: []string{filepath.Join(t.TempDir(), "marker")}}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate rejected a fully-populated probe: %v", err)
	}
}

// TestValidate_InstallProbe_WhitespacePaddedValueRejected is the FINDING-3 (codex
// catalog r4 P2) regression: a probe value with leading/trailing whitespace (e.g.
// "go ") passes the trimmed-non-empty check yet the runtime probe passes the
// ORIGINAL padded token to exec.LookPath / os.Stat, so the row never enables even
// when the tool is installed (invisible-whitespace permanent-disable). Reject the
// padded value up front, naming the offending token.
func TestValidate_InstallProbe_WhitespacePaddedValueRejected(t *testing.T) {
	cases := []struct {
		name  string
		probe *AvailabilityProbe
		want  string
	}{
		{"trailing-space-binary", &AvailabilityProbe{Binaries: []string{"go "}}, "binaries[0] \"go \" has leading/trailing whitespace"},
		{"leading-space-binary", &AvailabilityProbe{Binaries: []string{" go"}}, "binaries[0] \" go\" has leading/trailing whitespace"},
		{"trailing-tab-file", &AvailabilityProbe{Files: []string{"/opt/x/marker\t"}}, "files[0] \"/opt/x/marker\\t\" has leading/trailing whitespace"},
		{"good-bin-padded-file", &AvailabilityProbe{Binaries: []string{"go"}, Files: []string{" /opt/x/marker"}}, "files[0] \" /opt/x/marker\" has leading/trailing whitespace"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := baseStdioManifest()
			m.Availability = AvailabilityWatch
			m.InstallProbe = tc.probe
			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a whitespace-padded probe value (%s); want reject", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err, tc.want)
			}
		})
	}
	// A clean (un-padded) probe still passes. Host-absolute file path so the
	// IsAbs rule is satisfied on every OS (t.TempDir is absolute everywhere).
	m := baseStdioManifest()
	m.Availability = AvailabilityWatch
	m.InstallProbe = &AvailabilityProbe{Binaries: []string{"go"}, Files: []string{filepath.Join(t.TempDir(), "marker")}}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate rejected a clean probe: %v", err)
	}
}

// TestValidate_InstallProbe_RelativeFilePathRejected is the FINDING-2 (codex
// catalog r4 P2) regression: a relative install_probe.files[] entry (e.g.
// "marker") is os.Stat'd as-is by the runtime probe, so the gate would depend on
// the GUI/CLI process working directory — an unrelated "./marker" in one dir
// enables the row while the same manifest stays blocked from another dir. Reject
// a relative file probe at validation; an absolute path is accepted. binaries[]
// are NOT paths (exec.LookPath resolves a bare name against PATH), so a bare
// binary name is unaffected.
func TestValidate_InstallProbe_RelativeFilePathRejected(t *testing.T) {
	cases := []struct {
		name  string
		probe *AvailabilityProbe
		want  string
	}{
		{"bare-relative-file", &AvailabilityProbe{Files: []string{"marker"}}, "files[0] \"marker\" must be an absolute path"},
		{"dot-relative-file", &AvailabilityProbe{Files: []string{"./marker"}}, "must be an absolute path"},
		{"nested-relative-file", &AvailabilityProbe{Files: []string{"sub/marker"}}, "must be an absolute path"},
		{"good-bin-relative-file", &AvailabilityProbe{Binaries: []string{"go"}, Files: []string{"marker"}}, "files[0] \"marker\" must be an absolute path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := baseStdioManifest()
			m.Availability = AvailabilityWatch
			m.InstallProbe = tc.probe
			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a relative file probe (%s); want reject", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err, tc.want)
			}
		})
	}
	// A bare binary name (NOT a path) plus a HOST-absolute file probe is accepted.
	m := baseStdioManifest()
	m.Availability = AvailabilityWatch
	m.InstallProbe = &AvailabilityProbe{Binaries: []string{"go"}, Files: []string{filepath.Join(t.TempDir(), "marker")}}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate rejected a bare binary + absolute file probe: %v", err)
	}
}
