package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"mcp-local-hub/servers"
)

// deAdoptManifestReadDir is the filesystem seam for the de-adopt-only strict
// manifest enumeration. Production uses os.ReadDir; executor tests replace it
// to reproduce a non-ENOENT directory-listing failure deterministically.
var deAdoptManifestReadDir = os.ReadDir

// manifestDirForTests is a test-only override consulted by
// ScanManifestEnv and the embed-aware helpers when set via
// MCPHUB_MANIFEST_DIR_OVERRIDE. When the override is non-empty the
// embed FS is bypassed entirely; tests get the test directory's
// manifests with no leakage from the binary's shipped set
// (which include `secret:` refs from wolfram, paper-search-mcp).
func manifestDirForTests() string {
	return os.Getenv("MCPHUB_MANIFEST_DIR_OVERRIDE")
}

// Manifest-source abstraction.
//
// Read-side API calls prefer the embedded manifest set and use the canonical
// installation manifest directory only for disk fallback.
//
// Fix: all read paths go through embeddedManifestNames /
// loadManifestYAMLEmbedFirst, which prefer the embed FS (the source of
// truth shipped with the binary) and fall back to disk only when the
// embed is empty (dev-checkout dev-flow without a rebuild).
//
// Write paths (ManifestCreate / ManifestEdit / ManifestDelete) continue
// to use disk. Editing the embedded FS at runtime is impossible; write
// ops are a dev-workflow feature and documented as such.

// embeddedManifestNames returns the sorted list of server names that
// have a manifest.yaml baked into the binary.
func embeddedManifestNames() []string {
	names, _ := embeddedManifestNamesObserved()
	return names
}

