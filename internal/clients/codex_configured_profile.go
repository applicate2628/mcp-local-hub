package clients

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"unicode/utf8"
)

// Native configured JSON remains private: inline environment/header values can
// contain secrets. RawMessage retains null/empty distinctions and number text.
type codexConfiguredMCP struct {
	name      string
	enabled   bool
	fields    map[string]json.RawMessage
	transport map[string]json.RawMessage
}

// CodexConfiguredStdioLaunch is an observation issued only by an existing
// configured profile. It retains no launch or mutation capability.
type CodexConfiguredStdioLaunch struct {
	source     *CodexConfiguredProjectProfile
	command    string
	args       []string
	env        map[string]string
	envNull    bool
	references []CodexConfiguredEnvironmentReference
	cwd        *string
}

type CodexConfiguredEnvironmentReference struct {
	name   string
	source string
	legacy bool
}

// Protected replay DATA from the genuine original profile. No field can issue
// a profile, snapshot, native handle, consent or publication authority.
type CodexSharedStdioSourceEvidenceV1 struct {
	ProjectRoot               string          `json:"project_root"`
	ProjectIdentitySHA256     string          `json:"project_identity_sha256"`
	ProjectConfigPath         string          `json:"project_config_path"`
	ProjectFileIdentitySHA256 string          `json:"project_file_identity_sha256"`
	ProjectBytes              []byte          `json:"project_bytes"`
	ProjectSHA256             string          `json:"project_sha256"`
	ReplayContextSHA256       string          `json:"replay_context_sha256"`
	ConfiguredPayload         json.RawMessage `json:"configured_payload"`
}

func (CodexSharedStdioSourceEvidenceV1) String() string {
	return "configured replay source (private DATA)"
}
func (v CodexSharedStdioSourceEvidenceV1) GoString() string { return v.String() }

var ErrCodexSharedStdioReplaySourceChanged = errors.New("CODEX_SHARED_STDIO_REPLAY_SOURCE_CHANGED")

func (CodexConfiguredStdioLaunch) String() string     { return "configured STDIO observation (private)" }
func (v CodexConfiguredStdioLaunch) GoString() string { return v.String() }

func (r CodexConfiguredEnvironmentReference) Name() string       { return r.name }
func (r CodexConfiguredEnvironmentReference) Source() string     { return r.source }
func (r CodexConfiguredEnvironmentReference) LegacyString() bool { return r.legacy }

func (p *CodexConfiguredProjectProfile) ConfiguredStdioLaunch() (*CodexConfiguredStdioLaunch, error) {
	if _, err := p.Fingerprint(); err != nil {
		return nil, err
	}
	var kind string
	if json.Unmarshal(p.configured.transport["type"], &kind) != nil || kind != "stdio" {
		return nil, errors.New("PROJECT_CLIENT_CONFIGURED_STDIO_REQUIRED")
	}
	// Reuse the same decoder contract; the projection never repairs missing or
	// null required fields and preserves the original native payload on p.
	t := p.configured.transport
	if !codexConfiguredKeys(t, "type", "command", "args", "env", "env_vars", "cwd") || !codexConfiguredNonemptyString(t["command"]) || !codexConfiguredStringArray(t["args"], false) || !codexConfiguredStringMap(t["env"]) || !codexConfiguredEnvVars(t["env_vars"]) || !codexConfiguredNullableString(t["cwd"]) {
		return nil, errors.New("PROJECT_CLIENT_CONFIGURED_STDIO_INVALID")
	}
	view := &CodexConfiguredStdioLaunch{source: p, envNull: bytes.Equal(t["env"], []byte("null"))}
	if err := json.Unmarshal(t["command"], &view.command); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(t["args"], &view.args); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(t["env"], &view.env); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(t["cwd"], &view.cwd); err != nil {
		return nil, err
	}
	var refs []json.RawMessage
	if err := json.Unmarshal(t["env_vars"], &refs); err != nil {
		return nil, err
	}
	view.references = make([]CodexConfiguredEnvironmentReference, 0, len(refs))
	for _, raw := range refs {
		if codexConfiguredNonemptyString(raw) {
			var name string
			if err := json.Unmarshal(raw, &name); err != nil {
				return nil, err
			}
			view.references = append(view.references, CodexConfiguredEnvironmentReference{name: name, source: "local", legacy: true})
			continue
		}
		object, err := codexConfiguredObject(raw)
		if err != nil {
			return nil, err
		}
		var name, source string
		if err := json.Unmarshal(object["name"], &name); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(object["source"], &source); err != nil {
			return nil, err
		}
		view.references = append(view.references, CodexConfiguredEnvironmentReference{name: name, source: source})
	}
	return view, nil
}

