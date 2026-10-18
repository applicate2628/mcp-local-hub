package clients

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Observations originate at the existing project lock owner. Nil success is
// undefined, including failure before acquisition; error classes are not evidence.
type ProjectConfigLockExecution struct {
	BodyEntered      bool
	ReleaseDefined   bool
	ReleaseSucceeded *bool
	PrimaryError     error
	CleanupError     error
}

type CodexProjectConfigContextLockRunner interface {
	RunMutationContext(context.Context, func() error) (ProjectConfigLockExecution, error)
}

// Configured source acquisition needs observed, cancellable READ settlement.
// Mutation-only runners remain supported by their existing consumers.
type CodexProjectConfigContextReadLockRunner interface {
	RunReadContext(context.Context, func() error) (ProjectConfigLockExecution, error)
}

type ProjectHTTPAttachmentPolicyExecution struct {
	BodyEntered             bool
	ProjectWriteAttempted   bool
	ProjectCommitted        *bool
	GlobalPolicySHA256      *string
	ProjectBeforeSHA256     *string
	ProjectAfterSHA256      *string
	GlobalReleaseDefined    bool
	GlobalReleaseSucceeded  *bool
	ProjectReleaseDefined   bool
	ProjectReleaseSucceeded *bool
	PrimaryError            error
	CleanupError            error
}

type ProjectHTTPAttachmentPolicyTransactionOwner interface {
	WithProjectHTTPAttachmentPolicyTransaction(context.Context, Client, *ProjectHTTPAttachmentPreparation, func(context.Context, *ProjectHTTPAttachmentPolicyTxn) error) (ProjectHTTPAttachmentPolicyExecution, error)
}

type projectPolicyTransactionContextKey struct{}

// A transaction is valid only in its callback. It never exposes concrete owners
// or a global mutation operation, and does not call either locking decorator.
type ProjectHTTPAttachmentPolicyTxn struct {
	project      *lockingClient
	preparation  *ProjectHTTPAttachmentPreparation
	global       []byte
	ctx          context.Context
	active       bool
	backupCalled bool
	createCalled bool
	execution    *ProjectHTTPAttachmentPolicyExecution
}

func (t *ProjectHTTPAttachmentPolicyTxn) check(ctx context.Context) error {
	if t == nil || !t.active || ctx == nil || ctx.Value(projectPolicyTransactionContextKey{}) != t || t.ctx.Err() != nil {
		if t != nil && t.ctx != nil && t.ctx.Err() != nil {
			return t.ctx.Err()
		}
		return ErrProjectClientInvalid
	}
	return ctx.Err()
}

func (t *ProjectHTTPAttachmentPolicyTxn) CaptureBackup(ctx context.Context, owner ManagedRegistrationBackupIO) (ManagedRegistrationBackupID, error) {
	if err := t.check(ctx); err != nil {
		return ManagedRegistrationBackupID{}, err
	}
	if t.backupCalled {
		return ManagedRegistrationBackupID{}, ErrProjectClientInvalid
	}
	t.backupCalled = true
	return t.project.captureProjectHTTPAttachmentBackupLockHeld(ctx, t.preparation, owner)
}

func (t *ProjectHTTPAttachmentPolicyTxn) Create(ctx context.Context) (ProjectHTTPAttachmentResult, error) {
	if err := t.check(ctx); err != nil {
		return ProjectHTTPAttachmentResult{}, err
	}
	if t.createCalled {
		return ProjectHTTPAttachmentResult{}, ErrProjectClientInvalid
	}
	t.createCalled = true
	c, _ := t.project.projectCodex()
	r, err := c.CreateProjectHTTPAttachment(t.preparation, t.global)
	t.execution.ProjectWriteAttempted = r.WriteAttempted
	committed := r.Committed
	if !r.WriteAttempted || r.Committed {
		t.execution.ProjectCommitted = &committed
	} else {
		t.execution.ProjectCommitted = nil
	}
	if r.Committed {
		after := r.AfterSHA256
		t.execution.ProjectAfterSHA256 = &after
	}
	return r, err
}

func (t *ProjectHTTPAttachmentPolicyTxn) Verify(ctx context.Context) error {
	if err := t.check(ctx); err != nil {
		return err
	}
	return t.project.verifyProjectHTTPAttachmentLockHeld(t.preparation)
}

