package reversedepgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/portname"
	"mcp-local-hub/internal/vcpkgmcp/portresolution"
)

type universeEnumerator struct {
	ctx       context.Context
	args      Args
	names     map[string]struct{}
	bytesRead int64
	entries   int
	facts     []string
}

func EnumerateUniverse(ctx context.Context, args Args) UniverseOutcome {
	enumerator := universeEnumerator{ctx: ctx, args: args, names: map[string]struct{}{}}
	if failure := enumerator.recordConfigurationInputs(); failure != nil {
		return enumerator.outcome(failure)
	}
	if args.ManifestRoot == "" {
		if failure := enumerator.enumerateClassicRoots(); failure != nil {
			return enumerator.outcome(failure)
		}
	} else {
		if failure := enumerator.enumerateManifestRoots(); failure != nil {
			return enumerator.outcome(failure)
		}
	}
	if len(enumerator.names) > MaxDistinctPorts {
		return enumerator.outcome(resourceFailure("distinct_ports"))
	}
	names := make([]string, 0, len(enumerator.names))
	for name := range enumerator.names {
		names = append(names, name)
	}
	sort.Strings(names)
	candidates := make([]Candidate, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return enumerator.outcome(cancelFailure(err))
		}
		candidate, failure := enumerator.resolveCandidate(name)
		if failure != nil {
			return enumerator.outcome(failure)
		}
		candidates = append(candidates, candidate)
	}
	if _, exists := enumerator.names[args.Port]; !exists {
		candidates = append(candidates, Candidate{Name: args.Port, Inspectable: false})
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	}
	enumerator.facts = append(enumerator.facts, "candidates="+strings.Join(candidateNames(candidates), ","))
	for _, candidate := range candidates {
		enumerator.facts = append(enumerator.facts, candidate.Name+"|"+candidate.WinnerDirectory+"|"+candidate.WinnerSource+"|"+candidate.DefinitionHash+"|"+strings.Join(candidate.DeclaredDependencies, ","))
	}
	sort.Strings(enumerator.facts)
	digest := sha256.Sum256([]byte(strings.Join(enumerator.facts, "\n")))
	return UniverseOutcome{Complete: true, Candidates: candidates, Digest: hex.EncodeToString(digest[:]), BytesRead: enumerator.bytesRead, Entries: enumerator.entries}
}

func (enumerator *universeEnumerator) recordConfigurationInputs() *Failure {
	enumerator.facts = append(enumerator.facts,
		"vcpkg-root="+filepath.Clean(enumerator.args.VcpkgRoot),
		"manifest-root="+filepath.Clean(enumerator.args.ManifestRoot),
	)
	for index, root := range enumerator.args.OverlayPorts {
		enumerator.facts = append(enumerator.facts, fmt.Sprintf("overlay-port[%02d]=%s", index, filepath.Clean(root)))
	}
	triplets := []string{enumerator.args.Triplet, enumerator.args.HostTriplet}
	sort.Strings(triplets)
	triplets = compactStrings(triplets)
	for index, root := range enumerator.args.OverlayTriplets {
		enumerator.facts = append(enumerator.facts, fmt.Sprintf("overlay-triplet[%02d]=%s", index, filepath.Clean(root)))
		if _, failure := enumerator.readDirectory(root); failure != nil {
			return failure
		}
		for _, triplet := range triplets {
			if failure := enumerator.recordOptionalConfigurationFile("overlay-triplet-file", filepath.Join(root, triplet+".cmake")); failure != nil {
				return failure
			}
		}
	}
	for _, root := range []string{filepath.Join(enumerator.args.VcpkgRoot, "triplets"), filepath.Join(enumerator.args.VcpkgRoot, "triplets", "community")} {
		for _, triplet := range triplets {
			if failure := enumerator.recordOptionalConfigurationFile("builtin-triplet-file", filepath.Join(root, triplet+".cmake")); failure != nil {
				return failure
			}
		}
	}
	return nil
}

func (enumerator *universeEnumerator) recordOptionalConfigurationFile(label, path string) *Failure {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			enumerator.facts = append(enumerator.facts, label+"="+filepath.Clean(path)+"|absent")
			return nil
		}
		return universeFailure(label + " unreadable")
	}
	if !info.Mode().IsRegular() {
		return universeFailure(label + " is not a regular file")
	}
	body, failure := enumerator.readMetadata(path)
	if failure != nil {
		return failure
	}
	enumerator.addHashFact(label, path, body)
	return nil
}

