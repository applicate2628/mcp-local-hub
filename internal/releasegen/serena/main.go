// Command serena synchronizes checked-in release projections for Serena,
// Fetch, and Sequential Thinking from their canonical release.yaml records. It
// owns only each declared manifest, generated README, and marketplace row; the
// target row uses LF so plain git diff --check remains clean on Windows without
// reformatting unrelated catalog rows.
package main

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	repositoryURL        = "https://github.com/oraios/serena"
	serversRepositoryURL = "https://github.com/modelcontextprotocol/servers"

	sequentialNodePackage   = "@modelcontextprotocol/node"
	sequentialServerPackage = "@modelcontextprotocol/server"
)

var (
	commitPattern        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	calverPattern        = regexp.MustCompile(`^[0-9]{4}\.[0-9]{1,2}\.[0-9]{1,2}$`)
	sha256Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	semverPattern        = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	npmIntegrityPattern  = regexp.MustCompile(`^sha512-[A-Za-z0-9+/]+={0,2}$`)
	nodeEnginePattern    = regexp.MustCompile(`^>=[1-9][0-9]*$`)
	pinnedCommentPattern = regexp.MustCompile(`pinned serena commit [0-9a-f]{40}`)
)

var sequentialV2VersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Descriptor is the sole checked-in source of Serena release metadata.
type Descriptor struct {
	SchemaVersion      int    `yaml:"schema_version"`
	UpstreamRepository string `yaml:"upstream_repository"`
	Version            string `yaml:"version"`
	Commit             string `yaml:"commit"`
	ReadmePath         string `yaml:"readme_path"`
}

type runtimeDescriptor struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
}

type fetchArtifacts struct {
	FetchWheelSHA256 string `yaml:"fetch_wheel_sha256"`
	MCPVersion       string `yaml:"mcp_version"`
	MCPWheelSHA256   string `yaml:"mcp_wheel_sha256"`
}

// FetchDescriptor is the sole checked-in source of Fetch release metadata.
type FetchDescriptor struct {
	SchemaVersion      int               `yaml:"schema_version"`
	ServerID           string            `yaml:"server_id"`
	UpstreamRepository string            `yaml:"upstream_repository"`
	Version            string            `yaml:"version"`
	SourceCommit       string            `yaml:"source_commit"`
	ReadmePath         string            `yaml:"readme_path"`
	Runtime            runtimeDescriptor `yaml:"runtime"`
	Artifacts          fetchArtifacts    `yaml:"artifacts"`
}

type sequentialArtifacts struct {
	PackageIntegrity string `yaml:"package_integrity"`
	NodeEngine       string `yaml:"node_engine"`
}

type resolvedSDKDescriptor struct {
	Version   string `yaml:"version"`
	Integrity string `yaml:"integrity"`
}

type sequentialLockDescriptor struct {
	LockfileVersion              int                              `yaml:"lockfile_version"`
	PackageCount                 int                              `yaml:"package_count"`
	SHA256                       string                           `yaml:"sha256"`
	ResolvedSDK                  resolvedSDKDescriptor            `yaml:"resolved_sdk"`
	ResolvedProtocolDependencies map[string]resolvedSDKDescriptor `yaml:"resolved_protocol_dependencies"`
}

// SequentialDescriptor is the sole checked-in source of Sequential Thinking
// release and exact resolved-lock evidence.
type SequentialDescriptor struct {
	SchemaVersion      int                      `yaml:"schema_version"`
	ServerID           string                   `yaml:"server_id"`
	UpstreamRepository string                   `yaml:"upstream_repository"`
	Version            string                   `yaml:"version"`
	SourceCommit       string                   `yaml:"source_commit"`
	ReadmePath         string                   `yaml:"readme_path"`
	Runtime            runtimeDescriptor        `yaml:"runtime"`
	Artifacts          sequentialArtifacts      `yaml:"artifacts"`
	Lock               sequentialLockDescriptor `yaml:"lock"`
}

func (d FetchDescriptor) validate() error {
	if d.SchemaVersion != 1 || d.ServerID != "fetch" {
		return errors.New("Fetch descriptor must use schema_version 1 and server_id fetch")
	}
	if d.UpstreamRepository != serversRepositoryURL || !calverPattern.MatchString(d.Version) {
		return errors.New("Fetch descriptor repository or version is not an exact release")
	}
	if !commitPattern.MatchString(d.SourceCommit) {
		return errors.New("Fetch source_commit is not an immutable revision")
	}
	if d.ReadmePath != "src/fetch/README.md" {
		return errors.New("Fetch readme_path must be src/fetch/README.md")
	}
	if !semverPattern.MatchString(d.Artifacts.MCPVersion) {
		return errors.New("Fetch mcp_version must be an exact numeric release (major.minor.patch)")
	}
	if d.Runtime.Command != "uvx" || !equalStrings(d.Runtime.Args, fetchExpectedArgs(d)) {
		return errors.New("Fetch runtime command does not equal the descriptor-derived exact package pair")
	}
	if !sha256Pattern.MatchString(d.Artifacts.FetchWheelSHA256) || !sha256Pattern.MatchString(d.Artifacts.MCPWheelSHA256) {
		return errors.New("Fetch artifact hashes must be exact SHA-256 values")
	}
	return nil
}

func (d FetchDescriptor) readmeURL() string {
	return "https://raw.githubusercontent.com/modelcontextprotocol/servers/" + d.SourceCommit + "/" + d.ReadmePath
}

func loadFetchDescriptor(path string) (FetchDescriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return FetchDescriptor{}, err
	}
	var descriptor FetchDescriptor
	if err := yaml.Unmarshal(data, &descriptor); err != nil {
		return FetchDescriptor{}, err
	}
	if err := descriptor.validate(); err != nil {
		return FetchDescriptor{}, err
	}
	return descriptor, nil
}

func (d SequentialDescriptor) validate() error {
	if d.ServerID != "sequential-thinking" {
		return fmt.Errorf("Sequential descriptor must use schema_version %d and server_id sequential-thinking", d.SchemaVersion)
	}
	if d.UpstreamRepository != serversRepositoryURL || !semverPattern.MatchString(d.Version) {
		return errors.New("Sequential descriptor repository or version is not an exact release")
	}
	if !commitPattern.MatchString(d.SourceCommit) {
		return errors.New("Sequential source_commit is not an immutable revision")
	}
	if d.ReadmePath != "src/sequentialthinking/README.md" {
		return errors.New("Sequential readme_path must be src/sequentialthinking/README.md")
	}
	if d.Runtime.Command != "npx" || !equalStrings(d.Runtime.Args, sequentialExpectedArgs(d)) {
		return errors.New("Sequential runtime command does not equal the descriptor-derived exact package release")
	}
	return nil
}

