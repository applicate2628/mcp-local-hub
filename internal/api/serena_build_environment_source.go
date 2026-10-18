package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"
	"net/url"
	"path/filepath"
	"strings"
)

// This entry is the preparation branch of the existing declaration resolver.
// The caller supplies raw winning declarations BEFORE resolution, never a
// reconstructed already-applied overlay or final decrypted environment map.
// Root assembly supplies its SAME resolver; this method does not open a vault.
func NewSerenaPreparationEnvironmentSource(ctx context.Context, manifest *config.ServerManifest, resolver *secrets.Resolver, winningDeclarations map[string]string, originKinds map[string]string, bootstrapDeclarations map[string]string, consumer ...SerenaConsumerEnvironmentSourceBinding) (SerenaPreparationEnvironmentSource, error) {
	if ctx == nil || manifest == nil || manifest.Name != SerenaServerName || resolver == nil {
		return nil, errors.New("SERENA_BUILD_DECLARATION_SOURCE_UNAVAILABLE")
	}
	if _, defined := ctx.Deadline(); !defined {
		return nil, errors.New("SERENA_BUILD_SOURCE_DEADLINE_REQUIRED")
	}
	if len(manifest.BaseArgs) < 3 || manifest.BaseArgs[0] != "--from" || manifest.BaseArgs[2] != "serena" {
		return nil, errors.New("SERENA_BUILD_MANIFEST_SOURCE_INVALID")
	}
	// Runtime-only declarations do not participate in package preparation.
	// Use the SAME value owner to classify them before resolving optional
	// runtime secrets; never turn an unrelated missing runtime ref into a build
	// failure or retain its decrypted value in this source.
	buildDeclarations := map[string]string{}
	for key, value := range winningDeclarations {
		if IsSerenaPreparationBuildDeclaration(key) {
			buildDeclarations[key] = value
		}
	}
	values, provenance, err := resolver.ResolveMapWithProvenance(buildDeclarations)
	if err != nil {
		return nil, err
	}
	kinds := map[string]string{}
	for key, kind := range originKinds {
		if _, present := winningDeclarations[key]; !present {
			return nil, errors.New("SERENA_BUILD_SOURCE_KIND_FOREIGN")
		}
		if _, build := buildDeclarations[key]; !build {
			continue
		}
		if kind != "literal" && kind != "vault-ref" && kind != "file-ref" && kind != "environment-ref" && kind != "overlay-literal" && kind != "forwarded" {
			return nil, errors.New("SERENA_BUILD_SOURCE_KIND_INVALID")
		}
		actual := provenance[key].Kind()
		if kind == "overlay-literal" {
			if actual != "literal" {
				return nil, errors.New("SERENA_BUILD_SOURCE_KIND_MISMATCH")
			}
		} else if kind == "forwarded" {
			if actual != "environment-ref" {
				return nil, errors.New("SERENA_BUILD_SOURCE_KIND_MISMATCH")
			}
		} else if kind != actual {
			return nil, errors.New("SERENA_BUILD_SOURCE_KIND_MISMATCH")
		}
		kinds[key] = kind
	}
	// No runtime Env, workspace, context, ports, or raw manifest bytes enter
	// this projection. Build entries and credential references are separate.
	source := manifest.BaseArgs[1]
	at := strings.LastIndex(source, "@")
	if at < 0 {
		return nil, errors.New("SERENA_BUILD_SOURCE_INVALID")
	}
	commit := source[at+1:]
	if len(commit) != 40 || strings.ToLower(commit) != commit {
		return nil, errors.New("SERENA_BUILD_SOURCE_INVALID")
	}
	if _, err = hex.DecodeString(commit); err != nil {
		return nil, err
	}
	repository, err := url.Parse(strings.TrimPrefix(source[:at], "git+"))
	if err != nil || repository.Scheme != "https" || repository.Host == "" || repository.Path == "" || repository.User != nil || repository.RawQuery != "" || repository.Fragment != "" {
		return nil, errors.New("SERENA_BUILD_SOURCE_INVALID")
	}
	manifestRaw, err := json.Marshal(struct {
		Repository   string `json:"repository"`
		Commit       string `json:"commit"`
		Distribution string `json:"distribution"`
		Entrypoint   string `json:"entrypoint"`
	}{repository.String(), commit, "serena-agent", "serena"})
	if err != nil {
		return nil, err
	}
	bootstrap, bootstrapValues, err := acquireSerenaBuildBootstrap(ctx, bootstrapDeclarations)
	if err != nil {
		return nil, err
	}
	result := &serenaPreparationEnvironmentSource{issuer: &struct{ marker byte }{1}, values: values, provenance: provenance, originKinds: kinds, packageManifestSHA256: preparedSerenaHash(manifestRaw), bootstrapSHA256: bootstrap, bootstrapValues: bootstrapValues}
	if len(consumer) > 1 {
		return nil, errors.New("SERENA_CONSUMER_SOURCE_BINDING_AMBIGUOUS")
	}
	if len(consumer) == 1 {
		binding := consumer[0]
		if binding.TaskName == "" || !preparedSerenaDigest(binding.RawManifestSHA256) {
			return nil, errors.New("SERENA_CONSUMER_SOURCE_BINDING_MISSING")
		}
		if err := validateConsumerSourceReads(binding.Reads); err != nil {
			return nil, err
		}
		marked, err := buildinfo.CurrentPublisherSource()
		if err != nil {
			return nil, err
		}
		if err = marked.Validate(marked.Epoch()); err != nil {
			return nil, err
		}
		result.consumer = &serenaConsumerSourceBinding{task: binding.TaskName, rawManifestSHA: binding.RawManifestSHA256, epoch: marked.Epoch(), source: marked, reads: append([]*StateFileReadObservation(nil), binding.Reads...)}
	}
	return result, ctx.Err()
}

