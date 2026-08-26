package clients

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// NewCodexCLI returns a Client bound to ~/.codex/config.toml.
func NewCodexCLI() (Client, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return newLockingClient(&codexCLI{path: filepath.Join(home, ".codex", "config.toml")}), nil
}

type codexCLI struct {
	path string
}

// CodexTransport identifies the transport fields a Codex MCP table contributes.
// It deliberately has no "mixed" success value: a table carrying both fields is
// malformed for this interoperability boundary and is refused by its caller.
type CodexTransport string

const (
	CodexTransportNone  CodexTransport = "none"
	CodexTransportHTTP  CodexTransport = "http"
	CodexTransportStdio CodexTransport = "stdio"
)

var (
	ErrCodexDesiredTransportInvalid  = errors.New("CODEX_DESIRED_TRANSPORT_INVALID")
	ErrCodexLayerRootUnresolved      = errors.New("CODEX_LAYER_ROOT_UNRESOLVED")
	ErrCodexLayerParseFailed         = errors.New("CODEX_LAYER_PARSE_FAILED")
	ErrCodexTargetNameConflict       = errors.New("CODEX_TARGET_NAME_CONFLICT")
	ErrCodexLayerCollisionUnowned    = errors.New("CODEX_LAYER_COLLISION_UNOWNED_GLOBAL")
	ErrCodexConfigReadbackFailed     = errors.New("CODEX_CLIENT_CONFIG_READBACK_FAILED")
	ErrCodexConfigRollbackIncomplete = errors.New("CODEX_CLIENT_CONFIG_ROLLBACK_INCOMPLETE")
)

// CodexTransportTarget is the planner-owned physical Codex key for one logical
// MCP name. Project layers are inspected only; filesystem paths stay private to
// the resolver and are never returned to API, CLI, GUI, or event consumers.
type CodexTransportTarget struct {
	LogicalEntryName    string
	TargetEntryName     string
	CrossLayerCollision bool
	// ExistingExactTarget reports only a read-time classification: the resolved
	// alias is already the exact desired hub HTTP entry. It grants no write
	// authority; the caller must independently validate its source and plan.
	ExistingExactTarget bool
	ProjectLayerPresent bool
}

// CodexTransportTargetRequest carries the composition-root authority used for
// read-only project-layer discovery. No transaction reads the process working
// directory; callers must explicitly supply both bounded paths.
type CodexTransportTargetRequest struct {
	LogicalEntryName string
	// DesiredTransport is the transport the caller plans to write. It is
	// intentionally distinct from an observed global table: every applicable
	// layer is compared against this target, including when the global config
	// or its same-name table is absent.
	DesiredTransport CodexTransport
	// DesiredEntry is the typed hub entry shape used only to classify an
	// existing alias as exact. It does not authorize mutation.
	DesiredEntry MCPEntry
	ProjectRoot  string
	WorkingDir   string
}

type CodexHTTPRelocationOutcome string

const (
	CodexHTTPRelocationCommitted         CodexHTTPRelocationOutcome = "committed"
	CodexHTTPRelocationAlreadyConfigured CodexHTTPRelocationOutcome = "already-configured"
)

type CodexHTTPRelocationResult struct {
	LogicalSource    string
	TargetEntry      string
	WriteTarget      string
	DesiredTransport CodexTransport
	CollisionReason  string
	Action           string
	Outcome          CodexHTTPRelocationOutcome
	SourceSnapshot   map[string]any
	Readback         CodexHTTPRelocationReadback
}

// CodexHTTPRelocationReadback is the closed receiving-state observation owned
// by the locked Codex transaction. A successful relocation result is not
// constructible without Exact.
type CodexHTTPRelocationReadback string

const CodexHTTPRelocationReadbackExact CodexHTTPRelocationReadback = "exact"

// CodexHTTPRelocation is the one-file mutation request accepted by the Codex
// adapter. The caller must have already authorized the old global table through
// its captured source/provenance ownership. This type intentionally contains no
// project path: project config files are never writer targets.
type CodexHTTPRelocation struct {
	SourceEntryName string
	TargetEntryName string
	Entry           MCPEntry
	ExpectedSource  CodexTransport
	// SourceSnapshot is required on a source-absent repeat. It is the exact
	// source table returned by the first committed transaction and later pinned
	// by provenance; a target shape alone never authorizes repeat ownership.
	SourceSnapshot map[string]any
	// WriteConfig is a test-only scoped writer seam. Production leaves it nil,
	// which routes through the ordinary secure config writer.
	WriteConfig WriteConfigFileFunc
}

type CodexHTTPInverseOutcome string

const (
	CodexHTTPInverseRestored        CodexHTTPInverseOutcome = "restored"
	CodexHTTPInverseAlreadyRestored CodexHTTPInverseOutcome = "already-restored"
)

type CodexHTTPInverseResult struct{ Outcome CodexHTTPInverseOutcome }