func (l sequentialLockDescriptor) validateMetadata() error {
	switch {
	case l.LockfileVersion != 3:
		return errors.New("Sequential lock.lockfile_version must be 3")
	case l.PackageCount <= 0:
		return errors.New("Sequential lock.package_count must be positive")
	case !sha256Pattern.MatchString(l.SHA256):
		return errors.New("Sequential lock.sha256 must be an exact SHA-256 digest")
	default:
		return nil
	}
}

func (d SequentialDescriptor) validateV1Evidence() error {
	if !npmIntegrityPattern.MatchString(d.Artifacts.PackageIntegrity) || d.Lock.validateMetadata() != nil {
		return errors.New("Sequential package or npm-lock evidence is not an exact closure")
	}
	if !semverPattern.MatchString(d.Lock.ResolvedSDK.Version) || !npmIntegrityPattern.MatchString(d.Lock.ResolvedSDK.Integrity) {
		return errors.New("Sequential resolved SDK evidence is not exact")
	}
	return nil
}

func (d SequentialDescriptor) readmeURL() string {
	return "https://raw.githubusercontent.com/modelcontextprotocol/servers/" + d.SourceCommit + "/" + d.ReadmePath
}

func loadSequentialDescriptor(path string) (SequentialDescriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return SequentialDescriptor{}, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("Sequential descriptor is missing schema_version")
		}
		return SequentialDescriptor{}, err
	}
	var header struct {
		SchemaVersion int `yaml:"schema_version"`
	}
	if err := document.Decode(&header); err != nil {
		return SequentialDescriptor{}, err
	}
	var validateEvidence func(SequentialDescriptor) error
	switch header.SchemaVersion {
	case 1:
		// Decode only the first document with the legacy YAML rules.
		if err := rejectSequentialV1Hybrid(&document); err != nil {
			return SequentialDescriptor{}, err
		}
		validateEvidence = SequentialDescriptor.validateV1Evidence
	case 2:
		var trailing yaml.Node
		if err := decoder.Decode(&trailing); err == nil {
			return SequentialDescriptor{}, errors.New("Sequential schema_version 2 requires a single YAML document")
		} else if !errors.Is(err, io.EOF) {
			return SequentialDescriptor{}, fmt.Errorf("Sequential schema_version 2 trailing YAML: %w", err)
		}
		if err := validateSequentialV2Shape(document.Content[0]); err != nil {
			return SequentialDescriptor{}, err
		}
		validateEvidence = SequentialDescriptor.validateV2Evidence
	default:
		return SequentialDescriptor{}, fmt.Errorf("unsupported Sequential schema_version %d (want 1 or 2)", header.SchemaVersion)
	}
	var descriptor SequentialDescriptor
	if err := document.Decode(&descriptor); err != nil {
		return SequentialDescriptor{}, err
	}
	if err := descriptor.validate(); err != nil {
		return SequentialDescriptor{}, err
	}
	if err := validateEvidence(descriptor); err != nil {
		return SequentialDescriptor{}, err
	}
	return descriptor, nil
}

func rejectSequentialV1Hybrid(document *yaml.Node) error {
	// Node values distinguish a missing key from an explicitly null value
	// while the decoder retains V1 alias, merge, and unknown-field behavior.
	var reserved struct {
		Artifacts struct {
			NodeEngine yaml.Node `yaml:"node_engine"`
		} `yaml:"artifacts"`
		Lock struct {
			ResolvedProtocolDependencies yaml.Node `yaml:"resolved_protocol_dependencies"`
		} `yaml:"lock"`
	}
	if err := document.Decode(&reserved); err != nil {
		return err
	}
	if !reserved.Artifacts.NodeEngine.IsZero() {
		return errors.New("Sequential schema_version 1 reserves artifacts.node_engine for schema_version 2")
	}
	if !reserved.Lock.ResolvedProtocolDependencies.IsZero() {
		return errors.New("Sequential schema_version 1 reserves lock.resolved_protocol_dependencies for schema_version 2")
	}
	return nil
}

func validateSequentialV2Shape(root *yaml.Node) error {
	fields, err := sequentialV2Mapping(root, "Sequential descriptor", map[string]string{
		"schema_version": "!!int", "server_id": "!!str", "upstream_repository": "!!str",
		"version": "!!str", "source_commit": "!!str", "readme_path": "!!str",
		"runtime": "!!map", "artifacts": "!!map", "lock": "!!map",
	})
	if err != nil {
		return err
	}
	runtime, err := sequentialV2Mapping(fields["runtime"], "runtime", map[string]string{
		"command": "!!str", "args": "!!seq",
	})
	if err != nil {
		return err
	}
	for i, arg := range runtime["args"].Content {
		if !sequentialV2Value(arg, "!!str") {
			return fmt.Errorf("Sequential runtime.args[%d] must be a nonempty YAML string", i)
		}
	}
	if _, err := sequentialV2Mapping(fields["artifacts"], "artifacts", map[string]string{
		"package_integrity": "!!str", "node_engine": "!!str",
	}); err != nil {
		return err
	}
	lock, err := sequentialV2Mapping(fields["lock"], "lock", map[string]string{
		"lockfile_version": "!!int", "package_count": "!!int", "sha256": "!!str",
		"resolved_protocol_dependencies": "!!map",
	})
	if err != nil {
		return err
	}
	dependencies, err := sequentialV2Mapping(lock["resolved_protocol_dependencies"], "lock.resolved_protocol_dependencies", map[string]string{
		sequentialNodePackage: "!!map", sequentialServerPackage: "!!map",
	})
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(dependencies)) {
		if _, err := sequentialV2Mapping(dependencies[name], "lock.resolved_protocol_dependencies."+name, map[string]string{
			"version": "!!str", "integrity": "!!str",
		}); err != nil {
			return err
		}
	}
	return nil
}