func (enumerator *universeEnumerator) addHashFact(label, path string, body []byte) {
	digest := sha256.Sum256(body)
	enumerator.facts = append(enumerator.facts, label+"="+filepath.Clean(path)+"|"+hex.EncodeToString(digest[:]))
}

func (enumerator *universeEnumerator) outcome(failure *Failure) UniverseOutcome {
	reason := ReasonIncompletePortUniverse
	if failure != nil && failure.Reason != "" {
		reason = failure.Reason
	}
	return UniverseOutcome{Complete: false, Reason: reason, Failure: failure, BytesRead: enumerator.bytesRead, Entries: enumerator.entries}
}

func (enumerator *universeEnumerator) enumerateClassicRoots() *Failure {
	for _, root := range enumerator.args.OverlayPorts {
		if failure := enumerator.enumeratePortRoot(root); failure != nil {
			return failure
		}
	}
	return enumerator.enumeratePortCollection(filepath.Join(enumerator.args.VcpkgRoot, "ports"))
}

func (enumerator *universeEnumerator) enumeratePortRoot(root string) *Failure {
	individualName, individual, failure := enumerator.individualPortName(root)
	if failure != nil {
		return failure
	}
	if individual {
		enumerator.names[individualName] = struct{}{}
		enumerator.facts = append(enumerator.facts, "individual="+root+"|"+individualName)
		return nil
	}
	return enumerator.enumeratePortCollection(root)
}

func (enumerator *universeEnumerator) enumeratePortCollection(root string) *Failure {
	entries, failure := enumerator.readDirectory(root)
	if failure != nil {
		return failure
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		definition := filepath.Join(root, entry.Name())
		if !hasPortDefinition(definition) {
			continue
		}
		if _, err := portname.Parse(entry.Name()); err != nil {
			return universeFailure("invalid port definition directory")
		}
		enumerator.names[entry.Name()] = struct{}{}
	}
	enumerator.facts = append(enumerator.facts, "port-root="+root)
	return nil
}

