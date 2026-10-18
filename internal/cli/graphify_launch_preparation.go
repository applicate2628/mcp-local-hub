package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/daemon_env_overlay"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/secrets"
)

type projectServiceLaunchPreparationReader struct{}

func newProjectServiceLaunchPreparationReader() api.ProjectServiceLaunchPreparationReader {
	return &projectServiceLaunchPreparationReader{}
}

func (*projectServiceLaunchPreparationReader) ReadPreparedProjectServiceLaunch(ctx context.Context, request *api.ProjectServiceLaunchPreparationRequest) (*api.ProjectServiceLaunchPreparationObservation, error) {
	source := &projectServiceLaunchReadSource{request: request}
	return request.ObservePreparedLaunch(ctx, source)
}

type projectServiceReferenceRead struct {
	path   string
	bytes  []byte
	proof  *api.StateFileReadObservation
	absent bool
	cause  error
}

type projectServiceLaunchReadSource struct {
	request            *api.ProjectServiceLaunchPreparationRequest
	context            *daemon.StdioLaunchContext
	sourceHost         *daemon.StdioHost
	manifestHost       *daemon.StdioHost
	sourcePrepared     *daemon.StdioLaunchPreparation
	manifestPrepared   *daemon.StdioLaunchPreparation
	manifestRead       *api.ManifestSourceObservation
	freshManifestReads []*api.ManifestSourceObservation
	overlayPath        string
	overlayRead        *daemon_env_overlay.ReadObservation
	freshOverlayReads  []*daemon_env_overlay.ReadObservation
	references         []projectServiceReferenceRead
	freshReferences    []projectServiceReferenceRead
	stateRoot          string
	stateResolution    *api.StateDirResolutionObservation
	failure            error
	recheckFailures    []error
	service            api.ProjectServiceKind
	sourceKind         api.ProjectServicePreparationSourceKind
	providerPasses     [][]projectServiceReferenceRead
}

func (*projectServiceLaunchReadSource) String() string {
	return "ProjectService CLI launch source (private)"
}
func (s *projectServiceLaunchReadSource) GoString() string { return s.String() }