func sequentialV2Mapping(node *yaml.Node, path string, required map[string]string) (map[string]*yaml.Node, error) {
	if !sequentialV2Value(node, "!!map") {
		return nil, fmt.Errorf("%s must be a YAML mapping", path)
	}
	fields := make(map[string]*yaml.Node, len(required))
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if !sequentialV2Value(key, "!!str") {
			return nil, fmt.Errorf("%s has an invalid YAML key %q", path, key.Value)
		}
		tag, known := required[key.Value]
		if !known {
			return nil, fmt.Errorf("%s has unexpected field %q", path, key.Value)
		}
		if _, duplicate := fields[key.Value]; duplicate {
			return nil, fmt.Errorf("%s has duplicate field %q", path, key.Value)
		}
		if !sequentialV2Value(value, tag) {
			return nil, fmt.Errorf("%s.%s must be a nonempty YAML %s", path, key.Value, strings.TrimPrefix(tag, "!!"))
		}
		fields[key.Value] = value
	}
	for _, key := range slices.Sorted(maps.Keys(required)) {
		if _, present := fields[key]; !present {
			return nil, fmt.Errorf("%s is missing required field %q", path, key)
		}
	}
	return fields, nil
}

func sequentialV2Value(node *yaml.Node, tag string) bool {
	if node.ShortTag() != tag {
		return false
	}
	switch tag {
	case "!!map":
		return node.Kind == yaml.MappingNode
	case "!!seq":
		return node.Kind == yaml.SequenceNode
	default:
		return node.Kind == yaml.ScalarNode && node.Value != ""
	}
}

func (d SequentialDescriptor) validateV2Evidence() error {
	if !sequentialV2VersionPattern.MatchString(d.Version) {
		return errors.New("Sequential version must be an exact numeric release without leading zeros")
	}
	if !exactSHA512Integrity(d.Artifacts.PackageIntegrity) {
		return errors.New("Sequential artifacts.package_integrity must be an exact SHA-512 digest")
	}
	if !nodeEnginePattern.MatchString(d.Artifacts.NodeEngine) {
		return errors.New("Sequential artifacts.node_engine must be >= followed by a positive integer major version")
	}
	if err := d.Lock.validateMetadata(); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(d.Lock.ResolvedProtocolDependencies)) {
		pin := d.Lock.ResolvedProtocolDependencies[name]
		if !sequentialV2VersionPattern.MatchString(pin.Version) {
			return fmt.Errorf("Sequential locked %s version must be an exact numeric release without leading zeros", name)
		}
		if !exactSHA512Integrity(pin.Integrity) {
			return fmt.Errorf("Sequential locked %s integrity must be an exact SHA-512 digest", name)
		}
	}
	return nil
}

func exactSHA512Integrity(token string) bool {
	if !npmIntegrityPattern.MatchString(token) {
		return false
	}
	digest, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(token, "sha512-"))
	return err == nil && len(digest) == sha512.Size
}

func (d Descriptor) validate() error {
	if d.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema_version %d", d.SchemaVersion)
	}
	if d.UpstreamRepository != repositoryURL {
		return fmt.Errorf("upstream_repository = %q, want %q", d.UpstreamRepository, repositoryURL)
	}
	if !semverPattern.MatchString(d.Version) {
		return fmt.Errorf("version = %q, want an exact semantic version", d.Version)
	}
	if !commitPattern.MatchString(d.Commit) {
		return fmt.Errorf("commit must be a 40-character lower-case revision")
	}
	if d.ReadmePath != "README.md" {
		return fmt.Errorf("readme_path = %q, want README.md", d.ReadmePath)
	}
	return nil
}

// SourceArgument returns the uvx --from argument derived from the descriptor.
func (d Descriptor) SourceArgument() string {
	return "git+" + d.UpstreamRepository + "@" + d.Commit
}

func (d Descriptor) readmeURL() string {
	return "https://raw.githubusercontent.com/oraios/serena/" + d.Commit + "/" + d.ReadmePath
}

// LoadDescriptor reads and validates a release descriptor.
func LoadDescriptor(path string) (Descriptor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Descriptor{}, err
	}
	var descriptor Descriptor
	if err := yaml.Unmarshal(data, &descriptor); err != nil {
		return Descriptor{}, err
	}
	if err := descriptor.validate(); err != nil {
		return Descriptor{}, err
	}
	return descriptor, nil
}

type target struct {
	path string
	data []byte
}

type stagedTarget struct {
	target   string
	temp     string
	preimage []byte
	existed  bool
}

type projectionRegistration struct {
	serverID       string
	descriptorPath string
	manifestPath   string
	readmePath     string
	load           func(string) (loadedProjection, error)
}

type loadedProjection struct {
	registration     projectionRegistration
	manifest         []byte
	readme           []byte
	checkManifest    func([]byte) error
	generateManifest func([]byte) ([]byte, error)
	checkCatalog     func([]byte) error
	generateCatalog  func([]byte) ([]byte, error)
}

var declaredReleaseProjections = [...]projectionRegistration{
	{
		serverID:       "serena",
		descriptorPath: "servers/serena/release.yaml",
		manifestPath:   "servers/serena/manifest.yaml",
		load:           loadSerenaProjection,
	},
	{
		serverID:       "fetch",
		descriptorPath: "servers/fetch/release.yaml",
		manifestPath:   "servers/fetch/manifest.yaml",
		readmePath:     "servers/fetch/README.md",
		load:           loadFetchProjection,
	},
	{
		serverID:       "sequential-thinking",
		descriptorPath: "servers/sequential-thinking/release.yaml",
		manifestPath:   "servers/sequential-thinking/manifest.yaml",
		readmePath:     "servers/sequential-thinking/README.md",
		load:           loadSequentialProjection,
	},
}

// publishOps keeps filesystem effects injectable for deterministic transaction
// tests. Production obtains a fresh immutable set for each publication.
type publishOps struct {
	readFile   func(string) ([]byte, error)
	createTemp func(string, string) (*os.File, error)
	writeTemp  func(*os.File, []byte) error
	rename     func(string, string) error
	remove     func(string) error
}

var releaseCatalogPaths = [...]string{
	"marketplace/v1/catalog.json",
	"marketplace/v2/catalog.json",
}

type projectionPathError struct {
	path string
	err  error
}

func (e *projectionPathError) Error() string { return e.path + ": " + e.err.Error() }
func (e *projectionPathError) Unwrap() error { return e.err }

