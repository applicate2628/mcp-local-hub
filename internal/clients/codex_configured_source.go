package clients

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mcp-local-hub/internal/process"
)

// This shared supported producer definition is a protocol/cohort version, not a
// build attestation or deployed image fingerprint. Route bytes are observed per
// operation. Unsupported releases require a separately reviewed definition.
type codexConfiguredDefinition struct {
	version, commit, defaultsURI string
}

func codexConfiguredProducerDefinition(version string) (codexConfiguredDefinition, error) {
	var commit string
	switch version {
	case "0.162.0":
		commit = "c1382380de69521303b416720a52f42d51af6248"
	case "0.162.1":
		commit = "092d3acd6bec3e3a14bdc7e7a2810ab628ab759d"
	default:
		return codexConfiguredDefinition{}, errors.New("native package definition version unsupported")
	}
	return codexConfiguredDefinition{version: version, commit: commit,
		defaultsURI: "https://raw.githubusercontent.com/openai/codex/" + commit + "/codex-rs/config/defaults.toml"}, nil
}

// Byte-exact inspected source definition (554 bytes, SHA256 C669759D...).
// This binds the embedded-default semantics of the supported package/version;
// it does not assert a reproducible build of the observed deployed native image.
const codexConfiguredPackagedDefaults = `# Fixed defaults for packaged Codex clients.
include_permissions_instructions = true
include_apps_instructions = true
include_collaboration_mode_instructions = true
include_environment_context = true
cli_auth_credentials_store = "file"
mcp_oauth_credentials_store = "auto"
project_doc_max_bytes = 32768
project_doc_fallback_filenames = []
background_terminal_max_timeout = 300000
file_opener = "vscode"
hide_agent_reasoning = false
chatgpt_base_url = "https://chatgpt.com/backend-api/"
project_root_markers = [".git"]

[history]
persistence = "save-all"
`

type CodexConfiguredProjectProfileReader interface {
	ReadConfiguredProjectProfile(context.Context, string) (*CodexConfiguredProjectProfile, error)
}

// Only the bound source operation issues this value. Accidental JSON exposes no
// private paths, configuration, native captures, environment or credentials.
type CodexConfiguredProjectProfile struct {
	owner                    *lockingClient
	logical                  string
	binding                  codexConfiguredSourceBinding
	fingerprint              string
	configured               codexConfiguredMCP
	global, project, payload []byte
	result                   process.StrictRunResult
	globalRead               configLockExecution
	projectRead              ProjectConfigLockExecution
	before, after            codexConfiguredContext
}

func (CodexConfiguredProjectProfile) String() string {
	return "configured project observation (private)"
}
func (p CodexConfiguredProjectProfile) GoString() string { return p.String() }

func (p *CodexConfiguredProjectProfile) Fingerprint() (string, error) {
	if p == nil || p.owner == nil || p.fingerprint == "" {
		return "", ErrProjectClientInvalid
	}
	return p.fingerprint, nil
}

func (p *CodexConfiguredProjectProfile) ConfiguredEnabled() (bool, error) {
	if _, err := p.Fingerprint(); err != nil {
		return false, err
	}
	return p.configured.enabled, nil
}

// Observation is a display of configured settings, never launched-state proof.
// A caller cannot instantiate the private ordered binding to acquire authority.
type codexConfiguredSourceBinding struct {
	Schema                  string `json:"schema"`
	Client                  string `json:"client"`
	ProjectIdentitySHA256   string `json:"project_identity_sha256"`
	LogicalKeySHA256        string `json:"logical_key_sha256"`
	QueryCWDSHA256          string `json:"query_cwd_sha256"`
	NativeRouteSHA256       string `json:"native_route_sha256"`
	LoaderContextSHA256     string `json:"loader_context_sha256"`
	LayerSetSHA256          string `json:"layer_set_sha256"`
	ConfiguredPayloadSHA256 string `json:"configured_payload_sha256"`
	ConfiguredEnabled       bool   `json:"configured_enabled"`
	ConfiguredPolicySHA256  string `json:"configured_policy_sha256"`
	RawPolicySHA256         string `json:"raw_policy_sha256"`
}