// CodexHTTPInverseRelocation is the provenance-driven inverse of one distinct
// target relocation. SourceSnapshot is the validated pre-relocation GLOBAL
// table captured by the caller; project layers never participate in this write.
type CodexHTTPInverseRelocation struct {
	SourceEntryName string
	TargetEntryName string
	Target          MCPEntry
	SourceSnapshot  map[string]any
	WriteConfig     WriteConfigFileFunc
}

func (c *codexCLI) Name() string       { return "codex-cli" }
func (c *codexCLI) ConfigPath() string { return c.path }

// ResolveTransportTarget returns the deterministic physical table key required
// when an applicable project layer contributes the opposite transport under the
// logical id. The root-to-working-directory chain is supplied by the composition
// root and is never inferred by walking outside root.
func (c *codexCLI) ResolveTransportTarget(req CodexTransportTargetRequest) (CodexTransportTarget, error) {
	result := CodexTransportTarget{LogicalEntryName: req.LogicalEntryName, TargetEntryName: req.LogicalEntryName}
	if req.LogicalEntryName == "" {
		return result, fmt.Errorf("%w: logical entry name is empty", ErrCodexLayerRootUnresolved)
	}
	if req.DesiredTransport != CodexTransportHTTP && req.DesiredTransport != CodexTransportStdio {
		return result, fmt.Errorf("%w: desired transport %q is invalid", ErrCodexDesiredTransportInvalid, req.DesiredTransport)
	}
	rootAbs, err := filepath.Abs(req.ProjectRoot)
	if err != nil || req.ProjectRoot == "" {
		return result, fmt.Errorf("%w: resolve project root", ErrCodexLayerRootUnresolved)
	}
	if info, statErr := os.Stat(rootAbs); statErr != nil || !info.IsDir() {
		return result, fmt.Errorf("%w: project root is not a directory", ErrCodexLayerRootUnresolved)
	}
	workingAbs, err := filepath.Abs(req.WorkingDir)
	if err != nil || req.WorkingDir == "" {
		return result, fmt.Errorf("%w: resolve working directory", ErrCodexLayerRootUnresolved)
	}
	rel, err := filepath.Rel(rootAbs, workingAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return result, fmt.Errorf("%w: working directory is outside project root", ErrCodexLayerRootUnresolved)
	}
	globalTransport, globalPresent, err := codexTransportAtOptional(c.path, req.LogicalEntryName)
	if err != nil {
		return result, err
	}
	if globalPresent && codexOppositeTransport(req.DesiredTransport, globalTransport) {
		result.CrossLayerCollision = true
	}
	projectPaths := []string{filepath.Join(rootAbs, ".codex", "config.toml")}
	if rel != "." {
		current := rootAbs
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if part == "" || part == "." {
				continue
			}
			current = filepath.Join(current, part)
			projectPaths = append(projectPaths, filepath.Join(current, ".codex", "config.toml"))
		}
	}
	for _, projectPath := range projectPaths {
		transport, present, readErr := codexTransportAtOptional(projectPath, req.LogicalEntryName)
		if readErr != nil {
			return result, readErr
		}
		if present {
			result.ProjectLayerPresent = true
			if codexOppositeTransport(req.DesiredTransport, transport) {
				result.CrossLayerCollision = true
			}
		}
	}
	if result.CrossLayerCollision {
		result.TargetEntryName = req.LogicalEntryName + "-mcphub"
		present, exact, err := codexExactDesiredHubEntryAt(c.path, result.TargetEntryName, req.DesiredEntry)
		if err != nil {
			return result, err
		} else if present && !exact {
			return result, fmt.Errorf("%w: global target %q already exists", ErrCodexTargetNameConflict, result.TargetEntryName)
		}
		result.ExistingExactTarget = present && exact
		for _, projectPath := range projectPaths {
			present, exact, err := codexExactDesiredHubEntryAt(projectPath, result.TargetEntryName, req.DesiredEntry)
			if err != nil {
				return result, err
			} else if present && !exact {
				return result, fmt.Errorf("%w: project target %q already exists", ErrCodexTargetNameConflict, result.TargetEntryName)
			}
			result.ExistingExactTarget = result.ExistingExactTarget || (present && exact)
		}
	}
	return result, nil
}

func codexOppositeTransport(left, right CodexTransport) bool {
	return (left == CodexTransportHTTP && right == CodexTransportStdio) ||
		(left == CodexTransportStdio && right == CodexTransportHTTP)
}

func codexTransportAtOptional(path, name string) (CodexTransport, bool, error) {
	raw, present, err := codexRawEntryAtOptional(path, name)
	if err != nil || !present {
		return CodexTransportNone, false, err
	}
	return codexTransportOfEntry(raw, name)
}

func codexTransportOfEntry(raw map[string]any, name string) (CodexTransport, bool, error) {
	url, hasURL := raw["url"]
	command, hasCommand := raw["command"]
	switch {
	case hasURL && hasCommand:
		return CodexTransportNone, true, fmt.Errorf("%w: transport_mixed", ErrCodexLayerParseFailed)
	case hasURL:
		if value, ok := url.(string); !ok || value == "" {
			return CodexTransportNone, true, fmt.Errorf("%w: transport_type_invalid", ErrCodexLayerParseFailed)
		}
		return CodexTransportHTTP, true, nil
	case hasCommand:
		if value, ok := command.(string); !ok || value == "" {
			return CodexTransportNone, true, fmt.Errorf("%w: transport_type_invalid", ErrCodexLayerParseFailed)
		}
		return CodexTransportStdio, true, nil
	default:
		return CodexTransportNone, true, fmt.Errorf("%w: transport_missing", ErrCodexLayerParseFailed)
	}
}