func (enumerator *universeEnumerator) individualPortName(root string) (string, bool, *Failure) {
	portfile, err := os.Lstat(filepath.Join(root, "portfile.cmake"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, universeFailure("individual overlay portfile unreadable")
	}
	if !portfile.Mode().IsRegular() {
		return "", false, universeFailure("individual overlay portfile is not regular")
	}
	manifestPath := filepath.Join(root, "vcpkg.json")
	if body, failure := enumerator.readMetadata(manifestPath); failure == nil {
		var manifest struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(body, &manifest) != nil {
			return "", false, universeFailure("invalid individual overlay vcpkg.json")
		}
		if _, err := portname.Parse(manifest.Name); err != nil {
			return "", false, universeFailure("invalid individual overlay name")
		}
		return manifest.Name, true, nil
	} else if !os.IsNotExist(failure) {
		return "", false, failure
	}
	controlPath := filepath.Join(root, "CONTROL")
	body, failure := enumerator.readMetadata(controlPath)
	if failure != nil {
		if os.IsNotExist(failure) {
			return "", false, universeFailure("individual overlay metadata missing")
		}
		return "", false, failure
	}
	for _, line := range strings.Split(string(body), "\n") {
		key, value, found := strings.Cut(line, ":")
		if found && strings.EqualFold(strings.TrimSpace(key), "Source") {
			name := strings.TrimSpace(value)
			if _, err := portname.Parse(name); err != nil {
				return "", false, universeFailure("invalid CONTROL Source")
			}
			return name, true, nil
		}
	}
	return "", false, universeFailure("CONTROL Source missing")
}

func (enumerator *universeEnumerator) enumerateManifestRoots() *Failure {
	manifestPath := filepath.Join(enumerator.args.ManifestRoot, "vcpkg.json")
	manifestBody, failure := enumerator.readMetadata(manifestPath)
	if failure != nil {
		return failure
	}
	enumerator.addHashFact("manifest", manifestPath, manifestBody)
	var manifest struct {
		Configuration json.RawMessage `json:"vcpkg-configuration"`
	}
	if json.Unmarshal(manifestBody, &manifest) != nil {
		return universeFailure("invalid manifest vcpkg.json")
	}
	configuration := manifest.Configuration
	standalone := filepath.Join(enumerator.args.ManifestRoot, "vcpkg-configuration.json")
	if _, err := os.Lstat(standalone); err == nil {
		configuration, failure = enumerator.readMetadata(standalone)
		if failure != nil {
			return failure
		}
		enumerator.addHashFact("configuration", standalone, configuration)
	} else if !os.IsNotExist(err) {
		return universeFailure("configuration unreadable")
	}
	for _, overlay := range enumerator.args.OverlayPorts {
		if failure := enumerator.enumeratePortRoot(overlay); failure != nil {
			return failure
		}
	}
	if len(configuration) == 0 {
		return enumerator.enumerateVersionRoot(filepath.Join(enumerator.args.VcpkgRoot, "versions"))
	}
	var config struct {
		DefaultRegistry *registryConfig  `json:"default-registry"`
		Registries      []registryConfig `json:"registries"`
	}
	if json.Unmarshal(configuration, &config) != nil {
		return universeFailure("invalid vcpkg registry configuration")
	}
	localCount := 0
	if config.DefaultRegistry == nil {
		if failure := enumerator.enumerateVersionRoot(filepath.Join(enumerator.args.VcpkgRoot, "versions")); failure != nil {
			return failure
		}
	} else if failure := enumerator.enumerateRegistry(*config.DefaultRegistry, &localCount); failure != nil {
		return failure
	}
	for _, registry := range config.Registries {
		if failure := enumerator.enumerateRegistry(registry, &localCount); failure != nil {
			return failure
		}
	}
	return nil
}

type registryConfig struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

func (enumerator *universeEnumerator) enumerateRegistry(registry registryConfig, localCount *int) *Failure {
	switch registry.Kind {
	case "builtin":
		return enumerator.enumerateVersionRoot(filepath.Join(enumerator.args.VcpkgRoot, "versions"))
	case "filesystem":
		*localCount++
		if *localCount > MaxLocalRegistries {
			return resourceFailure("local_registries")
		}
		if registry.Path == "" || strings.Contains(registry.Path, "://") || strings.Contains(registry.Path, "?") {
			return networkFailure("filesystem registry path invalid")
		}
		root := registry.Path
		if !filepath.IsAbs(root) {
			root = filepath.Join(enumerator.args.ManifestRoot, root)
		}
		root = canonicalForComparison(root)
		if !pathContains(canonicalForComparison(enumerator.args.ManifestRoot), root) && !filepath.IsAbs(registry.Path) {
			return networkFailure("relative filesystem registry escapes configuration root")
		}
		if pathsOverlap(root, enumerator.args.ScratchRoot) {
			return &Failure{ID: FailureScratchIO, Reason: ReasonScratchIOFailed, Stage: "universe", Detail: "scratch_root overlaps filesystem registry"}
		}
		enumerator.facts = append(enumerator.facts, fmt.Sprintf("filesystem-registry[%02d]=%s", *localCount-1, root))
		return enumerator.enumerateVersionRoot(filepath.Join(root, "versions"))
	case "git", "artifact", "http", "https", "":
		return networkFailure("non-local registry kind refused")
	default:
		return networkFailure("unknown registry kind refused")
	}
}

func (enumerator *universeEnumerator) enumerateVersionRoot(root string) *Failure {
	buckets, failure := enumerator.readDirectory(root)
	if failure != nil {
		return failure
	}
	for _, bucket := range buckets {
		if !bucket.IsDir() || bucket.Type()&os.ModeSymlink != 0 {
			continue
		}
		files, failure := enumerator.readDirectory(filepath.Join(root, bucket.Name()))
		if failure != nil {
			return failure
		}
		for _, file := range files {
			if file.IsDir() || filepath.Ext(file.Name()) != ".json" || file.Type()&os.ModeSymlink != 0 {
				continue
			}
			name := strings.TrimSuffix(file.Name(), ".json")
			if _, err := portname.Parse(name); err != nil || bucket.Name() != name[:1]+"-" {
				return universeFailure("version index filename/name mismatch")
			}
			body, failure := enumerator.readMetadata(filepath.Join(root, bucket.Name(), file.Name()))
			if failure != nil {
				return failure
			}
			var index struct {
				Versions []json.RawMessage `json:"versions"`
			}
			if json.Unmarshal(body, &index) != nil || len(index.Versions) == 0 {
				return universeFailure("invalid version index")
			}
			digest := sha256.Sum256(body)
			enumerator.facts = append(enumerator.facts, "version="+name+"|"+hex.EncodeToString(digest[:]))
			enumerator.names[name] = struct{}{}
		}
	}
	return nil
}

func (enumerator *universeEnumerator) resolveCandidate(name string) (Candidate, *Failure) {
	resolutionArgs := portresolution.Args{Port: name, OverlayPorts: enumerator.args.OverlayPorts}
	if enumerator.args.ManifestRoot == "" {
		resolutionArgs.VcpkgRoot = enumerator.args.VcpkgRoot
	}
	resolved := portresolution.ResolvePortContext(enumerator.ctx, resolutionArgs, portresolution.DefaultDeps())
	candidate := Candidate{Name: name}
	if resolved.Status == evidence.StatusOK && resolved.Winner != nil {
		candidate.WinnerDirectory = resolved.Winner.Directory
		candidate.WinnerSource = resolved.Winner.Source
		for _, shadow := range resolved.Shadows {
			candidate.Shadows = append(candidate.Shadows, shadow.Directory)
		}
	} else if enumerator.args.ManifestRoot != "" && (resolved.Reason == portresolution.ReasonPortNotFound || resolved.Reason == portresolution.ReasonNoRootsSupplied) {
		return candidate, nil
	} else {
		return Candidate{}, universeFailure("port resolution could not settle candidate " + name)
	}
	manifestPath := filepath.Join(candidate.WinnerDirectory, "vcpkg.json")
	body, failure := enumerator.readMetadata(manifestPath)
	if failure != nil {
		if os.IsNotExist(failure) {
			return candidate, nil
		}
		return Candidate{}, failure
	}
	digest := sha256.Sum256(body)
	candidate.DefinitionHash = hex.EncodeToString(digest[:])
	candidate.DeclaredDependencies, candidate.Inspectable = ScanDeclaredSuperset(body)
	return candidate, nil
}

func (enumerator *universeEnumerator) readDirectory(root string) ([]os.DirEntry, *Failure) {
	if err := enumerator.ctx.Err(); err != nil {
		return nil, cancelFailure(err)
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, universeFailure("directory unreadable or not a regular directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, universeFailure("directory enumeration failed")
	}
	if len(entries) > MaxEntriesPerRoot {
		return nil, resourceFailure("directory_entries")
	}
	enumerator.entries += len(entries)
	return entries, nil
}

func (enumerator *universeEnumerator) readMetadata(path string) ([]byte, *Failure) {
	if err := enumerator.ctx.Err(); err != nil {
		return nil, cancelFailure(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, &Failure{ID: FailureUniverseIncomplete, Reason: ReasonIncompletePortUniverse, Stage: "universe", Detail: fmt.Sprintf("path error: %v", err), cause: err}
	}
	if !info.Mode().IsRegular() || info.Size() > MaxMetadataBytes {
		return nil, universeFailure("metadata is not regular or exceeds limit")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, universeFailure("metadata read failed")
	}
	enumerator.bytesRead += int64(len(body))
	if enumerator.bytesRead > MaxEnumeratorBytes {
		return nil, resourceFailure("enumerator_bytes")
	}
	return body, nil
}

func hasPortDefinition(dir string) bool {
	for _, name := range []string{"vcpkg.json", "CONTROL", "portfile.cmake"} {
		if info, err := os.Lstat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func universeFailure(detail string) *Failure {
	return &Failure{ID: FailureUniverseIncomplete, Reason: ReasonIncompletePortUniverse, Stage: "universe", Detail: detail}
}

func networkFailure(detail string) *Failure {
	return &Failure{ID: FailureNetworkRegistryRefused, Reason: ReasonNetworkDisabledRegistry, Stage: "universe", Detail: detail}
}

func resourceFailure(detail string) *Failure {
	return &Failure{ID: FailureResourceLimit, Reason: ReasonResourceLimitExceeded, Stage: "resource", Detail: detail}
}

func cancelFailure(err error) *Failure {
	return &Failure{ID: FailureCancelled, Reason: ReasonRequestCancelled, Stage: "universe", Detail: err.Error()}
}
