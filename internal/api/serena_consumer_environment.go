package api

import (
	"context"
	"encoding/json"
	"errors"
	"mcp-local-hub/internal/buildinfo"
)

// Values are data; only the original observed readers and sealed source factory
// qualify capture. No Boolean here can assert resource closure.
type SerenaConsumerEnvironmentSourceBinding struct {
	TaskName, RawManifestSHA256 string
	Reads                       []*StateFileReadObservation
}
type serenaConsumerSourceBinding struct {
	task, rawManifestSHA, epoch string
	source                      *buildinfo.PublisherSourceObservation
	reads                       []*StateFileReadObservation
}
type SerenaConsumerEnvironmentObservation struct {
	source                   *serenaPreparationEnvironmentSource
	binding                  *serenaConsumerSourceBinding
	policySHA, projectionSHA string
	projection               SerenaBuildEnvironmentProjectionV1
	policy                   SerenaPreparationPolicyV2
}

func validateConsumerSourceReads(reads []*StateFileReadObservation) error {
	if len(reads) == 0 {
		return errors.New("SERENA_CONSUMER_SOURCE_READS_UNAVAILABLE")
	}
	for _, r := range reads {
		if r == nil || r.issuer == nil || !r.finalized || !r.view.OwnedResourcesComplete || !r.view.BorrowedResourcesComplete || (!r.view.Complete && !r.absenceQualified) {
			return errors.New("SERENA_CONSUMER_SOURCE_CLOSE_UNQUALIFIED")
		}
	}
	return nil
}

func NewSerenaConsumerEnvironmentObservation(ctx context.Context, source SerenaPreparationEnvironmentSource, rawSHA, task string, policy SerenaPreparationPolicyV2) (*SerenaConsumerEnvironmentObservation, error) {
	s, ok := source.(*serenaPreparationEnvironmentSource)
	if !ok || s == nil || s.issuer == nil || s.consumer == nil || ctx == nil || ctx.Err() != nil || s.consumer.task != task || s.consumer.rawManifestSHA != rawSHA {
		return nil, errors.New("SERENA_CONSUMER_ENVIRONMENT_SOURCE_UNAVAILABLE")
	}
	if err := validateConsumerSourceReads(s.consumer.reads); err != nil {
		return nil, err
	}
	if err := s.consumer.source.Validate(s.consumer.epoch); err != nil {
		return nil, err
	}
	projection, _, err := BuildSerenaBuildEnvironmentProjection(ctx, s, policy)
	if err != nil {
		return nil, err
	}
	policySHA, err := policy.IdentitySHA256()
	if err != nil {
		return nil, err
	}
	sha, err := projection.SHA256()
	if err != nil {
		return nil, err
	}
	return &SerenaConsumerEnvironmentObservation{source: s, binding: s.consumer, policySHA: policySHA, projectionSHA: sha, projection: projection, policy: policy}, ctx.Err()
}
func (o *SerenaConsumerEnvironmentObservation) Validate(ctx context.Context, rawSHA, task, policySHA, expectedSHA string) error {
	if o == nil || ctx == nil || ctx.Err() != nil || o.source == nil || o.source.consumer != o.binding || o.binding == nil || o.binding.rawManifestSHA != rawSHA || o.binding.task != task || o.policySHA != policySHA || o.projectionSHA != expectedSHA {
		return errors.New("SERENA_CONSUMER_ENVIRONMENT_BINDING_DRIFT")
	}
	if err := o.binding.source.Validate(o.binding.epoch); err != nil {
		return err
	}
	return validateConsumerSourceReads(o.binding.reads)
}
func (o *SerenaConsumerEnvironmentObservation) Projection(ctx context.Context) (SerenaBuildEnvironmentProjectionV1, error) {
	if o == nil || o.binding == nil {
		return SerenaBuildEnvironmentProjectionV1{}, errors.New("SERENA_CONSUMER_ENVIRONMENT_MISSING")
	}
	if err := o.Validate(ctx, o.binding.rawManifestSHA, o.binding.task, o.policySHA, o.projectionSHA); err != nil {
		return SerenaBuildEnvironmentProjectionV1{}, err
	}
	raw, err := json.Marshal(o.projection)
	if err != nil {
		return SerenaBuildEnvironmentProjectionV1{}, err
	}
	var copy SerenaBuildEnvironmentProjectionV1
	err = json.Unmarshal(raw, &copy)
	return copy, err
}