func codexExactDesiredHubEntryAt(path, name string, desired MCPEntry) (present bool, exact bool, err error) {
	raw, present, err := codexRawEntryAtOptional(path, name)
	if err != nil || !present {
		return present, false, err
	}
	exact, err = codexHubEntryMatches(raw, desired)
	return true, exact, err
}

func codexRawEntryAtOptional(path, name string) (map[string]any, bool, error) {
	data, present, err := readCodexConfigNoReparse(path)
	if err != nil || !present {
		return nil, false, err
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, false, fmt.Errorf("%w: parse Codex config", ErrCodexLayerParseFailed)
	}
	serversValue, serversPresent := doc["mcp_servers"]
	if !serversPresent {
		return nil, false, nil
	}
	servers, ok := serversValue.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("%w: container_type_invalid", ErrCodexLayerParseFailed)
	}
	rawValue, present := servers[name]
	if !present {
		return nil, false, nil
	}
	raw, ok := rawValue.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("%w: entry_type_invalid", ErrCodexLayerParseFailed)
	}
	return raw, true, nil
}

const codexConfigMaxPathComponents = 256

type codexConfigPathComponent struct {
	path string
	info os.FileInfo
}

// readCodexConfigNoReparse is the single read owner for Codex layer discovery.
// It rejects links/reparse points at every existing path component, retains the
// leaf identity across open, and rechecks the component identities after open so
// a path substitution cannot silently supply bytes from another file. Missing
// components retain the documented optional-project-config behavior.
func readCodexConfigNoReparse(path string) ([]byte, bool, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, false, fmt.Errorf("%w: resolve Codex config path", ErrCodexLayerParseFailed)
	}
	root, parts, err := splitCodexConfigAbsolutePath(absPath)
	if err != nil {
		return nil, false, err
	}

	components := make([]codexConfigPathComponent, 0, len(parts)+1)
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, false, fmt.Errorf("%w: inspect Codex config root", ErrCodexLayerParseFailed)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, false, fmt.Errorf("%w: Codex config root is not a real directory", ErrCodexLayerParseFailed)
	}
	components = append(components, codexConfigPathComponent{path: root, info: rootInfo})

	current := root
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			return nil, false, nil
		}
		if statErr != nil {
			return nil, false, fmt.Errorf("%w: inspect Codex config component", ErrCodexLayerParseFailed)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, false, fmt.Errorf("%w: Codex config path contains a link or reparse point", ErrCodexLayerParseFailed)
		}
		leaf := index == len(parts)-1
		if leaf {
			if !info.Mode().IsRegular() {
				return nil, false, fmt.Errorf("%w: Codex config is not a regular file", ErrCodexLayerParseFailed)
			}
		} else if !info.IsDir() {
			return nil, false, fmt.Errorf("%w: Codex config path ancestor is not a directory", ErrCodexLayerParseFailed)
		}
		components = append(components, codexConfigPathComponent{path: current, info: info})
	}

	file, err := os.Open(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("%w: open Codex config", ErrCodexLayerParseFailed)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || len(components) == 0 || !os.SameFile(components[len(components)-1].info, openedInfo) {
		return nil, false, fmt.Errorf("%w: Codex config identity changed during open", ErrCodexLayerParseFailed)
	}
	if err := recheckCodexConfigPathComponents(components); err != nil {
		return nil, false, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, false, fmt.Errorf("%w: read Codex config", ErrCodexLayerParseFailed)
	}
	return data, true, nil
}

func splitCodexConfigAbsolutePath(path string) (string, []string, error) {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	rest := strings.TrimPrefix(clean, volume)
	separator := string(filepath.Separator)
	if !strings.HasPrefix(rest, separator) {
		return "", nil, fmt.Errorf("%w: Codex config path is not absolute", ErrCodexLayerParseFailed)
	}
	root := volume + separator
	rest = strings.TrimPrefix(rest, separator)
	if rest == "" {
		return "", nil, fmt.Errorf("%w: Codex config path has no file component", ErrCodexLayerParseFailed)
	}
	parts := strings.Split(rest, separator)
	if len(parts) > codexConfigMaxPathComponents {
		return "", nil, fmt.Errorf("%w: Codex config path has too many components", ErrCodexLayerParseFailed)
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", nil, fmt.Errorf("%w: Codex config path contains an invalid component", ErrCodexLayerParseFailed)
		}
	}
	return root, parts, nil
}