func atProjectionPath(path string, err error) error {
	if err == nil {
		return nil
	}
	return &projectionPathError{path: path, err: err}
}

func wrapProjectionPath(err error, wrap func(string, error) error) error {
	var pathErr *projectionPathError
	if errors.As(err, &pathErr) {
		return wrap(pathErr.path, pathErr.err)
	}
	return wrap("declared release projection registry", err)
}

func validateDeclaredReleaseProjections() error {
	seenIDs := make(map[string]struct{}, len(declaredReleaseProjections))
	seenPaths := make(map[string]string, len(declaredReleaseProjections)*3)
	for _, registration := range declaredReleaseProjections {
		if registration.serverID == "" || registration.descriptorPath == "" || registration.manifestPath == "" || registration.load == nil {
			return errors.New("release projection registration is incomplete")
		}
		if _, exists := seenIDs[registration.serverID]; exists {
			return fmt.Errorf("duplicate release projection server %q", registration.serverID)
		}
		seenIDs[registration.serverID] = struct{}{}
		for _, path := range []string{registration.descriptorPath, registration.manifestPath, registration.readmePath} {
			if path == "" {
				continue
			}
			if owner, exists := seenPaths[path]; exists {
				return fmt.Errorf("release projection path %q is shared by %s and %s", path, owner, registration.serverID)
			}
			seenPaths[path] = registration.serverID
		}
	}
	return nil
}

func loadDeclaredProjection(root string, registration projectionRegistration) (loadedProjection, error) {
	projection, err := registration.load(root)
	if err != nil {
		return loadedProjection{}, atProjectionPath(registration.descriptorPath, err)
	}
	projection.registration = registration
	projection.manifest, err = os.ReadFile(filepath.Join(root, filepath.FromSlash(registration.manifestPath)))
	if err != nil {
		return loadedProjection{}, atProjectionPath(registration.manifestPath, err)
	}
	return projection, nil
}

func loadDeclaredProjections(root string) ([]loadedProjection, error) {
	loaded := make([]loadedProjection, 0, len(declaredReleaseProjections))
	for _, registration := range declaredReleaseProjections {
		projection, err := loadDeclaredProjection(root, registration)
		if err != nil {
			return nil, err
		}
		loaded = append(loaded, projection)
	}
	return loaded, nil
}

func readReleaseCatalogs(root string) ([len(releaseCatalogPaths)][]byte, error) {
	var catalogs [len(releaseCatalogPaths)][]byte
	for index, relative := range releaseCatalogPaths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return catalogs, atProjectionPath(relative, err)
		}
		catalogs[index] = data
	}
	return catalogs, nil
}

func checkDeclaredProjection(root string, projection loadedProjection, catalogs [len(releaseCatalogPaths)][]byte) error {
	if err := projection.checkManifest(projection.manifest); err != nil {
		return atProjectionPath(projection.registration.manifestPath, err)
	}
	if projection.registration.readmePath != "" {
		readme, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(projection.registration.readmePath)))
		if err != nil {
			return atProjectionPath(projection.registration.readmePath, err)
		}
		if !bytes.Equal(readme, projection.readme) {
			return atProjectionPath(projection.registration.readmePath, errors.New("README does not equal descriptor-derived release metadata"))
		}
	}
	for index, data := range catalogs {
		if err := projection.checkCatalog(data); err != nil {
			return atProjectionPath(releaseCatalogPaths[index], err)
		}
	}
	return nil
}

func generateDeclaredProjection(projection loadedProjection, catalogs [len(releaseCatalogPaths)][]byte) ([]target, [len(releaseCatalogPaths)][]byte, error) {
	generatedManifest, err := projection.generateManifest(projection.manifest)
	if err != nil {
		return nil, catalogs, atProjectionPath(projection.registration.manifestPath, err)
	}
	generatedCatalogs := catalogs
	for index, data := range catalogs {
		generatedCatalogs[index], err = projection.generateCatalog(data)
		if err != nil {
			return nil, catalogs, atProjectionPath(releaseCatalogPaths[index], err)
		}
	}
	if err := projection.checkManifest(generatedManifest); err != nil {
		return nil, catalogs, atProjectionPath(projection.registration.manifestPath, fmt.Errorf("generated output invalid: %w", err))
	}
	targets := []target{{
		path: filepath.Join(projection.registration.manifestPath),
		data: generatedManifest,
	}}
	if projection.registration.readmePath != "" {
		targets = append(targets, target{
			path: projection.registration.readmePath,
			data: projection.readme,
		})
	}
	return targets, generatedCatalogs, nil
}

func loadSerenaProjection(root string) (loadedProjection, error) {
	descriptor, err := LoadDescriptor(filepath.Join(root, "servers", "serena", "release.yaml"))
	if err != nil {
		return loadedProjection{}, err
	}
	return loadedProjection{
		checkManifest:    func(data []byte) error { return checkManifest(data, descriptor) },
		generateManifest: func(data []byte) ([]byte, error) { return generateManifest(data, descriptor) },
		checkCatalog:     func(data []byte) error { return checkCatalog(data, descriptor) },
		generateCatalog:  func(data []byte) ([]byte, error) { return generateCatalog(data, descriptor) },
	}, nil
}

func loadFetchProjection(root string) (loadedProjection, error) {
	descriptor, err := loadFetchDescriptor(filepath.Join(root, "servers", "fetch", "release.yaml"))
	if err != nil {
		return loadedProjection{}, err
	}
	return loadedProjection{
		readme: renderFetchReadme(descriptor),
		checkManifest: func(data []byte) error {
			return checkRuntimeManifest(data, descriptor.ServerID, descriptor.Runtime.Command, "stdio-bridge", descriptor.Runtime.Args)
		},
		generateManifest: func(data []byte) ([]byte, error) {
			return generateRuntimeManifest(data, descriptor.ServerID, descriptor.Runtime.Command, "stdio-bridge", descriptor.Runtime.Args)
		},
		checkCatalog: func(data []byte) error {
			return checkPackageCatalog(data, descriptor.ServerID, descriptor.Runtime.Command, descriptor.Runtime.Args, descriptor.readmeURL())
		},
		generateCatalog: func(data []byte) ([]byte, error) {
			return generatePackageCatalog(data, descriptor.ServerID, descriptor.Runtime.Command, descriptor.Runtime.Args, descriptor.readmeURL())
		},
	}, nil
}