type codexConfiguredSourceFailure struct {
	cause                       error
	result                      process.StrictRunResult
	globalRead                  configLockExecution
	projectRead                 ProjectConfigLockExecution
	before, after               codexConfiguredContext
	original, received          *CodexConfiguredProjectProfile
	bindingBefore, bindingAfter *codexConfiguredGlobalBinding
	projectSourceRead           *codexConfiguredFile
	replaySourceBefore          *CodexSharedStdioSourceEvidenceV1
	replayLocationBefore        *CodexConfiguredGlobalLocationV1
}

func (e *codexConfiguredSourceFailure) Error() string {
	return "PROJECT_CLIENT_SOURCE_PROFILE_UNAVAILABLE"
}
func (e *codexConfiguredSourceFailure) Unwrap() error { return e.cause }
func (codexConfiguredSourceFailure) String() string {
	return "PROJECT_CLIENT_SOURCE_PROFILE_UNAVAILABLE"
}
func (e codexConfiguredSourceFailure) GoString() string { return e.String() }

type codexConfiguredLayer struct {
	PathSHA256     string  `json:"path_sha256"`
	Present        bool    `json:"present"`
	IdentitySHA256 *string `json:"identity_sha256"`
	ContentSHA256  *string `json:"content_sha256"`
}

type codexConfiguredFile struct {
	layer                                                    codexConfiguredLayer
	info                                                     os.FileInfo
	objectIdentity                                           *CodexConfiguredObjectIdentityV1
	bytes                                                    []byte
	reader                                                   *os.File
	readEntered, readReturned, closeAttempted, closeReturned bool
	readError, closeError, ownerError                        error
	globalBinding                                            *codexConfiguredGlobalBinding
	globalAfter                                              *codexConfiguredGlobalBinding
}

type codexConfiguredPathObservation struct {
	path, rawLinkTarget                                                string
	info                                                               os.FileInfo
	kind                                                               string
	reparse                                                            bool
	identity                                                           *CodexConfiguredObjectIdentityV1
	reader                                                             *os.File
	nativeHandle                                                       uintptr
	openAttempted, openReturned, statAttempted, statReturned           bool
	identityAttempted, identityReturned, closeAttempted, closeReturned bool
	openError, statError, identityError, closeError, ownerError        error
	linkReadAttempted, linkReadReturned                                bool
	linkReadError                                                      error
}

const codexConfiguredIdentityScheme = "windows-volume-index-v1"

// These serializable observations are protected DATA. They carry no handle,
// profile, snapshot, consent or permission to publish a configuration.
type CodexConfiguredObjectIdentityV1 struct {
	Scheme string `json:"scheme"`
	Value  string `json:"value"`
}

type CodexConfiguredGlobalLocationRowV1 struct {
	Path          string                           `json:"path"`
	Kind          string                           `json:"kind"`
	Identity      *CodexConfiguredObjectIdentityV1 `json:"identity"`
	RawLinkTarget *string                          `json:"raw_link_target"`
}

type CodexConfiguredGlobalLocationV1 struct {
	LogicalPath  string                               `json:"logical_path"`
	ReferentPath string                               `json:"referent_path"`
	LogicalRows  []CodexConfiguredGlobalLocationRowV1 `json:"logical_rows"`
	ReferentRows []CodexConfiguredGlobalLocationRowV1 `json:"referent_rows"`
}

func (CodexConfiguredGlobalLocationV1) String() string {
	return "configured global location (private DATA)"
}
func (v CodexConfiguredGlobalLocationV1) GoString() string { return v.String() }