func recheckCodexConfigPathComponents(components []codexConfigPathComponent) error {
	for _, component := range components {
		info, err := os.Lstat(component.path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(component.info, info) {
			return fmt.Errorf("%w: Codex config path changed during open", ErrCodexLayerParseFailed)
		}
	}
	return nil
}

// RelocateHTTPEntry removes an approved old global table and writes the HTTP
// hub table under its frozen target id. The caller normally reaches this through
// lockingClient; keeping the body here lets the lock owner execute precisely one
// read-modify-write transaction.
func (c *codexCLI) RelocateHTTPEntry(req CodexHTTPRelocation) (CodexHTTPRelocationResult, error) {
	var result CodexHTTPRelocationResult
	if req.SourceEntryName == "" || req.TargetEntryName == "" || req.Entry.URL == "" {
		return result, fmt.Errorf("%w: relocation requires source, target, and URL", ErrCodexLayerCollisionUnowned)
	}
	originalBytes, err := os.ReadFile(c.path)
	if err != nil {
		return result, fmt.Errorf("%w: read global snapshot: %v", ErrCodexLayerCollisionUnowned, err)
	}
	before, err := c.readTOML()
	if err != nil {
		return result, err
	}
	expected := cloneCodexDocument(before)
	servers, ok := expected["mcp_servers"].(map[string]any)
	if !ok {
		return result, fmt.Errorf("%w: source global table %q is absent", ErrCodexLayerCollisionUnowned, req.SourceEntryName)
	}
	rawValue, sourcePresent := servers[req.SourceEntryName]
	raw, exists := rawValue.(map[string]any)
	if sourcePresent && !exists {
		return result, fmt.Errorf("%w: entry_type_invalid", ErrCodexLayerParseFailed)
	}
	if !exists && req.TargetEntryName != req.SourceEntryName {
		targetValue, targetPresent := servers[req.TargetEntryName]
		target, targetExists := targetValue.(map[string]any)
		if targetPresent && !targetExists {
			return result, fmt.Errorf("%w: entry_type_invalid", ErrCodexLayerParseFailed)
		}
		if targetExists {
			matches, matchErr := codexHubEntryMatches(target, req.Entry)
			if matchErr != nil {
				return result, matchErr
			}
			if len(req.SourceSnapshot) != 0 && matches {
				return codexRelocationResult(req, CodexHTTPRelocationAlreadyConfigured, nil), nil
			}
			if len(req.SourceSnapshot) == 0 {
				return result, fmt.Errorf("%w: target global table %q has no caller-proven source snapshot", ErrCodexLayerCollisionUnowned, req.TargetEntryName)
			}
			return result, fmt.Errorf("%w: target global table %q is not the expected owned hub entry", ErrCodexTargetNameConflict, req.TargetEntryName)
		}
		return result, fmt.Errorf("%w: source global table %q is absent", ErrCodexLayerCollisionUnowned, req.SourceEntryName)
	}
	if !exists {
		return result, fmt.Errorf("%w: source global table %q is absent or malformed", ErrCodexLayerCollisionUnowned, req.SourceEntryName)
	}
	actual, _, classifyErr := codexTransportOfEntry(raw, req.SourceEntryName)
	if classifyErr != nil {
		return result, classifyErr
	}
	if actual != req.ExpectedSource {
		return result, fmt.Errorf("%w: source global table %q transport is %s, expected %s", ErrCodexLayerCollisionUnowned, req.SourceEntryName, actual, req.ExpectedSource)
	}
	sourceSnapshot := cloneCodexRawEntry(raw)
	if req.TargetEntryName != req.SourceEntryName {
		if targetValue, targetPresent := servers[req.TargetEntryName]; targetPresent {
			target, targetIsTable := targetValue.(map[string]any)
			if !targetIsTable {
				return result, fmt.Errorf("%w: entry_type_invalid", ErrCodexLayerParseFailed)
			}
			if _, matchErr := codexHubEntryMatches(target, req.Entry); matchErr != nil {
				return result, matchErr
			}
			return result, fmt.Errorf("%w: target global table %q already exists", ErrCodexTargetNameConflict, req.TargetEntryName)
		}
		delete(servers, req.SourceEntryName)
	}
	entryMap := map[string]any{"url": req.Entry.URL, "startup_timeout_sec": 10.0}
	if len(req.Entry.Headers) > 0 {
		entryMap["http_headers"] = codexDecodedHeaderMap(req.Entry.Headers)
	}
	servers[req.TargetEntryName] = entryMap
	expected["mcp_servers"] = servers
	if err := c.writeTOMLWithWriter(expected, req.WriteConfig); err != nil {
		return result, c.rollbackCodexSnapshot(originalBytes, fmt.Errorf("CODEX_CLIENT_CONFIG_WRITE_FAILED: %w", err))
	}
	readback, err := c.readTOML()
	if err != nil {
		return result, c.rollbackCodexSnapshot(originalBytes, fmt.Errorf("%w: %v", ErrCodexConfigReadbackFailed, err))
	}
	if !reflect.DeepEqual(readback, expected) {
		return result, c.rollbackCodexSnapshot(originalBytes, fmt.Errorf("%w: semantic readback differs", ErrCodexConfigReadbackFailed))
	}
	return codexRelocationResult(req, CodexHTTPRelocationCommitted, sourceSnapshot), nil
}

// codexDecodedHeaderMap constructs the same map shape readTOML returns after
// TOML serialization. It is deliberately limited to the new header table; the
// surrounding document remains subject to exact semantic readback equality.
func codexDecodedHeaderMap(headers map[string]string) map[string]any {
	decoded := make(map[string]any, len(headers))
	for key, value := range headers {
		decoded[key] = value
	}
	return decoded
}

func codexRelocationResult(req CodexHTTPRelocation, outcome CodexHTTPRelocationOutcome, sourceSnapshot map[string]any) CodexHTTPRelocationResult {
	return CodexHTTPRelocationResult{
		LogicalSource: req.SourceEntryName, TargetEntry: req.TargetEntryName,
		WriteTarget: "codex_global", DesiredTransport: CodexTransportHTTP,
		CollisionReason: "cross_layer_opposite_transport", Action: "relocate",
		Outcome: outcome, SourceSnapshot: cloneCodexRawEntry(sourceSnapshot),
		Readback: CodexHTTPRelocationReadbackExact,
	}
}

func cloneCodexDocument(document map[string]any) map[string]any {
	clone := make(map[string]any, len(document))
	for key, value := range document {
		clone[key] = cloneCodexValue(value)
	}
	return clone
}

func cloneCodexValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneCodexDocument(typed)
	case []any:
		clone := make([]any, len(typed))
		for index, item := range typed {
			clone[index] = cloneCodexValue(item)
		}
		return clone
	default:
		return value
	}
}