func loadSequentialProjection(root string) (loadedProjection, error) {
	descriptor, err := loadSequentialDescriptor(filepath.Join(root, "servers", "sequential-thinking", "release.yaml"))
	if err != nil {
		return loadedProjection{}, err
	}
	return loadedProjection{
		readme: renderSequentialReadme(descriptor),
		checkManifest: func(data []byte) error {
			return checkRuntimeManifest(data, descriptor.ServerID, descriptor.Runtime.Command, "stdio-bridge", descriptor.Runtime.Args)
		},
		generateManifest: func(data []byte) ([]byte, error) {
			return generateRuntimeManifest(data, descriptor.ServerID, descriptor.Runtime.Command, "stdio-bridge", descriptor.Runtime.Args)
		},
		checkCatalog: func(data []byte) error {
			return checkPackageCatalog(data, descriptor.ServerID, descriptor.Runtime.Command, descriptor.Runtime.Args, descriptor.readmeURL())
		},
		generateCatalog: func(data []byte) ([]byte, error) {
			return generatePackageCatalog(data, descriptor.ServerID, descriptor.Runtime.Command, descriptor.Runtime.Args, descriptor.readmeURL())
		},
	}, nil
}

// Check is the drift gate for every server in the ordered release registry.
func Check(root string) error {
	if err := validateDeclaredReleaseProjections(); err != nil {
		return drift("declared release projection registry", err)
	}
	projections, err := loadDeclaredProjections(root)
	if err != nil {
		return wrapProjectionPath(err, drift)
	}
	catalogs, err := readReleaseCatalogs(root)
	if err != nil {
		return wrapProjectionPath(err, drift)
	}
	for _, projection := range projections {
		if err := checkDeclaredProjection(root, projection, catalogs); err != nil {
			return wrapProjectionPath(err, drift)
		}
	}
	return nil
}

// Generate derives every replacement in memory before publishing any target.
func Generate(root string) error {
	if err := validateDeclaredReleaseProjections(); err != nil {
		return generationError("declared release projection registry", err)
	}
	projections, err := loadDeclaredProjections(root)
	if err != nil {
		return wrapProjectionPath(err, generationError)
	}
	catalogs, err := readReleaseCatalogs(root)
	if err != nil {
		return wrapProjectionPath(err, generationError)
	}
	targets := make([]target, 0, len(projections)*2+len(releaseCatalogPaths))
	for _, projection := range projections {
		projectionTargets, generatedCatalogs, generateErr := generateDeclaredProjection(projection, catalogs)
		if generateErr != nil {
			return wrapProjectionPath(generateErr, generationError)
		}
		for index := range projectionTargets {
			projectionTargets[index].path = filepath.Join(root, filepath.FromSlash(projectionTargets[index].path))
		}
		targets = append(targets, projectionTargets...)
		catalogs = generatedCatalogs
	}
	for _, projection := range projections {
		for index, data := range catalogs {
			if err := projection.checkCatalog(data); err != nil {
				return generationError(releaseCatalogPaths[index], fmt.Errorf("generated output invalid: %w", err))
			}
		}
	}
	for index, relative := range releaseCatalogPaths {
		targets = append(targets, target{
			path: filepath.Join(root, filepath.FromSlash(relative)),
			data: catalogs[index],
		})
	}
	return publishTargets(targets)
}

func drift(target string, err error) error {
	return fmt.Errorf("RELEASE_PROJECTION_DRIFT: %s: %w", target, err)
}

func generationError(target string, err error) error {
	return fmt.Errorf("RELEASE_GENERATION_FAILED: %s: %w", target, err)
}

func expectedArgs(descriptor Descriptor) []string {
	return []string{"--from", descriptor.SourceArgument(), "serena", "start-mcp-server", "--transport", "streamable-http"}
}

func fetchExpectedArgs(descriptor FetchDescriptor) []string {
	return []string{"--with", "mcp==" + descriptor.Artifacts.MCPVersion, "mcp-server-fetch@" + descriptor.Version}
}

func sequentialExpectedArgs(descriptor SequentialDescriptor) []string {
	return []string{"-y", "@modelcontextprotocol/server-sequential-thinking@" + descriptor.Version}
}

func checkRuntimeManifest(data []byte, serverID, command, transport string, args []string) error {
	root, err := parseYAMLDocument(data)
	if err != nil {
		return err
	}
	gotID, err := mappingString(root, "name")
	if err != nil {
		return err
	}
	gotCommand, err := mappingString(root, "command")
	if err != nil {
		return err
	}
	gotTransport, err := mappingString(root, "transport")
	if err != nil {
		return err
	}
	gotArgs, err := mappingStrings(root, "base_args")
	if err != nil {
		return err
	}
	if gotID != serverID || gotCommand != command || gotTransport != transport || !equalStrings(gotArgs, args) {
		return fmt.Errorf("runtime projection mismatch for %s", serverID)
	}
	return nil
}

func generateRuntimeManifest(data []byte, serverID, command, transport string, args []string) ([]byte, error) {
	root, err := parseYAMLDocument(data)
	if err != nil {
		return nil, err
	}
	gotID, err := mappingString(root, "name")
	if err != nil {
		return nil, err
	}
	gotCommand, err := mappingString(root, "command")
	if err != nil {
		return nil, err
	}
	gotTransport, err := mappingString(root, "transport")
	if err != nil {
		return nil, err
	}
	oldArgs, err := mappingStrings(root, "base_args")
	if err != nil {
		return nil, err
	}
	if gotID != serverID || gotCommand != command || gotTransport != transport || len(oldArgs) != len(args) {
		return nil, fmt.Errorf("unexpected runtime projection shape for %s", serverID)
	}
	output := append([]byte(nil), data...)
	for index := range oldArgs {
		output, err = replaceTextExactlyOwned(output, oldArgs[index], args[index], serverID)
		if err != nil {
			return nil, err
		}
	}
	return output, nil
}