// The binding is private source DATA, not a borrowed write handle or consent.
// FileInfo/retained readers identify observations; they do not prove liveness.
type codexConfiguredGlobalBinding struct {
	logicalPath, referentPath string
	logical, referent         []codexConfiguredPathObservation
	err                       error
}

func captureCodexConfiguredPath(path string) (observed []codexConfiguredPathObservation, resultErr error) {
	root, parts, err := splitCodexConfigAbsolutePath(path)
	if err != nil {
		return nil, err
	}
	current := root
	for index := -1; index < len(parts); index++ {
		if index >= 0 {
			current = filepath.Join(current, parts[index])
		}
		row, err := observeCodexConfiguredPathObject(current)
		observed = append(observed, row)
		if err != nil {
			return observed, err
		}
		if row.kind != "symlink" && index < len(parts)-1 && !row.info.IsDir() {
			return observed, ErrCodexLayerParseFailed
		}
	}
	return observed, nil
}

func captureCodexConfiguredGlobalBinding(path string) (binding *codexConfiguredGlobalBinding, resultErr error) {
	binding = &codexConfiguredGlobalBinding{logicalPath: path}
	defer func() { binding.err = resultErr }()
	var err error
	binding.logical, err = captureCodexConfiguredPath(path)
	if err != nil {
		return binding, err
	}
	binding.referentPath, err = filepath.EvalSymlinks(path)
	if err != nil {
		return binding, err
	}
	binding.referentPath, err = filepath.Abs(binding.referentPath)
	if err != nil {
		return binding, err
	}
	binding.referent, err = captureCodexConfiguredPath(binding.referentPath)
	return binding, err
}

func (b *codexConfiguredGlobalBinding) equal(other *codexConfiguredGlobalBinding) bool {
	if b == nil || other == nil {
		return b == other
	}
	if b.err != nil || other.err != nil || b.logicalPath != other.logicalPath || b.referentPath != other.referentPath {
		return false
	}
	for index, rows := range [][]codexConfiguredPathObservation{b.logical, b.referent} {
		otherRows := other.logical
		if index == 1 {
			otherRows = other.referent
		}
		if len(rows) != len(otherRows) {
			return false
		}
		for n, row := range rows {
			candidate := otherRows[n]
			if row.info == nil || candidate.info == nil || row.path != candidate.path || row.kind != candidate.kind || row.rawLinkTarget != candidate.rawLinkTarget || !codexConfiguredObjectIdentitiesEqual(row.identity, candidate.identity) || !os.SameFile(row.info, candidate.info) {
				return false
			}
		}
	}
	return true
}

// Publication can replace the referent file; the original links and parents
// must still be the same. This comparison is DATA, never write authority.
func (b *codexConfiguredGlobalBinding) linksAndParentsEqual(other *codexConfiguredGlobalBinding) bool {
	if b == nil || other == nil || b.err != nil || other.err != nil || b.logicalPath != other.logicalPath || b.referentPath != other.referentPath {
		return false
	}
	for index, rows := range [][]codexConfiguredPathObservation{b.logical, b.referent} {
		otherRows := other.logical
		if index == 1 {
			otherRows = other.referent
		}
		if len(rows) != len(otherRows) {
			return false
		}
		for n, row := range rows {
			candidate := otherRows[n]
			if row.info == nil || candidate.info == nil || row.path != candidate.path || row.kind != candidate.kind || row.rawLinkTarget != candidate.rawLinkTarget {
				return false
			}
			if n == len(rows)-1 && row.kind == "regular" && candidate.kind == "regular" {
				continue
			}
			if !codexConfiguredObjectIdentitiesEqual(row.identity, candidate.identity) || !os.SameFile(row.info, candidate.info) {
				return false
			}
		}
	}
	return true
}

func codexConfiguredObjectIdentitiesEqual(left, right *CodexConfiguredObjectIdentityV1) bool {
	return left != nil && right != nil && left.Scheme == codexConfiguredIdentityScheme && left.Value != "" && *left == *right
}

