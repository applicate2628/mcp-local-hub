package api

import (
	"context"
	"encoding/json"
	"errors"
	"mcp-local-hub/internal/secrets"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type SerenaBuildEnvironmentEntryV1 struct {
	Key             string `json:"key"`
	ValueKind       string `json:"value_kind"`
	SourceKind      string `json:"source_kind"`
	SourceRefSHA256 string `json:"source_ref_sha256"`
	ValueSHA256     string `json:"value_sha256"`
}
type SerenaBuildCredentialReferenceV1 struct {
	Key             string `json:"key"`
	Purpose         string `json:"purpose"`
	SourceRefSHA256 string `json:"source_ref_sha256"`
}
type SerenaBuildEnvironmentProjectionV1 struct {
	Schema                          string                             `json:"schema"`
	Version                         int                                `json:"version"`
	PolicySHA256                    string                             `json:"policy_sha256"`
	PackageManifestProjectionSHA256 string                             `json:"package_manifest_projection_sha256"`
	BootstrapSHA256                 string                             `json:"bootstrap_sha256"`
	Entries                         []SerenaBuildEnvironmentEntryV1    `json:"entries"`
	CredentialReferences            []SerenaBuildCredentialReferenceV1 `json:"credential_references"`
	OwnedPathRoles                  []string                           `json:"owned_path_roles"`
}
type SerenaPreparationEnvironment struct {
	values        []string
	networkPolicy *serenaPackageNetworkPolicy
}
type serenaPackageNetworkPolicy struct {
	source                   *serenaPreparationEnvironmentSource
	decisionDefined, offline bool
	decisionSource           string
	entry                    SerenaBuildEnvironmentEntryV1
}

func (p *serenaPackageNetworkPolicy) validateExecution(environment, argv []string) error {
	if p == nil || p.source == nil || p.source.issuer == nil || !p.decisionDefined || (p.decisionSource != "uv-default" && p.decisionSource != "declared") {
		return errors.New("SERENA_PACKAGE_NETWORK_POLICY_UNAVAILABLE")
	}
	count := 0
	for _, item := range environment {
		key, value, ok := strings.Cut(item, "=")
		if ok && strings.EqualFold(key, "UV_OFFLINE") {
			count++
			if p.decisionSource != "declared" || value != strconv.FormatBool(p.offline) {
				return errors.New("SERENA_PACKAGE_NETWORK_ENV_DRIFT")
			}
		}
	}
	if (p.decisionSource == "declared" && count != 1) || (p.decisionSource == "uv-default" && count != 0) {
		return errors.New("SERENA_PACKAGE_NETWORK_ENV_DRIFT")
	}
	flags := 0
	for _, arg := range argv {
		if arg == "--offline" {
			flags++
		}
		if arg == "--no-offline" || arg == "--online" {
			return errors.New("SERENA_PACKAGE_NETWORK_ARG_DRIFT")
		}
	}
	if (p.offline && flags != 1) || (!p.offline && flags != 0) {
		return errors.New("SERENA_PACKAGE_NETWORK_ARG_DRIFT")
	}
	return nil
}

// The same declaration/resolution/overlay owner issues this private source.
// A final string map or an arbitrary implementation of this interface cannot
// provide original reference provenance or qualified bootstrap identity.
type SerenaPreparationEnvironmentSource interface{ serenaPreparationEnvironmentSource() }
type serenaPreparationEnvironmentSource struct {
	issuer                                 *struct{ marker byte }
	values                                 map[string]string
	provenance                             map[string]secrets.ResolvedReferenceProvenance
	originKinds                            map[string]string
	packageManifestSHA256, bootstrapSHA256 string
	bootstrapValues                        map[string]string
	consumer                               *serenaConsumerSourceBinding
}

func (*serenaPreparationEnvironmentSource) serenaPreparationEnvironmentSource() {}
func (p SerenaBuildEnvironmentProjectionV1) SHA256() (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return preparedSerenaHash(raw), nil
}

func BuildSerenaBuildEnvironmentProjection(ctx context.Context, source SerenaPreparationEnvironmentSource, policy SerenaPreparationPolicyV1) (projection SerenaBuildEnvironmentProjectionV1, execution SerenaPreparationEnvironment, retErr error) {
	actual, ok := source.(*serenaPreparationEnvironmentSource)
	if !ok || actual == nil || actual.issuer == nil || ctx == nil || !preparedSerenaDigest(actual.packageManifestSHA256) || !preparedSerenaDigest(actual.bootstrapSHA256) {
		return projection, execution, errors.New("SERENA_BUILD_ENVIRONMENT_SOURCE_UNAVAILABLE")
	}
	if err := ctx.Err(); err != nil {
		return projection, execution, err
	}
	if _, defined := ctx.Deadline(); !defined {
		return projection, execution, errors.New("SERENA_BUILD_ENVIRONMENT_DEADLINE_REQUIRED")
	}
	policySHA, err := policy.IdentitySHA256()
	if err != nil {
		return projection, execution, err
	}
	projection = SerenaBuildEnvironmentProjectionV1{Schema: "serena-build-environment-v1", Version: 1, PolicySHA256: policySHA, PackageManifestProjectionSHA256: actual.packageManifestSHA256, BootstrapSHA256: actual.bootstrapSHA256, Entries: []SerenaBuildEnvironmentEntryV1{}, CredentialReferences: []SerenaBuildCredentialReferenceV1{}, OwnedPathRoles: []string{"attempt-temp", "build-cache", "tool-bin", "tool-environment"}}
	values := map[string]string{}
	network := &serenaPackageNetworkPolicy{source: actual, decisionSource: "uv-default"}
	for key, value := range actual.bootstrapValues {
		values[key] = value
	}
	keys := make([]string, 0, len(actual.values))
	seen := map[string]bool{}
	for key := range actual.values {
		canonical := strings.ToUpper(key)
		if canonical == "" || seen[canonical] {
			return projection, execution, errors.New("SERENA_BUILD_ENVIRONMENT_KEY_AMBIGUOUS")
		}
		seen[canonical] = true
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return strings.ToUpper(keys[i]) < strings.ToUpper(keys[j]) })
	indexNames := map[string]bool{}
	for _, key := range keys {
		if strings.ToUpper(key) == "UV_INDEX" {
			for _, entry := range strings.Fields(actual.values[key]) {
				name, endpoint, named := strings.Cut(entry, "=")
				if !named || name == "" {
					continue
				}
				if _, err := serenaBuildIndexURL(endpoint); err != nil {
					return projection, execution, err
				}
				indexNames[strings.ToUpper(strings.ReplaceAll(name, "-", "_"))] = true
			}
		}
	}
	for _, key := range keys {
		canonical := strings.ToUpper(key)
		value := actual.values[key]
		origin := actual.provenance[key]
		if origin.ValidateMapKey(key) != nil || origin.ValidateResolvedValue(value) != nil {
			return projection, execution, errors.New("SERENA_BUILD_ENVIRONMENT_PROVENANCE_LOST")
		}
		purpose := ""
		for name := range indexNames {
			if canonical == "UV_INDEX_"+name+"_USERNAME" {
				purpose = "named-index-username"
			}
			if canonical == "UV_INDEX_"+name+"_PASSWORD" {
				purpose = "named-index-password"
			}
		}
		if purpose != "" {
			if value == "" {
				return projection, execution, errors.New("SERENA_BUILD_CREDENTIAL_EMPTY")
			}
			refSHA, err := origin.ReferenceIdentitySHA256()
			if err != nil {
				return projection, execution, errors.Join(err, errors.New("SERENA_BUILD_LITERAL_CREDENTIAL_REFUSED"))
			}
			projection.CredentialReferences = append(projection.CredentialReferences, SerenaBuildCredentialReferenceV1{Key: canonical, Purpose: purpose, SourceRefSHA256: refSHA})
			values[canonical] = value
			continue
		}
		kind, normalized, err := normalizeSerenaBuildValue(canonical, value)
		if err != nil {
			return projection, execution, err
		}
		if kind == "runtime-only" {
			continue
		}
		if origin.Kind() == "vault-ref" || origin.Kind() == "composite-environment-ref" {
			return projection, execution, errors.New("SERENA_BUILD_VALUE_AFFECTING_SECRET_REFUSED")
		}
		sourceKind := actual.originKinds[key]
		if sourceKind == "" {
			sourceKind = origin.Kind()
		}
		refSHA := ""
		if origin.Kind() == "literal" {
			refSHA = preparedSerenaHash([]byte(sourceKind + ":" + canonical))
		} else {
			refSHA, err = origin.ReferenceIdentitySHA256()
			if err != nil {
				return projection, execution, err
			}
		}
		projection.Entries = append(projection.Entries, SerenaBuildEnvironmentEntryV1{Key: canonical, ValueKind: kind, SourceKind: sourceKind, SourceRefSHA256: refSHA, ValueSHA256: preparedSerenaHash([]byte(normalized))})
		if canonical == "UV_OFFLINE" {
			network.offline = normalized == "true"
			network.decisionSource = "declared"
			network.entry = projection.Entries[len(projection.Entries)-1]
		}
		values[canonical] = normalized
	}
	values["PYTHONDONTWRITEBYTECODE"] = "1"
	values["UV_PYTHON_DOWNLOADS"] = "never"
	if network.decisionSource == "uv-default" {
		network.entry = SerenaBuildEnvironmentEntryV1{Key: "UV_OFFLINE", ValueKind: "boolean", SourceKind: "uv-default", SourceRefSHA256: preparedSerenaHash([]byte("uv/0.9.21:package-network-default:offline=false")), ValueSHA256: preparedSerenaHash([]byte("false"))}
		projection.Entries = append(projection.Entries, network.entry)
	}
	sort.Slice(projection.Entries, func(i, j int) bool { return projection.Entries[i].Key < projection.Entries[j].Key })
	network.decisionDefined = true
	execution.networkPolicy = network
	execution.values = preparedSerenaEnvironmentWith(nil, values)
	return projection, execution, ctx.Err()
}

func serenaBuildIndexURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("SERENA_BUILD_INDEX_URI_REFUSED")
	}
	return u.String(), nil
}
func normalizeSerenaBuildValue(key, value string) (string, string, error) {
	if strings.IndexByte(value, 0) >= 0 {
		return "", "", errors.New("SERENA_BUILD_VALUE_INVALID")
	}
	switch key {
	case "UV_OFFLINE", "UV_NO_BUILD_ISOLATION":
		if value != "true" && value != "false" {
			return "", "", errors.New("SERENA_BUILD_BOOLEAN_INVALID")
		}
		return "boolean", value, nil
	case "UV_RESOLUTION":
		return serenaBuildEnum(value, "highest", "lowest", "lowest-direct")
	case "UV_PRERELEASE":
		return serenaBuildEnum(value, "disallow", "allow", "if-necessary", "explicit", "if-necessary-or-explicit")
	case "UV_INDEX_STRATEGY":
		return serenaBuildEnum(value, "first-index", "unsafe-first-match", "unsafe-best-match")
	case "UV_LINK_MODE":
		return serenaBuildEnum(value, "clone", "copy", "hardlink", "symlink")
	case "UV_DEFAULT_INDEX", "UV_INDEX_URL":
		endpoint, err := serenaBuildIndexURL(value)
		return "index-endpoint", endpoint, err
	case "UV_INDEX", "UV_EXTRA_INDEX_URL":
		entries := strings.Fields(value)
		if len(entries) == 0 {
			return "", "", errors.New("SERENA_BUILD_INDEX_EMPTY")
		}
		for i, entry := range entries {
			name, endpoint, named := strings.Cut(entry, "=")
			if key == "UV_EXTRA_INDEX_URL" || !named {
				endpoint = entry
				name = ""
			}
			normalized, err := serenaBuildIndexURL(endpoint)
			if err != nil {
				return "", "", err
			}
			if name != "" {
				for _, r := range name {
					if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-", r) {
						return "", "", errors.New("SERENA_BUILD_INDEX_NAME_INVALID")
					}
				}
				normalized = name + "=" + normalized
			}
			entries[i] = normalized
		}
		return "ordered-index-list", strings.Join(entries, " "), nil
	case "UV_FIND_LINKS", "UV_CONSTRAINT", "UV_BUILD_CONSTRAINT", "UV_OVERRIDE":
		return "", "", errors.New("SERENA_BUILD_ARTIFACT_SOURCE_UNQUALIFIED")
	case "PYTHONDONTWRITEBYTECODE":
		if value != "1" {
			return "", "", errors.New("SERENA_BUILD_BYTECODE_POLICY_CONFLICT")
		}
		return "policy", value, nil
	case "PYTHONHOME", "PYTHONPATH", "VIRTUAL_ENV":
		return "", "", errors.New("SERENA_BUILD_IMPORT_OVERRIDE_REFUSED")
	}
	if strings.HasPrefix(key, "UV_") || strings.HasPrefix(key, "PIP_") || strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "PYTHON") {
		return "", "", errors.New("SERENA_BUILD_INPUT_UNSUPPORTED")
	}
	return "runtime-only", "", nil
}

// This classifier grants no execution. It keeps pre-resolution selection in
// the SAME build-input owner; unsupported declared build keys remain selected
// so their later typed validation refuses rather than silently dropping them.
func IsSerenaPreparationBuildDeclaration(key string) bool {
	kind, _, _ := normalizeSerenaBuildValue(strings.ToUpper(key), "")
	return kind != "runtime-only"
}
func serenaBuildEnum(value string, allowed ...string) (string, string, error) {
	for _, candidate := range allowed {
		if value == candidate {
			return "enum", value, nil
		}
	}
	return "", "", errors.New("SERENA_BUILD_ENUM_INVALID")
}
