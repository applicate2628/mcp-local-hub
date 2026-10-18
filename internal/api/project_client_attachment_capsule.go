package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"

	"mcp-local-hub/internal/clients"
)

// Tokens and raw keys stay in the private backup-owner sibling only.
type projectClientOwnedDelta struct {
	LogicalKey       string `json:"logical_key"`
	ManagedKey       string `json:"managed_key"`
	MaskTokenBase64  string `json:"mask_token_base64"`
	AliasTokenBase64 string `json:"alias_token_base64"`
	SeparatorsBase64 string `json:"separators_base64"`
}
type projectClientBackendReceipt struct {
	Ownership string                   `json:"ownership"`
	Receipt   InstallMutationReceiptV1 `json:"receipt"`
}
type projectClientAttachmentCapsule struct {
	Schema             string                              `json:"schema"`
	Phase              string                              `json:"phase"`
	GenerationID       string                              `json:"generation_id"`
	Plan               ProjectClientCreatePlanV1           `json:"plan"`
	BeforeConfigSHA256 string                              `json:"before_config_sha256"`
	AfterConfigSHA256  string                              `json:"after_config_sha256"`
	BackupID           clients.ManagedRegistrationBackupID `json:"backup_id"`
	OwnedDelta         projectClientOwnedDelta             `json:"owned_delta"`
	BackendReceipt     projectClientBackendReceipt         `json:"backend_receipt"`
}

func (c projectClientAttachmentCapsule) validate() error {
	if err := c.Plan.validate(); err != nil {
		return err
	}
	if c.Schema == "project-client-create-v1" {
		if c.Plan.Operation != "create-absent" {
			return clients.ErrProjectClientInvalid
		}
	} else if c.Schema == "project-client-attachment-v2" {
		if c.Plan.Operation != "replace-enable-override" {
			return clients.ErrProjectClientInvalid
		}
	} else {
		return clients.ErrProjectClientInvalid
	}
	switch c.Phase {
	case "prepared", "project-written", "committed", "rollback-pending", "rolled-back":
	default:
		return clients.ErrProjectClientInvalid
	}
	if !managedRegistrationSafeIdentifier(c.GenerationID) || c.BeforeConfigSHA256 != c.Plan.ConfigBeforeSHA256 || !managedRegistrationBackupDigestValid(c.AfterConfigSHA256) {
		return clients.ErrProjectClientInvalid
	}
	if _, err := clients.ParseManagedRegistrationBackupID(c.BackupID.String()); err != nil {
		return err
	}
	d := c.OwnedDelta
	if !managedRegistrationSafeIdentifier(d.LogicalKey) || d.ManagedKey != d.LogicalKey+"-mcphub" || projectClientHash([]byte(d.LogicalKey)) != c.Plan.LogicalKeySHA256 || projectClientHash([]byte(d.ManagedKey)) != c.Plan.ManagedKeySHA256 {
		return clients.ErrProjectClientInvalid
	}
	for _, text := range []string{d.MaskTokenBase64, d.AliasTokenBase64} {
		b, err := base64.StdEncoding.Strict().DecodeString(text)
		if err != nil || len(b) == 0 || base64.StdEncoding.EncodeToString(b) != text {
			return clients.ErrProjectClientInvalid
		}
	}
	// The existing physical table owner can include every inserted separator
	// in a table token. An explicitly present empty residual is then exact.
	if b, err := base64.StdEncoding.Strict().DecodeString(d.SeparatorsBase64); err != nil || base64.StdEncoding.EncodeToString(b) != d.SeparatorsBase64 {
		return clients.ErrProjectClientInvalid
	}
	if c.BackendReceipt.Ownership != "created" && c.BackendReceipt.Ownership != "reused" {
		return clients.ErrProjectClientInvalid
	}
	r := c.BackendReceipt.Receipt
	if r.BackendInstall == nil || r.BackendInstall.outcome != "sealed" {
		return clients.ErrProjectClientInvalid
	}
	if r.DryRun || !r.Committed || len(r.ExpectedTasks) != 1 || r.ExpectedTasks[0] == "" {
		return clients.ErrProjectClientInvalid
	}
	return nil
}

func decodeProjectClientAttachmentCapsule(data []byte) (projectClientAttachmentCapsule, error) {
	var c projectClientAttachmentCapsule
	if err := decodeProjectAttachmentJSON(data, &c); err != nil {
		return c, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return c, err
	}
	if !projectClientExactJSONFields(fields, "schema", "phase", "generation_id", "plan", "before_config_sha256", "after_config_sha256", "backup_id", "owned_delta", "backend_receipt") {
		return c, clients.ErrProjectClientInvalid
	}
	var owned, backend map[string]json.RawMessage
	if json.Unmarshal(fields["owned_delta"], &owned) != nil || !projectClientExactJSONFields(owned, "logical_key", "managed_key", "mask_token_base64", "alias_token_base64", "separators_base64") || json.Unmarshal(fields["backend_receipt"], &backend) != nil || !projectClientExactJSONFields(backend, "ownership", "receipt") {
		return c, clients.ErrProjectClientInvalid
	}
	if _, err := DecodeProjectClientCreatePlan(fields["plan"]); err != nil {
		return c, err
	}
	var receipt map[string]json.RawMessage
	if json.Unmarshal(backend["receipt"], &receipt) != nil || len(receipt) != 9 {
		return c, clients.ErrProjectClientInvalid
	}
	for _, name := range []string{"managed_backend_install", "DryRun", "Committed", "ExpectedTasks", "RequireBindings", "CanMigrate", "StartupBindDeadline", "MutationRows", "ClientConfigSettlements"} {
		if _, ok := receipt[name]; !ok {
			return c, clients.ErrProjectClientInvalid
		}
	}
	return c, c.validate()
}