func (s *projectServiceLaunchReadSource) ObserveProjectServiceLaunch(ctx context.Context, request *api.ProjectServiceLaunchPreparationRequest) (projection api.ProjectServiceLaunchProjection, resultErr error) {
	defer func() { s.failure = resultErr }()
	if request == nil || request != s.request || s.context != nil || ctx == nil {
		return projection, errors.New("PROJECT_SERVICE_CLI_REQUEST_INVALID")
	}
	if _, finite := ctx.Deadline(); !finite {
		return projection, errors.New("PROJECT_SERVICE_CLI_DEADLINE_REQUIRED")
	}
	var err error
	s.context, err = daemon.CaptureStdioLaunchContext(ctx)
	if err != nil {
		return projection, err
	}
	name, raw, err := request.ManifestForPreparation(ctx)
	if err != nil {
		return projection, err
	}
	existing, err := request.SelectedManifestForPreparation(ctx)
	if err != nil {
		return projection, err
	}
	if existing {
		s.manifestRead, err = api.NewAPI().ManifestGetObserved(ctx, name)
		if err != nil {
			return projection, err
		}
		if !bytes.Equal(raw, s.manifestRead.Bytes()) {
			return projection, errors.New("PROJECT_SERVICE_SELECTED_MANIFEST_CHANGED")
		}
	}
	manifest, err := s.context.ParseManifest(bytes.NewReader(raw))
	if err != nil {
		return projection, err
	}
	if manifest.Name != name || manifest.Kind != "global" || manifest.Transport != "stdio-bridge" || len(manifest.Daemons) != 1 {
		return projection, errors.New("PROJECT_SERVICE_SELECTED_MANIFEST_NOT_ORDINARY_STDIO")
	}
	s.service, s.sourceKind, err = request.SourceKindForPreparation(ctx)
	if err != nil {
		return projection, err
	}
	var launch *clients.CodexConfiguredStdioLaunch
	if s.sourceKind == api.ProjectServiceConfiguredStdio {
		launch, err = request.ConfiguredLaunchForPreparation(ctx)
		if err != nil {
			return projection, err
		}
		for _, reference := range launch.EnvironmentReferences() {
			if reference.Source() != "local" {
				return projection, errors.New("PROJECT_SERVICE_REMOTE_ENV_REFERENCE_UNSUPPORTED")
			}
		}
	}
	s.stateRoot, s.stateResolution, err = api.ResolveDaemonStateDirObserved(ctx)
	if err != nil {
		return projection, err
	}
	s.overlayPath = filepath.Join(s.stateRoot, overlayBaseName)
	if injected, present := s.context.LookupEnvironment(daemonOverlayPathEnvVar); present && injected != "" {
		s.overlayPath = injected
	}
	s.overlayRead, err = daemon_env_overlay.LoadObserved(s.overlayPath)
	if err != nil {
		return projection, err
	}
	row := daemon_env_overlay.LookupOverlay(s.overlayRead.Overlay(), `\mcp-local-hub-`+name+"-"+manifest.Daemons[0].Name)
	parent := s.parentOverlayValues(row)
	if daemonOverlayAlreadyApplied(parent) {
		row = overlayValuesFromEnv(row, parent)
	} else {
		row, err = daemon_env_overlay.ExpandParentPath(row, parent)
		if err != nil {
			return projection, err
		}
	}
	vault, vaultErr := secrets.OpenVaultOptionalWithReader(defaultKeyPath(), defaultVaultPath(), func(path string) ([]byte, error) {
		read := readProjectServiceReference(ctx, path)
		s.references = append(s.references, read)
		return bytes.Clone(read.bytes), read.cause
	})
	// Strict qualification retains an irrelevant optional-vault failure but does
	// not manufacture original reference closure; it cannot attest that failure.
	if vaultErr != nil {
		return projection, vaultErr
	}
	resolver, err := s.context.BindResolver(secrets.NewResolver(vault, nil))
	if err != nil {
		return projection, err
	}
	resolved, omitted, err := resolver.ResolveMapBestEffort(manifest.Env)
	if err != nil {
		return projection, err
	}
	merged, unset, err := mergeResolvedDaemonEnvWithOverlayLookup(`\mcp-local-hub-`+name+"-"+manifest.Daemons[0].Name, resolved, omitted, func(string) (map[string]string, error) { return row, nil }, s.context.LookupEnvironment, manifest.EnvForwardLocal)
	if err != nil {
		return projection, err
	}
	// Keep the actual materialization on the original host. No resolver, map or
	// retained command is exposed through the API's read-only observation.
	s.manifestHost, err = daemon.NewPreparedStdioHost(ctx, daemon.StdioLaunchInputs{Context: s.context, Config: daemon.HostConfig{Command: manifest.Command, Env: merged, UnsetEnv: unset, WorkingDir: manifest.Daemons[0].Cwd, MCPProtocolCompatibilityProfile: manifest.Daemons[0].MCPProtocolCompatibilityProfile}, BaseArgs: manifest.BaseArgs, ExtraArgs: manifest.Daemons[0].ExtraArgs})
	if err != nil {
		return projection, err
	}
	s.manifestPrepared, err = s.manifestHost.PreparedLaunch()
	if err != nil {
		return projection, err
	}
	if s.sourceKind == api.ProjectServiceConfiguredStdio {
		cwd := ""
		if configured := launch.ConfiguredCWD(); configured != nil {
			cwd = *configured
		}
		inline, _ := launch.Environment()
		forward := make([]string, 0, len(launch.EnvironmentReferences()))
		for _, reference := range launch.EnvironmentReferences() {
			forward = append(forward, reference.Name())
		}
		sourceEnv, sourceUnset, sourceErr := mergeResolvedDaemonEnvWithOverlayLookup("", inline, nil, func(string) (map[string]string, error) { return nil, nil }, s.context.LookupEnvironment, forward)
		if sourceErr != nil {
			return projection, sourceErr
		}
		s.sourceHost, err = daemon.NewPreparedStdioHost(ctx, daemon.StdioLaunchInputs{Context: s.context, Config: daemon.HostConfig{Command: launch.Command(), Args: launch.Args(), Env: sourceEnv, UnsetEnv: sourceUnset, WorkingDir: cwd, MCPProtocolCompatibilityProfile: manifest.Daemons[0].MCPProtocolCompatibilityProfile}})
		if err != nil {
			return projection, err
		}
		s.sourcePrepared, err = s.sourceHost.PreparedLaunch()
		if err != nil {
			return projection, err
		}
	}
	inputs, err := request.ProviderFilesForPreparation(ctx)
	if err != nil {
		return projection, err
	}
	return request.ProjectPreparedLaunch(ctx, s.originalSourcePort(), s.manifestPrepared, inputs.Reads, []api.ProjectServiceLaunchReadObservation{s})
}

