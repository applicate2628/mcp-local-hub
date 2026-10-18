package clients

import (
	"bytes"
	"context"
	"crypto/sha256"

	"errors"
	"fmt"
	"maps"
	"reflect"
)

var ErrProjectClientInvalid = errors.New("PROJECT_CLIENT_INVALID")
var ErrProjectClientPlanStale = errors.New("PROJECT_CLIENT_PLAN_STALE")
var ErrProjectClientEntryConflict = errors.New("PROJECT_CLIENT_ENTRY_CONFLICT")
var ErrProjectClientRollbackIncomplete = errors.New("PROJECT_CLIENT_ROLLBACK_INCOMPLETE")

// ProjectHTTPAttachmentPreparation keeps all names, policy and tokens private.
// Its version follows the measured source state, never an omitted caller field.
type ProjectHTTPAttachmentPreparation struct {
	projectIdentity, configSHA, globalSHA, logical, managed, schema, operation string
	before, after, originalToken, maskToken, aliasToken, appended              []byte
	desired                                                                    MCPEntry
}

func (p *ProjectHTTPAttachmentPreparation) SchemaOperation() (string, string) {
	return p.schema, p.operation
}
func (p *ProjectHTTPAttachmentPreparation) BeforeSHA256() string { return p.configSHA }
func (p *ProjectHTTPAttachmentPreparation) AfterSHA256() string  { return codexAttachmentSHA(p.after) }
func (p *ProjectHTTPAttachmentPreparation) DesiredEntry() MCPEntry {
	e := p.desired
	e.Raw = cloneCodexRawEntry(e.Raw)
	e.Headers = maps.Clone(e.Headers)
	return e
}

// ReadProjectAttachmentPolicySnapshot reads through the Codex global owner.
// The global read lock ends before the caller enters the project lock.
func (c *codexCLI) ReadProjectAttachmentPolicySnapshot() ([]byte, error) {
	if c.isProjectBound() {
		return nil, ErrProjectClientInvalid
	}
	b, present, err := readCodexConfigNoReparse(c.path)
	if err != nil {
		return nil, err
	}
	if !present {
		return []byte{}, nil
	}
	return b, nil
}
func (l *lockingClient) ReadProjectAttachmentPolicySnapshot() (b []byte, err error) {
	err = l.withReadLock(func() error {
		c, ok := l.Client.(*codexCLI)
		if !ok {
			return ErrProjectClientInvalid
		}
		b, err = c.ReadProjectAttachmentPolicySnapshot()
		return err
	})
	return
}

// RecoverProjectHTTPAttachment reconstructs private inverse authority from the
// verified full preimage and owned postimage, not from a guessed default token.
func (c *codexCLI) RecoverProjectHTTPAttachment(logical, operation string, before []byte, expectedAfterSHA string, mask, alias, separators []byte) (*ProjectHTTPAttachmentPreparation, error) {
	if !c.isProjectBound() || logical != c.projectLogicalEntry {
		return nil, ErrProjectClientInvalid
	}
	managed := logical + "-mcphub"
	original, err := deriveCodexAttachmentOriginalToken(before, logical, managed, operation)
	if err != nil {
		return nil, err
	}
	aliasDoc, err := decodeCodexTOMLBytes(alias)
	if err != nil {
		return nil, err
	}
	entries, ok := aliasDoc["mcp_servers"].(map[string]any)
	if !ok || len(entries) != 1 {
		return nil, ErrProjectClientRollbackIncomplete
	}
	fields, ok := entries[managed].(map[string]any)
	if !ok {
		return nil, ErrProjectClientRollbackIncomplete
	}
	after, wantMask, wantAlias, appended, err := createCodexProjectAttachmentBytes(before, logical, managed, fields, operation)
	if err != nil {
		return nil, err
	}
	schema := "project-client-create-plan-v1"
	if operation == "replace-enable-override" {
		schema = "project-client-attachment-plan-v2"
	}
	p := &ProjectHTTPAttachmentPreparation{projectIdentity: c.projectIdentitySHA256, configSHA: codexAttachmentSHA(before), logical: logical, managed: managed, schema: schema, operation: operation, before: append([]byte(nil), before...), after: after, originalToken: original, maskToken: wantMask, aliasToken: wantAlias, appended: appended}
	_, _, _, _, wantSeparators := p.OwnedDelta()
	if codexAttachmentSHA(after) != expectedAfterSHA || !bytes.Equal(mask, wantMask) || !bytes.Equal(alias, wantAlias) || !bytes.Equal(separators, wantSeparators) {
		return nil, ErrProjectClientRollbackIncomplete
	}
	return p, nil
}
func (l *lockingClient) RecoverProjectHTTPAttachment(logical, operation string, before []byte, afterSHA string, mask, alias, separators []byte) (p *ProjectHTTPAttachmentPreparation, err error) {
	err = l.withReadLock(func() error {
		c, ok := l.projectCodex()
		if !ok {
			return ErrProjectClientInvalid
		}
		p, err = c.RecoverProjectHTTPAttachment(logical, operation, before, afterSHA, mask, alias, separators)
		return err
	})
	return
}