func SerenaBuildBootstrapDeclarationKeys() []string {
	return []string{"SYSTEMROOT", "WINDIR", "COMSPEC", "PATH"}
}

func normalizeSerenaBuildBootstrapDeclarations(declarations map[string]string) (map[string]string, error) {
	values := make(map[string]string, len(declarations))
	allowed := map[string]bool{}
	for _, key := range SerenaBuildBootstrapDeclarationKeys() {
		allowed[key] = true
	}
	for key, value := range declarations {
		ascii := []byte(key)
		for i, b := range ascii {
			if b > 127 {
				return nil, errors.New("SERENA_BUILD_BOOTSTRAP_INPUT_UNKNOWN")
			}
			if b >= 'a' && b <= 'z' {
				ascii[i] = b - ('a' - 'A')
			}
		}
		canonical := string(ascii)
		if !allowed[canonical] {
			return nil, errors.New("SERENA_BUILD_BOOTSTRAP_INPUT_UNKNOWN")
		}
		if _, exists := values[canonical]; exists {
			return nil, errors.New("SERENA_BUILD_BOOTSTRAP_CASE_ALIAS_COLLISION")
		}
		values[canonical] = value
	}
	for _, key := range SerenaBuildBootstrapDeclarationKeys() {
		if value, exists := values[key]; !exists || value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("SERENA_BUILD_BOOTSTRAP_INPUT_MISSING")
		}
	}
	return values, nil
}

func acquireSerenaBuildBootstrap(ctx context.Context, declarations map[string]string) (string, map[string]string, error) {
	if ctx == nil {
		return "", nil, errors.New("SERENA_BUILD_BOOTSTRAP_CONTEXT_REQUIRED")
	}
	values, err := normalizeSerenaBuildBootstrapDeclarations(declarations)
	if err != nil {
		return "", nil, err
	}
	root, err := qualifyPreparedSerenaDirectory(ctx, values["SYSTEMROOT"])
	if err != nil {
		return "", nil, err
	}
	windir, err := qualifyPreparedSerenaDirectory(ctx, values["WINDIR"])
	if err != nil {
		return "", nil, err
	}
	if root.Device != windir.Device || root.FileID != windir.FileID {
		return "", nil, errors.New("SERENA_BUILD_BOOTSTRAP_OS_ROOT_MISMATCH")
	}
	command, _, err := qualifyPreparedSerenaFile(ctx, values["COMSPEC"], 64<<20)
	if err != nil {
		return "", nil, err
	}
	paths := filepath.SplitList(values["PATH"])
	if len(paths) == 0 {
		return "", nil, errors.New("SERENA_BUILD_BOOTSTRAP_PATH_EMPTY")
	}
	qualified := make([]PreparedSerenaFile, 0, len(paths))
	for _, path := range paths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return "", nil, errors.New("SERENA_BUILD_BOOTSTRAP_PATH_UNQUALIFIED")
		}
		directory, err := qualifyPreparedSerenaDirectory(ctx, path)
		if err != nil {
			return "", nil, err
		}
		qualified = append(qualified, directory)
	}
	raw, err := json.Marshal(struct {
		SystemRoot, Windir, Command PreparedSerenaFile
		Path                        []PreparedSerenaFile
	}{root, windir, command, qualified})
	if err != nil {
		return "", nil, err
	}
	return preparedSerenaHash(raw), values, ctx.Err()
}
