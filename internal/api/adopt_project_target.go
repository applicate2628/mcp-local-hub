package api

import (
	"errors"
	"fmt"
	"path/filepath"

	"mcp-local-hub/internal/clients"
)

const (
	adoptClientConfigTargetKindCodexProject     = "codex_project"
	adoptClientConfigTargetKindCodexSharedStdio = "codex_shared_stdio"
)

var errProjectAdoptConfigTargetInvalid = errors.New("PROJECT_ADOPT_CONFIG_TARGET_INVALID")

type AdoptClientConfigTargetV1 struct {
	Kind                  string                         `json:"kind"`
	ProjectRoot           string                         `json:"project_root"`
	ProjectIdentitySHA256 string                         `json:"project_identity_sha256"`
	SharedStdio           *AdoptCodexSharedStdioTargetV1 `json:"shared_stdio,omitempty"`
}

// AdoptCodexSharedStdioTargetV1 describes constructor-bound destinations. It is
// evidence DATA; neither paths nor a validated descriptor authorize publication.
type AdoptCodexSharedStdioTargetV1 struct {
	GlobalLogicalPath  string                            `json:"global_logical_path"`
	GlobalReferentPath string                            `json:"global_referent_path"`
	Consumers          []AdoptCodexSharedStdioConsumerV1 `json:"consumers"`
	RelayURL           string                            `json:"relay_url"`
}

type AdoptCodexSharedStdioConsumerV1 struct {
	ProjectRoot           string `json:"project_root"`
	ProjectIdentitySHA256 string `json:"project_identity_sha256"`
}

func cloneAdoptCodexSharedStdioTarget(target *AdoptCodexSharedStdioTargetV1) *AdoptCodexSharedStdioTargetV1 {
	if target == nil {
		return nil
	}
	cloned := *target
	cloned.Consumers = append([]AdoptCodexSharedStdioConsumerV1(nil), target.Consumers...)
	return &cloned
}

// Validation runs before either factory; it preserves the frozen consumer order
// rather than silently sorting or deduplicating a different descriptor.
func validateAdoptCodexSharedStdioTarget(target *AdoptCodexSharedStdioTargetV1) error {
	if target == nil || target.GlobalLogicalPath == "" || target.GlobalReferentPath == "" || target.RelayURL == "" || len(target.Consumers) == 0 {
		return invalidProjectAdoptTarget("shared stdio target requires global paths, relay URL and consumers")
	}
	for _, path := range []string{target.GlobalLogicalPath, target.GlobalReferentPath} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return invalidProjectAdoptTarget("shared stdio global paths must be clean and absolute")
		}
	}
	previous := ""
	for _, consumer := range target.Consumers {
		if _, ok := decodeLowerProjectAdoptSHA256(consumer.ProjectIdentitySHA256); !ok || consumer.ProjectIdentitySHA256 <= previous {
			return invalidProjectAdoptTarget("shared stdio consumers require unique identities in canonical identity order")
		}
		actual, err := clients.CodexProjectIdentitySHA256ForRoot(consumer.ProjectRoot)
		if err != nil || actual != consumer.ProjectIdentitySHA256 {
			return invalidProjectAdoptTarget("shared stdio consumer root and identity differ: %v", err)
		}
		previous = consumer.ProjectIdentitySHA256
	}
	return nil
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
	sharedStdio     *AdoptCodexSharedStdioTargetV1
	projects        []clients.Client
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

	if hasConfigTarget && ref.configTarget.Kind == adoptClientConfigTargetKindCodexSharedStdio {
		if ref.configTarget.ProjectRoot != "" || ref.configTarget.ProjectIdentitySHA256 != "" {
			return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("shared stdio target cannot contain project variant fields")
		}
		target := cloneAdoptCodexSharedStdioTarget(ref.configTarget.SharedStdio)
		if err := validateAdoptCodexSharedStdioTarget(target); err != nil {
			return resolvedAdoptClientTarget{}, err
		}
		if r.globalCatalogFactory == nil || r.projectClientFactory == nil {
			return resolvedAdoptClientTarget{}, fmt.Errorf("shared stdio client factories are unavailable")
		}
		global, ok := r.globalCatalogFactory()[ref.clientName]
		if !ok || global == nil {
			return resolvedAdoptClientTarget{}, fmt.Errorf("client %q is unavailable", ref.clientName)
		}
		if global.ConfigPath() != target.GlobalLogicalPath {
			return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("shared stdio logical path differs from bound global client")
		}
		projects := make([]clients.Client, 0, len(target.Consumers))
		for _, consumer := range target.Consumers {
			project, err := r.projectClientFactory(consumer.ProjectRoot, ref.targetEntryName)
			if err != nil {
				return resolvedAdoptClientTarget{}, err
			}
			if project == nil {
				return resolvedAdoptClientTarget{}, fmt.Errorf("project client factory returned no client")
			}
			projects = append(projects, project)
		}
		return resolvedAdoptClientTarget{client: global, targetEntryName: ref.targetEntryName, sharedStdio: target, projects: projects}, nil
	}
	if hasConfigTarget && ref.configTarget.SharedStdio != nil {
		return resolvedAdoptClientTarget{}, invalidProjectAdoptTarget("shared stdio operand requires its explicit kind")
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