func codexConfiguredIdentityComplete(value *CodexConfiguredObjectIdentityV1) bool {
	if value == nil || value.Scheme != codexConfiguredIdentityScheme || strings.ToLower(value.Value) != value.Value {
		return false
	}
	parts := strings.Split(value.Value, ":")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		decoded, err := hex.DecodeString(part)
		if err != nil || len(part) != 8 || len(decoded) != 4 {
			return false
		}
	}
	return parts[1] != "00000000" || parts[2] != "00000000"
}

func (row codexConfiguredPathObservation) replayEvidence() (CodexConfiguredGlobalLocationRowV1, error) {
	if row.path == "" || row.info == nil || row.reader == nil || !row.openAttempted || !row.openReturned || row.openError != nil || !row.statAttempted || !row.statReturned || row.statError != nil || !row.identityAttempted || !row.identityReturned || row.identityError != nil || !row.closeAttempted || !row.closeReturned || row.closeError != nil || row.ownerError != nil || !codexConfiguredIdentityComplete(row.identity) {
		return CodexConfiguredGlobalLocationRowV1{}, errors.New("original configured path identity/close evidence unavailable")
	}
	id := *row.identity
	value := CodexConfiguredGlobalLocationRowV1{Path: row.path, Kind: row.kind, Identity: &id}
	switch row.kind {
	case "symlink":
		if !row.reparse || !row.linkReadAttempted || !row.linkReadReturned || row.linkReadError != nil {
			return CodexConfiguredGlobalLocationRowV1{}, ErrProjectClientPlanStale
		}
		target := row.rawLinkTarget
		value.RawLinkTarget = &target
	case "directory":
		if !row.info.IsDir() || row.reparse {
			return CodexConfiguredGlobalLocationRowV1{}, ErrProjectClientPlanStale
		}
	case "regular":
		if !row.info.Mode().IsRegular() || row.reparse {
			return CodexConfiguredGlobalLocationRowV1{}, ErrProjectClientPlanStale
		}
	default:
		return CodexConfiguredGlobalLocationRowV1{}, ErrProjectClientInvalid
	}
	return value, nil
}

func (b *codexConfiguredGlobalBinding) replayEvidence() (out CodexConfiguredGlobalLocationV1, err error) {
	if b == nil || b.err != nil || b.logicalPath == "" || b.referentPath == "" || len(b.logical) == 0 || len(b.referent) == 0 {
		return out, ErrProjectClientInvalid
	}
	out.LogicalPath, out.ReferentPath = b.logicalPath, b.referentPath
	for _, pair := range []struct {
		rows   []codexConfiguredPathObservation
		target *[]CodexConfiguredGlobalLocationRowV1
	}{{b.logical, &out.LogicalRows}, {b.referent, &out.ReferentRows}} {
		for _, row := range pair.rows {
			value, err := row.replayEvidence()
			if err != nil {
				return CodexConfiguredGlobalLocationV1{}, err
			}
			*pair.target = append(*pair.target, value)
		}
	}
	if out.ReferentRows[len(out.ReferentRows)-1].Kind != "regular" {
		return CodexConfiguredGlobalLocationV1{}, ErrProjectClientInvalid
	}
	return out, nil
}

func readCodexConfiguredGlobalFile(path string, limit int64, originalClose func(*os.File) error) (observed codexConfiguredFile, resultErr error) {
	observed.layer.PathSHA256 = codexAttachmentSHA([]byte(CanonicalProjectKey(path)))
	binding, err := captureCodexConfiguredGlobalBinding(path)
	observed.globalBinding = binding
	if err != nil {
		observed.ownerError = err
		return observed, err
	}
	observed, resultErr = readCodexConfiguredFile(binding.referentPath, limit, originalClose)
	observed.globalBinding = binding
	observed.layer.PathSHA256 = codexAttachmentSHA([]byte(CanonicalProjectKey(path)))
	if resultErr != nil {
		return observed, resultErr
	}
	after, err := captureCodexConfiguredGlobalBinding(path)
	observed.globalAfter = after
	if err != nil || !binding.equal(after) || observed.info == nil || len(after.referent) == 0 || !os.SameFile(observed.info, after.referent[len(after.referent)-1].info) {
		resultErr = errors.Join(err, ErrProjectClientPlanStale)
		observed.ownerError = errors.Join(observed.ownerError, resultErr)
	}
	return observed, resultErr
}

