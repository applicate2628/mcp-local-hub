package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/servers"
	"os"
	"path/filepath"
	"reflect"
)

type PreparedSerenaLaunchObservation struct {
	api                                                *API
	prepared                                           *PreparedSerenaParsedInstall
	descriptor                                         SupervisorDaemon
	binding                                            PreparedSerenaRuntimeBinding
	task, requestSHA, epoch, rawManifestSHA, intentSHA string
	generation                                         uint64
	operationID                                        string
	root, intentPath                                   string
	manifest                                           *config.ServerManifest
	manifestRaw                                        []byte
	environment                                        *SerenaConsumerEnvironmentObservation
	policy                                             SerenaPreparationPolicyV2
	source                                             *buildinfo.PublisherSourceObservation
	namespace                                          *SupervisorStateNamespaceObservation
	namespaceClose                                     *StateFileFreshNamespaceCloseObservation
	readers                                            []*StateFileReadObservation
	received                                           bool
}

func (a *API) ObservePreparedSerenaLaunch(ctx context.Context, task string) (*PreparedSerenaLaunchObservation, error) {
	return a.observePreparedSerenaLaunch(ctx, task, nil)
}

func (a *API) observePreparedSerenaLaunch(ctx context.Context, task string, prepared *PreparedSerenaParsedInstall) (launch *PreparedSerenaLaunchObservation, retErr error) {
	if a == nil || ctx == nil || ctx.Err() != nil || task == "" {
		return nil, errors.New("SERENA_PREPARED_LAUNCH_INPUT_MISSING")
	}
	if _, finite := ctx.Deadline(); !finite {
		return nil, errors.New("SERENA_PREPARED_LAUNCH_DEADLINE_MISSING")
	}
	if a.serenaPrivatePublisher != nil {
		bounded, cancel, err := a.serenaPrivatePublisher.boundedContext(ctx, a)
		if err != nil {
			return nil, err
		}
		defer cancel()
		ctx = bounded
	}
	if prepared != nil {
		if err := prepared.validateConsumerLaunchSource(ctx, a); err != nil {
			return nil, err
		}
	}
	assembly, ok := a.serenaPreparationAssembly.(SerenaConsumerEnvironmentAssembly)
	if !ok || assembly == nil {
		return nil, errors.New("SERENA_CONSUMER_ENVIRONMENT_UNAVAILABLE")
	}
	source, err := buildinfo.CurrentPublisherSource()
	if err != nil {
		return nil, err
	}
	if err = source.Validate(source.Epoch()); err != nil {
		return nil, err
	}
	operation, err := NewSerenaConsumerOperationID(ctx)
	if err != nil {
		return nil, err
	}
	root, resolution, err := ResolveDaemonStateDirObserved(ctx)
	if err != nil {
		return nil, err
	}
	if err = resolution.Validate(ctx, root); err != nil {
		return nil, err
	}
	path := filepath.Join(root, supervisorIntentFileLeaf)
	if injected := os.Getenv(SupervisorIntentPathEnvVar); injected != "" && injected != path {
		return nil, errors.New("SERENA_PREPARED_CONTROL_NAMESPACE_DRIFT")
	}
	requestRaw, err := json.Marshal(struct{ Task, Operation, Source string }{task, operation, source.Epoch()})
	if err != nil {
		return nil, err
	}
	observation := &PreparedSerenaLaunchObservation{api: a, task: task, operationID: operation, root: root, intentPath: path, source: source, epoch: source.Epoch(), requestSHA: preparedSerenaHash(requestRaw), policy: DefaultSerenaPreparationPolicy()}
	namespace, acquireErr := AcquireSupervisorStateNamespaceObserved(ctx, SupervisorStateNamespaceRequest{SourceOperationID: operation, RequestSHA256: observation.requestSHA, SourceEpoch: observation.epoch, LeafRoles: []SupervisorStateLeafRole{SupervisorIntentLeaf}, CreateMissing: false})
	observation.namespace = namespace
	defer func() {
		if namespace != nil {
			var closeErr error
			observation.namespaceClose, closeErr = namespace.CloseObserved()
			retErr = errors.Join(retErr, closeErr, observation.namespaceClose.ValidateSource(namespace))
		}
		if ctx.Err() != nil {
			retErr = errors.Join(retErr, ctx.Err())
		}
		if retErr == nil {
			observation.received = true
			if err := observation.validateSource(ctx); err != nil {
				retErr = err
			}
		}
		if retErr == nil {
			launch = observation
		} else {
			launch = nil
		}
	}()
	if acquireErr != nil {
		return nil, acquireErr
	}
	if namespace == nil {
		return nil, errors.New("SERENA_PREPARED_LAUNCH_NAMESPACE_MISSING")
	}
	cap := stateFileReadCapBytes(path)
	raw, reader, err := ReadStateFileInodeAnchoredObserved(ctx, path, cap)
	observation.readers = append(observation.readers, reader)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, errors.New("SERENA_PREPARED_LAUNCH_READER_MISSING")
	}
	if err = reader.Validate(ctx, path, cap, raw, preparedSerenaHash(raw)); err != nil {
		return nil, err
	}
	intent, _, err := decodeSupervisorIntentFile(path, raw)
	if err != nil {
		return nil, err
	}
	if intent.Version != 2 || intent.IntentGeneration == 0 {
		return nil, errors.New("SERENA_PREPARED_LAUNCH_SCHEMA_UNSUPPORTED")
	}
	matches := 0
	for _, d := range intent.Daemons {
		if d.TaskName == task {
			matches++
			observation.descriptor = d
		}
	}
	if matches != 1 {
		return nil, errors.New("SERENA_PREPARED_LAUNCH_TASK_AMBIGUOUS")
	}
	descriptor := observation.descriptor
	if descriptor.Server != SerenaServerName || !preparedSerenaDigest(descriptor.ManifestHash) {
		return nil, errors.New("SERENA_PREPARED_LAUNCH_MANIFEST_BINDING_MISSING")
	}
	selected, err := preparedSerenaDescriptorBinding(descriptor)
	if err != nil || selected == nil {
		return nil, errors.Join(err, errors.New("SERENA_PREPARED_LAUNCH_BINDING_MISSING"))
	}
	observation.binding = *selected
	observation.intentSHA = preparedSerenaHash(raw)
	observation.generation = intent.IntentGeneration
	var policyOverride *SerenaPreparationPolicyV2
	if prepared != nil {
		observation.prepared = prepared
		observation.rawManifestSHA = prepared.manifestSHA
		observation.policy = prepared.policy
		if err = observation.validatePreparedSource(ctx); err != nil {
			return nil, err
		}
		observation.manifest, err = prepared.consumerLaunchManifest()
		if err != nil {
			return nil, err
		}
		policyOverride = &observation.policy
	} else {
		var manifestRaw []byte
		if manifestDirForTests() != "" {
			return nil, errors.New("SERENA_PREPARED_UNQUALIFIED_MANIFEST_OVERRIDE")
		}
		manifestRaw, err = fs.ReadFile(servers.Manifests, descriptor.Server+"/manifest.yaml")
		if err != nil {
			dir, resolveErr := CanonicalManifestDir()
			if resolveErr != nil {
				return nil, resolveErr
			}
			manifestPath := filepath.Join(dir, descriptor.Server, "manifest.yaml")
			manifestRaw, reader, err = ReadStateFileInodeAnchoredObserved(ctx, manifestPath, 64<<20)
			observation.readers = append(observation.readers, reader)
			if err != nil {
				return nil, err
			}
			if reader == nil {
				return nil, errors.New("SERENA_PREPARED_MANIFEST_READER_MISSING")
			}
			if err = reader.Validate(ctx, manifestPath, 64<<20, manifestRaw, preparedSerenaHash(manifestRaw)); err != nil {
				return nil, err
			}
		}
		observation.rawManifestSHA = preparedSerenaHash(manifestRaw)
		if observation.rawManifestSHA != descriptor.ManifestHash {
			return nil, errors.New("SERENA_PREPARED_MANIFEST_SOURCE_DRIFT")
		}
		observation.manifestRaw = append([]byte(nil), manifestRaw...)
		observation.manifest, err = config.ParseManifest(bytes.NewReader(manifestRaw))
		if err != nil {
			return nil, err
		}
	}
	observation.environment, err = assembly.ObserveConsumerEnvironment(ctx, observation.manifest, observation.rawManifestSHA, task, policyOverride)
	if err != nil {
		return nil, err
	}
	return observation, nil
}