// RestoreRelocatedHTTPEntry is the locked-owner inverse: it removes the exact
// hub target alias and restores the caller-validated source table in one global
// TOML write. Any failed write/readback restores the full pre-operation bytes.
func (c *codexCLI) RestoreRelocatedHTTPEntry(req CodexHTTPInverseRelocation) (CodexHTTPInverseResult, error) {
	var result CodexHTTPInverseResult
	if req.SourceEntryName == "" || req.TargetEntryName == "" || req.Target.URL == "" || len(req.SourceSnapshot) == 0 {
		return result, fmt.Errorf("%w: inverse relocation requires source, target, hub entry, and source snapshot", ErrCodexLayerCollisionUnowned)
	}
	originalBytes, err := os.ReadFile(c.path)
	if err != nil {
		return result, fmt.Errorf("%w: read global snapshot: %v", ErrCodexLayerCollisionUnowned, err)
	}
	m, err := c.readTOML()
	if err != nil {
		return result, err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return result, fmt.Errorf("%w: target global table %q is absent", ErrCodexLayerCollisionUnowned, req.TargetEntryName)
	}
	target, targetExists := servers[req.TargetEntryName].(map[string]any)
	source, sourceExists := servers[req.SourceEntryName].(map[string]any)
	if !targetExists {
		if sourceExists && reflect.DeepEqual(source, req.SourceSnapshot) {
			return CodexHTTPInverseResult{Outcome: CodexHTTPInverseAlreadyRestored}, nil
		}
		return result, fmt.Errorf("%w: target global table %q is absent", ErrCodexLayerCollisionUnowned, req.TargetEntryName)
	}
	matches, matchErr := codexHubEntryMatches(target, req.Target)
	if matchErr != nil {
		return result, matchErr
	}
	if !matches {
		return result, fmt.Errorf("%w: target global table %q is not the expected owned hub entry", ErrCodexTargetNameConflict, req.TargetEntryName)
	}
	if sourceExists && !reflect.DeepEqual(source, req.SourceSnapshot) {
		return result, fmt.Errorf("%w: source global table %q conflicts with the saved source snapshot", ErrCodexLayerCollisionUnowned, req.SourceEntryName)
	}
	delete(servers, req.TargetEntryName)
	servers[req.SourceEntryName] = cloneCodexRawEntry(req.SourceSnapshot)
	m["mcp_servers"] = servers
	if err := c.writeTOMLWithWriter(m, req.WriteConfig); err != nil {
		return result, c.rollbackCodexSnapshot(originalBytes, fmt.Errorf("CODEX_CLIENT_CONFIG_WRITE_FAILED: %w", err))
	}
	readback, err := c.readTOML()
	if err != nil {
		return result, c.rollbackCodexSnapshot(originalBytes, fmt.Errorf("%w: %v", ErrCodexConfigReadbackFailed, err))
	}
	readbackServers, _ := readback["mcp_servers"].(map[string]any)
	actualSource, sourcePresent := readbackServers[req.SourceEntryName].(map[string]any)
	if !sourcePresent || !reflect.DeepEqual(actualSource, req.SourceSnapshot) {
		return result, c.rollbackCodexSnapshot(originalBytes, fmt.Errorf("%w: source global table %q did not restore exactly", ErrCodexConfigReadbackFailed, req.SourceEntryName))
	}
	if _, remains := readbackServers[req.TargetEntryName]; remains {
		return result, c.rollbackCodexSnapshot(originalBytes, fmt.Errorf("%w: target global table %q remains", ErrCodexConfigReadbackFailed, req.TargetEntryName))
	}
	return CodexHTTPInverseResult{Outcome: CodexHTTPInverseRestored}, nil
}