func (s *projectServiceLaunchReadSource) parentOverlayValues(row map[string]string) []string {
	var parent []string
	seen := map[string]bool{}
	for _, name := range append([]string{"PATH", daemonOverlayAppliedEnvVar}, mapProjectServiceKeys(row)...) {
		if seen[name] {
			continue
		}
		seen[name] = true
		if value, present := s.context.LookupEnvironment(name); present {
			parent = append(parent, name+"="+value)
		}
	}
	return parent
}

func mapProjectServiceKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func readProjectServiceReference(ctx context.Context, path string) projectServiceReferenceRead {
	raw, proof, err := api.ReadStateFileInodeAnchoredObserved(ctx, path, 64<<20)
	read := projectServiceReferenceRead{path: path, bytes: bytes.Clone(raw), proof: proof, cause: err}
	if proof == nil {
		read.cause = errors.Join(err, errors.New("PROJECT_SERVICE_REFERENCE_READ_OUTCOME_UNAVAILABLE"))
		return read
	}
	if err != nil {
		absenceErr := proof.ValidateAbsence(ctx, path, api.StateFileReadLeafNotFound)
		if absenceErr == nil {
			read.absent = true
			return read
		}
		read.cause = errors.Join(err, absenceErr)
		return read
	}
	digest := sha256.Sum256(raw)
	read.cause = proof.Validate(ctx, path, 64<<20, raw, hex.EncodeToString(digest[:]))
	return read
}

func (s *projectServiceLaunchReadSource) ValidateProjectServiceLaunchRead(ctx context.Context) error {
	if s == nil || s.request == nil || s.context == nil || s.failure != nil || s.manifestPrepared == nil {
		return errors.New("PROJECT_SERVICE_CLI_READ_SOURCE_INCOMPLETE")
	}
	if err := s.stateResolution.Validate(ctx, s.stateRoot); err != nil {
		return err
	}
	if _, err := s.overlayRead.Fingerprint(); err != nil {
		return err
	}
	if s.manifestRead != nil {
		if err := s.manifestRead.ValidateProjectServiceLaunchRead(ctx); err != nil {
			return err
		}
	}
	if s.sourceKind == api.ProjectServiceConfiguredStdio {
		if _, err := s.sourcePrepared.ObserveExecutionProfile(ctx); err != nil {
			return err
		}
	} else if s.sourceKind != api.ProjectServiceManagedDefinition || s.sourcePrepared != nil {
		return errors.New("PROJECT_SERVICE_CLI_SOURCE_KIND_CHANGED")
	}
	_, err := s.manifestPrepared.ObserveExecutionProfile(ctx)
	return err
}