func (v *CodexConfiguredStdioLaunch) Command() string { return v.command }
func (v *CodexConfiguredStdioLaunch) Args() []string {
	return append([]string{}, v.args...)
}
func (v *CodexConfiguredStdioLaunch) Environment() (map[string]string, bool) {
	if v.env == nil {
		return nil, v.envNull
	}
	clone := make(map[string]string, len(v.env))
	for k, value := range v.env {
		clone[k] = value
	}
	return clone, v.envNull
}
func (v *CodexConfiguredStdioLaunch) EnvironmentReferences() []CodexConfiguredEnvironmentReference {
	return append([]CodexConfiguredEnvironmentReference{}, v.references...)
}
func (v *CodexConfiguredStdioLaunch) ConfiguredCWD() *string {
	if v.cwd == nil {
		return nil
	}
	value := *v.cwd
	return &value
}

// Source preimages are cloned DATA for the existing protected publisher. They
// are not consent, a ConfigWriteSnapshot or a surviving native capability.
func (p *CodexConfiguredProjectProfile) ConfiguredSourcePreimages() (global, project []byte, err error) {
	if err = p.validateConfiguredSource(); err != nil {
		return nil, nil, err
	}
	return bytes.Clone(p.global), bytes.Clone(p.project), nil
}

func (p *CodexConfiguredProjectProfile) validateConfiguredSource() error {
	if _, err := p.Fingerprint(); err != nil {
		return err
	}
	if len(p.before.files) == 0 || p.before.files[0].globalBinding == nil {
		return ErrProjectClientInvalid
	}
	return nil
}

func (p *CodexConfiguredProjectProfile) ConfiguredGlobalSourcePath() (logical, referent string, links map[string]string, err error) {
	if err = p.validateConfiguredSource(); err != nil {
		return "", "", nil, err
	}
	b := p.before.files[0].globalBinding
	links = make(map[string]string)
	for _, row := range append(append([]codexConfiguredPathObservation(nil), b.logical...), b.referent...) {
		if row.linkReadAttempted && row.linkReadReturned && row.linkReadError == nil {
			links[row.path] = row.rawLinkTarget
		}
	}
	return b.logicalPath, b.referentPath, links, nil
}