func (c *codexCLI) rollbackCodexSnapshot(snapshot []byte, cause error) error {
	if rollbackErr := writeConfigFileWith(nil, c.path, snapshot); rollbackErr != nil {
		return errors.Join(cause, fmt.Errorf("%w: restore global config snapshot: %v", ErrCodexConfigRollbackIncomplete, rollbackErr))
	}
	actual, readErr := os.ReadFile(c.path)
	if readErr != nil || !bytes.Equal(actual, snapshot) {
		return errors.Join(cause, fmt.Errorf("%w: restore global config snapshot readback", ErrCodexConfigRollbackIncomplete))
	}
	return cause
}

func cloneCodexRawEntry(raw map[string]any) map[string]any {
	clone := make(map[string]any, len(raw))
	for key, value := range raw {
		clone[key] = value
	}
	return clone
}

func codexHubEntryMatches(raw map[string]any, expected MCPEntry) (bool, error) {
	transport, _, err := codexTransportOfEntry(raw, expected.Name)
	if err != nil {
		return false, err
	}
	if transport != CodexTransportHTTP {
		return false, nil
	}
	url, _ := raw["url"].(string)
	if url != expected.URL {
		return false, nil
	}
	timeout, exists := raw["startup_timeout_sec"]
	if !exists || !codexTimeoutEqualsTen(timeout) {
		return false, nil
	}
	if len(expected.Headers) == 0 {
		return len(raw) == 2, nil
	}
	headers, ok := raw["http_headers"].(map[string]any)
	if !ok || len(headers) != len(expected.Headers) || len(raw) != 3 {
		return false, nil
	}
	for key, want := range expected.Headers {
		got, ok := headers[key].(string)
		if !ok || got != want {
			return false, nil
		}
	}
	return true, nil
}

func codexTimeoutEqualsTen(value any) bool {
	switch n := value.(type) {
	case int64:
		return n == 10
	case float64:
		return n == 10
	default:
		return false
	}
}

// IsRelayStdio reports false: codex-cli is a URL-native HTTP MCP client.
func (c *codexCLI) IsRelayStdio() bool { return false }

func (c *codexCLI) Exists() bool {
	_, err := os.Stat(c.path)
	return err == nil
}

func (c *codexCLI) Backup() (string, error) {
	return writeBackup(c.path, c.Name(), 0)
}

func (c *codexCLI) BackupKeep(keepN int) (string, error) {
	return writeBackup(c.path, c.Name(), keepN)
}

// InitEmpty seeds ~/.codex/config.toml with an empty `[mcp_servers]`
// TOML table if the file is absent. The table header is intentionally
// declared (rather than dropping an empty file) so a user inspecting
// the stub sees exactly where AddEntry will append new servers.
// codex-cli reads many other settings from the same config.toml, but
// because InitEmpty fires only when the file is missing, no
// user-authored configuration can be clobbered.
func (c *codexCLI) InitEmpty() (created bool, err error) {
	return EnsureClientConfigStub(c.path, []byte("[mcp_servers]\n"))
}

func (c *codexCLI) Restore(backupPath string) error {
	// Route the live-config rewrite through WriteConfigFile so
	// production restores inherit the SecureWriteClientConfig pipeline.
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return err
	}
	return WriteConfigFile(c.path, data)
}

// readTOML / writeTOML round-trip through map[string]any so unknown sections survive.
func (c *codexCLI) readTOML() (map[string]any, error) {
	data, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", c.path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func (c *codexCLI) writeTOML(m map[string]any) error {
	return c.writeTOMLWithWriter(m, nil)
}

func (c *codexCLI) writeTOMLWithWriter(m map[string]any, writer WriteConfigFileFunc) error {
	out, err := toml.Marshal(m)
	if err != nil {
		return err
	}
	// Route through WriteConfigFile so production gets the
	// SecureWriteClientConfig pipeline (handle-relative + DACL-bound)
	// for token-bearing rewrites; tests get the os.WriteFile fallback.
	return writeConfigFileWith(writer, c.path, out)
}

func (c *codexCLI) AddEntry(entry MCPEntry) error {
	return c.AddEntryWithConfigWriter(entry, nil)
}

func (c *codexCLI) AddEntryWithConfigWriter(entry MCPEntry, writer WriteConfigFileFunc) error {
	m, err := c.readTOML()
	if err != nil {
		return err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	// Replace the entry wholesale — this drops any stdio-era fields like `command`/`args`.
	entryMap := map[string]any{
		"url":                 entry.URL,
		"startup_timeout_sec": 10.0,
	}
	if len(entry.Headers) > 0 {
		entryMap["http_headers"] = entry.Headers
	}
	servers[entry.Name] = entryMap
	m["mcp_servers"] = servers
	return c.writeTOMLWithWriter(m, writer)
}

func (c *codexCLI) RemoveEntry(name string) error {
	m, err := c.readTOML()
	if err != nil {
		return err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return nil
	}
	delete(servers, name)
	m["mcp_servers"] = servers
	return c.writeTOML(m)
}

func (c *codexCLI) GetEntry(name string) (*MCPEntry, error) {
	m, err := c.readTOML()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return nil, nil
	}
	raw, ok := servers[name].(map[string]any)
	if !ok {
		return nil, nil
	}
	return classifyURLRawEntry(name, raw, "url", "http_headers"), nil
}