type codexConfiguredReadIO struct {
	now          func() time.Time
	environ      func() []string
	lookup       func(string) (string, error)
	knownFolders func() (string, string, error)
	run          func(context.Context, process.StrictRunInvocation) (process.StrictRunResult, error)
	shimClose    func(*os.File) error
	sourceClose  func(*os.File) error
}

func (l *lockingClient) ReadConfiguredProjectProfile(ctx context.Context, logical string) (*CodexConfiguredProjectProfile, error) {
	return l.readConfiguredProjectProfile(ctx, logical, codexConfiguredReadIO{
		now: time.Now, environ: os.Environ, lookup: exec.LookPath,
		knownFolders: codexConfiguredKnownFolders, run: process.RunStrictlyContained,
	})
}

// A private per-operation fixture seam exercises the same acquisition and issuing
// boundary; it grants no exported caller runner, environment or seal constructor.
func (l *lockingClient) readConfiguredProjectProfile(ctx context.Context, logical string, readIO codexConfiguredReadIO) (profile *CodexConfiguredProjectProfile, resultErr error) {
	var native process.StrictRunResult
	var globalRead configLockExecution
	var projectRead ProjectConfigLockExecution
	var before, after codexConfiguredContext
	defer func() {
		if resultErr != nil {
			profile = nil
			resultErr = &codexConfiguredSourceFailure{cause: resultErr, result: native, globalRead: globalRead, projectRead: projectRead, before: before, after: after}
		} else if profile != nil {
			profile.globalRead, profile.projectRead = globalRead, projectRead
		}
	}()
	c, bound := l.projectCodex()
	if !bound || ctx == nil || logical == "" || logical != c.projectLogicalEntry || readIO.now == nil || readIO.environ == nil || readIO.lookup == nil || readIO.knownFolders == nil || readIO.run == nil || ctx.Value(projectPolicyTransactionContextKey{}) != nil {
		return nil, ErrProjectClientInvalid
	}
	runner, err := l.projectRunner()
	if err != nil {
		return nil, err
	}
	readRunner, ok := runner.(CodexProjectConfigContextReadLockRunner)
	if !ok {
		return nil, ErrCodexProjectConfigLockUnavailable
	}
	// Refuse unsupported home/auth selectors before an advisory lock can create
	// its existing owned lock leaf. The same owner rechecks this context inside
	// the source READ scope and again after the getter.
	_, userRoot, _, err := codexConfiguredHomeContext(readIO.environ(), readIO.knownFolders)
	if err != nil {
		return nil, err
	}
	// Resolve the real global owner without accepting caller-supplied bytes/path.
	globalClient, err := NewCodexCLI()
	if err != nil {
		return nil, err
	}
	global := globalClient.(*lockingClient).Client.(*codexCLI)
	if strings.EqualFold(filepath.Clean(c.path), filepath.Clean(global.path)) || !strings.EqualFold(filepath.Clean(global.path), filepath.Join(filepath.Clean(userRoot), ".codex", "config.toml")) {
		return nil, ErrProjectClientInvalid
	}
	globalRead, err = withConfigLockExecutionContextPolicy(global.path, ctx, func() error {
		var projectErr error
		projectRead, projectErr = readRunner.RunReadContext(ctx, func() error {
			if err := ctx.Err(); err != nil {
				return err
			}
			root := filepath.Dir(filepath.Dir(c.path))
			before, err = captureCodexConfiguredContext(ctx, root, global.path, c.path, logical, readIO)
			if err != nil {
				return err
			}
			// The clock is acquired immediately before native admission, after route
			// and contributing-source reads. The same deadline bounds startup/run.
			clock := readIO.now()
			if err := before.nonrefreshing(clock, codexProviderInventoryTimeout); err != nil {
				return err
			}
			deadline := clock.Add(codexProviderInventoryTimeout)
			getterCtx, cancel := context.WithDeadline(ctx, deadline)
			defer cancel()
			native, err = readIO.run(getterCtx, process.StrictRunInvocation{
				Command: before.command, Input: []byte{}, InputLimit: 1,
				StdoutLimit: codexProviderInventoryStdoutLimit, StderrLimit: codexProviderInventoryStderrLimit,
			})
			if err != nil {
				return err
			}
			if getterCtx.Err() != nil || readIO.now().After(deadline) || native.SettledExit == nil || native.SettledExit.ExitCode != 0 || native.SettledExit.TimedOut || native.Stdout.Truncated || native.Stderr.Truncated || native.Stdout.Bytes != len(native.Stdout.Prefix) || native.Stderr.Bytes != len(native.Stderr.Prefix) {
				return errors.New("configured native read incomplete")
			}
			configured, err := decodeCodexConfiguredMCP(native.Stdout.Prefix, logical)
			if err != nil {
				return err
			}
			after, err = captureCodexConfiguredContext(ctx, root, global.path, c.path, logical, readIO)
			if err != nil {
				return err
			}
			if getterCtx.Err() != nil || readIO.now().After(deadline) {
				return errors.New("configured native read budget elapsed")
			}
			if err := after.nonrefreshing(readIO.now(), 0); err != nil {
				return err
			}
			if !before.equal(after) {
				return ErrProjectClientPlanStale
			}
			policy := make(map[string]json.RawMessage)
			for _, key := range []string{"enabled", "disabled_reason", "enabled_tools", "disabled_tools", "startup_timeout_sec", "tool_timeout_sec"} {
				policy[key] = configured.fields[key]
			}
			policySHA, err := providerFingerprint(policy)
			if err != nil {
				return err
			}
			binding := codexConfiguredSourceBinding{
				Schema: "codex-configured-project-profile-v1", Client: "codex-cli",
				ProjectIdentitySHA256: c.projectIdentitySHA256, LogicalKeySHA256: codexAttachmentSHA([]byte(logical)),
				QueryCWDSHA256:    codexAttachmentSHA([]byte(CanonicalProjectKey(root))),
				NativeRouteSHA256: before.routeSHA, LoaderContextSHA256: before.contextSHA,
				LayerSetSHA256: before.layerSHA, ConfiguredPayloadSHA256: codexAttachmentSHA(native.Stdout.Prefix),
				ConfiguredEnabled: configured.enabled, ConfiguredPolicySHA256: policySHA, RawPolicySHA256: before.policySHA,
			}
			fingerprint, err := providerFingerprint(binding)
			if err != nil {
				return err
			}
			profile = &CodexConfiguredProjectProfile{owner: l, logical: logical, binding: binding, fingerprint: fingerprint, configured: configured,
				global: append([]byte(nil), before.global...), project: append([]byte(nil), before.project...), payload: append([]byte(nil), native.Stdout.Prefix...), result: native, before: before, after: after}
			return nil
		})
		return projectErr
	}, false, true)
	if err != nil {
		return nil, err
	}
	if !globalRead.releaseDefined || !globalRead.releaseConfirmed || !projectRead.BodyEntered || !projectRead.ReleaseDefined || projectRead.ReleaseSucceeded == nil || !*projectRead.ReleaseSucceeded || projectRead.PrimaryError != nil || projectRead.CleanupError != nil || profile == nil {
		return nil, errors.Join(ErrCodexProjectConfigLockReleaseUnconfirmed, projectRead.PrimaryError, projectRead.CleanupError)
	}
	return profile, nil
}