func renderFetchReadme(descriptor FetchDescriptor) []byte {
	return []byte(fmt.Sprintf(`# Fetch MCP server

Release metadata in this file is generated from [release.yaml](release.yaml).

- Runtime command: `+"`%s %s`"+`
- Immutable upstream README: %s
- mcp-server-fetch %s wheel SHA-256: `+"`%s`"+`
- mcp %s wheel SHA-256: `+"`%s`"+`

The runtime command selects the exact MCP release recorded above.

## Terms and Abbreviations

- MCP: Model Context Protocol.
- SHA-256: Secure Hash Algorithm 256-bit artifact digest.
`, descriptor.Runtime.Command, strings.Join(descriptor.Runtime.Args, " "), descriptor.readmeURL(), descriptor.Version,
		descriptor.Artifacts.FetchWheelSHA256, descriptor.Artifacts.MCPVersion, descriptor.Artifacts.MCPWheelSHA256))
}

func renderSequentialReadme(descriptor SequentialDescriptor) []byte {
	var readme strings.Builder
	fmt.Fprintf(&readme, `# Sequential Thinking MCP server

Release metadata in this file is generated from [release.yaml](release.yaml).

- Runtime command: `+"`%s %s`"+`
- Immutable upstream README: %s
- Package integrity: `+"`%s`"+`
`, descriptor.Runtime.Command, strings.Join(descriptor.Runtime.Args, " "), descriptor.readmeURL(), descriptor.Artifacts.PackageIntegrity)
	if descriptor.SchemaVersion == 2 {
		fmt.Fprintf(&readme, `- Published Node.js engine: `+"`%s`"+`
- Upstream monorepo npm lockfile at the source commit: v%d, %d lockfile entries, SHA-256 `+"`%s`"+`
- Resolved protocol dependencies for workspace `+"`%s`"+`:
`, descriptor.Artifacts.NodeEngine, descriptor.Lock.LockfileVersion, descriptor.Lock.PackageCount,
			descriptor.Lock.SHA256, filepath.ToSlash(filepath.Dir(descriptor.ReadmePath)))
		for _, name := range slices.Sorted(maps.Keys(descriptor.Lock.ResolvedProtocolDependencies)) {
			pin := descriptor.Lock.ResolvedProtocolDependencies[name]
			fmt.Fprintf(&readme, "  - `%s@%s`, integrity `%s`\n", name, pin.Version, pin.Integrity)
		}
		readme.WriteString(`
npx launches the pinned outer release and resolves its dependencies at launch.

## Terms and Abbreviations

- MCP: Model Context Protocol.
- npm: Node Package Manager.
- npx: npm package runner.
- SHA-256 and SHA-512: Secure Hash Algorithm artifact digests.
`)
		return []byte(readme.String())
	}
	fmt.Fprintf(&readme, `- npm lockfile v%d closure: %d packages, SHA-256 `+"`%s`"+`
- Resolved @modelcontextprotocol/sdk %s integrity: `+"`%s`"+`

The exact lock evidence records the isolated candidate closure used by the
release probe; runtime still launches the exact outer package through npx.

## Terms and Abbreviations

- MCP: Model Context Protocol.
- npm: Node Package Manager.
- SDK: Software Development Kit.
- SHA-256: Secure Hash Algorithm 256-bit artifact digest.
`, descriptor.Lock.LockfileVersion, descriptor.Lock.PackageCount, descriptor.Lock.SHA256,
		descriptor.Lock.ResolvedSDK.Version, descriptor.Lock.ResolvedSDK.Integrity)
	return []byte(readme.String())
}

func checkManifest(data []byte, descriptor Descriptor) error {
	root, err := parseYAMLDocument(data)
	if err != nil {
		return err
	}
	if err := validateManifestShape(root); err != nil {
		return err
	}
	args, err := mappingStrings(root, "base_args")
	if err != nil {
		return err
	}
	if !equalStrings(args, expectedArgs(descriptor)) {
		return fmt.Errorf("base_args does not equal descriptor-derived Serena command")
	}
	comments := pinnedCommentPattern.FindAllString(string(data), -1)
	for _, comment := range comments {
		if comment != "pinned serena commit "+descriptor.Commit {
			return fmt.Errorf("live Serena revision comment = %q", comment)
		}
	}
	if strings.Count(string(data), "git+"+repositoryURL+"@") != 1 {
		return fmt.Errorf("expected exactly one Serena Git source argument")
	}
	return nil
}

func generateManifest(data []byte, descriptor Descriptor) ([]byte, error) {
	root, err := parseYAMLDocument(data)
	if err != nil {
		return nil, err
	}
	if err := validateManifestShape(root); err != nil {
		return nil, err
	}
	args, err := mappingStrings(root, "base_args")
	if err != nil {
		return nil, err
	}
	output, err := replaceTextExactlyOwned(data, args[1], descriptor.SourceArgument(), "Serena")
	if err != nil {
		return nil, err
	}
	comments := pinnedCommentPattern.FindAllString(string(output), -1)
	if len(comments) > 1 {
		return nil, errors.New("multiple live Serena revision comments")
	}
	if len(comments) == 1 {
		output, err = replaceTextExactlyOwned(output, comments[0], "pinned serena commit "+descriptor.Commit, "Serena")
		if err != nil {
			return nil, err
		}
	}
	return output, nil
}

func parseYAMLDocument(data []byte) (*yaml.Node, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("manifest must contain one YAML mapping document")
	}
	return document.Content[0], nil
}

func validateManifestShape(root *yaml.Node) error {
	name, err := mappingString(root, "name")
	if err != nil {
		return err
	}
	if name != "serena" {
		return fmt.Errorf("name = %q, want serena", name)
	}
	command, err := mappingString(root, "command")
	if err != nil {
		return err
	}
	transport, err := mappingString(root, "transport")
	if err != nil {
		return err
	}
	if command != "uvx" || transport != "native-http" {
		return fmt.Errorf("unexpected Serena command shape command=%q transport=%q", command, transport)
	}
	args, err := mappingStrings(root, "base_args")
	if err != nil {
		return err
	}
	if len(args) != 6 || args[0] != "--from" || !strings.HasPrefix(args[1], "git+"+repositoryURL+"@") ||
		args[2] != "serena" || args[3] != "start-mcp-server" || args[4] != "--transport" || args[5] != "streamable-http" {
		return errors.New("unexpected Serena base_args shape")
	}
	return nil
}

func mappingValue(root *yaml.Node, key string) (*yaml.Node, error) {
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("expected YAML mapping")
	}
	var result *yaml.Node
	for index := 0; index < len(root.Content); index += 2 {
		if root.Content[index].Value != key {
			continue
		}
		if result != nil {
			return nil, fmt.Errorf("duplicate YAML key %q", key)
		}
		result = root.Content[index+1]
	}
	if result == nil {
		return nil, fmt.Errorf("missing YAML key %q", key)
	}
	return result, nil
}

