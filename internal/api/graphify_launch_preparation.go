package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"mcp-local-hub/internal/clients"
)

type ProjectServiceExecutionProfile struct {
	CommandPath                string   `json:"-"`
	Arguments                  []string `json:"-"`
	WorkingDirectory           string   `json:"-"`
	EffectiveEnvironmentSHA256 string   `json:"-"`
}

func (ProjectServiceExecutionProfile) String() string                  { return "project-service execution (private)" }
func (v ProjectServiceExecutionProfile) GoString() string              { return v.String() }
func (v ProjectServiceExecutionProfile) Format(s fmt.State, verb rune) { fmt.Fprint(s, v.String()) }

// The trusted production preparation implements this port directly. DATA views
// do not implement equality or reconstruct the underlying preparation.
type ProjectServicePreparedLaunch interface {
	ProjectServiceExecutionProfile(context.Context) (ProjectServiceExecutionProfile, error)
	SameProjectServiceExecution(context.Context, ProjectServicePreparedLaunch) (bool, error)
	EnvironmentValueMatches(string, string) (bool, bool)
}
type ProjectServicePreparationSourceKind string

const (
	ProjectServiceConfiguredStdio   ProjectServicePreparationSourceKind = "configured-stdio"
	ProjectServiceManagedDefinition ProjectServicePreparationSourceKind = "managed-definition"
)
const (
	ProjectServiceProviderLauncher    = "launcher"
	ProjectServiceProviderInterpreter = "interpreter"
	ProjectServiceProviderVenv        = "pyvenv"
	ProjectServiceProviderEntryPoints = "entry-points"
	ProjectServiceProviderMetadata    = "package-metadata"
	ProjectServiceProviderEntry       = "provider-entry"
	ProjectServiceProviderPaths       = "provider-paths"
)

type ProjectServiceProviderFileInput struct {
	Role string `json:"-"`
	Path string `json:"-"`
}

func (ProjectServiceProviderFileInput) String() string {
	return "project-service provider input (private)"
}
func (v ProjectServiceProviderFileInput) GoString() string              { return v.String() }
func (v ProjectServiceProviderFileInput) Format(s fmt.State, verb rune) { fmt.Fprint(s, v.String()) }

type ProjectServiceProviderFileRead struct {
	Observation *StateFileReadObservation `json:"-"`
	Raw         []byte                    `json:"-"`
}

func (ProjectServiceProviderFileRead) String() string {
	return "project-service provider read (private)"
}
func (v ProjectServiceProviderFileRead) GoString() string              { return v.String() }
func (v ProjectServiceProviderFileRead) Format(s fmt.State, verb rune) { fmt.Fprint(s, v.String()) }
func (r ProjectServiceProviderFileRead) validate(ctx context.Context) error {
	o := r.Observation
	if o == nil || len(r.Raw) == 0 {
		return errors.New("PROJECT_SERVICE_PROVIDER_ORIGINAL_READ_REQUIRED")
	}
	return o.Validate(ctx, o.path, o.maxBytes, r.Raw, stateFileReadObservationDigest(r.Raw))
}

type ProjectServiceProviderInputRead struct {
	Execution ProjectServicePreparedLaunch      `json:"-"`
	Files     []ProjectServiceProviderFileInput `json:"-"`
	Reads     []ProjectServiceProviderFileRead  `json:"-"`
}

func (ProjectServiceProviderInputRead) String() string {
	return "project-service provider inputs (private)"
}
func (v ProjectServiceProviderInputRead) GoString() string              { return v.String() }
func (v ProjectServiceProviderInputRead) Format(s fmt.State, verb rune) { fmt.Fprint(s, v.String()) }

