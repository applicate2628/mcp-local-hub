package clients

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
)

var ErrCodexProjectSourceRootInvalid = errors.New("CODEX_PROJECT_SOURCE_ROOT_INVALID")
var ErrCodexProjectSourceChanged = errors.New("CODEX_PROJECT_SOURCE_CHANGED")
var ErrCodexProjectSourcePreviewRequired = errors.New("CODEX_PROJECT_SOURCE_PREVIEW_REQUIRED")
var ErrCodexProjectConfigLockUnavailable = errors.New("CODEX_PROJECT_CONFIG_LOCK_UNAVAILABLE")
var ErrCodexProjectConfigLockReleaseUnconfirmed = errors.New("CODEX_PROJECT_CONFIG_LOCK_RELEASE_UNCONFIRMED")
var ErrCodexProjectLegacyBackupUnsupported = errors.New("CODEX_PROJECT_LEGACY_BACKUP_UNSUPPORTED")

// CodexProjectConfigLockRunner owns the project-identity lock around one
// project config critical section. It accepts no path: the API implementation
// derives its private state-lock leaf from the validated identity supplied to
// the factory below.
type CodexProjectConfigLockRunner interface {
	RunRead(func() error) error
	RunMutation(func() error) error
}

// CodexProjectConfigLockRunnerFactory is invoked only after the constructor has
// validated and fingerprinted the exact project root and config target.
type CodexProjectConfigLockRunnerFactory func(CodexProjectIdentity) (CodexProjectConfigLockRunner, error)

// CodexProjectIdentity is the validated private identity of one project-bound
// Codex config. Its fields are intentionally unexported so serializing the value
// cannot disclose a project path through accidental serialization. Downstream
// owners must deliberately call an accessor for either private value.
type CodexProjectIdentity struct {
	canonicalRoot         string
	projectIdentitySHA256 string
}

func (i CodexProjectIdentity) CanonicalRoot() string { return i.canonicalRoot }

func (i CodexProjectIdentity) ProjectIdentitySHA256() string {
	return i.projectIdentitySHA256
}

// CodexProjectConfigSnapshot is one physically validated, immutable read of a
// project Codex config. Its fields stay private so a plan cannot serialize the
// project root or config bytes accidentally.
type CodexProjectConfigSnapshot struct {
	identity     CodexProjectIdentity
	configSHA256 string
	configBytes  []byte
}

func (s CodexProjectConfigSnapshot) Identity() CodexProjectIdentity {
	return s.identity
}

func (s CodexProjectConfigSnapshot) ConfigSHA256() string {
	return s.configSHA256
}

func (s CodexProjectConfigSnapshot) Bytes() []byte {
	return append([]byte(nil), s.configBytes...)
}

// Entry parses one logical entry from the already-captured bytes. It performs
// no filesystem read and rejects ambiguous mixed transport tables.
func (s CodexProjectConfigSnapshot) Entry(logicalEntryName string) (*MCPEntry, error) {
	document, err := decodeCodexTOMLBytes(s.configBytes)
	if err != nil {
		return nil, err
	}
	serversValue, present := document["mcp_servers"]
	if !present {
		return nil, nil
	}
	servers, ok := serversValue.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: container_type_invalid", ErrCodexLayerParseFailed)
	}
	rawValue, present := servers[logicalEntryName]
	if !present {
		return nil, nil
	}
	raw, ok := rawValue.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: entry_type_invalid", ErrCodexLayerParseFailed)
	}
	transport, _, err := codexTransportOfEntry(raw, logicalEntryName)
	if err != nil {
		return nil, err
	}
	entry := &MCPEntry{Name: logicalEntryName, Raw: cloneCodexRawEntry(raw), Disabled: mcpEntryDisabled(raw)}
	if transport == CodexTransportHTTP {
		entry = classifyURLRawEntry(logicalEntryName, raw, "url", "http_headers")
		entry.Raw = cloneCodexRawEntry(raw)
	}
	entry.ToolTimeoutSec, err = codexToolTimeoutSec(raw)
	if err != nil {
		return nil, err
	}
	return entry, nil
}

// CodexProjectIdentitySHA256ForRoot derives the path-free project identity
// without touching the filesystem. Physical validation belongs to
// ReadCodexProjectConfigSnapshot.
func CodexProjectIdentitySHA256ForRoot(projectRoot string) (string, error) {
	identity, err := codexProjectIdentityForRoot(projectRoot)
	if err != nil {
		return "", err
	}
	return identity.projectIdentitySHA256, nil
}

