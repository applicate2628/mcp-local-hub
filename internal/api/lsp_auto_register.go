package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

var lspEnsureLocks sync.Map // map[string]*sync.Mutex, keyed by workspaceKey + "\x00" + language

// loadLSPRegisterManifestFn is the test seam for the embedded-manifest load in
// EnsureLSPRegistered. Production wires the real loadLSPRegisterManifest (loads
// the shipped mcp-language-server manifest); tests inject a synthetic manifest
// (e.g. an inert availability=watch row) to exercise the D-3 admission gate
// hermetically without a catalog edit. Mirrors the package's other `…Fn` seams
// (proxyReadinessFn, registerSupervisorReconcileFn).
var loadLSPRegisterManifestFn = loadLSPRegisterManifest

const lspExistingProxyProbeTimeout = 500 * time.Millisecond

// EnsureLSPRegistered idempotently creates the supervised workspace-proxy row
// for one (workspaceKey, language) tuple without writing any client configs.
// The per-tuple lock spans registry write, supervisor intent write, reconcile,
// and proxy readiness so concurrent first-touch calls cannot route to an
// unready port or allocate two ports for the same language.
func (a *API) EnsureLSPRegistered(ctx context.Context, workspaceKey, workspacePath, language string) (entry WorkspaceEntry, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if language == "" {
		return WorkspaceEntry{}, fmt.Errorf("language is required")
	}
	canonical, err := CanonicalWorkspacePath(workspacePath)
	if err != nil {
		return WorkspaceEntry{}, err
	}
	computedKey := WorkspaceKey(canonical)
	if workspaceKey == "" {
		workspaceKey = computedKey
	}
	keyMismatchErr := error(nil)
	if workspaceKey != computedKey {
		keyMismatchErr = fmt.Errorf("workspace key %q does not match canonical workspace %q key %q", workspaceKey, canonical, computedKey)
	}

	mu := lspEnsureMutex(workspaceKey, language)
	mu.Lock()
	defer mu.Unlock()

	if err := ctx.Err(); err != nil {
		return WorkspaceEntry{}, err
	}

	m, spec, err := loadLSPRegisterManifestFn(language)
	if err != nil {
		return WorkspaceEntry{}, err
	}
	// D-3 availability admission gate (shared single owner), run immediately
	// after the manifest load and BEFORE any registry write, supervisor-intent
	// upsert, reconcile, or spawn. It guards BOTH downstream branches below — the
	// prior-row promotion branch (legacy → supervised) and the first-registration
	// branch — so a watch / disabled-until-probe manifest whose install-probe has
	// not passed never reaches an intent/reconcile mutation. ADDITIVE: the shipped
	// mcp-language-server manifest carries no availability/install_probe, so this
	// returns nil immediately (byte-identical first-touch behavior).
	if err := AvailabilityAdmission(m); err != nil {
		return WorkspaceEntry{}, err
	}
	if m.PortPool == nil {
		return WorkspaceEntry{}, fmt.Errorf("manifest %s has no port_pool", m.Name)
	}

	regPath, err := registryPathForRegister()
	if err != nil {
		return WorkspaceEntry{}, err
	}
	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return WorkspaceEntry{}, fmt.Errorf("acquire registry lock: %w", err)
	}
	defer func() { ReleaseAndJoin(&err, unlock) }()
	if err := reg.Load(); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("load registry: %w", err)
	}
	if prior, ok := reg.Get(workspaceKey, language); ok {
		releaseErr := unlock()
		unlock = nil
		if releaseErr != nil {
			return prior, releaseErr
		}
		portReady := proxyReadinessFn(prior.Port, lspExistingProxyProbeTimeout) == nil
		owned, err := lspSupervisorIntentDescriptorExists(prior.WorkspaceKey, prior.Language)
		if err != nil {
			return WorkspaceEntry{}, err
		}
		if portReady && owned {
			return prior, nil
		}
		if err := ctx.Err(); err != nil {
			return WorkspaceEntry{}, err
		}
		canonicalExe, err := canonicalMcphubPath()
		if err != nil {
			return WorkspaceEntry{}, err
		}
		if _, err := os.Stat(canonicalExe); err != nil {
			return WorkspaceEntry{}, fmt.Errorf("%s not present — run `mcphub setup` once: %w", canonicalExe, err)
		}
		restoreLegacyTask := func() {}
		if !owned {
			if sch, schErr := newScheduler(); schErr == nil {
				var legacyXML []byte
				if xml, xerr := sch.ExportXML(prior.TaskName); xerr == nil {
					legacyXML = xml
				} else if !errors.Is(xerr, scheduler.ErrTaskNotFound) {
					return WorkspaceEntry{}, fmt.Errorf("export legacy LSP task %s: %w", prior.TaskName, xerr)
				}
				if len(legacyXML) > 0 {
					capturedXML := legacyXML
					legacyTaskDeleted := false
					restoreLegacyTask = func() {
						if !legacyTaskDeleted {
							return
						}
						if prior.Port > 0 {
							_ = killByPortFn(prior.Port, 5*time.Second)
						}
						_ = sch.ImportXML(prior.TaskName, capturedXML)
						_ = sch.Run(prior.TaskName)
					}
					if err := killObservedLiveLSPProxy(prior.Port, prior.TaskName, portReady); err != nil {
						return WorkspaceEntry{}, err
					}
					if derr := sch.Delete(prior.TaskName); derr != nil && !errors.Is(derr, scheduler.ErrTaskNotFound) {
						restoreLegacyTask()
						return WorkspaceEntry{}, fmt.Errorf("delete legacy LSP task %s before promote: %w", prior.TaskName, derr)
					}
					legacyTaskDeleted = true
				} else if err := killObservedLiveLSPProxy(prior.Port, prior.TaskName, portReady); err != nil {
					return WorkspaceEntry{}, err
				}
			} else if schedulerUnavailableError(schErr) {
				// No scheduler (Linux/macOS): no legacy task exists; still free a
				// stale unowned proxy on the port so reconcile can bind cleanly.
				if err := killObservedLiveLSPProxy(prior.Port, prior.TaskName, portReady); err != nil {
					return WorkspaceEntry{}, err
				}
			} else {
				return WorkspaceEntry{}, schErr
			}
		}
		restoreIntent, err := a.upsertLSPSupervisorIntent(prior, canonicalExe)
		if err != nil {
			restoreLegacyTask()
			return WorkspaceEntry{}, err
		}
		intentWritten := true
		rollbackIntent := func() {
			if intentWritten {
				restoreIntent()
				intentWritten = false
			}
		}

		reconcileCtx, cancel := context.WithTimeout(ctx, DefaultReconcileTimeout)
		if _, err := registerSupervisorReconcileFn(reconcileCtx, true); err != nil {
			cancel()
			rollbackIntent()
			restoreLegacyTask()
			return WorkspaceEntry{}, fmt.Errorf("supervisor reconcile after LSP intent write: %w", err)
		}
		cancel()

		if err := proxyReadinessFn(prior.Port, 10*time.Second); err != nil {
			rollbackIntent()
			restoreLegacyTask()
			return WorkspaceEntry{}, fmt.Errorf("proxy readiness on port %d: %w", prior.Port, err)
		}
		return prior, nil
	}
	if keyMismatchErr != nil {
		return WorkspaceEntry{}, keyMismatchErr
	}

	port, err := AllocatePort(reg, *m.PortPool)
	if err != nil {
		return WorkspaceEntry{}, err
	}
	canonicalExe, err := canonicalMcphubPath()
	if err != nil {
		return WorkspaceEntry{}, err
	}
	if _, err := os.Stat(canonicalExe); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("%s not present — run `mcphub setup` once: %w", canonicalExe, err)
	}

	entry = WorkspaceEntry{
		WorkspaceKey:  workspaceKey,
		WorkspacePath: canonical,
		Language:      language,
		Backend:       spec.Backend,
		Port:          port,
		TaskName:      LSPTaskNameForWorkspaceLanguage(workspaceKey, language),
		ClientEntries: map[string]string{},
		WeeklyRefresh: resolveWeeklyRefresh(a, RegisterOpts{}),
		Lifecycle:     LifecycleConfigured,
	}
	if err := reg.PutLSP(entry); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("ensure LSP register: row write rejected: %w", err)
	}
	if err := reg.Save(); err != nil {
		return WorkspaceEntry{}, fmt.Errorf("persist registry: %w", err)
	}
	releaseErr := unlock()
	unlock = nil
	forwardCommitted := releaseErr != nil

	supervisorSpawnRequested := false
	rollback := func() error {
		if forwardCommitted {
			return nil
		}
		return rollbackLSPRegistration(entry, supervisorSpawnRequested, func() error {
			return removeLSPRegistryRow(regPath, workspaceKey, language)
		})
	}

	if err := ctx.Err(); err != nil {
		return entry, errors.Join(releaseErr, err, rollback())
	}
	restoreIntent, err := a.upsertLSPSupervisorIntent(entry, canonicalExe)
	if err != nil {
		return entry, errors.Join(releaseErr, err, rollback())
	}
	intentWritten := true
	rollbackIntent := func() {
		if intentWritten {
			restoreIntent()
			intentWritten = false
		}
	}

	reconcileCtx, cancel := context.WithTimeout(ctx, DefaultReconcileTimeout)
	if _, err := registerSupervisorReconcileFn(reconcileCtx, true); err != nil {
		cancel()
		if !forwardCommitted {
			rollbackIntent()
		}
		return entry, errors.Join(releaseErr, fmt.Errorf("supervisor reconcile after LSP intent write: %w", err), rollback())
	}
	cancel()
	supervisorSpawnRequested = true

	if err := proxyReadinessFn(port, 10*time.Second); err != nil {
		if !forwardCommitted {
			rollbackIntent()
		}
		return entry, errors.Join(releaseErr, fmt.Errorf("proxy readiness on port %d: %w", port, err), rollback())
	}

	return entry, releaseErr
}