type ProjectServiceLaunchPreparationReader interface {
	ReadPreparedProjectServiceLaunch(context.Context, *ProjectServiceLaunchPreparationRequest) (*ProjectServiceLaunchPreparationObservation, error)
}
type ProjectServiceLaunchObservationSource interface {
	ObserveProjectServiceLaunch(context.Context, *ProjectServiceLaunchPreparationRequest) (ProjectServiceLaunchProjection, error)
	RecheckProjectServiceLaunch(context.Context, *ProjectServiceLaunchPreparationRequest) (ProjectServiceLaunchProjection, error)
	ReadProjectServiceProviderInputs(context.Context, *ProjectServiceLaunchPreparationRequest) (ProjectServiceProviderInputRead, error)
}
type ProjectServiceLaunchReadObservation interface {
	ValidateProjectServiceLaunchRead(context.Context) error
}
type ProjectServiceLaunchPreparationRequest struct {
	api                *API
	factory            *projectServicePreparationFactory
	service            ProjectServiceKind
	sourceKind         ProjectServicePreparationSourceKind
	canonicalWorkspace string
	source             *clients.CodexConfiguredProjectProfile
	launch             *clients.CodexConfiguredStdioLaunch
	managed            *AdoptProvenanceRecord
	bindExisting       *AdoptProvenanceRecord
	manifestName       string
	manifest           []byte
	manifestHash       string
	selectedExisting   bool
	ctx                context.Context
	passMu             sync.Mutex
	passSource         ProjectServiceLaunchObservationSource
	passObservation    *ProjectServiceLaunchPreparationObservation
	passInput          *ProjectServiceProviderInputRead
	providerReading    bool
}
type projectServicePreparationFactory struct {
	api    *API
	reader ProjectServiceLaunchPreparationReader
}
type ProjectServiceLaunchProjection struct {
	request         *ProjectServiceLaunchPreparationRequest
	sourcePrepared  ProjectServicePreparedLaunch
	desiredPrepared ProjectServicePreparedLaunch
	execution       ProjectServiceExecutionProfile
	recipe          *projectServiceRecipe
	providerFiles   []ProjectServiceProviderFileRead
	reads           []ProjectServiceLaunchReadObservation
}
type ProjectServiceLaunchPreparationObservation struct {
	request          *ProjectServiceLaunchPreparationRequest
	source           ProjectServiceLaunchObservationSource
	original         ProjectServiceLaunchProjection
	rechecks         []ProjectServiceLaunchProjection
	providerInputs   []ProjectServiceProviderInputRead
	providerFailures []error
	originalFailure  error
	recheckFailures  []error
	qualified        bool
}
type projectServiceLaunchObservationFailure struct {
	operation   *projectServicePlan
	observation *ProjectServiceLaunchPreparationObservation
	cause       error
}

func (*ProjectServiceLaunchPreparationRequest) String() string {
	return "project-service launch request (private)"
}
func (r *ProjectServiceLaunchPreparationRequest) GoString() string { return r.String() }
func (r *ProjectServiceLaunchPreparationRequest) Format(s fmt.State, verb rune) {
	fmt.Fprint(s, r.String())
}
func (ProjectServiceLaunchProjection) String() string {
	return "project-service launch projection (private)"
}
func (p ProjectServiceLaunchProjection) GoString() string              { return p.String() }
func (p ProjectServiceLaunchProjection) Format(s fmt.State, verb rune) { fmt.Fprint(s, p.String()) }
func (*ProjectServiceLaunchPreparationObservation) String() string {
	return "project-service launch observation (private)"
}
func (o *ProjectServiceLaunchPreparationObservation) GoString() string { return o.String() }
func (o *ProjectServiceLaunchPreparationObservation) Format(s fmt.State, verb rune) {
	fmt.Fprint(s, o.String())
}
func (projectServiceLaunchObservationFailure) Error() string {
	return "PROJECT_SERVICE_LAUNCH_OBSERVATION_UNQUALIFIED"
}
func (e projectServiceLaunchObservationFailure) Unwrap() error    { return e.cause }
func (e projectServiceLaunchObservationFailure) GoString() string { return e.Error() }
func (e projectServiceLaunchObservationFailure) Format(s fmt.State, verb rune) {
	fmt.Fprint(s, e.Error())
}