// This is the original read binding, not publication permission. The API still
// requires its genuine consent/capture/expected-snapshot transaction.
func (p *CodexConfiguredProjectProfile) ValidateConfiguredGlobalBinding(ctx context.Context) error {
	if ctx == nil {
		return ErrProjectClientInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.validateConfiguredSource(); err != nil {
		return err
	}
	original := p.before.files[0].globalBinding
	current, err := captureCodexConfiguredGlobalBinding(original.logicalPath)
	if err != nil || !original.equal(current) {
		return &codexConfiguredSourceFailure{cause: errors.Join(err, ErrProjectClientPlanStale), original: p, bindingBefore: original, bindingAfter: current}
	}
	return nil
}

// Compensation may replace the referent inode. Its original link and parent
// binding remains DATA; the caller still owns capture, consent and publication.
func (p *CodexConfiguredProjectProfile) ValidateConfiguredGlobalLocationBinding(ctx context.Context) error {
	if ctx == nil {
		return ErrProjectClientInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.validateConfiguredSource(); err != nil {
		return err
	}
	original := p.before.files[0].globalBinding
	current, err := captureCodexConfiguredGlobalBinding(original.logicalPath)
	if err != nil || !original.linksAndParentsEqual(current) {
		return &codexConfiguredSourceFailure{cause: errors.Join(err, ErrProjectClientPlanStale), original: p, bindingBefore: original, bindingAfter: current}
	}
	return nil
}

// Relate the original native source to the caller's genuine current project
// capture. The existing coordinator owns locks; this method does no getter,
// mutation, lock acquisition or authorization.
func (p *CodexConfiguredProjectProfile) ValidateConfiguredProjectSourceSnapshot(ctx context.Context, project Client, snapshot *ConfigWriteSnapshot) (resultErr error) {
	var current codexConfiguredFile
	readAttempted := false
	defer func() {
		if resultErr != nil && p != nil {
			failure := &codexConfiguredSourceFailure{cause: resultErr, result: p.result, globalRead: p.globalRead, projectRead: p.projectRead, before: p.before, after: p.after, original: p}
			if readAttempted {
				failure.projectSourceRead = &current
			}
			resultErr = failure
		}
	}()
	if ctx == nil {
		return ErrProjectClientInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.validateConfiguredSource(); err != nil {
		return err
	}
	local, ok := project.(*lockingClient)
	if !ok || local != p.owner {
		return ErrProjectClientInvalid
	}
	c, bound := local.projectCodex()
	if !bound || p.logical != c.projectLogicalEntry || p.binding.ProjectIdentitySHA256 != c.projectIdentitySHA256 || snapshot == nil || snapshot.path != c.path || len(p.before.files) < 2 {
		return ErrProjectClientInvalid
	}
	original := p.before.files[1]
	if original.info == nil || snapshot.fileInfo == nil || !original.layer.Present || !os.SameFile(snapshot.fileInfo, original.info) || !bytes.Equal(snapshot.contents, p.project) {
		return ErrProjectClientPlanStale
	}
	if err := snapshot.ValidateObservedCapture(ctx, c.path); err != nil {
		return err
	}
	readAttempted = true
	current, resultErr = readCodexConfiguredFile(c.path, 4<<20)
	if resultErr != nil {
		return resultErr
	}
	if !current.layer.Present || current.info == nil || !os.SameFile(current.info, original.info) || !bytes.Equal(current.bytes, p.project) {
		return ErrProjectClientPlanStale
	}
	return snapshot.ValidateObservedCapture(ctx, c.path)
}

// Export only the retained original observations. These methods perform no
// native query or reopen, and each call owns fresh mutable output buffers.
func (p *CodexConfiguredProjectProfile) SharedStdioSourceEvidence() (CodexSharedStdioSourceEvidenceV1, error) {
	source, _, err := p.sharedStdioOriginalEvidence()
	return cloneCodexSharedStdioSourceEvidence(source), err
}

func (p *CodexConfiguredProjectProfile) SharedStdioGlobalLocationEvidence() (CodexConfiguredGlobalLocationV1, error) {
	_, location, err := p.sharedStdioOriginalEvidence()
	return location, err
}

func (p *CodexConfiguredProjectProfile) sharedStdioOriginalEvidence() (source CodexSharedStdioSourceEvidenceV1, location CodexConfiguredGlobalLocationV1, resultErr error) {
	defer func() {
		if resultErr != nil {
			source, location = CodexSharedStdioSourceEvidenceV1{}, CodexConfiguredGlobalLocationV1{}
			if p != nil {
				resultErr = &codexConfiguredSourceFailure{cause: resultErr, result: p.result, globalRead: p.globalRead, projectRead: p.projectRead, before: p.before, after: p.after, original: p}
			}
		}
	}()
	if err := p.validateConfiguredSource(); err != nil {
		return source, location, err
	}
	c, bound := p.owner.projectCodex()
	if !bound || c.projectLogicalEntry != p.logical || c.projectIdentitySHA256 != p.binding.ProjectIdentitySHA256 || len(p.before.files) < 2 || len(p.after.files) < 2 || p.before.replayContextSHA == "" || p.before.replayContextSHA != p.after.replayContextSHA {
		return source, location, ErrProjectClientPlanStale
	}
	if !p.before.files[0].layer.Present || !p.before.files[1].layer.Present || !p.after.files[0].layer.Present || !p.after.files[1].layer.Present {
		return source, location, ErrProjectClientPlanStale
	}
	for _, captured := range []codexConfiguredContext{p.before, p.after} {
		for _, file := range captured.files {
			if file.ownerError != nil {
				return source, location, file.ownerError
			}
			if file.layer.Present && (file.reader == nil || file.info == nil || !file.readEntered || !file.readReturned || file.readError != nil || !file.closeAttempted || !file.closeReturned || file.closeError != nil || !codexConfiguredIdentityComplete(file.objectIdentity) || file.layer.IdentitySHA256 == nil || file.layer.ContentSHA256 == nil || *file.layer.ContentSHA256 == "" || codexAttachmentSHA([]byte(file.objectIdentity.Value)) != *file.layer.IdentitySHA256) {
				return source, location, errors.New("original configured file identity/read/close evidence unavailable")
			}
		}
		for _, binding := range []*codexConfiguredGlobalBinding{captured.files[0].globalBinding, captured.files[0].globalAfter} {
			observed, err := binding.replayEvidence()
			if err != nil {
				return source, location, err
			}
			if location.LogicalPath == "" {
				location = observed
			} else if !reflect.DeepEqual(location, observed) {
				return source, location, ErrProjectClientPlanStale
			}
		}
		leaf := location.ReferentRows[len(location.ReferentRows)-1]
		if !codexConfiguredObjectIdentitiesEqual(leaf.Identity, captured.files[0].objectIdentity) {
			return source, location, ErrProjectClientPlanStale
		}
	}
	project := p.before.files[1]
	if project.layer.PathSHA256 != codexAttachmentSHA([]byte(CanonicalProjectKey(c.path))) || !bytes.Equal(project.bytes, p.before.project) || !bytes.Equal(p.before.project, p.project) || !bytes.Equal(p.before.project, p.after.project) || len(p.payload) == 0 {
		return source, location, ErrProjectClientPlanStale
	}
	source = CodexSharedStdioSourceEvidenceV1{
		ProjectRoot: filepath.Dir(filepath.Dir(c.path)), ProjectIdentitySHA256: c.projectIdentitySHA256, ProjectConfigPath: c.path,
		ProjectFileIdentitySHA256: *project.layer.IdentitySHA256, ProjectBytes: p.before.project, ProjectSHA256: *project.layer.ContentSHA256,
		ReplayContextSHA256: p.before.replayContextSHA, ConfiguredPayload: p.payload,
	}
	return source, location, nil
}

func cloneCodexSharedStdioSourceEvidence(value CodexSharedStdioSourceEvidenceV1) CodexSharedStdioSourceEvidenceV1 {
	value.ProjectBytes = bytes.Clone(value.ProjectBytes)
	value.ConfiguredPayload = bytes.Clone(value.ConfiguredPayload)
	return value
}

func cloneCodexConfiguredGlobalLocation(value CodexConfiguredGlobalLocationV1) CodexConfiguredGlobalLocationV1 {
	for _, rows := range []*[]CodexConfiguredGlobalLocationRowV1{&value.LogicalRows, &value.ReferentRows} {
		*rows = append([]CodexConfiguredGlobalLocationRowV1(nil), (*rows)...)
		for index := range *rows {
			row := &(*rows)[index]
			if row.Identity != nil {
				identity := *row.Identity
				row.Identity = &identity
			}
			if row.RawLinkTarget != nil {
				target := *row.RawLinkTarget
				row.RawLinkTarget = &target
			}
		}
	}
	return value
}

// Compare original protected DATA with this independently issued CURRENT
// profile. Only the regular global leaf identity may differ. The caller still
// needs current full-inode capture, consent, locks and the original publisher.
func (p *CodexConfiguredProjectProfile) ValidateSharedStdioReplaySource(ctx context.Context, original CodexSharedStdioSourceEvidenceV1, originalLocation CodexConfiguredGlobalLocationV1) (resultErr error) {
	original = cloneCodexSharedStdioSourceEvidence(original)
	originalLocation = cloneCodexConfiguredGlobalLocation(originalLocation)
	defer func() {
		if resultErr != nil {
			cause := errors.Join(ErrProjectClientPlanStale, ErrCodexSharedStdioReplaySourceChanged, resultErr)
			failure := &codexConfiguredSourceFailure{cause: cause, received: p, replaySourceBefore: &original, replayLocationBefore: &originalLocation}
			if p != nil {
				failure.result, failure.globalRead, failure.projectRead, failure.before, failure.after = p.result, p.globalRead, p.projectRead, p.before, p.after
			}
			resultErr = failure
		}
	}()
	if ctx == nil {
		return ErrProjectClientInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, location, err := p.sharedStdioOriginalEvidence()
	if err != nil {
		return err
	}
	if original.ProjectRoot != current.ProjectRoot || original.ProjectIdentitySHA256 != current.ProjectIdentitySHA256 || original.ProjectConfigPath != current.ProjectConfigPath || original.ProjectFileIdentitySHA256 != current.ProjectFileIdentitySHA256 || !bytes.Equal(original.ProjectBytes, current.ProjectBytes) || original.ProjectSHA256 != current.ProjectSHA256 || original.ReplayContextSHA256 != current.ReplayContextSHA256 || codexAttachmentSHA(original.ProjectBytes) != original.ProjectSHA256 {
		return ErrProjectClientPlanStale
	}
	if _, err := decodeCodexConfiguredMCP(original.ConfiguredPayload, p.logical); err != nil {
		return err
	}
	if originalLocation.LogicalPath != location.LogicalPath || originalLocation.ReferentPath != location.ReferentPath {
		return ErrProjectClientPlanStale
	}
	for _, pair := range []struct {
		original, current []CodexConfiguredGlobalLocationRowV1
	}{{originalLocation.LogicalRows, location.LogicalRows}, {originalLocation.ReferentRows, location.ReferentRows}} {
		if len(pair.original) == 0 || len(pair.original) != len(pair.current) {
			return ErrProjectClientPlanStale
		}
		for index, before := range pair.original {
			after := pair.current[index]
			if before.Path != after.Path || before.Kind != after.Kind || !codexConfiguredIdentityComplete(before.Identity) || !reflect.DeepEqual(before.RawLinkTarget, after.RawLinkTarget) {
				return ErrProjectClientPlanStale
			}
			if index == len(pair.original)-1 && before.Kind == "regular" {
				continue
			}
			if !codexConfiguredObjectIdentitiesEqual(before.Identity, after.Identity) {
				return ErrProjectClientPlanStale
			}
		}
	}
	return ctx.Err()
}

// Only configured-contributor consistency is checked here. Equality does not
// attest runtime backend, native main environment, cwd or project association.
func (p *CodexConfiguredProjectProfile) ValidateSharedStdioContributors(shared *CodexConfiguredStdioLaunch, target MCPEntry) error {
	source, err := p.ConfiguredStdioLaunch()
	if err != nil {
		return err
	}
	if shared == nil || shared.source == nil {
		return ErrProjectClientInvalid
	}
	if err := p.validateConfiguredSource(); err != nil {
		return err
	}
	if err := shared.source.validateConfiguredSource(); err != nil {
		return err
	}
	if shared.source.logical != p.logical || !bytes.Equal(p.global, shared.source.global) {
		return errors.New("CODEX_SHARED_STDIO_SOURCE_CONTRIBUTORS_CHANGED")
	}
	if source.command != shared.command || !reflect.DeepEqual(source.args, shared.args) || source.envNull != shared.envNull || !reflect.DeepEqual(source.env, shared.env) || !reflect.DeepEqual(source.references, shared.references) || !reflect.DeepEqual(source.cwd, shared.cwd) {
		return errors.New("CODEX_SHARED_STDIO_CONFIGURED_SOURCE_DIFFERENT")
	}
	if target.Name != p.logical || target.RelayExePath != "mcphub" || target.RelayURL == "" {
		return ErrProjectClientInvalid
	}
	args := []string{"relay", "--url", target.RelayURL}
	if raw, present := p.before.projectEntry["args"]; present {
		contributed, err := providerStringSlice(raw)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(contributed, args) {
			return errors.New("CODEX_SHARED_STDIO_PROJECT_ARGS_INCOMPATIBLE_WITH_TARGET")
		}
	}
	return nil
}

// The composition invokes this original getter only after its writer/capture
// and lock resources settle. Received profile DATA does not seal a backend.
func (p *CodexConfiguredProjectProfile) ReadConfiguredSharedStdioTarget(ctx context.Context, target MCPEntry) (received *CodexConfiguredProjectProfile, resultErr error) {
	if err := p.validateConfiguredSource(); err != nil {
		return nil, err
	}
	if target.Name != p.logical || target.RelayExePath != "mcphub" || target.RelayURL == "" {
		return nil, ErrProjectClientInvalid
	}
	received, resultErr = p.owner.ReadConfiguredProjectProfile(ctx, p.logical)
	if resultErr != nil {
		var failure *codexConfiguredSourceFailure
		if errors.As(resultErr, &failure) {
			failure.original = p
		} else {
			resultErr = &codexConfiguredSourceFailure{cause: resultErr, original: p}
		}
		return nil, resultErr
	}
	return p.receiveConfiguredSharedStdioTarget(received, target)
}

func (p *CodexConfiguredProjectProfile) receiveConfiguredSharedStdioTarget(received *CodexConfiguredProjectProfile, target MCPEntry) (_ *CodexConfiguredProjectProfile, resultErr error) {
	defer func() {
		if resultErr != nil && received != nil {
			resultErr = &codexConfiguredSourceFailure{cause: resultErr, result: received.result, globalRead: received.globalRead, projectRead: received.projectRead, before: received.before, after: received.after, original: p, received: received}
		}
	}()
	if received == nil || received.owner != p.owner || received.logical != p.logical || !bytes.Equal(received.project, p.project) || received.binding.ProjectIdentitySHA256 != p.binding.ProjectIdentitySHA256 {
		return nil, ErrProjectClientPlanStale
	}
	if err := received.validateConfiguredSource(); err != nil {
		return nil, err
	}
	if !p.before.files[0].globalBinding.linksAndParentsEqual(received.before.files[0].globalBinding) {
		return nil, ErrProjectClientPlanStale
	}
	launch, err := received.ConfiguredStdioLaunch()
	if err != nil {
		return nil, err
	}
	if launch.command != target.RelayExePath || !reflect.DeepEqual(launch.args, []string{"relay", "--url", target.RelayURL}) {
		return nil, errors.New("CODEX_SHARED_STDIO_NATIVE_TARGET_DIFFERENT")
	}
	originalPolicy, receivedPolicy := cloneCodexRawEntry(p.before.globalEntry), cloneCodexRawEntry(received.before.globalEntry)
	for key := range originalPolicy {
		if codexTransportField(key) {
			delete(originalPolicy, key)
		}
	}
	for key := range receivedPolicy {
		if codexTransportField(key) {
			delete(receivedPolicy, key)
		}
	}
	if !reflect.DeepEqual(originalPolicy, receivedPolicy) {
		return nil, errors.New("CODEX_SHARED_STDIO_GLOBAL_POLICY_CHANGED")
	}
	for _, key := range []string{"enabled", "disabled_reason", "enabled_tools", "disabled_tools", "startup_timeout_sec", "tool_timeout_sec"} {
		if !bytes.Equal(p.configured.fields[key], received.configured.fields[key]) {
			return nil, errors.New("CODEX_SHARED_STDIO_NATIVE_POLICY_CHANGED")
		}
	}
	return received, nil
}

func decodeCodexConfiguredMCP(raw []byte, logical string) (codexConfiguredMCP, error) {
	var out codexConfiguredMCP
	fields, err := codexConfiguredObject(raw)
	if err != nil || !codexConfiguredKeys(fields, "name", "enabled", "disabled_reason", "transport", "enabled_tools", "disabled_tools", "startup_timeout_sec", "tool_timeout_sec") {
		return out, errors.New("configured MCP object shape invalid")
	}
	if err := json.Unmarshal(fields["name"], &out.name); err != nil || out.name != logical || logical == "" {
		return out, errors.New("configured MCP name mismatch")
	}
	if bytes.Equal(fields["enabled"], []byte("null")) || json.Unmarshal(fields["enabled"], &out.enabled) != nil {
		return out, errors.New("configured MCP enabled invalid")
	}
	if !codexConfiguredNullableString(fields["disabled_reason"]) || !codexConfiguredStringArray(fields["enabled_tools"], true) || !codexConfiguredStringArray(fields["disabled_tools"], true) || !codexConfiguredTimeout(fields["startup_timeout_sec"]) || !codexConfiguredTimeout(fields["tool_timeout_sec"]) {
		return out, errors.New("configured MCP policy type invalid")
	}
	transport, err := codexConfiguredObject(fields["transport"])
	if err != nil {
		return out, errors.New("configured MCP transport invalid")
	}
	var kind string
	if json.Unmarshal(transport["type"], &kind) != nil {
		return out, errors.New("configured MCP transport type missing")
	}
	switch kind {
	case "stdio":
		if !codexConfiguredKeys(transport, "type", "command", "args", "env", "env_vars", "cwd") || !codexConfiguredNonemptyString(transport["command"]) || !codexConfiguredStringArray(transport["args"], false) || !codexConfiguredStringMap(transport["env"]) || !codexConfiguredEnvVars(transport["env_vars"]) || !codexConfiguredNullableString(transport["cwd"]) {
			return out, errors.New("configured MCP stdio shape invalid")
		}
	case "streamable_http":
		if !codexConfiguredKeys(transport, "type", "url", "bearer_token_env_var", "http_headers", "env_http_headers", "http_headers_helper") || !codexConfiguredNonemptyString(transport["url"]) || !codexConfiguredNullableString(transport["bearer_token_env_var"]) || !codexConfiguredStringMap(transport["http_headers"]) || !codexConfiguredStringMap(transport["env_http_headers"]) || !codexConfiguredNullableString(transport["http_headers_helper"]) {
			return out, errors.New("configured MCP HTTP shape invalid")
		}
	default:
		return out, errors.New("configured MCP transport unsupported")
	}
	out.fields, out.transport = fields, transport
	return out, nil
}

// Exact field names and one occurrence are required at every object boundary;
// encoding/json's case-insensitive struct matching is not the native protocol.
func codexConfiguredObject(raw []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("configured object UTF-8 invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("configured object required")
	}
	out := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err = decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errors.New("configured object key invalid")
		}
		if _, exists := out[key]; exists {
			return nil, errors.New("configured object duplicate key")
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, errors.New("configured object value invalid")
		}
		out[key] = append(json.RawMessage(nil), value...)
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("configured object terminator invalid")
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errors.New("configured object trailing input")
	}
	return out, nil
}

func codexConfiguredKeys(object map[string]json.RawMessage, keys ...string) bool {
	if len(object) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, exists := object[key]; !exists {
			return false
		}
	}
	return true
}

func codexConfiguredNullableString(raw []byte) bool {
	if bytes.Equal(raw, []byte("null")) {
		return true
	}
	var value string
	return len(raw) != 0 && json.Unmarshal(raw, &value) == nil
}

func codexConfiguredNonemptyString(raw []byte) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && value != ""
}