func (s *projectServiceLaunchReadSource) RecheckProjectServiceLaunch(ctx context.Context, request *api.ProjectServiceLaunchPreparationRequest) (projection api.ProjectServiceLaunchProjection, resultErr error) {
	defer func() { s.recheckFailures = append(s.recheckFailures, resultErr) }()
	if request == nil || request != s.request {
		return projection, errors.New("PROJECT_SERVICE_CLI_REQUEST_CHANGED")
	}
	if err := s.ValidateProjectServiceLaunchRead(ctx); err != nil {
		return projection, err
	}
	if s.manifestRead != nil {
		name, _, err := request.ManifestForPreparation(ctx)
		if err != nil {
			return projection, err
		}
		fresh, err := api.NewAPI().ManifestGetObserved(ctx, name)
		s.freshManifestReads = append(s.freshManifestReads, fresh)
		if err != nil {
			return projection, err
		}
		if !s.manifestRead.SameSelection(fresh) {
			return projection, errors.New("PROJECT_SERVICE_MANIFEST_SELECTION_DRIFT")
		}
	}
	fresh, err := daemon_env_overlay.LoadObserved(s.overlayPath)
	s.freshOverlayReads = append(s.freshOverlayReads, fresh)
	if err != nil {
		return projection, err
	}
	before, err := s.overlayRead.Fingerprint()
	if err != nil {
		return projection, err
	}
	after, err := fresh.Fingerprint()
	if err != nil {
		return projection, err
	}
	if before != after {
		return projection, errors.New("PROJECT_SERVICE_OVERLAY_DRIFT")
	}
	for _, original := range s.references {
		fresh := readProjectServiceReference(ctx, original.path)
		s.freshReferences = append(s.freshReferences, fresh)
		if fresh.cause != nil && !fresh.absent {
			return projection, fresh.cause
		}
		if fresh.absent != original.absent || !bytes.Equal(fresh.bytes, original.bytes) {
			return projection, errors.New("PROJECT_SERVICE_REFERENCE_DRIFT")
		}
	}
	service, kind, err := request.SourceKindForPreparation(ctx)
	if err != nil {
		return projection, err
	}
	if service != s.service || kind != s.sourceKind {
		return projection, errors.New("PROJECT_SERVICE_SOURCE_KIND_DRIFT")
	}
	inputs, err := request.ProviderFilesForPreparation(ctx)
	if err != nil {
		return projection, err
	}
	return request.ProjectPreparedLaunch(ctx, s.originalSourcePort(), s.manifestPrepared, inputs.Reads, []api.ProjectServiceLaunchReadObservation{s})
}

func (s *projectServiceLaunchReadSource) originalSourcePort() api.ProjectServicePreparedLaunch {
	if s.sourcePrepared == nil {
		return nil
	}
	return s.sourcePrepared
}