func projectClientCapsuleLeaf(id clients.ManagedRegistrationBackupID, schema string) (string, error) {
	if _, err := clients.ParseManagedRegistrationBackupID(id.String()); err != nil {
		return "", err
	}
	switch schema {
	case "project-client-create-v1":
		return id.String() + ".project-client-create-v1.json", nil
	case "project-client-attachment-v2":
		return id.String() + ".project-client-attachment-v2.json", nil
	default:
		return "", clients.ErrProjectClientInvalid
	}
}

func newProjectClientAttachmentCapsule(p *projectClientAttachmentPlan, id clients.ManagedRegistrationBackupID, backend projectClientBackendReceipt) (projectClientAttachmentCapsule, error) {
	if p == nil || p.preparation == nil {
		return projectClientAttachmentCapsule{}, clients.ErrProjectClientInvalid
	}
	logical, managed, mask, alias, separators := p.preparation.OwnedDelta()
	schema := "project-client-create-v1"
	if p.public.Operation == "replace-enable-override" {
		schema = "project-client-attachment-v2"
	}
	c := projectClientAttachmentCapsule{Schema: schema, Phase: "prepared", GenerationID: "project-" + p.public.PlanSHA256, Plan: p.public, BeforeConfigSHA256: p.public.ConfigBeforeSHA256, AfterConfigSHA256: p.preparation.AfterSHA256(), BackupID: id, OwnedDelta: projectClientOwnedDelta{LogicalKey: logical, ManagedKey: managed, MaskTokenBase64: base64.StdEncoding.EncodeToString(mask), AliasTokenBase64: base64.StdEncoding.EncodeToString(alias), SeparatorsBase64: base64.StdEncoding.EncodeToString(separators)}, BackendReceipt: backend}
	return c, c.validate()
}

// Caller holds the same project identity lease for phase transitions. The
// sibling path is derived only from the validated backup ID, never user input.
func (owner managedRegistrationBackupIO) createProjectClientCapsule(ctx context.Context, parent string, c projectClientAttachmentCapsule) error {
	if err := c.validate(); err != nil {
		return err
	}
	leaf, err := projectClientCapsuleLeaf(c.BackupID, c.Schema)
	if err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	r, err := owner.Create(ctx, parent, leaf, b, projectClientHash(b))
	if err != nil {
		return err
	}
	if r.Disposition != clients.ManagedRegistrationBackupCommitted {
		return clients.ErrProjectClientInvalid
	}
	readback, err := owner.Read(ctx, parent, leaf, projectClientHash(b))
	if err != nil {
		return err
	}
	if !bytes.Equal(b, readback) {
		return clients.ErrProjectClientInvalid
	}
	return nil
}

func (owner managedRegistrationBackupIO) transitionProjectClientCapsule(ctx context.Context, parent string, before, after projectClientAttachmentCapsule) error {
	if err := before.validate(); err != nil {
		return err
	}
	if err := after.validate(); err != nil {
		return err
	}
	x, y := before, after
	x.Phase = ""
	y.Phase = ""
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	if !bytes.Equal(xb, yb) {
		return clients.ErrProjectClientPlanStale
	}
	allowed := before.Phase == "prepared" && (after.Phase == "project-written" || after.Phase == "rollback-pending") || before.Phase == "project-written" && (after.Phase == "committed" || after.Phase == "rollback-pending") || before.Phase == "committed" && after.Phase == "rollback-pending" || before.Phase == "rollback-pending" && after.Phase == "rolled-back"
	if !allowed {
		return clients.ErrProjectClientInvalid
	}
	leaf, err := projectClientCapsuleLeaf(before.BackupID, before.Schema)
	if err != nil {
		return err
	}
	b, err := json.Marshal(before)
	if err != nil {
		return err
	}
	current, err := owner.Read(ctx, parent, leaf, projectClientHash(b))
	if err != nil {
		return err
	}
	if !bytes.Equal(current, b) {
		return clients.ErrProjectClientPlanStale
	}
	next, err := json.Marshal(after)
	if err != nil {
		return err
	}
	if err := secureWritePathBased(filepath.Join(parent, leaf), next); err != nil {
		return err
	}
	readback, err := owner.Read(ctx, parent, leaf, projectClientHash(next))
	if err != nil {
		return err
	}
	if !bytes.Equal(next, readback) {
		return clients.ErrProjectClientInvalid
	}
	return nil
}

func recoverProjectClientAttachment(project clients.Client, identity clients.CodexProjectIdentity, c projectClientAttachmentCapsule, backup []byte) (*clients.ProjectHTTPAttachmentPreparation, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if project == nil || c.Plan.ProjectIdentitySHA256 != identity.ProjectIdentitySHA256() || projectClientHash([]byte(project.ConfigPath())) != c.Plan.ConfigPathSHA256 || projectClientHash(backup) != c.BeforeConfigSHA256 {
		return nil, clients.ErrProjectClientRollbackIncomplete
	}
	d := c.OwnedDelta
	mask, _ := base64.StdEncoding.Strict().DecodeString(d.MaskTokenBase64)
	alias, _ := base64.StdEncoding.Strict().DecodeString(d.AliasTokenBase64)
	separators, _ := base64.StdEncoding.Strict().DecodeString(d.SeparatorsBase64)
	owner, ok := project.(clients.ProjectHTTPAttachmentClient)
	if !ok {
		return nil, clients.ErrProjectClientInvalid
	}
	return owner.RecoverProjectHTTPAttachment(d.LogicalKey, c.Plan.Operation, backup, c.AfterConfigSHA256, mask, alias, separators)
}