// LatestBackupPath delegates to the shared helper.
func (c *codexCLI) LatestBackupPath() (string, bool, error) {
	return latestBackup(c.path, c.Name())
}

// RestoreEntryFromBackup reads the TOML backup, extracts the
// [mcp_servers.<name>] table (if present), and writes it over the live
// config's corresponding entry. Other [mcp_servers.*] tables are left
// untouched.
//
// Defensively refuses if the backup's copy of the named entry is
// already in hub-HTTP form (has a `url` key and no `command` key) —
// see ErrBackupEntryAlreadyMigrated.
func (c *codexCLI) RestoreEntryFromBackup(backupPath, name string) error {
	return c.restoreEntryFromBackup(backupPath, name, false)
}

// RestoreEntryFromBackupForRollback restores the backup's entry verbatim,
// bypassing the ErrBackupEntryAlreadyMigrated guard (see the interface
// doc on Client.RestoreEntryFromBackupForRollback). Install rollback and
// Serena migrate rollback use it when the timestamped backup is the source of
// truth.
func (c *codexCLI) RestoreEntryFromBackupForRollback(backupPath, name string) error {
	return c.restoreEntryFromBackup(backupPath, name, true)
}

func (c *codexCLI) RestoreEntryFromBackupForRollbackWithConfigWriter(backupPath, name string, writer WriteConfigFileFunc) error {
	return c.restoreEntryFromBackupWithWriter(backupPath, name, true, writer)
}

// restoreEntryFromBackup is the shared body. When allowHubEntry is false
// (demigrate) it refuses a hub-HTTP-shaped backup entry with
// ErrBackupEntryAlreadyMigrated; when true (migrate rollback) it writes
// the backup bytes verbatim regardless of shape.
func (c *codexCLI) restoreEntryFromBackup(backupPath, name string, allowHubEntry bool) error {
	return c.restoreEntryFromBackupWithWriter(backupPath, name, allowHubEntry, nil)
}

func (c *codexCLI) restoreEntryFromBackupWithWriter(backupPath, name string, allowHubEntry bool, writer WriteConfigFileFunc) error {
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	// Re-attach the backup file path to any error the path-free core returns, so
	// the file-backed demigrate/rollback caller keeps a "which backup failed"
	// diagnostic (the core stays path-free for Phase 3's in-memory snapshot
	// bytes). %w preserves ErrBackupEntryAlreadyMigrated for errors.Is callers.
	if err := c.restoreEntryFromBytes(backupData, name, allowHubEntry, writer); err != nil {
		return fmt.Errorf("restore backup %s: %w", backupPath, err)
	}
	return nil
}