// This projects only the two declared provider cuts in491F. File roles are
// acquisition DATA; the API owns source/entry/options/dataset qualification.
func projectServiceProviderInputs(service api.ProjectServiceKind, profile daemon.StdioLaunchExecutionProfile, selected []byte) ([]api.ProjectServiceProviderFileInput, error) {
	command := filepath.Clean(profile.CommandPath)
	if !filepath.IsAbs(command) {
		return nil, errors.New("PROJECT_SERVICE_SELECTED_COMMAND_UNAVAILABLE")
	}
	switch service {
	case api.ProjectServiceGraphify:
		zipStart := bytes.Index(selected, []byte{'P', 'K', 3, 4})
		if zipStart < 0 {
			return nil, errors.New("PROJECT_SERVICE_GRAPHIFY_SELECTED_SHIM_UNAVAILABLE")
		}
		shebang := bytes.LastIndex(selected[:zipStart], []byte("#!"))
		if shebang < 0 {
			return nil, errors.New("PROJECT_SERVICE_GRAPHIFY_SELECTED_INTERPRETER_UNAVAILABLE")
		}
		line := strings.SplitN(string(selected[shebang+2:zipStart]), "\n", 2)[0]
		interpreter := strings.Trim(strings.TrimSpace(line), "\"")
		if !filepath.IsAbs(interpreter) {
			return nil, errors.New("PROJECT_SERVICE_GRAPHIFY_SELECTED_INTERPRETER_UNAVAILABLE")
		}
		venv := filepath.Dir(filepath.Dir(interpreter))
		packages := filepath.Join(venv, "Lib", "site-packages")
		metadata := filepath.Join(packages, "graphifyy-0.9.78.dist-info")
		return []api.ProjectServiceProviderFileInput{
			{Role: api.ProjectServiceProviderLauncher, Path: command},
			{Role: api.ProjectServiceProviderInterpreter, Path: interpreter},
			{Role: api.ProjectServiceProviderVenv, Path: filepath.Join(venv, "pyvenv.cfg")},
			{Role: api.ProjectServiceProviderEntryPoints, Path: filepath.Join(metadata, "entry_points.txt")},
			{Role: api.ProjectServiceProviderMetadata, Path: filepath.Join(metadata, "METADATA")},
			{Role: api.ProjectServiceProviderEntry, Path: filepath.Join(packages, "graphify", "serve.py")},
			{Role: api.ProjectServiceProviderPaths, Path: filepath.Join(packages, "graphify", "paths.py")},
		}, nil
	case api.ProjectServiceGraft:
		name := strings.ToLower(filepath.Base(command))
		if name != "node" && name != "node.exe" {
			return nil, errors.New("PROJECT_SERVICE_GRAFT_ORIGINAL_DIRECT_NODE_REQUIRED")
		}
		if len(profile.Arguments) == 0 || profile.Arguments[0] == "" || strings.HasPrefix(profile.Arguments[0], "-") {
			return nil, errors.New("PROJECT_SERVICE_GRAFT_ORIGINAL_DIRECT_MODULE_REQUIRED")
		}
		module := profile.Arguments[0]
		if !filepath.IsAbs(module) {
			if filepath.VolumeName(module) != "" || strings.HasPrefix(module, "/") || strings.HasPrefix(module, "\\") || !filepath.IsAbs(profile.WorkingDirectory) {
				return nil, errors.New("PROJECT_SERVICE_GRAFT_MODULE_CONTEXT_REQUIRED")
			}
			module = filepath.Join(profile.WorkingDirectory, module)
		}
		return []api.ProjectServiceProviderFileInput{
			{Role: api.ProjectServiceProviderInterpreter, Path: command},
			{Role: api.ProjectServiceProviderEntry, Path: filepath.Clean(module)},
			{Role: api.ProjectServiceProviderMetadata, Path: filepath.Join(filepath.Dir(filepath.Dir(module)), "package.json")},
		}, nil
	default:
		return nil, errors.New("PROJECT_SERVICE_KIND_INVALID")
	}
}

func (s *projectServiceLaunchReadSource) ReadProjectServiceProviderInputs(ctx context.Context, request *api.ProjectServiceLaunchPreparationRequest) (input api.ProjectServiceProviderInputRead, resultErr error) {
	if s == nil || request == nil || request != s.request || s.context == nil || s.manifestPrepared == nil {
		return input, errors.New("PROJECT_SERVICE_PROVIDER_ORIGINAL_SOURCE_REQUIRED")
	}
	service, kind, err := request.SourceKindForPreparation(ctx)
	if err != nil {
		return input, err
	}
	if service != s.service || kind != s.sourceKind {
		return input, errors.New("PROJECT_SERVICE_PROVIDER_SOURCE_KIND_CHANGED")
	}
	profile, err := s.manifestPrepared.ObserveExecutionProfile(ctx)
	if err != nil {
		return input, err
	}
	input.Execution = s.manifestPrepared
	pass := len(s.providerPasses)
	s.providerPasses = append(s.providerPasses, nil)
	read := func(path string) (api.ProjectServiceProviderFileRead, error) {
		observed := readProjectServiceReference(ctx, path)
		s.providerPasses[pass] = append(s.providerPasses[pass], observed)
		value := api.ProjectServiceProviderFileRead{Observation: observed.proof, Raw: bytes.Clone(observed.bytes)}
		input.Reads = append(input.Reads, value)
		return value, observed.cause
	}
	// The prepared command, not ambient PATH or a package/cache inventory,
	// selects the first original file. Its actual bytes supply the shim edge.
	selected, err := read(profile.CommandPath)
	if err != nil {
		return input, err
	}
	input.Files, err = projectServiceProviderInputs(service, profile, selected.Raw)
	if err != nil {
		return input, err
	}
	for _, file := range input.Files {
		if filepath.Clean(file.Path) == filepath.Clean(profile.CommandPath) {
			continue
		}
		if _, err := read(file.Path); err != nil {
			return input, err
		}
	}
	return input, nil
}