func codexConfiguredStringArray(raw []byte, nullable bool) bool {
	if bytes.Equal(raw, []byte("null")) {
		return nullable
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return false
	}
	for _, rawValue := range values {
		var value string
		if bytes.Equal(rawValue, []byte("null")) || json.Unmarshal(rawValue, &value) != nil {
			return false
		}
	}
	return true
}

func codexConfiguredStringMap(raw []byte) bool {
	if bytes.Equal(raw, []byte("null")) {
		return true
	}
	values, err := codexConfiguredObject(raw)
	if err != nil {
		return false
	}
	for _, value := range values {
		var text string
		if bytes.Equal(value, []byte("null")) || json.Unmarshal(value, &text) != nil {
			return false
		}
	}
	return true
}

func codexConfiguredTimeout(raw []byte) bool {
	if bytes.Equal(raw, []byte("null")) {
		return true
	}
	var number json.Number
	if json.Unmarshal(raw, &number) != nil || len(raw) == 0 || raw[0] == '"' {
		return false
	}
	value, err := number.Float64()
	return err == nil && value >= 0 && !math.IsInf(value, 0) && !math.IsNaN(value)
}

func codexConfiguredEnvVars(raw []byte) bool {
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return false
	}
	for _, rawValue := range values {
		if codexConfiguredNonemptyString(rawValue) {
			continue
		}
		object, err := codexConfiguredObject(rawValue)
		if err != nil || !codexConfiguredKeys(object, "name", "source") || !codexConfiguredNonemptyString(object["name"]) {
			return false
		}
		var source string
		if json.Unmarshal(object["source"], &source) != nil || (source != "local" && source != "remote") {
			return false
		}
	}
	return true
}