// restoreEntryFromBytes is the post-ReadFile restore core: given the
// already-read backup bytes it parses them, reads the live config once, and
// surgically restores (or strips) the named entry.
// restoreEntryFromBackupWithWriter is the thin file-reading wrapper over this
// core. The parse error omits the source path because this core also serves
// callers that pass in-memory bytes with no backing file.
func (c *codexCLI) restoreEntryFromBytes(backupData []byte, name string, allowHubEntry bool, writer WriteConfigFileFunc) error {
	// err is declared here because the os.ReadFile that previously declared it
	// now lives in the wrapper; the demigrate branch below assigns it via
	// `liveMap, err = c.readTOML()`.
	var err error
	var backupMap map[string]any
	if len(backupData) > 0 {
		if err := toml.Unmarshal(backupData, &backupMap); err != nil {
			return fmt.Errorf("parse backup: %w", err)
		}
	}
	if backupMap == nil {
		backupMap = map[string]any{}
	}
	backupServers, _ := backupMap["mcp_servers"].(map[string]any)

	// The live map is read exactly ONCE per path and never falls back to a stale
	// snapshot (design round-7, Sol+Terra P1). Rollback (allowHubEntry=true) reads
	// AFTER the whole-file recovery helper so the surgical write reflects current
	// on-disk state; demigrate (false) keeps its single early read.
	var liveMap map[string]any
	var liveServers map[string]any
	if allowHubEntry {
		// Whole-file-gone recovery FIRST (design round-5): SecureWrite path #1 may have
		// REMOVED the write-target file (target entry + siblings). Recover the whole
		// backup before the entry-scoped skip, which would else false-skip the
		// both-absent case or surgically recreate only the target entry (losing siblings).
		if handled, werr := wholeFileRestoreIfWriteTargetGone(c.path, backupData); handled {
			return werr
		}
		// The helper fell through (handled=false) — either the common file-present
		// path #2, or a create/no-replace CONFLICT (an external process recreated
		// c.path, possibly with a NEW sibling S', in the stat→create window; the
		// no-replace create sees EEXIST and did NOT publish the backup bytes). Read
		// the live map ONCE here, AFTER the helper, and treat it as AUTHORITATIVE:
		// the surgical write below (unlike the JSONC bodies' setMember/deleteMember,
		// which re-read at mutate time) serializes THIS whole map, so it must reflect
		// current on-disk state and preserve S' rather than clobber it. For the
		// non-race path #2 the read equals the pre-helper state (no-op-equivalent).
		// A read FAILURE (transient / partial TOML written by the racing recreate)
		// must ABORT with that error — it flows to the Install rollback closure
		// (InstallClientRollbackIncompleteError) so adopt PRESERVES the provenance;
		// it must NEVER fall back to a stale/earlier map and silently clobber S'
		// while reporting success (design round-7, Sol+Terra P1). Still under the
		// single withConfigLock hold, so this stays TOCTOU-free.
		freshMap, rerr := c.readTOML()
		if rerr != nil {
			return rerr
		}
		liveMap = freshMap
		liveServers, _ = liveMap["mcp_servers"].(map[string]any)
		if liveServers == nil {
			liveServers = map[string]any{}
		}
		// Rollback-only atomic entry-scoped skip-if-unchanged (design round-4): the
		// live read above + the write below run under the SAME withConfigLock hold, so
		// this compare-then-restore is TOCTOU-free. If the write-target already holds
		// the backup's entry value (the client was never mutated, or a sibling edit
		// left THIS entry untouched) return nil WITHOUT writing — no redundant/damaging
		// restore.
		le, lp := liveServers[name]
		be, bp := backupServers[name]
		if entryRestoreIsNoop(le, lp, be, bp) {
			return nil
		}
	} else {
		// Demigrate path: single early read, unchanged. It keeps its
		// ErrBackupEntryAlreadyMigrated guard (below) exactly as before.
		liveMap, err = c.readTOML()
		if err != nil {
			return err
		}
		liveServers, _ = liveMap["mcp_servers"].(map[string]any)
		if liveServers == nil {
			liveServers = map[string]any{}
		}
	}
	if backupServers != nil {
		if backupEntry, present := backupServers[name]; present {
			// Defensive: refuse hub-HTTP-shaped backup entries for
			// Codex CLI (loopback `url` present, `command` absent).
			// User-configured remote HTTP entries (non-loopback url)
			// pass through. The rollback caller (allowHubEntry=true)
			// bypasses this guard to restore the pre-reconcile legacy
			// hub entry verbatim.
			if !allowHubEntry {
				if rawMap, ok := backupEntry.(map[string]any); ok {
					if isHubURLShapeEntry(rawMap, "url") {
						return ErrBackupEntryAlreadyMigrated
					}
				}
			}
			liveServers[name] = backupEntry
			liveMap["mcp_servers"] = liveServers
			return c.writeTOMLWithWriter(liveMap, writer)
		}
	}
	delete(liveServers, name)
	liveMap["mcp_servers"] = liveServers
	return c.writeTOMLWithWriter(liveMap, writer)
}

// AllStdioEntries returns every stdio entry from [mcp_servers.*].
func (c *codexCLI) AllStdioEntries() ([]StdioEntry, error) {
	m, err := c.readTOML()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	return collectStdioEntries(servers), nil
}

// FindStdioLanguageServerEntries scans [mcp_servers.*] for stdio
// entries matching the mcp-language-server invocation pattern.
func (c *codexCLI) FindStdioLanguageServerEntries() ([]LanguageServerStdioEntry, error) {
	m, err := c.readTOML()
	if err != nil {
		return nil, err
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	return findLanguageServerStdioInMap(servers), nil
}

// BackupContainsEntry reports whether the backup file at backupPath
// has an [mcp_servers.<name>] table.
func (c *codexCLI) BackupContainsEntry(backupPath, name string) (bool, error) {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return false, nil
	}
	// Require the entry to be a table (map). A scalar value would
	// be malformed in TOML at this path; treat as absent so
	// sentinel fallback refuses rather than silently writes
	// corrupted data via RestoreEntryFromBackup.
	entry, ok := servers[name].(map[string]any)
	return ok && entry != nil, nil
}

// BackupEntryIsHubManaged reports whether [mcp_servers.<name>] in the
// TOML backup at backupPath is in Codex CLI's hub-HTTP shape (loopback
// `url` present, `command` absent). See Client.BackupEntryIsHubManaged.
func (c *codexCLI) BackupEntryIsHubManaged(backupPath, name string) (bool, error) {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return false, fmt.Errorf("read backup %s: %w", backupPath, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		return false, fmt.Errorf("parse backup %s: %w", backupPath, err)
	}
	servers, _ := m["mcp_servers"].(map[string]any)
	if servers == nil {
		return false, nil
	}
	entry, ok := servers[name].(map[string]any)
	if !ok || entry == nil {
		return false, nil
	}
	return isHubURLShapeEntry(entry, "url"), nil
}