// Reentry uses the same fresh native source owner, not the stored clock or a
// caller fingerprint. This method does no mutation or backend association.
func (l *lockingClient) VerifyConfiguredProjectProfile(ctx context.Context, profile *CodexConfiguredProjectProfile) error {
	if profile == nil || profile.owner != l {
		return ErrProjectClientInvalid
	}
	current, err := l.ReadConfiguredProjectProfile(ctx, profile.logical)
	if err != nil {
		var failure *codexConfiguredSourceFailure
		if errors.As(err, &failure) {
			failure.original = profile
		} else {
			err = &codexConfiguredSourceFailure{cause: err, original: profile, received: current}
		}
		return err
	}
	if current.fingerprint != profile.fingerprint || !profile.before.equal(current.before) {
		return &codexConfiguredSourceFailure{cause: ErrProjectClientPlanStale, result: current.result, globalRead: current.globalRead, projectRead: current.projectRead, before: current.before, after: current.after, original: profile, received: current}
	}
	return nil
}

func readCodexConfiguredFile(path string, limit int64, injectedClose ...func(*os.File) error) (observed codexConfiguredFile, resultErr error) {
	observed.layer.PathSHA256 = codexAttachmentSHA([]byte(CanonicalProjectKey(path)))
	originalClose := (*os.File).Close
	if len(injectedClose) > 1 {
		return observed, ErrProjectClientInvalid
	}
	if len(injectedClose) == 1 && injectedClose[0] != nil {
		originalClose = injectedClose[0]
	}
	defer func() { observed.ownerError = resultErr }()
	var identity string
	raw, present, err := readCodexConfigNoReparse(path, codexConfigReadIO{
		read: func(file *os.File) (value []byte, readErr error) {
			observed.reader, observed.readEntered = file, true
			defer func() { observed.readReturned, observed.readError = true, readErr }()
			var err error
			identity, err = codexConfiguredFileIdentity(file)
			if err != nil {
				return nil, err
			}
			observed.objectIdentity = &CodexConfiguredObjectIdentityV1{Scheme: codexConfiguredIdentityScheme, Value: identity}
			observed.info, err = file.Stat()
			if err != nil || observed.info.Size() > limit {
				return nil, errors.Join(err, errors.New("configured source exceeds read bound"))
			}
			value, err = io.ReadAll(io.LimitReader(file, limit+1))
			observed.bytes = bytes.Clone(value)
			if err != nil || int64(len(value)) > limit {
				return nil, errors.Join(err, errors.New("configured source exceeds read bound"))
			}
			after, err := file.Stat()
			if err != nil || !os.SameFile(observed.info, after) || after.Size() != observed.info.Size() || after.ModTime() != observed.info.ModTime() {
				return nil, errors.Join(err, ErrProjectClientPlanStale)
			}
			return value, nil
		}, close: func(file *os.File) error {
			observed.reader, observed.closeAttempted = file, true
			observed.closeError = originalClose(file)
			observed.closeReturned = true
			return observed.closeError
		},
	})
	if err != nil {
		return observed, err
	}
	observed.layer.Present, observed.bytes = present, raw
	if present {
		identitySHA, contentSHA := codexAttachmentSHA([]byte(identity)), codexAttachmentSHA(raw)
		observed.layer.IdentitySHA256, observed.layer.ContentSHA256 = &identitySHA, &contentSHA
	}
	return observed, nil
}

func codexConfiguredLayerDigest(files []codexConfiguredFile) (string, error) {
	rows := make([]codexConfiguredLayer, len(files))
	for index := range files {
		rows[index] = files[index].layer
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].PathSHA256 < rows[j].PathSHA256 })
	return providerFingerprint(rows)
}