// The private context is captured only after actual qualified input probes.
// It is carried by the original request, never reconstructed from JSON data.
type serenaPreparationContext struct {
	policy            SerenaPreparationPolicyV2
	source            SerenaPreparationEnvironmentSource
	request           PreparedSerenaRequest
	uvProbe, abiProbe *preparedSerenaCommandResult
}

func (c *serenaPreparationContext) qualification(r PreparedSerenaRequest) (PreparedSerenaInterpreterQualificationV1, error) {
	var q PreparedSerenaInterpreterQualificationV1
	if c == nil || c.source == nil || c.uvProbe == nil || c.abiProbe == nil {
		return q, errors.New("SERENA_PREPARATION_ORIGINAL_INPUTS_MISSING")
	}
	actual := r
	actual.preparationContext = nil
	if actual != c.request {
		return q, errors.New("SERENA_PREPARATION_ORIGINAL_INPUTS_DRIFT")
	}
	sha, e := c.policy.IdentitySHA256()
	if e != nil || sha != r.PolicySHA256 || c.policy.WorkLimit != r.WorkLimit || c.policy.CleanupLimit != r.CleanupLimit || c.policy.OwnedTreeLimits != r.OwnedTreeLimits {
		return q, errors.Join(e, errors.New("SERENA_PREPARATION_POLICY_REQUEST_DRIFT"))
	}
	if e = validateSerenaUVProbe(*c.uvProbe, r.OperationID, c.policy, nil); e != nil {
		return q, e
	}
	abi := c.abiProbe
	if !abi.Closure.complete(r.OperationID) || !abi.Closure.RootExitKnown || abi.Closure.RootExitCode != 0 || preparedSerenaHash(abi.Stdout) != r.PlatformABISHA256 {
		return q, errors.New("SERENA_PREPARATION_ORIGINAL_ABI_UNQUALIFIED")
	}
	decoded, e := decodeSerenaInterpreterABI(abi.Stdout)
	if e != nil {
		return q, e
	}
	if e = c.policy.validatePythonVersion(decoded.VersionInfo); e != nil {
		return q, e
	}
	q = PreparedSerenaInterpreterQualificationV1{ProbeStdout: string(abi.Stdout), ClosureReceiptSHA256: abi.Closure.ReceiptSHA256, InterpreterSHA256: r.Interpreter.SHA256}
	return q, q.validate()
}

// One immutable selected context is used for warm, rebind, prior-Ready and cold
// receiving. Original identity and current admissibility are distinct inputs.
type serenaSelectedContext struct {
	policySHA, manifestSHA, environmentSHA, omissionRule, operationSHA string
	limits                                                             SerenaTreeLimits
	current                                                            SerenaPreparationPolicyV2
}