func mappingString(root *yaml.Node, key string) (string, error) {
	node, err := mappingValue(root, key)
	if err != nil {
		return "", err
	}
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", fmt.Errorf("YAML key %q must be a string", key)
	}
	return node.Value, nil
}

func mappingStrings(root *yaml.Node, key string) ([]string, error) {
	node, err := mappingValue(root, key)
	if err != nil {
		return nil, err
	}
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("YAML key %q must be a sequence", key)
	}
	values := make([]string, len(node.Content))
	for index, item := range node.Content {
		if item.Kind != yaml.ScalarNode || item.Tag != "!!str" {
			return nil, fmt.Errorf("YAML key %q has a non-string argument", key)
		}
		values[index] = item.Value
	}
	return values, nil
}

type catalogDocument struct {
	Entries []json.RawMessage `json:"entries"`
}

type catalogEntry struct {
	ID        string   `json:"id"`
	ReadmeURL string   `json:"readme_url"`
	Transport string   `json:"transport"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
}

var catalogArgsPattern = regexp.MustCompile(`(?s)"args"\s*:\s*\[[^\]]*\]`)

func checkPackageCatalog(data []byte, serverID, command string, args []string, readmeURL string) error {
	row, entry, err := catalogRow(data, serverID)
	if err != nil {
		return err
	}
	if bytes.Contains(row, []byte("\r")) {
		return fmt.Errorf("%s catalog row must use LF line endings", serverID)
	}
	if entry.Command != command || entry.Transport != "stdio" || !equalStrings(entry.Args, args) || entry.ReadmeURL != readmeURL {
		return fmt.Errorf("catalog projection mismatch for %s", serverID)
	}
	return nil
}

func generatePackageCatalog(data []byte, serverID, command string, args []string, readmeURL string) ([]byte, error) {
	row, entry, err := catalogRow(data, serverID)
	if err != nil {
		return nil, err
	}
	if entry.Command != command || entry.Transport != "stdio" {
		return nil, fmt.Errorf("unexpected catalog command shape for %s", serverID)
	}
	updated := append([]byte(nil), row...)
	updated, err = replaceJSONStringExactlyOwned(updated, entry.ReadmeURL, readmeURL, serverID)
	if err != nil {
		return nil, err
	}
	match := catalogArgsPattern.FindIndex(updated)
	if match == nil {
		return nil, fmt.Errorf("missing args field in %s catalog row", serverID)
	}
	matched := updated[match[0]:match[1]]
	arrayOffset := bytes.IndexByte(matched, '[')
	if arrayOffset < 0 {
		return nil, fmt.Errorf("missing args array in %s catalog row", serverID)
	}
	lineStart := bytes.LastIndex(updated[:match[0]], []byte("\n")) + 1
	indent := append([]byte(nil), updated[lineStart:match[0]]...)
	formattedArgs := formatJSONArray(args, indent)
	replacement := append(append([]byte(nil), matched[:arrayOffset]...), formattedArgs...)
	updated = append(append(append([]byte(nil), updated[:match[0]]...), replacement...), updated[match[1]:]...)
	if bytes.Count(data, row) != 1 {
		return nil, fmt.Errorf("%s catalog row is not uniquely addressable", serverID)
	}
	return bytes.Replace(data, row, normalizeCatalogLineEndings(updated), 1), nil
}

func catalogRow(data []byte, serverID string) ([]byte, catalogEntry, error) {
	var document catalogDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, catalogEntry{}, err
	}
	var row []byte
	var entry catalogEntry
	for _, raw := range document.Entries {
		var candidate catalogEntry
		if err := json.Unmarshal(raw, &candidate); err != nil {
			return nil, catalogEntry{}, err
		}
		if candidate.ID != serverID {
			continue
		}
		if row != nil {
			return nil, catalogEntry{}, fmt.Errorf("duplicate %s catalog rows", serverID)
		}
		row = raw
		entry = candidate
	}
	if row == nil {
		return nil, catalogEntry{}, fmt.Errorf("missing %s catalog row", serverID)
	}
	return row, entry, nil
}

func formatJSONArray(values []string, indent []byte) []byte {
	var output bytes.Buffer
	output.WriteString("[\n")
	for index, value := range values {
		output.Write(indent)
		output.WriteString("  ")
		output.WriteString(strconv.Quote(value))
		if index+1 < len(values) {
			output.WriteByte(',')
		}
		output.WriteByte('\n')
	}
	output.Write(indent)
	output.WriteByte(']')
	return output.Bytes()
}

func replaceJSONStringExactlyOwned(data []byte, oldValue, newValue, owner string) ([]byte, error) {
	if oldValue == newValue {
		return data, nil
	}
	oldJSON := []byte(strconv.Quote(oldValue))
	if bytes.Count(data, oldJSON) != 1 {
		return nil, fmt.Errorf("expected one JSON value %q in %s row", oldValue, owner)
	}
	return bytes.Replace(data, oldJSON, []byte(strconv.Quote(newValue)), 1), nil
}

func replaceTextExactlyOwned(data []byte, oldValue, newValue, owner string) ([]byte, error) {
	if oldValue == newValue {
		return data, nil
	}
	old := []byte(oldValue)
	if bytes.Count(data, old) != 1 {
		return nil, fmt.Errorf("expected one %s-owned value %q", owner, oldValue)
	}
	return bytes.Replace(data, old, []byte(newValue), 1), nil
}

func checkCatalog(data []byte, descriptor Descriptor) error {
	row, entry, err := catalogRow(data, "serena")
	if err != nil {
		return err
	}
	if bytes.Contains(row, []byte("\r")) {
		return errors.New("Serena catalog row must use LF line endings")
	}
	if err := validateCatalogShape(entry); err != nil {
		return err
	}
	if !equalStrings(entry.Args, expectedArgs(descriptor)) {
		return errors.New("args does not equal descriptor-derived Serena command")
	}
	if entry.ReadmeURL != descriptor.readmeURL() {
		return fmt.Errorf("readme_url = %q, want descriptor-pinned URL", entry.ReadmeURL)
	}
	if strings.Count(string(data), "git+"+repositoryURL+"@") != 1 || bytes.Count(data, row) != 1 {
		return errors.New("catalog must contain exactly one Serena Git source row")
	}
	return nil
}

func generateCatalog(data []byte, descriptor Descriptor) ([]byte, error) {
	row, entry, err := catalogRow(data, "serena")
	if err != nil {
		return nil, err
	}
	if err := validateCatalogShape(entry); err != nil {
		return nil, err
	}
	updatedRow := append([]byte(nil), row...)
	updatedRow, err = replaceJSONStringExactlyOwned(updatedRow, entry.Args[1], descriptor.SourceArgument(), "Serena")
	if err != nil {
		return nil, err
	}
	updatedRow, err = replaceJSONStringExactlyOwned(updatedRow, entry.ReadmeURL, descriptor.readmeURL(), "Serena")
	if err != nil {
		return nil, err
	}
	if bytes.Count(data, row) != 1 {
		return nil, errors.New("Serena catalog row is not uniquely addressable")
	}
	return bytes.Replace(data, row, normalizeCatalogLineEndings(updatedRow), 1), nil
}

func normalizeCatalogLineEndings(data []byte) []byte {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
}

func validateCatalogShape(entry catalogEntry) error {
	if entry.Command != "uvx" || entry.Transport != "native-http" {
		return fmt.Errorf("unexpected Serena command shape command=%q transport=%q", entry.Command, entry.Transport)
	}
	if len(entry.Args) != 6 || entry.Args[0] != "--from" || !strings.HasPrefix(entry.Args[1], "git+"+repositoryURL+"@") ||
		entry.Args[2] != "serena" || entry.Args[3] != "start-mcp-server" || entry.Args[4] != "--transport" || entry.Args[5] != "streamable-http" {
		return errors.New("unexpected Serena args shape")
	}
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func publishTargets(targets []target) error {
	return publishTargetsWithOps(targets, systemPublishOps())
}

func publishTargetsWithOps(targets []target, ops publishOps) error {
	staged := make([]stagedTarget, 0, len(targets))
	for _, candidate := range targets {
		preimage, err := ops.readFile(candidate.path)
		existed := true
		if errors.Is(err, os.ErrNotExist) {
			existed = false
			preimage = nil
		} else if err != nil {
			return errors.Join(
				fmt.Errorf("read preimage %q: %w", candidate.path, err),
				cleanupStaged(staged, ops),
			)
		}
		if existed && bytes.Equal(candidate.data, preimage) {
			continue
		}
		file, err := ops.createTemp(filepath.Dir(candidate.path), "."+filepath.Base(candidate.path)+".releasegen-*")
		if err != nil {
			return errors.Join(
				fmt.Errorf("stage temporary file for %q: %w", candidate.path, err),
				cleanupStaged(staged, ops),
			)
		}
		name := file.Name()
		err = ops.writeTemp(file, candidate.data)
		if err != nil {
			return errors.Join(
				fmt.Errorf("write temporary file for %q: %w", candidate.path, err),
				cleanupTemporary(name, ops),
				cleanupStaged(staged, ops),
			)
		}
		staged = append(staged, stagedTarget{target: candidate.path, temp: name, preimage: preimage, existed: existed})
	}
	for index, candidate := range staged {
		if err := ops.rename(candidate.temp, candidate.target); err != nil {
			rollbackErr := restorePublished(staged[:index], ops)
			cleanupErr := cleanupStaged(staged, ops)
			return errors.Join(
				fmt.Errorf("replace %q: %w", candidate.target, err),
				rollbackErr,
				cleanupErr,
			)
		}
	}
	return nil
}

func systemPublishOps() publishOps {
	return publishOps{
		readFile:   os.ReadFile,
		createTemp: os.CreateTemp,
		writeTemp:  writeAndSyncTemp,
		rename:     os.Rename,
		remove:     os.Remove,
	}
}

func writeAndSyncTemp(file *os.File, data []byte) (err error) {
	written, writeErr := file.Write(data)
	if writeErr != nil {
		err = writeErr
	} else if written != len(data) {
		err = fmt.Errorf("short temporary-file write: wrote %d of %d bytes", written, len(data))
	} else if syncErr := file.Sync(); syncErr != nil {
		err = syncErr
	}
	return errors.Join(err, file.Close())
}

func restorePublished(published []stagedTarget, ops publishOps) error {
	var restoreErrors []error
	for index := len(published) - 1; index >= 0; index-- {
		candidate := published[index]
		if !candidate.existed {
			if err := ops.remove(candidate.target); err != nil && !errors.Is(err, os.ErrNotExist) {
				restoreErrors = append(restoreErrors, fmt.Errorf("remove newly-created projection %q: %w", candidate.target, err))
			}
			continue
		}
		file, err := ops.createTemp(filepath.Dir(candidate.target), "."+filepath.Base(candidate.target)+".releasegen-rollback-*")
		if err != nil {
			restoreErrors = append(restoreErrors, fmt.Errorf("stage restore %q: %w", candidate.target, err))
			continue
		}
		temp := file.Name()
		if err := ops.writeTemp(file, candidate.preimage); err != nil {
			restoreErrors = append(restoreErrors, errors.Join(
				fmt.Errorf("write restore %q: %w", candidate.target, err),
				cleanupTemporary(temp, ops),
			))
			continue
		}
		if err := ops.rename(temp, candidate.target); err != nil {
			restoreErrors = append(restoreErrors, errors.Join(
				fmt.Errorf("replace restore %q: %w", candidate.target, err),
				cleanupTemporary(temp, ops),
			))
		}
	}
	return errors.Join(restoreErrors...)
}

func cleanupStaged(staged []stagedTarget, ops publishOps) error {
	var cleanupErrors []error
	for _, candidate := range staged {
		cleanupErrors = append(cleanupErrors, cleanupTemporary(candidate.temp, ops))
	}
	return errors.Join(cleanupErrors...)
}

func cleanupTemporary(path string, ops publishOps) error {
	err := ops.remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("remove temporary file %q: %w", path, err)
}

func main() {
	root := flag.String("root", ".", "repository root")
	check := flag.Bool("check", false, "fail when release projections drift from their descriptors")
	flag.Parse()
	var err error
	if *check {
		err = Check(*root)
	} else {
		err = Generate(*root)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *check {
		fmt.Println("Release projections are synchronized.")
	} else {
		fmt.Println("Release projections synchronized.")
	}
}