func embeddedManifestNamesObserved() ([]string, error) {
	entries, err := fs.ReadDir(servers.Manifests, ".")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// embed.FS contains only paths declared in //go:embed, so every
		// subdirectory here is guaranteed to have a manifest.yaml.
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// embeddedManifestNamesContains reports RAW membership in the binary's embed
// FS — the OVERRIDE-INDEPENDENT core of the embed-name question. It reads the
// real embed set directly (embeddedManifestNames ignores
// MCPHUB_MANIFEST_DIR_OVERRIDE), so it answers "is this exact name baked into
// servers.Manifests?" regardless of any test seam.
//
// CatalogManifestGet dedups its inline membership loop onto this core: that
// endpoint is DELIBERATELY override-independent (it reads the embed FS directly
// for secret-safety, so a disk manifest with a literal secret is unreachable),
// so it must NOT go through the override-symmetric isEmbeddedManifestName below.
func embeddedManifestNamesContains(name string) bool {
	for _, n := range embeddedManifestNames() {
		if n == name {
			return true
		}
	}
	return false
}

// isEmbeddedManifestName is the WRITE-GATE / warn / readiness / marketplace
// membership predicate: it mirrors loadManifestYAMLEmbedFirst's embed branch
// EXACTLY, INCLUDING the test-override symmetry. When
// MCPHUB_MANIFEST_DIR_OVERRIDE is set the loader bypasses the embed FS and
// reads the override dir (manifest_source.go:77-79), so for the purposes of
// "would a disk create/read shadow a SHIPPED manifest?" the answer is false —
// there is no shipped manifest in play under the override. That keeps the
// predicate byte-symmetric with the read path it guards, so hermetic tests
// that seed a temp manifest dir via the override are unaffected by the
// embed-name write refusal.
//
// It is layered on the override-independent core embeddedManifestNamesContains,
// so the raw membership loop is single-owner; only the override symmetry is
// added here (that is the sole difference from the CatalogManifestGet core).
func isEmbeddedManifestName(name string) bool {
	if manifestDirForTests() != "" {
		return false
	}
	return embeddedManifestNamesContains(name)
}

// IsEmbeddedManifestName is the exported gate for callers outside this package
// (the internal/gui readiness Save-&-Install dry-run mirror) that must reflect
// the same embed-membership decision the ManifestCreateIn write gate applies.
// (The marketplace handler does NOT use this predicate — it maps the write
// gate's ErrManifestNameEmbedded refusal via errors.Is instead.) It mirrors
// the CheckManifestName/checkManifestName exported-wrapper idiom so the
// predicate stays single-owner across package boundaries.
func IsEmbeddedManifestName(name string) bool {
	return isEmbeddedManifestName(name)
}

// embeddedDiskShadowWarning returns a human-readable warn line when `name` is a
// shipped (embedded) server AND a same-named manifest.yaml ALSO exists on disk
// under manifestDir — the disk-fallback location the embed-first loader
// (loadManifestYAMLEmbedFirst) consults only on an embed MISS. Because the
// embed read wins for a shipped name, that disk manifest is silently ignored at
// install: the operator's edits never take effect. The warn says so and points
// at the rename remedy.
//
// This is the READ-side surface for PRE-EXISTING collisions (new ones are
// refused at the ManifestCreateIn write gate); it NEVER deletes the user's
// file. Empty string == no collision (name not embedded — which includes the
// test-override case via isEmbeddedManifestName — or no disk file), so callers
// emit nothing. Single owner of the collision predicate + message; each
// consumer routes the returned string to its own sink (install → writer,
// scan → log).
//
// IDENTICAL-BYTES SUPPRESSION: a disk copy whose bytes EQUAL the embed
// manifest changes nothing — the embed read serving instead of the disk read
// is unobservable — so warning about it is pure noise. The dominant producer
// of identical copies is a dev checkout / built binary sitting next to the
// canonical `servers/` tree, where every shipped server would otherwise warn
// on every install --all
// and every scan. Only a DIFFERING disk copy (the operator actually edited
// something the embed read silently discards) warns.
// DiskShadowWarningObservation is the raw snapshot of an embedded manifest
// shadow that a long-lived caller may deduplicate. Identity changes whenever
// the name, embedded bytes, or disk bytes change.
type DiskShadowWarningObservation struct {
	Name     string
	Message  string
	Identity [sha256.Size]byte
}

func embeddedDiskShadowObservation(name, manifestDir string) (DiskShadowWarningObservation, bool) {
	if !isEmbeddedManifestName(name) {
		return DiskShadowWarningObservation{}, false
	}
	if manifestDir == "" {
		return DiskShadowWarningObservation{}, false
	}
	// isEmbeddedManifestName(name) == true implies `name` literally matched an
	// embed FS directory entry, so it is join-safe by construction (no
	// separators / traversal). Read the disk shadow directly: any read failure
	// (IsNotExist, permission) means there is no observable shadow to warn
	// about — same no-warn outcome the earlier stat-based check produced.
	diskBytes, err := os.ReadFile(filepath.Join(manifestDir, name, "manifest.yaml"))
	if err != nil {
		return DiskShadowWarningObservation{}, false
	}
	// Identical bytes → pure noise, skip (see header). An embed read failure
	// cannot really happen for a name proven in the embed set; if it somehow
	// does, fall through to the truthful warn (conservative).
	if embedBytes, err := fs.ReadFile(servers.Manifests, name+"/manifest.yaml"); err == nil && bytes.Equal(diskBytes, embedBytes) {
		return DiskShadowWarningObservation{}, false
	}
	message := fmt.Sprintf(
		"disk manifest for shipped server %q is ignored; the shipped built-in manifest is what installs — rename your copy (e.g. %q) to install a customized version",
		name, name+"-custom",
	)
	embedBytes, _ := fs.ReadFile(servers.Manifests, name+"/manifest.yaml")
	identity := sha256.Sum256(bytes.Join([][]byte{[]byte(name), embedBytes, diskBytes}, []byte{0}))
	return DiskShadowWarningObservation{Name: name, Message: message, Identity: identity}, true
}

// embeddedDiskShadowWarning retains the existing one-shot string projection
// used by install and direct callers.
func embeddedDiskShadowWarning(name, manifestDir string) string {
	observation, ok := embeddedDiskShadowObservation(name, manifestDir)
	if !ok {
		return ""
	}
	return observation.Message
}

// loadManifestYAMLEmbedFirst returns the raw YAML bytes for the named
// server. Consults the embed FS first; on miss (server not in the
// binary's shipped set), falls back to the on-disk dev-checkout path.
//
// checkManifestName runs at the loader boundary so a bad server name
// cannot drive a pre-validation filesystem probe (existence check,
// special-file open, etc.) via callers that validated only after the
// raw read. Production wrappers ManifestGet/ManifestEdit/etc. already
// gate on the same regex, so the redundant call here is cheap; the
// guard exists for the direct callers (install, uninstall, scan,
// status_enrich, secrets_scan, migrate) that compose `name` straight
// into the loader without their own pre-load check.
func loadManifestYAMLEmbedFirst(name string) ([]byte, error) {
	observation := loadManifestSourceObserved(name, nil)
	return bytes.Clone(observation.bytes), observation.selectionError
}

// The original selection owner also serves consumers retaining observed reads.
func loadManifestYAMLEmbedFirstWithReader(name string, readFile func(string) ([]byte, error)) ([]byte, error) {
	observation := selectManifestSource(name, nil, func(kind, path string) manifestSourceReadAttempt {
		if kind == "embedded" {
			return readManifestSourceAttempt(kind, path)
		}
		raw, err := readFile(path)
		return manifestSourceReadAttempt{kind: kind, path: path, bytes: raw, readError: err}
	})
	return bytes.Clone(observation.bytes), observation.selectionError
}

func loadManifestYAMLEmbedFirstFromDir(name, diskDir string) ([]byte, error) {
	observation := loadManifestSourceObserved(name, &diskDir)
	return bytes.Clone(observation.bytes), observation.selectionError
}

type manifestSourceReadAttempt struct {
	kind           string
	path           string
	reader         fs.File
	bytes          []byte
	opened         bool
	closeAttempted bool
	closeDefined   bool
	readError      error
	closeError     error
}

type ManifestSourceObservation struct {
	name           string
	selectedKind   string
	selectedPath   string
	bytes          []byte
	attempts       []manifestSourceReadAttempt
	selectionError error
}

type manifestSourceObservationFailure struct {
	observation *ManifestSourceObservation
	cause       error
}

func (e *manifestSourceObservationFailure) Error() string {
	return "GRAPHIFY_SELECTED_MANIFEST_READ_UNQUALIFIED"
}
func (e *manifestSourceObservationFailure) Unwrap() error { return e.cause }
func (ManifestSourceObservation) String() string          { return "manifest source observation (private)" }
func (o ManifestSourceObservation) GoString() string      { return o.String() }
func (o *ManifestSourceObservation) Bytes() []byte {
	if o == nil {
		return nil
	}
	return bytes.Clone(o.bytes)
}

// One selector core preserves the ordinary loader's fallback and error policy.
// Strict observation evaluates all actual prior acquisition and Close outcomes
// separately, without rewriting a failed read as absence or a successful seal.
func loadManifestSourceObserved(name string, diskDir *string) *ManifestSourceObservation {
	return selectManifestSource(name, diskDir, readManifestSourceAttempt)
}

func selectManifestSource(name string, diskDir *string, acquire func(string, string) manifestSourceReadAttempt) *ManifestSourceObservation {
	observation := &ManifestSourceObservation{name: name}
	if err := checkManifestName(name); err != nil {
		observation.selectionError = err
		return observation
	}
	if dir := manifestDirForTests(); dir != "" {
		read := acquire("override", filepath.Join(dir, name, "manifest.yaml"))
		observation.attempts = append(observation.attempts, read)
		observation.selectedKind, observation.selectedPath = read.kind, read.path
		observation.bytes, observation.selectionError = read.bytes, read.readError
		return observation
	}
	embedded := acquire("embedded", name+"/manifest.yaml")
	observation.attempts = append(observation.attempts, embedded)
	if embedded.readError == nil {
		observation.selectedKind, observation.selectedPath = embedded.kind, embedded.path
		observation.bytes = embedded.bytes
		return observation
	}
	dir := ""
	if diskDir != nil {
		dir = *diskDir
	} else {
		var err error
		dir, err = CanonicalManifestDir()
		if err != nil {
			observation.selectionError = fmt.Errorf("load manifest %s: %w", name, err)
			return observation
		}
	}
	disk := acquire("disk", filepath.Join(dir, name, "manifest.yaml"))
	observation.attempts = append(observation.attempts, disk)
	observation.selectedKind, observation.selectedPath = disk.kind, disk.path
	observation.bytes, observation.selectionError = disk.bytes, disk.readError
	return observation
}

func readManifestSourceAttempt(kind, path string) (observation manifestSourceReadAttempt) {
	var file fs.File
	var err error
	if kind == "embedded" {
		file, err = servers.Manifests.Open(path)
	} else {
		var original *os.File
		original, err = os.Open(path)
		if original != nil {
			file = original
		}
	}
	return consumeManifestSourceAttempt(kind, path, file, err)
}

func consumeManifestSourceAttempt(kind, path string, file fs.File, openErr error) (observation manifestSourceReadAttempt) {
	observation.kind, observation.path, observation.readError = kind, path, openErr
	if file == nil {
		if openErr == nil {
			observation.readError = errors.New("manifest original reader unavailable")
		}
		return observation
	}
	observation.reader = file
	observation.opened = true
	defer func() {
		observation.closeAttempted = true
		observation.closeError = file.Close()
		observation.closeDefined = true
	}()
	if openErr != nil {
		return observation
	}
	observation.bytes, observation.readError = io.ReadAll(file)
	return observation
}

func (o *ManifestSourceObservation) ValidateProjectServiceLaunchRead(ctx context.Context) error {
	if o == nil {
		return errors.New("GRAPHIFY_SELECTED_MANIFEST_OBSERVATION_UNISSUED")
	}
	if ctx == nil || ctx.Err() != nil || o.name == "" || o.selectedKind == "" || len(o.attempts) == 0 {
		cause := errors.Join(o.selectionError, errors.New("GRAPHIFY_SELECTED_MANIFEST_OBSERVATION_UNISSUED"))
		if ctx != nil {
			cause = errors.Join(cause, ctx.Err())
		}
		return &manifestSourceObservationFailure{observation: o, cause: cause}
	}
	cause := o.selectionError
	for _, read := range o.attempts {
		if read.opened && (read.reader == nil || !read.closeAttempted || !read.closeDefined) {
			cause = errors.Join(cause, errors.New("manifest close unavailable"))
		}
		cause = errors.Join(cause, read.closeError)
		if read.readError != nil && !(read.kind == "embedded" && errors.Is(read.readError, fs.ErrNotExist) && o.selectedKind == "disk") {
			cause = errors.Join(cause, read.readError)
		}
	}
	if cause != nil {
		return &manifestSourceObservationFailure{observation: o, cause: cause}
	}
	return nil
}

func (o *ManifestSourceObservation) SameSelection(other *ManifestSourceObservation) bool {
	return o != nil && other != nil && o.name == other.name && o.selectedKind == other.selectedKind && o.selectedPath == other.selectedPath && bytes.Equal(o.bytes, other.bytes)
}

// listManifestNamesEmbedFirst returns the set of available server
// names, unioning embed and disk so a dev-added manifest still shows
// up before a rebuild.
func listManifestNamesEmbedFirst() ([]string, error) {
	if dir := manifestDirForTests(); dir != "" {
		// Test-only override: skip the embed FS entirely so tests get
		// only the manifests they explicitly seed.
		entries, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		var names []string
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "manifest.yaml")); err == nil {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		return names, nil
	}
	dir, err := CanonicalManifestDir()
	if err != nil {
		return nil, fmt.Errorf("list manifests: %w", err)
	}
	return listManifestNamesEmbedFirstFromDir(dir)
}