func selectedSerenaPolicy(record PreparedSerenaRuntimeRecord, op PreparedSerenaOperationRecordV2, current SerenaPreparationPolicyV2) (serenaSelectedContext, error) {
	var c serenaSelectedContext
	if e := current.Validate(); e != nil {
		return c, e
	}
	if e := op.Validate(); e != nil {
		return c, e
	}
	if record.OperationID != op.OperationID || record.GenerationID != op.GenerationID || record.RequestIdentitySHA256 != op.RequestIdentitySHA256 || record.NamespaceSHA256 != op.NamespaceSHA256 || record.NamespaceSchema != op.NamespaceSchema || record.SourceSHA256 != op.SourceEpoch {
		return c, errors.New("SERENA_SELECTED_OPERATION_BINDING_DRIFT")
	}
	c = serenaSelectedContext{policySHA: record.PolicySHA256, manifestSHA: op.PackageManifestProjectionSHA256, environmentSHA: op.BuildEnvironmentSHA256, current: current}
	if op.Schema == PreparedSerenaPolicyOperationSchema {
		p := *op.PreparationPolicy
		sha, e := p.canonicalIdentitySHA256()
		if e != nil || sha != record.PolicySHA256 {
			return c, errors.Join(e, errors.New("SERENA_SELECTED_POLICY_HASH_DRIFT"))
		}
		q := op.InterpreterQualification
		if q.InterpreterSHA256 != record.InterpreterSHA256 || preparedSerenaHash([]byte(q.ProbeStdout)) != record.PlatformABISHA256 {
			return c, errors.New("SERENA_SELECTED_INTERPRETER_EVIDENCE_DRIFT")
		}
		abi, e := decodeSerenaInterpreterABI([]byte(q.ProbeStdout))
		if e != nil {
			return c, e
		}
		if e = p.validatePythonVersion(abi.VersionInfo); e != nil {
			return c, e
		}
		if e = current.validatePythonVersion(abi.VersionInfo); e != nil {
			return c, e
		}
		c.limits = p.OwnedTreeLimits
		c.omissionRule = serenaUVNetworkOmissionRule
	} else {
		// This exact frozen factory identifies historical semantics only. Its
		// original qualified exact Python guarantee proves the current lower bound;
		// the opaque ABI digest by itself supplies no version information.
		p := historicalSerenaPreparationPolicy()
		sha, e := p.IdentitySHA256()
		if e != nil || sha != record.PolicySHA256 {
			return c, errors.Join(e, errors.New("SERENA_ORIGINAL_POLICY_CONTEXT_UNAVAILABLE"))
		}
		version, e := serenaNumericVersion(p.InterpreterSelector, 2)
		if e != nil {
			return c, e
		}
		if e = current.validatePythonVersion(version[:]); e != nil {
			return c, e
		}
		c.limits = p.OwnedTreeLimits
		c.omissionRule = "uv/0.9.21:package-network-default:offline=false"
	}
	raw, e := json.Marshal(op)
	if e != nil {
		return c, e
	}
	c.operationSHA = preparedSerenaHash(raw)
	return c, nil
}
func receiveSerenaSelectedContext(ctx context.Context, record PreparedSerenaRuntimeRecord, op PreparedSerenaOperationRecordV2, environment *SerenaConsumerEnvironmentObservation, preparation *serenaPreparationContext, received *serenaSelectedContext) (serenaSelectedContext, error) {
	var zero serenaSelectedContext
	var source SerenaPreparationEnvironmentSource
	var current SerenaPreparationPolicyV2
	if environment != nil {
		if preparation != nil {
			return zero, errors.New("SERENA_SELECTED_CONTEXT_AMBIGUOUS")
		}
		if e := environment.Validate(ctx, environment.bindingRawSHA(), environment.bindingTask(), environment.policySHA, environment.projectionSHA); e != nil {
			return zero, e
		}
		current = environment.policy
		source = environment.source
	} else if preparation != nil {
		if _, e := preparation.qualification(preparation.requestWithContext()); e != nil {
			return zero, e
		}
		current = preparation.policy
		source = preparation.source
	} else {
		return zero, errors.New("SERENA_SELECTED_CONTEXT_MISSING")
	}
	var c serenaSelectedContext
	if received == nil {
		var err error
		c, err = selectedSerenaPolicy(record, op, current)
		if err != nil {
			return zero, err
		}
	} else {
		// Excluded rereads compare the original received context. They never
		// resolve another policy, infer historical values or renew a default.
		c = *received
		raw, err := json.Marshal(op)
		if err != nil || c.operationSHA != preparedSerenaHash(raw) || c.policySHA != record.PolicySHA256 || c.current != current {
			return zero, errors.Join(err, errors.New("SERENA_SELECTED_CONTEXT_DRIFT"))
		}
	}
	projection, _, e := buildSerenaBuildEnvironmentProjection(ctx, source, c.policySHA, c.omissionRule)
	if e != nil {
		return zero, e
	}
	sha, e := projection.SHA256()
	if e != nil || projection.PackageManifestProjectionSHA256 != c.manifestSHA || sha != c.environmentSHA {
		return zero, errors.Join(e, errors.New("SERENA_SELECTED_BUILD_PROJECTION_DRIFT"))
	}
	return c, nil
}
func (c *serenaPreparationContext) requestWithContext() PreparedSerenaRequest {
	r := c.request
	r.preparationContext = c
	return r
}
func (o *SerenaConsumerEnvironmentObservation) bindingRawSHA() string {
	if o == nil || o.binding == nil {
		return ""
	}
	return o.binding.rawManifestSHA
}
func (o *SerenaConsumerEnvironmentObservation) bindingTask() string {
	if o == nil || o.binding == nil {
		return ""
	}
	return o.binding.task
}