func (l *lockingClient) WithProjectHTTPAttachmentPolicyTransaction(ctx context.Context, project Client, p *ProjectHTTPAttachmentPreparation, fn func(context.Context, *ProjectHTTPAttachmentPolicyTxn) error) (observed ProjectHTTPAttachmentPolicyExecution, resultErr error) {
	defer func() {
		if observed.PrimaryError == nil && observed.CleanupError == nil {
			observed.PrimaryError = resultErr
		}
	}()
	if l == nil || ctx == nil || fn == nil || p == nil || ctx.Value(projectPolicyTransactionContextKey{}) != nil {
		return observed, ErrProjectClientInvalid
	}
	global, ok := l.Client.(*codexCLI)
	local, localOK := project.(*lockingClient)
	if !ok || global == nil || global.isProjectBound() || !localOK || l == local {
		return observed, ErrProjectClientInvalid
	}
	c, ok := local.projectCodex()
	if !ok || p.projectIdentity != c.projectIdentitySHA256 || p.logical != c.projectLogicalEntry {
		return observed, ErrProjectClientInvalid
	}
	if strings.EqualFold(filepath.Clean(global.path), filepath.Clean(c.path)) {
		return observed, ErrProjectClientInvalid
	}
	gi, ge := os.Stat(global.path)
	pi, pe := os.Stat(c.path)
	if ge == nil && pe == nil && os.SameFile(gi, pi) {
		return observed, ErrProjectClientInvalid
	}
	runner, err := local.projectRunner()
	if err != nil {
		return observed, err
	}
	contextRunner, ok := runner.(CodexProjectConfigContextLockRunner)
	if !ok {
		return observed, ErrCodexProjectConfigLockUnavailable
	}
	txn := &ProjectHTTPAttachmentPolicyTxn{project: local, preparation: p, execution: &observed}
	txn.ctx = context.WithValue(ctx, projectPolicyTransactionContextKey{}, txn)
	projectObserved := false
	globalExecution, err := withConfigLockExecutionContextPolicy(global.path, ctx, func() error {
		current, readErr := global.ReadProjectAttachmentPolicySnapshot()
		if readErr != nil {
			observed.PrimaryError = readErr
			return readErr
		}
		sha := codexAttachmentSHA(current)
		observed.GlobalPolicySHA256 = &sha
		if sha != p.globalSHA {
			observed.PrimaryError = ErrProjectClientPlanStale
			return observed.PrimaryError
		}
		txn.global = append([]byte(nil), current...)
		projectExecution, projectErr := contextRunner.RunMutationContext(txn.ctx, func() error {
			if err := txn.ctx.Err(); err != nil {
				return err
			}
			before, readErr := c.readProjectConfigBytes()
			if readErr != nil {
				return readErr
			}
			beforeSHA := codexAttachmentSHA(before)
			observed.ProjectBeforeSHA256 = &beforeSHA
			if !bytes.Equal(before, p.before) {
				return ErrProjectClientPlanStale
			}
			txn.active = true
			defer func() { txn.active = false }()
			observed.BodyEntered = true
			return fn(txn.ctx, txn)
		})
		observed.ProjectReleaseDefined = projectExecution.ReleaseDefined
		projectObserved = true
		observed.ProjectReleaseSucceeded = projectExecution.ReleaseSucceeded
		observed.PrimaryError = projectExecution.PrimaryError
		observed.CleanupError = projectExecution.CleanupError
		return projectErr
	}, false, true)
	observed.GlobalReleaseDefined = globalExecution.releaseDefined
	if globalExecution.releaseDefined {
		succeeded := globalExecution.releaseConfirmed
		observed.GlobalReleaseSucceeded = &succeeded
	}
	observed.CleanupError = errors.Join(observed.CleanupError, globalExecution.releaseErr)
	if !projectObserved {
		observed.PrimaryError = globalExecution.primaryErr
	}
	return observed, err
}

// Native lock results stay at their producing owner and survive late releases.
// They are private failure observations, never serialized replay capabilities.
type codexSharedStdioLockFailure struct {
	cause    error
	global   configLockExecution
	projects []ProjectConfigLockExecution
	sources  []*CodexConfiguredProjectProfile
}

func (e *codexSharedStdioLockFailure) Error() string { return "shared STDIO mutation did not settle" }
func (e *codexSharedStdioLockFailure) Unwrap() error { return e.cause }