func (l *lockingClient) CaptureProjectHTTPAttachmentBackup(ctx context.Context, p *ProjectHTTPAttachmentPreparation, owner ManagedRegistrationBackupIO) (id ManagedRegistrationBackupID, err error) {
	err = l.withMutationLock(func() error {
		id, err = l.captureProjectHTTPAttachmentBackupLockHeld(ctx, p, owner)
		return err
	})
	return
}

func (l *lockingClient) captureProjectHTTPAttachmentBackupLockHeld(ctx context.Context, p *ProjectHTTPAttachmentPreparation, owner ManagedRegistrationBackupIO) (ManagedRegistrationBackupID, error) {
	c, ok := l.projectCodex()
	if !ok || p == nil || p.projectIdentity != c.projectIdentitySHA256 {
		return ManagedRegistrationBackupID{}, ErrProjectClientInvalid
	}
	b, err := c.readProjectConfigBytes()
	if err != nil {
		return ManagedRegistrationBackupID{}, err
	}
	if !bytes.Equal(b, p.before) {
		return ManagedRegistrationBackupID{}, ErrProjectClientPlanStale
	}
	return l.createManagedRegistrationBackup(ctx, owner, c.path, p.before, p.configSHA)
}
func (p *ProjectHTTPAttachmentPreparation) BeforeBytes() []byte {
	return append([]byte(nil), p.before...)
}
func (p *ProjectHTTPAttachmentPreparation) AfterBytes() []byte {
	return append([]byte(nil), p.after...)
}
func (p *ProjectHTTPAttachmentPreparation) OwnedDelta() (string, string, []byte, []byte, []byte) {
	separators := bytes.Replace(p.appended, p.aliasToken, nil, 1)
	if p.operation == "create-absent" {
		separators = bytes.Replace(separators, p.maskToken, nil, 1)
	}
	return p.logical, p.managed, append([]byte(nil), p.maskToken...), append([]byte(nil), p.aliasToken...), append([]byte(nil), separators...)
}
func codexAttachmentSHA(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

// PrepareProjectHTTPAttachment uses the same raw policy owner as transport
// relocation. It performs no disk write and requires an already existing
// validated project config and a separately frozen global source snapshot.
func (c *codexCLI) PrepareProjectHTTPAttachment(logical string, entry MCPEntry, policySource string, globalBytes []byte) (*ProjectHTTPAttachmentPreparation, error) {
	if !c.isProjectBound() || logical == "" || logical != c.projectLogicalEntry || entry.Name != logical+"-mcphub" || entry.URL == "" {
		return nil, ErrProjectClientInvalid
	}
	before, err := c.readProjectConfigBytes()
	if err != nil {
		return nil, err
	}
	document, err := decodeCodexTOMLBytes(before)
	if err != nil {
		return nil, err
	}
	servers, ok := document["mcp_servers"].(map[string]any)
	if !ok {
		if _, exists := document["mcp_servers"]; exists {
			return nil, ErrProjectClientInvalid
		}
		servers = map[string]any{}
		document["mcp_servers"] = servers
	}
	if _, exists := servers[entry.Name]; exists {
		return nil, ErrProjectClientEntryConflict
	}
	p := &ProjectHTTPAttachmentPreparation{projectIdentity: c.projectIdentitySHA256, configSHA: codexAttachmentSHA(before), globalSHA: codexAttachmentSHA(globalBytes), logical: logical, managed: entry.Name, before: append([]byte(nil), before...), schema: "project-client-create-plan-v1", operation: "create-absent"}
	raw, present := servers[logical]
	if present {
		override, valid := raw.(map[string]any)
		enabled, isBool := override["enabled"].(bool)
		if !valid || len(override) != 1 || !isBool || !enabled {
			return nil, ErrProjectClientEntryConflict
		}
		token, found, err := extractTOMLEntryPhysicalToken(before, "mcp_servers", logical)
		if err != nil || !found {
			return nil, ErrProjectClientEntryConflict
		}
		p.originalToken = append([]byte(nil), token.memberBytes...)
		p.schema, p.operation = "project-client-attachment-plan-v2", "replace-enable-override"
	}
	global, err := decodeCodexTOMLBytes(globalBytes)
	if err != nil {
		return nil, err
	}
	selected, _ := global["mcp_servers"].(map[string]any)
	if _, collision := selected[entry.Name]; collision {
		return nil, ErrProjectClientEntryConflict
	}
	var fields map[string]any
	switch policySource {
	case "inherited-global":
		if selected == nil {
			return nil, ErrProjectClientInvalid
		}
		source, ok := selected[logical].(map[string]any)
		if !ok {
			return nil, ErrProjectClientInvalid
		}
		fields = codexHTTPReplacement(source, entry)
	case "native-defaults":
		fields = codexNewHTTPEntryFields(entry)
	default:
		return nil, ErrProjectClientInvalid
	}
	if present {
		fields["enabled"] = true
	}
	entry.Raw = cloneCodexRawEntry(fields)
	entry.Disabled = mcpEntryDisabled(fields)
	entry.ToolTimeoutSec, err = codexToolTimeoutSec(fields)
	if err != nil {
		return nil, err
	}
	p.desired = entry
	after, mask, alias, appended, err := createCodexProjectAttachmentBytes(before, logical, entry.Name, fields, p.operation)
	if err != nil {
		return nil, err
	}
	p.after, p.maskToken, p.aliasToken, p.appended = after, mask, alias, appended
	return p, nil
}

func (l *lockingClient) VerifyProjectHTTPAttachment(p *ProjectHTTPAttachmentPreparation) (err error) {
	return l.withReadLock(func() error {
		return l.verifyProjectHTTPAttachmentLockHeld(p)
	})
}

func (l *lockingClient) verifyProjectHTTPAttachmentLockHeld(p *ProjectHTTPAttachmentPreparation) error {
	c, ok := l.projectCodex()
	if !ok || p == nil || p.projectIdentity != c.projectIdentitySHA256 {
		return ErrProjectClientInvalid
	}
	b, err := c.readProjectConfigBytes()
	if err != nil {
		return err
	}
	if !bytes.Equal(b, p.after) {
		return ErrProjectClientPlanStale
	}
	return nil
}

func (l *lockingClient) VerifyRestoredProjectHTTPAttachment(p *ProjectHTTPAttachmentPreparation) error {
	return l.withReadLock(func() error {
		c, ok := l.projectCodex()
		if !ok || p == nil || p.projectIdentity != c.projectIdentitySHA256 {
			return ErrProjectClientInvalid
		}
		b, err := c.readProjectConfigBytes()
		if err != nil {
			return err
		}
		_, aliasPresent, err := extractTOMLEntryPhysicalToken(b, "mcp_servers", p.managed)
		if err != nil || aliasPresent {
			return ErrProjectClientRollbackIncomplete
		}
		logical, present, err := extractTOMLEntryPhysicalToken(b, "mcp_servers", p.logical)
		if err != nil {
			return err
		}
		if p.operation == "create-absent" {
			if present {
				return ErrProjectClientRollbackIncomplete
			}
			return nil
		}
		if !present || !bytes.Equal(logical.memberBytes, p.originalToken) {
			return ErrProjectClientRollbackIncomplete
		}
		return nil
	})
}

type ProjectHTTPAttachmentResult struct {
	Committed                 bool
	WriteAttempted            bool
	BeforeSHA256, AfterSHA256 string
	ExactInverse              *ProjectHTTPAttachmentPreparation
}

func (c *codexCLI) CreateProjectHTTPAttachment(p *ProjectHTTPAttachmentPreparation, globalBytes []byte) (ProjectHTTPAttachmentResult, error) {
	var result ProjectHTTPAttachmentResult
	if p == nil || !c.isProjectBound() || p.projectIdentity != c.projectIdentitySHA256 || p.logical != c.projectLogicalEntry || p.globalSHA != codexAttachmentSHA(globalBytes) {
		return result, ErrProjectClientPlanStale
	}
	current, err := c.readProjectConfigBytes()
	if err != nil {
		return result, err
	}
	if !bytes.Equal(current, p.before) {
		return result, ErrProjectClientPlanStale
	}
	// Recompute with the same byte owner; no caller-mintable postimage authority.
	after, mask, alias, appended, err := createCodexProjectAttachmentBytes(current, p.logical, p.managed, p.desired.Raw, p.operation)
	if err != nil {
		return result, err
	}
	if !bytes.Equal(after, p.after) || !bytes.Equal(mask, p.maskToken) || !bytes.Equal(alias, p.aliasToken) || !bytes.Equal(appended, p.appended) {
		return result, ErrProjectClientPlanStale
	}
	result.WriteAttempted = true
	if err := writeConfigFileWith(nil, c.path, after); err != nil {
		return result, err
	}
	result = ProjectHTTPAttachmentResult{Committed: true, WriteAttempted: true, BeforeSHA256: p.configSHA, AfterSHA256: codexAttachmentSHA(after), ExactInverse: p}
	readback, err := c.readProjectConfigBytes()
	if err != nil {
		return result, err
	}
	if !bytes.Equal(readback, after) {
		return result, ErrCodexProjectSourceChanged
	}
	return result, nil
}
func (c *codexCLI) RestoreCreatedProjectHTTPAttachment(p *ProjectHTTPAttachmentPreparation, verifiedBackup []byte) (ProjectHTTPAttachmentResult, error) {
	var result ProjectHTTPAttachmentResult
	if p == nil || !c.isProjectBound() || p.projectIdentity != c.projectIdentitySHA256 || p.logical != c.projectLogicalEntry || codexAttachmentSHA(verifiedBackup) != p.configSHA || !bytes.Equal(verifiedBackup, p.before) {
		return result, ErrProjectClientRollbackIncomplete
	}
	original, err := deriveCodexAttachmentOriginalToken(verifiedBackup, p.logical, p.managed, p.operation)
	if err != nil || !bytes.Equal(original, p.originalToken) {
		return result, ErrProjectClientRollbackIncomplete
	}
	current, err := c.readProjectConfigBytes()
	if err != nil {
		return result, err
	}
	var restored []byte
	if bytes.Equal(current, p.after) {
		restored = append([]byte(nil), verifiedBackup...)
	} else {
		restored, err = restoreCodexProjectAttachmentBytes(current, p.logical, p.managed, p.maskToken, p.aliasToken, p.appended, original, p.operation)
		if err != nil {
			return result, err
		}
	}
	beforeDoc, err := decodeCodexTOMLBytes(verifiedBackup)
	if err != nil {
		return result, err
	}
	afterDoc, err := decodeCodexTOMLBytes(restored)
	if err != nil {
		return result, err
	}
	b, _ := beforeDoc["mcp_servers"].(map[string]any)
	a, _ := afterDoc["mcp_servers"].(map[string]any)
	if !reflect.DeepEqual(a[p.logical], b[p.logical]) {
		return result, ErrProjectClientRollbackIncomplete
	}
	if _, present := a[p.managed]; present {
		return result, ErrProjectClientRollbackIncomplete
	}
	result.WriteAttempted = true
	if err := writeConfigFileWith(nil, c.path, restored); err != nil {
		return result, err
	}
	result.Committed = true
	result.BeforeSHA256 = codexAttachmentSHA(current)
	result.AfterSHA256 = codexAttachmentSHA(restored)
	readback, err := c.readProjectConfigBytes()
	if err != nil {
		return result, err
	}
	if !bytes.Equal(readback, restored) {
		return result, ErrProjectClientRollbackIncomplete
	}
	return result, nil
}

// Wrappers retain committed evidence when release is unconfirmed.
func (l *lockingClient) PrepareProjectHTTPAttachment(logical string, entry MCPEntry, policy string, global []byte) (p *ProjectHTTPAttachmentPreparation, err error) {
	err = l.withReadLock(func() error {
		c, ok := l.projectCodex()
		if !ok {
			return ErrProjectClientInvalid
		}
		p, err = c.PrepareProjectHTTPAttachment(logical, entry, policy, global)
		return err
	})
	return
}
func (l *lockingClient) CreateProjectHTTPAttachment(p *ProjectHTTPAttachmentPreparation, global []byte) (r ProjectHTTPAttachmentResult, err error) {
	err = l.withMutationLock(func() error {
		c, ok := l.projectCodex()
		if !ok {
			return ErrProjectClientInvalid
		}
		r, err = c.CreateProjectHTTPAttachment(p, global)
		return err
	})
	return
}
func (l *lockingClient) RestoreCreatedProjectHTTPAttachment(p *ProjectHTTPAttachmentPreparation, backup []byte) (r ProjectHTTPAttachmentResult, err error) {
	err = l.withMutationLock(func() error {
		c, ok := l.projectCodex()
		if !ok {
			return ErrProjectClientInvalid
		}
		r, err = c.RestoreCreatedProjectHTTPAttachment(p, backup)
		return err
	})
	return
}

type ProjectHTTPAttachmentClient interface {
	PrepareProjectHTTPAttachment(string, MCPEntry, string, []byte) (*ProjectHTTPAttachmentPreparation, error)
	CreateProjectHTTPAttachment(*ProjectHTTPAttachmentPreparation, []byte) (ProjectHTTPAttachmentResult, error)
	RestoreCreatedProjectHTTPAttachment(*ProjectHTTPAttachmentPreparation, []byte) (ProjectHTTPAttachmentResult, error)
	RecoverProjectHTTPAttachment(string, string, []byte, string, []byte, []byte, []byte) (*ProjectHTTPAttachmentPreparation, error)
	VerifyProjectHTTPAttachment(*ProjectHTTPAttachmentPreparation) error
	VerifyRestoredProjectHTTPAttachment(*ProjectHTTPAttachmentPreparation) error
}