// ReadCodexProjectConfigSnapshot reads exactly <projectRoot>/.codex/config.toml
// once through the existing no-reparse owner and binds the returned bytes,
// config hash, and physically validated project identity together.
func ReadCodexProjectConfigSnapshot(projectRoot string) (CodexProjectConfigSnapshot, error) {
	identity, err := codexProjectIdentityForRoot(projectRoot)
	if err != nil {
		return CodexProjectConfigSnapshot{}, err
	}
	return readCodexProjectConfigSnapshot(identity)
}

func codexProjectIdentityForRoot(projectRoot string) (CodexProjectIdentity, error) {
	if projectRoot == "" || !filepath.IsAbs(projectRoot) || filepath.Clean(projectRoot) != projectRoot {
		return CodexProjectIdentity{}, fmt.Errorf("%w: root must be clean and absolute", ErrCodexProjectSourceRootInvalid)
	}
	canonicalKey := CanonicalProjectKey(projectRoot)
	fingerprint := sha256.Sum256([]byte(canonicalKey))
	return CodexProjectIdentity{
		canonicalRoot:         projectRoot,
		projectIdentitySHA256: fmt.Sprintf("%x", fingerprint),
	}, nil
}

func readCodexProjectConfigSnapshot(identity CodexProjectIdentity) (CodexProjectConfigSnapshot, error) {
	configPath := filepath.Join(identity.canonicalRoot, ".codex", "config.toml")
	data, present, err := readCodexConfigNoReparse(configPath)
	if err != nil {
		return CodexProjectConfigSnapshot{}, fmt.Errorf("%w: config path validation failed: %v", ErrCodexProjectSourceRootInvalid, err)
	}
	if !present {
		return CodexProjectConfigSnapshot{}, fmt.Errorf("%w: project Codex config is absent", ErrCodexProjectSourceRootInvalid)
	}
	configBytes := append([]byte(nil), data...)
	configFingerprint := sha256.Sum256(configBytes)
	return CodexProjectConfigSnapshot{
		identity:     identity,
		configSHA256: fmt.Sprintf("%x", configFingerprint),
		configBytes:  configBytes,
	}, nil
}

// NewCodexCLIForProject returns the Codex adapter bound to exactly
// <projectRoot>/.codex/config.toml and configured with the injected
// project-identity lock runner. It never resolves or reads the user-global
// Codex home.
func NewCodexCLIForProject(projectRoot, logicalEntryName string, runnerFactory CodexProjectConfigLockRunnerFactory) (Client, CodexProjectIdentity, error) {
	var identity CodexProjectIdentity
	validatedIdentity, err := codexProjectIdentityForRoot(projectRoot)
	if err != nil {
		return nil, identity, err
	}
	if logicalEntryName == "" {
		return nil, identity, fmt.Errorf("%w: logical entry name is empty", ErrCodexProjectSourceRootInvalid)
	}
	snapshot, err := readCodexProjectConfigSnapshot(validatedIdentity)
	if err != nil {
		return nil, identity, err
	}
	identity = snapshot.Identity()
	configPath := filepath.Join(identity.canonicalRoot, ".codex", "config.toml")
	if runnerFactory == nil {
		return nil, identity, fmt.Errorf("%w: project config lock runner factory is nil", ErrCodexProjectConfigLockUnavailable)
	}
	runner, err := runnerFactory(identity)
	if err != nil {
		return nil, identity, fmt.Errorf("%w: initialize project config lock runner: %w", ErrCodexProjectConfigLockUnavailable, err)
	}
	if codexProjectConfigLockRunnerIsNil(runner) {
		return nil, identity, fmt.Errorf("%w: project config lock runner is nil", ErrCodexProjectConfigLockUnavailable)
	}
	return newProjectLockingClient(&codexCLI{
		path:                  configPath,
		projectIdentitySHA256: identity.projectIdentitySHA256,
		projectLogicalEntry:   logicalEntryName,
	}, runner), identity, nil
}

func codexProjectConfigLockRunnerIsNil(runner CodexProjectConfigLockRunner) bool {
	if runner == nil {
		return true
	}
	value := reflect.ValueOf(runner)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (c *codexCLI) isProjectBound() bool {
	return c != nil && c.projectIdentitySHA256 != ""
}