func (l *lockingClient) withSharedStdioReplay(ctx context.Context, sourceName, targetName string, target MCPEntry, evidence CodexSharedStdioReplayEvidenceV1, sources []*CodexConfiguredProjectProfile, scope CodexSharedStdioWriteScope, desired CodexSharedStdioTokenState) (result CodexSharedStdioMutationResult, retErr error) {
	var globalExecution configLockExecution
	var projectExecutions []ProjectConfigLockExecution
	defer func() {
		if retErr != nil {
			retErr = &codexSharedStdioLockFailure{cause: retErr, global: globalExecution, projects: projectExecutions, sources: sources}
		}
	}()
	if l == nil || ctx == nil || scope == nil || sourceName == "" || sourceName != targetName || ctx.Value(projectPolicyTransactionContextKey{}) != nil {
		return result, ErrProjectClientInvalid
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, finite := ctx.Deadline(); !finite {
		return result, ErrProjectClientInvalid
	}
	global, ok := l.Client.(*codexCLI)
	if !ok || global == nil || global.isProjectBound() {
		return result, ErrProjectClientInvalid
	}
	if err := ValidateCodexSharedStdioReplayEvidence(evidence, sourceName, target); err != nil {
		return result, err
	}
	if len(sources) != len(evidence.Sources) {
		return result, ErrCodexSharedStdioReplayEvidenceInvalid
	}
	runners := make([]CodexProjectConfigContextLockRunner, len(sources))
	for i, p := range sources {
		if p == nil || p.owner == nil || p.owner == l || p.logical != sourceName || p.binding.ProjectIdentitySHA256 != evidence.Sources[i].ProjectIdentitySHA256 {
			return result, ErrProjectClientInvalid
		}
		c, ok := p.owner.projectCodex()
		if !ok || c.projectIdentitySHA256 != evidence.Sources[i].ProjectIdentitySHA256 || c.path != evidence.Sources[i].ProjectConfigPath {
			return result, ErrProjectClientInvalid
		}
		logical, referent, _, err := p.ConfiguredGlobalSourcePath()
		if err != nil {
			return result, err
		}
		if global.path != logical || logical != evidence.GlobalLocation.LogicalPath || referent != evidence.GlobalLocation.ReferentPath || !bytes.Equal(p.global, sources[0].global) {
			return result, ErrProjectClientPlanStale
		}
		runner, err := p.owner.projectRunner()
		if err != nil {
			return result, err
		}
		runners[i], ok = runner.(CodexProjectConfigContextLockRunner)
		if !ok {
			return result, ErrCodexProjectConfigLockUnavailable
		}
	}
	globalExecution, retErr = withConfigLockExecutionContextPolicy(global.path, ctx, func() error {
		var lockProject func(int) error
		lockProject = func(index int) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if index < len(runners) {
				execution, err := runners[index].RunMutationContext(ctx, func() error { return lockProject(index + 1) })
				projectExecutions[index] = execution
				err = errors.Join(err, execution.PrimaryError, execution.CleanupError)
				if err == nil && (!execution.BodyEntered || !execution.ReleaseDefined || execution.ReleaseSucceeded == nil || !*execution.ReleaseSucceeded) {
					err = ErrConfigLockReleaseUnconfirmed
				}
				return err
			}
			for i, p := range sources {
				if err := p.ValidateSharedStdioReplaySource(ctx, evidence.Sources[i], evidence.GlobalLocation); err != nil {
					return err
				}
				if err := p.ValidateConfiguredGlobalBinding(ctx); err != nil {
					return err
				}
			}
			entered := false
			var err error
			result, err = scope(func(snapshot *ConfigWriteSnapshot, publish func(ConfigWriteRequest) error) (CodexSharedStdioMutationResult, error) {
				if entered {
					return CodexSharedStdioMutationResult{}, ErrProjectClientInvalid
				}
				entered = true
				if snapshot == nil || !bytes.Equal(snapshot.contents, sources[0].global) {
					return CodexSharedStdioMutationResult{}, ErrConfigWriteSnapshotConflict
				}
				return global.mutateSharedStdioEntry(ctx, sourceName, evidence, desired, snapshot, publish)
			})
			if !entered && err == nil {
				return ErrProjectClientInvalid
			}
			return err
		}
		projectExecutions = make([]ProjectConfigLockExecution, len(runners))
		return lockProject(0)
	}, false, true)
	if retErr == nil && (!globalExecution.bodyEntered || !globalExecution.releaseDefined || !globalExecution.releaseConfirmed) {
		retErr = ErrConfigLockReleaseUnconfirmed
	}
	return result, retErr
}