func NewAPIWithSerenaAndProjectServicePreparation(assembly SerenaPreparationAssembly, reader ProjectServiceLaunchPreparationReader) *API {
	a := NewAPIWithSerenaPreparation(assembly)
	a.projectServicePreparationFactory = &projectServicePreparationFactory{api: a, reader: reader}
	return a
}
func sameProjectServiceContext(left, right context.Context) bool {
	if left == nil || right == nil {
		return false
	}
	// Invocation identity is required. Unsupported non-comparable context
	// values cannot be compared as Go interfaces and must not panic a receiver.
	typeOf := reflect.TypeOf(left)
	return typeOf == reflect.TypeOf(right) && typeOf.Comparable() && left == right
}
func (r *ProjectServiceLaunchPreparationRequest) validate(ctx context.Context) error {
	if r == nil || r.api == nil || r.factory == nil || r.factory != r.api.projectServicePreparationFactory || r.factory.api != r.api || r.factory.reader == nil || !sameProjectServiceContext(r.ctx, ctx) || ctx.Err() != nil || r.source == nil || r.canonicalWorkspace == "" || r.manifestName == "" || len(r.manifest) == 0 || r.manifestHash != ManifestHashContent(r.manifest) {
		return errors.New("PROJECT_SERVICE_LAUNCH_REQUEST_UNISSUED_OR_STALE")
	}
	if _, finite := ctx.Deadline(); !finite {
		return errors.New("PROJECT_SERVICE_LAUNCH_DEADLINE_REQUIRED")
	}
	if _, err := r.source.Fingerprint(); err != nil {
		return err
	}
	if r.service != ProjectServiceGraphify && r.service != ProjectServiceGraft {
		return errors.New("PROJECT_SERVICE_KIND_INVALID")
	}
	switch r.sourceKind {
	case ProjectServiceConfiguredStdio:
		if r.launch == nil || r.managed != nil || r.bindExisting != nil {
			return errors.New("PROJECT_SERVICE_CONFIGURED_SOURCE_REQUIRED")
		}
	case ProjectServiceManagedDefinition:
		m := r.managed
		if m == nil && r.bindExisting != nil {
			b := r.bindExisting
			if b.ProjectService != nil || b.OperationState != AdoptOperationStateAdopted || b.ManifestName != r.manifestName || b.ExpectedManifestHash != r.manifestHash || b.SourceClient == "" || b.SourceEntryName == "" {
				return errors.New("PROJECT_SERVICE_BIND_EXISTING_SOURCE_UNQUALIFIED")
			}
			return nil
		}
		if m == nil || m.ProjectService == nil || m.ProjectService.WorkspacePath != r.canonicalWorkspace || m.ProjectService.Service != r.service || m.ManifestName != r.manifestName || m.ExpectedManifestHash != r.manifestHash {
			return errors.New("PROJECT_SERVICE_MANAGED_SOURCE_UNQUALIFIED")
		}
		if err := validateProjectServiceRecord(*m); err != nil {
			return err
		}
	default:
		return errors.New("PROJECT_SERVICE_SOURCE_KIND_INVALID")
	}
	return nil
}
func (r *ProjectServiceLaunchPreparationRequest) ManifestForPreparation(ctx context.Context) (string, []byte, error) {
	if err := r.validate(ctx); err != nil {
		return "", nil, err
	}
	return r.manifestName, bytes.Clone(r.manifest), nil
}
func (r *ProjectServiceLaunchPreparationRequest) ConfiguredLaunchForPreparation(ctx context.Context) (*clients.CodexConfiguredStdioLaunch, error) {
	if err := r.validate(ctx); err != nil {
		return nil, err
	}
	if r.sourceKind != ProjectServiceConfiguredStdio {
		return nil, errors.New("PROJECT_SERVICE_MANAGED_SOURCE_HAS_NO_PRIOR_STDIO")
	}
	return r.source.ConfiguredStdioLaunch()
}
func (r *ProjectServiceLaunchPreparationRequest) SelectedManifestForPreparation(ctx context.Context) (bool, error) {
	if err := r.validate(ctx); err != nil {
		return false, err
	}
	return r.selectedExisting, nil
}
func (r *ProjectServiceLaunchPreparationRequest) SourceKindForPreparation(ctx context.Context) (ProjectServiceKind, ProjectServicePreparationSourceKind, error) {
	if err := r.validate(ctx); err != nil {
		return "", "", err
	}
	return r.service, r.sourceKind, nil
}
func cloneProjectServiceProviderInput(v ProjectServiceProviderInputRead) ProjectServiceProviderInputRead {
	v.Files = slices.Clone(v.Files)
	v.Reads = slices.Clone(v.Reads)
	for i := range v.Reads {
		v.Reads[i].Raw = bytes.Clone(v.Reads[i].Raw)
	}
	return v
}
func (r *ProjectServiceLaunchPreparationRequest) ProviderFilesForPreparation(ctx context.Context) (ProjectServiceProviderInputRead, error) {
	if err := r.validate(ctx); err != nil {
		return ProjectServiceProviderInputRead{}, err
	}
	r.passMu.Lock()
	source, observation := r.passSource, r.passObservation
	if source == nil || observation == nil || r.providerReading {
		r.passMu.Unlock()
		return ProjectServiceProviderInputRead{}, errors.New("PROJECT_SERVICE_PROVIDER_READ_OUTSIDE_ORIGINAL_PASS")
	}
	r.providerReading = true
	r.passMu.Unlock()
	input, err := source.ReadProjectServiceProviderInputs(ctx, r)
	input = cloneProjectServiceProviderInput(input)
	r.passMu.Lock()
	r.providerReading = false
	observation.providerInputs = append(observation.providerInputs, input)
	observation.providerFailures = append(observation.providerFailures, err)
	if r.passObservation != observation {
		err = errors.Join(err, errors.New("PROJECT_SERVICE_PROVIDER_PASS_CHANGED"))
	}
	if err == nil {
		if input.Execution == nil || len(input.Files) == 0 {
			err = errors.New("PROJECT_SERVICE_PROVIDER_SELECTION_UNAVAILABLE")
		} else {
			_, err = input.Execution.ProjectServiceExecutionProfile(ctx)
		}
	}
	if err == nil {
		saved := cloneProjectServiceProviderInput(input)
		r.passInput = &saved
	}
	observation.providerFailures[len(observation.providerFailures)-1] = err
	r.passMu.Unlock()
	return cloneProjectServiceProviderInput(input), err
}
func (r *ProjectServiceLaunchPreparationRequest) ProjectPreparedLaunch(ctx context.Context, sourcePrepared, desiredPrepared ProjectServicePreparedLaunch, providerFiles []ProjectServiceProviderFileRead, reads []ProjectServiceLaunchReadObservation) (p ProjectServiceLaunchProjection, err error) {
	p = ProjectServiceLaunchProjection{request: r, sourcePrepared: sourcePrepared, desiredPrepared: desiredPrepared, providerFiles: slices.Clone(providerFiles), reads: slices.Clone(reads)}
	for i := range p.providerFiles {
		p.providerFiles[i].Raw = bytes.Clone(p.providerFiles[i].Raw)
	}
	if err = r.validate(ctx); err != nil {
		return p, err
	}
	if desiredPrepared == nil {
		return p, errors.New("PROJECT_SERVICE_DESIRED_PREPARATION_REQUIRED")
	}
	p.execution, err = desiredPrepared.ProjectServiceExecutionProfile(ctx)
	if err != nil {
		return p, err
	}
	p.execution.Arguments = slices.Clone(p.execution.Arguments)
	if r.sourceKind == ProjectServiceConfiguredStdio {
		if sourcePrepared == nil {
			return p, errors.New("PROJECT_SERVICE_SOURCE_PREPARATION_REQUIRED")
		}
		if _, err = sourcePrepared.ProjectServiceExecutionProfile(ctx); err != nil {
			return p, err
		}
		var equal bool
		equal, err = sourcePrepared.SameProjectServiceExecution(ctx, desiredPrepared)
		if err != nil {
			return p, err
		}
		if !equal {
			return p, errors.New("PROJECT_SERVICE_LAUNCH_DIFFERENT")
		}
	} else if sourcePrepared != nil {
		return p, errors.New("PROJECT_SERVICE_MANAGED_PRIOR_STDIO_UNEXPECTED")
	}
	r.passMu.Lock()
	input := r.passInput
	active := r.passSource != nil && r.passObservation != nil
	if input != nil {
		saved := cloneProjectServiceProviderInput(*input)
		input = &saved
	}
	r.passMu.Unlock()
	if !active || input == nil {
		return p, errors.New("PROJECT_SERVICE_PROVIDER_INPUTS_UNRECEIVED")
	}
	equal, compareErr := desiredPrepared.SameProjectServiceExecution(ctx, input.Execution)
	if compareErr != nil {
		return p, compareErr
	}
	if !equal {
		return p, errors.New("PROJECT_SERVICE_PROVIDER_EXECUTION_CHANGED")
	}
	p.recipe, err = qualifyProjectServiceRecipe(ctx, r, p.execution, desiredPrepared, *input, p.providerFiles)
	if err == nil {
		err = p.validate(ctx, r)
	}
	return p, err
}
func projectServiceExecutionEqual(a, b ProjectServiceExecutionProfile) bool {
	return a.CommandPath == b.CommandPath && a.WorkingDirectory == b.WorkingDirectory && a.EffectiveEnvironmentSHA256 == b.EffectiveEnvironmentSHA256 && slices.Equal(a.Arguments, b.Arguments)
}
func (p ProjectServiceLaunchProjection) validate(ctx context.Context, r *ProjectServiceLaunchPreparationRequest) error {
	if p.request != r || p.recipe == nil || p.desiredPrepared == nil || len(p.reads) == 0 {
		return errors.New("PROJECT_SERVICE_LAUNCH_PROJECTION_INCOMPLETE")
	}
	if err := r.validate(ctx); err != nil {
		return err
	}
	current, err := p.desiredPrepared.ProjectServiceExecutionProfile(ctx)
	if err != nil {
		return err
	}
	if !projectServiceExecutionEqual(current, p.execution) {
		return errors.New("PROJECT_SERVICE_EXECUTION_DRIFT")
	}
	for _, read := range p.reads {
		if read == nil {
			return errors.New("PROJECT_SERVICE_ORIGINAL_READ_UNAVAILABLE")
		}
		if err := read.ValidateProjectServiceLaunchRead(ctx); err != nil {
			return err
		}
	}
	for _, read := range p.providerFiles {
		if err := read.validate(ctx); err != nil {
			return err
		}
	}
	return nil
}
func (o *ProjectServiceLaunchPreparationObservation) invoke(ctx context.Context, recheck bool) (p ProjectServiceLaunchProjection, err error) {
	r := o.request
	r.passMu.Lock()
	if r.passSource != nil {
		r.passMu.Unlock()
		return p, errors.New("PROJECT_SERVICE_OBSERVATION_ALREADY_ACTIVE")
	}
	r.passSource, r.passObservation, r.passInput = o.source, o, nil
	r.passMu.Unlock()
	defer func() {
		r.passMu.Lock()
		r.passSource, r.passObservation, r.passInput = nil, nil, nil
		r.providerReading = false
		r.passMu.Unlock()
	}()
	if recheck {
		return o.source.RecheckProjectServiceLaunch(ctx, r)
	}
	return o.source.ObserveProjectServiceLaunch(ctx, r)
}
func (r *ProjectServiceLaunchPreparationRequest) ObservePreparedLaunch(ctx context.Context, source ProjectServiceLaunchObservationSource) (*ProjectServiceLaunchPreparationObservation, error) {
	o := &ProjectServiceLaunchPreparationObservation{request: r, source: source}
	var err error
	if source == nil {
		err = errors.New("PROJECT_SERVICE_LAUNCH_READ_SOURCE_UNAVAILABLE")
	} else if err = r.validate(ctx); err == nil {
		o.original, err = o.invoke(ctx, false)
		if err == nil {
			err = o.original.validate(ctx, r)
		}
	}
	o.originalFailure = err
	if err != nil {
		return o, &projectServiceLaunchObservationFailure{observation: o, cause: err}
	}
	o.qualified = true
	return o, nil
}
func (o *ProjectServiceLaunchPreparationObservation) recheck(ctx context.Context) error {
	if o == nil || !o.qualified || o.originalFailure != nil || o.source == nil || o.request == nil {
		return errors.New("PROJECT_SERVICE_OBSERVATION_NOT_QUALIFIED")
	}
	p, err := o.invoke(ctx, true)
	o.rechecks = append(o.rechecks, p)
	if err == nil {
		err = p.validate(ctx, o.request)
	}
	if err == nil && (!projectServiceExecutionEqual(p.execution, o.original.execution) || p.recipe.profileSHA256 != o.original.recipe.profileSHA256) {
		err = fmt.Errorf("%w: PROJECT_SERVICE_LAUNCH_RECEIVER_DRIFT", clients.ErrCodexProjectSourceChanged)
	}
	o.recheckFailures = append(o.recheckFailures, err)
	if err != nil {
		o.qualified = false
		return &projectServiceLaunchObservationFailure{observation: o, cause: err}
	}
	return nil
}