func (o *PreparedSerenaLaunchObservation) validatePreparedSource(ctx context.Context) error {
	prepared := o.prepared
	if prepared == nil {
		return nil
	}
	if err := prepared.validateConsumerLaunchSource(ctx, o.api); err != nil {
		return err
	}
	if o.binding != prepared.binding || o.rawManifestSHA != prepared.manifestSHA || o.descriptor.ManifestHash != prepared.manifestSHA || o.epoch != prepared.record.SourceSHA256 || o.binding.SourceSHA256 != o.epoch || o.policy != prepared.policy {
		return errors.New("SERENA_PARSED_PREPARATION_LAUNCH_DRIFT")
	}
	return nil
}

func (o *PreparedSerenaLaunchObservation) validateSource(ctx context.Context) error {
	if o == nil || ctx == nil || ctx.Err() != nil || o.api == nil || !o.received || o.namespaceClose.ValidateSource(o.namespace) != nil {
		return errors.New("SERENA_PREPARED_LAUNCH_RECEIVING_UNAVAILABLE")
	}
	if err := o.source.Validate(o.epoch); err != nil {
		return err
	}
	if err := o.validatePreparedSource(ctx); err != nil {
		return err
	}
	for _, r := range o.readers {
		if r == nil || r.issuer == nil || !r.finalized || !r.view.Complete || !r.view.OwnedResourcesComplete || !r.view.BorrowedResourcesComplete {
			return errors.New("SERENA_PREPARED_LAUNCH_READ_CLOSE_UNQUALIFIED")
		}
	}
	projection, err := o.environment.Projection(ctx)
	if err != nil {
		return err
	}
	sha, err := projection.SHA256()
	if err != nil {
		return err
	}
	policySHA, err := o.policy.IdentitySHA256()
	if err != nil {
		return err
	}
	if o.prepared != nil && sha != o.prepared.request.BuildEnvironmentSHA256 {
		return errors.New("SERENA_PARSED_PREPARATION_ENVIRONMENT_DRIFT")
	}
	return o.environment.Validate(ctx, o.rawManifestSHA, o.task, policySHA, sha)
}
func (o *PreparedSerenaLaunchObservation) Descriptor(ctx context.Context) (SupervisorDaemon, error) {
	if err := o.validateSource(ctx); err != nil {
		return SupervisorDaemon{}, err
	}
	copy := cloneSupervisorIntentFile(&SupervisorIntentFile{Version: 2, Daemons: []SupervisorDaemon{o.descriptor}})
	return copy.Daemons[0], nil
}
func (o *PreparedSerenaLaunchObservation) Validate(ctx context.Context, descriptor SupervisorDaemon) error {
	if err := o.validateSource(ctx); err != nil {
		return err
	}
	if !reflect.DeepEqual(descriptor, o.descriptor) {
		return errors.New("SERENA_PREPARED_LAUNCH_DESCRIPTOR_DRIFT")
	}
	return nil
}

func (o *PreparedSerenaLaunchObservation) Manifest(ctx context.Context) (*config.ServerManifest, error) {
	if err := o.validateSource(ctx); err != nil {
		return nil, err
	}
	if o.prepared != nil {
		return o.prepared.consumerLaunchManifest()
	}
	return config.ParseManifest(bytes.NewReader(o.manifestRaw))
}

func (o *PreparedSerenaLaunchObservation) consumerEnvironment(ctx context.Context, descriptor SupervisorDaemon) (*SerenaConsumerEnvironmentObservation, error) {
	if e := o.Validate(ctx, descriptor); e != nil {
		return nil, e
	}
	return o.environment, nil
}