func listManifestNamesEmbedFirstFromDir(dir string) ([]string, error) {
	// Production path: union embed + disk.
	seen := map[string]bool{}
	for _, n := range embeddedManifestNames() {
		seen[n] = true
	}
	// Union with canonical disk state so dev-created manifests appear before
	// they are compiled into the binary.
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		// Disk read failure is non-fatal — return what we have from embed.
		// The common case on an installed binary with no source tree is
		// that the canonical manifest directory does not exist.
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "manifest.yaml")); err == nil {
			seen[e.Name()] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// listManifestNamesEmbedFirstStrict is the fail-closed manifest enumerator for
// de-adopt's routed-secret scan. Unlike the install-facing forgiving lister, a
// non-ENOENT disk listing failure is fatal because an incomplete manifest set
// could make a shared routed secret appear unique. Every directory name is
// included without a manifest.yaml stat gate so a later load failure reaches
// de-adopt's existing conservative preserve-all-candidates branch.
func listManifestNamesEmbedFirstStrict() ([]string, error) {
	dir := manifestDirForTests()
	seen := map[string]bool{}
	if dir == "" {
		for _, name := range embeddedManifestNames() {
			seen[name] = true
		}
		var err error
		dir, err = CanonicalManifestDir()
		if err != nil {
			return nil, fmt.Errorf("list manifests strictly: %w", err)
		}
	}

	entries, err := deAdoptManifestReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			seen[entry.Name()] = true
		}
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}