func rollbackLSPRegistration(entry WorkspaceEntry, supervisorSpawnRequested bool, removeRow func() error) error {
	var rollbackErrs []error
	if supervisorSpawnRequested && entry.Port > 0 {
		rollbackErrs = append(rollbackErrs, killByPortFn(entry.Port, 5*time.Second))
	}
	if removeRow != nil {
		rollbackErrs = append(rollbackErrs, removeRow())
	}
	return errors.Join(rollbackErrs...)
}

func lspEnsureMutex(workspaceKey, language string) *sync.Mutex {
	key := workspaceKey + "\x00" + language
	v, _ := lspEnsureLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func loadLSPRegisterManifest(language string) (*config.ServerManifest, config.LanguageSpec, error) {
	data, err := loadManifestYAMLEmbedFirst("mcp-language-server")
	if err != nil {
		return nil, config.LanguageSpec{}, fmt.Errorf("load manifest mcp-language-server: %w", err)
	}
	m, err := parseManifestForName("mcp-language-server", data)
	if err != nil {
		return nil, config.LanguageSpec{}, err
	}
	if m.Kind != config.KindWorkspaceScoped {
		return nil, config.LanguageSpec{}, fmt.Errorf("manifest %s: not workspace-scoped", m.Name)
	}
	for _, spec := range m.Languages {
		if spec.Name == language {
			return m, spec, nil
		}
	}
	return nil, config.LanguageSpec{}, fmt.Errorf("unknown language %q (manifest %s supports: %v)", language, m.Name, sortedLanguageNames(m))
}

func removeLSPRegistryRow(regPath, workspaceKey, language string) (err error) {
	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return err
	}
	defer ReleaseAndJoin(&err, unlock)
	if err := reg.Load(); err != nil {
		return err
	}
	reg.Remove(workspaceKey, language)
	return reg.Save()
}
