package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/clients"
)

type InstallProjectClientRollback struct {
	ProjectRoot      string
	LogicalEntryName string
	GenerationID     string
	Apply            bool
}

func ValidateInstallProjectClientRollback(opts InstallOpts) error {
	c := opts.ProjectClientRollback
	if c == nil {
		return nil
	}
	if opts.ProjectClientCreate != nil || !filepath.IsAbs(c.ProjectRoot) || !managedRegistrationSafeIdentifier(c.LogicalEntryName) || !strings.HasPrefix(c.GenerationID, "project-") || !managedRegistrationBackupDigestValid(strings.TrimPrefix(c.GenerationID, "project-")) || opts.IncludeAllClients || opts.SkipClientConfigWrites || len(opts.ClientsInclude) != 0 || opts.DaemonFilter != "" || opts.CodexProjectRoot != "" || opts.CodexWorkingDir != "" || opts.DryRun == c.Apply {
		return clients.ErrProjectClientInvalid
	}
	return nil
}

func (a *API) rollbackProjectClientAttachment(ctx context.Context, opts InstallOpts, w io.Writer) error {
	if err := ValidateInstallProjectClientRollback(opts); err != nil {
		return err
	}
	c := opts.ProjectClientRollback
	project, identity, err := clients.NewCodexCLIForProject(c.ProjectRoot, c.LogicalEntryName, NewCodexProjectConfigMutationOwner)
	if err != nil {
		return err
	}
	parent := filepath.Dir(project.ConfigPath())
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	owner := managedRegistrationBackupIO{}
	var capsule projectClientAttachmentCapsule
	found := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), filepath.Base(project.ConfigPath())+".bak-mcp-local-hub-") || !(strings.HasSuffix(entry.Name(), ".project-client-create-v1.json") || strings.HasSuffix(entry.Name(), ".project-client-attachment-v2.json")) {
			continue
		}
		b, err := ReadClientConfigBackupBeneathRootNoFollow(ctx, parent, []string{entry.Name()}, "")
		if err != nil {
			return err
		}
		candidate, err := decodeProjectClientAttachmentCapsule(b)
		if err != nil {
			return err
		}
		if candidate.GenerationID != c.GenerationID {
			continue
		}
		if found {
			return clients.ErrProjectClientRollbackIncomplete
		}
		expectedLeaf, err := projectClientCapsuleLeaf(candidate.BackupID, candidate.Schema)
		if err != nil || expectedLeaf != entry.Name() {
			return clients.ErrProjectClientRollbackIncomplete
		}
		capsule = candidate
		found = true
	}
	if !found || capsule.OwnedDelta.LogicalKey != c.LogicalEntryName || capsule.Plan.ProjectIdentitySHA256 != identity.ProjectIdentitySHA256() {
		return clients.ErrProjectClientRollbackIncomplete
	}
	backup, err := owner.Read(ctx, parent, capsule.BackupID.String(), capsule.BeforeConfigSHA256)
	if err != nil {
		return err
	}
	preparation, err := recoverProjectClientAttachment(project, identity, capsule, backup)
	if err != nil {
		return err
	}
	p := &projectClientAttachmentPlan{public: capsule.Plan, preparation: preparation, project: project, identity: identity}
	if opts.DryRun {
		if w != nil {
			return json.NewEncoder(w).Encode(capsule.Plan)
		}
		return nil
	}
	clientOwner := project.(clients.ProjectHTTPAttachmentClient)
	if capsule.Phase == "rolled-back" {
		return clientOwner.VerifyRestoredProjectHTTPAttachment(preparation)
	}
	root, err := DaemonStateDir()
	if err != nil {
		return err
	}
	ref := capsule.BackendReceipt.Receipt.BackendInstall
	if ref == nil {
		return clients.ErrProjectClientRollbackIncomplete
	}
	lease, err := NewCodexProjectConfigMutationOwner(identity)
	if err != nil {
		return err
	}
	if capsule.Phase != "rollback-pending" {
		next := capsule
		next.Phase = "rollback-pending"
		if err := lease.RunMutation(func() error { return owner.transitionProjectClientCapsule(ctx, parent, capsule, next) }); err != nil {
			return err
		}
		capsule = next
	}
	if err := clientOwner.VerifyRestoredProjectHTTPAttachment(preparation); err != nil {
		if _, err := clientOwner.RestoreCreatedProjectHTTPAttachment(preparation, backup); err != nil {
			return err
		}
	}
	if err := retireProjectAttachmentProjection(ctx, root, p, &capsule); err != nil {
		return err
	}
	if _, err := a.RestoreOwnedBackendInstall(ctx, ref); err != nil {
		return errors.Join(clients.ErrProjectClientRollbackIncomplete, err)
	}
	next := capsule
	next.Phase = "rolled-back"
	return lease.RunMutation(func() error { return owner.transitionProjectClientCapsule(ctx, parent, capsule, next) })
}
