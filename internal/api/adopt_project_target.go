package api

import (
	"errors"
	"fmt"

	"mcp-local-hub/internal/clients"
)

const adoptClientConfigTargetKindCodexProject = "codex_project"

var errProjectAdoptConfigTargetInvalid = errors.New("PROJECT_ADOPT_CONFIG_TARGET_INVALID")

type AdoptClientConfigTargetV1 struct {
	Kind                  string `json:"kind"`
	ProjectRoot           string `json:"project_root"`
	ProjectIdentitySHA256 string `json:"project_identity_sha256"`
}

type adoptClientTargetRef struct {
	clientName      string
	targetEntryName string
	configTarget    *AdoptClientConfigTargetV1
	projectSource   *projectAdoptSource
}

type resolvedAdoptClientTarget struct {
	client          clients.Client
	targetEntryName string
	projectSource   *projectAdoptSource
}

type adoptGlobalClientCatalogFactory func() map[string]clients.Client
type adoptProjectClientFactory func(projectRoot, logicalEntryName string) (clients.Client, error)

type adoptClientTargetResolver struct {
	globalCatalogFactory adoptGlobalClientCatalogFactory
	projectClientFactory adoptProjectClientFactory
}

func newAdoptClientTargetResolver() adoptClientTargetResolver {
	return adoptClientTargetResolver{
		globalCatalogFactory: clients.AllClients,
		projectClientFactory: func(projectRoot, logicalEntryName string) (clients.Client, error) {
			client, _, err := clients.NewCodexCLIForProject(projectRoot, logicalEntryName, NewCodexProjectConfigMutationOwner)
			return client, err
		},
	}
}

func (r adoptClientTargetResolver) Resolve(ref adoptClientTargetRef) (resolvedAdoptClientTarget, error) {
	if ref.clientName == "" || ref.targetEntryName == "" {
		return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("client and logical target entry are required")
	}
	hasConfigTarget := ref.configTarget != nil
	hasProjectSource := ref.projectSource != nil
	if hasConfigTarget && hasProjectSource {
		return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("persisted target and plan snapshot are mutually exclusive")
	}
	if !hasConfigTarget && !hasProjectSource {
		if r.globalCatalogFactory == nil {
			return resolvedAdoptClientTarget{}, fmt.Errorf("global client catalog factory is unavailable")
		}
		client, ok := r.globalCatalogFactory()[ref.clientName]
		if !ok || client == nil {
			return resolvedAdoptClientTarget{}, fmt.Errorf("client %q is unavailable", ref.clientName)
		}
		return resolvedAdoptClientTarget{client: client, targetEntryName: ref.targetEntryName}, nil
	}
	if ref.clientName != "codex-cli" {
		return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("project target requires client codex-cli")
	}

	projectRoot := ""
	projectIdentitySHA256 := ""
	if hasConfigTarget {
		if ref.configTarget.Kind != adoptClientConfigTargetKindCodexProject {
			return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("target kind must be codex_project")
		}
		projectRoot = ref.configTarget.ProjectRoot
		projectIdentitySHA256 = ref.configTarget.ProjectIdentitySHA256
		if _, ok := decodeLowerProjectAdoptSHA256(projectIdentitySHA256); !ok {
			return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("project identity fingerprint must be lowercase 64-hex")
		}
	} else {
		if ref.projectSource.logicalEntryName != ref.targetEntryName {
			return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("plan snapshot logical entry differs from target entry")
		}
		projectRoot = ref.projectSource.identity.CanonicalRoot()
		projectIdentitySHA256 = ref.projectSource.identity.ProjectIdentitySHA256()
	}

	actualIdentitySHA256, err := clients.CodexProjectIdentitySHA256ForRoot(projectRoot)
	if err != nil {
		return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("project root is invalid: %v", err)
	}
	if actualIdentitySHA256 != projectIdentitySHA256 {
		return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("project identity fingerprint does not match root")
	}
	if r.projectClientFactory == nil {
		return resolvedAdoptClientTarget{}, fmt.Errorf("project client factory is unavailable")
	}
	client, err := r.projectClientFactory(projectRoot, ref.targetEntryName)
	if err != nil {
		return resolvedAdoptClientTarget{}, err
	}
	if client == nil {
		return resolvedAdoptClientTarget{}, fmt.Errorf("project client factory returned no client")
	}
	return resolvedAdoptClientTarget{
		client:          client,
		targetEntryName: ref.targetEntryName,
		projectSource:   ref.projectSource,
	}, nil
}

func (r adoptClientTargetResolver) resolvePlanClient(plan *AdoptPlan, clientName string) (resolvedAdoptClientTarget, error) {
	if plan == nil {
		return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("plan is required")
	}
	ref, ok := plan.clientTargets[clientName]
	if !ok {
		ref = adoptClientTargetRef{clientName: clientName, targetEntryName: plan.targetEntryName(clientName)}
	}
	if ref.clientName != clientName {
		return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("plan target identity changed")
	}
	if ref.targetEntryName != plan.targetEntryName(clientName) {
		if ref.configTarget != nil || ref.projectSource != nil {
			return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("plan target identity changed")
		}
		ref.targetEntryName = plan.targetEntryName(clientName)
	}
	return r.Resolve(ref)
}

func (r adoptClientTargetResolver) resolveRecordClient(rec AdoptProvenanceRecord, client AdoptClientProvenance) (resolvedAdoptClientTarget, error) {
	return r.Resolve(adoptClientTargetRef{
		clientName:      client.Client,
		targetEntryName: adoptClientTargetEntryName(rec, client),
		configTarget:    client.ConfigTarget,
	})
}

func invalidProjectAdoptTarget(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errProjectAdoptConfigTargetInvalid, fmt.Sprintf(format, args...))
}
