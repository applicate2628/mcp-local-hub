package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"

	"mcp-local-hub/internal/config"
)

type projectServiceRecipe struct {
	service       ProjectServiceKind
	dataset       string
	profileSHA256 string
}

func (projectServiceRecipe) String() string                  { return "project-service recipe (private)" }
func (p projectServiceRecipe) GoString() string              { return p.String() }
func (p projectServiceRecipe) Format(s fmt.State, verb rune) { fmt.Fprint(s, p.String()) }

func sameProjectServicePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
func projectServiceProviderFiles(ctx context.Context, input ProjectServiceProviderInputRead, reads []ProjectServiceProviderFileRead) (map[string]ProjectServiceProviderFileRead, error) {
	byPath := make(map[string]ProjectServiceProviderFileRead, len(reads))
	for _, read := range reads {
		if err := read.validate(ctx); err != nil {
			return nil, err
		}
		path := read.Observation.path
		if _, exists := byPath[path]; exists {
			return nil, errors.New("PROJECT_SERVICE_PROVIDER_DUPLICATE_READ")
		}
		byPath[path] = read
	}
	// Original selection reads returned by the producer remain the very same
	// original observations, including when further declared reads were needed.
	for _, original := range input.Reads {
		if err := original.validate(ctx); err != nil {
			return nil, err
		}
		actual, ok := byPath[original.Observation.path]
		if !ok || actual.Observation != original.Observation || !bytes.Equal(actual.Raw, original.Raw) {
			return nil, errors.New("PROJECT_SERVICE_PROVIDER_SELECTION_READ_CHANGED")
		}
	}
	byRole := make(map[string]ProjectServiceProviderFileRead, len(input.Files))
	declared := make(map[string]bool, len(input.Files))
	for _, file := range input.Files {
		if file.Role == "" || !filepath.IsAbs(file.Path) {
			return nil, errors.New("PROJECT_SERVICE_PROVIDER_DECLARATION_INCOMPLETE")
		}
		path, err := stateReadRequestPath(file.Path)
		if err != nil {
			return nil, err
		}
		read, ok := byPath[path]
		if !ok || declared[path] {
			return nil, errors.New("PROJECT_SERVICE_PROVIDER_DECLARED_READ_UNAVAILABLE")
		}
		if _, exists := byRole[file.Role]; exists {
			return nil, errors.New("PROJECT_SERVICE_PROVIDER_ROLE_AMBIGUOUS")
		}
		declared[path], byRole[file.Role] = true, read
	}
	if len(declared) != len(byPath) {
		return nil, errors.New("PROJECT_SERVICE_PROVIDER_UNDECLARED_CONTRIBUTOR")
	}
	return byRole, nil
}
func requireProjectServiceRoles(files map[string]ProjectServiceProviderFileRead, roles ...string) error {
	if len(files) != len(roles) {
		return errors.New("PROJECT_SERVICE_PROVIDER_CONTRIBUTORS_UNSUPPORTED")
	}
	for _, role := range roles {
		if _, ok := files[role]; !ok {
			return errors.New("PROJECT_SERVICE_PROVIDER_CONTRIBUTOR_MISSING")
		}
	}
	return nil
}
func projectServiceEmptyEnvironment(p ProjectServicePreparedLaunch, names ...string) error {
	for _, name := range names {
		if present, matches := p.EnvironmentValueMatches(name, ""); present && !matches {
			return errors.New("PROJECT_SERVICE_PROVIDER_LOADER_CONTEXT_UNQUALIFIED")
		}
	}
	return nil
}
func projectServiceMetadataValue(raw []byte, key string) string {
	for _, line := range strings.Split(string(raw), string(byte(10))) {
		if strings.HasPrefix(line, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+":"))
		}
	}
	return ""
}

// These two projections encode the frozen provider cuts named by boundary491F.
// Source-cut checks follow an original command/module/metadata/read join; they
// do not select a service by hash or create a recipe from caller DATA.
func qualifyGraphifyProjectService(p ProjectServiceExecutionProfile, prepared ProjectServicePreparedLaunch, files map[string]ProjectServiceProviderFileRead) (string, error) {
	if err := requireProjectServiceRoles(files, ProjectServiceProviderLauncher, ProjectServiceProviderInterpreter, ProjectServiceProviderVenv, ProjectServiceProviderEntryPoints, ProjectServiceProviderMetadata, ProjectServiceProviderEntry, ProjectServiceProviderPaths); err != nil {
		return "", err
	}
	launcher, interpreter := files[ProjectServiceProviderLauncher], files[ProjectServiceProviderInterpreter]
	if !sameProjectServicePath(p.CommandPath, launcher.Observation.path) {
		return "", errors.New("PROJECT_SERVICE_GRAPHIFY_SELECTED_LAUNCHER_CHANGED")
	}
	if err := projectServiceEmptyEnvironment(prepared, "PYTHONHOME", "PYTHONPATH"); err != nil {
		return "", err
	}
	// Verify the selected known source cut before ZIP decompression. Unknown
	// compressed input is refused; no arbitrary expansion ceiling is introduced.
	if strings.ToUpper(ManifestHashContent(launcher.Raw)) != "F9FC52BE9AA3BB13B3D06DF0B7870CC60594DB850DABA8C7B7F096EC9112D298" ||
		strings.ToUpper(ManifestHashContent(files[ProjectServiceProviderEntry].Raw)) != "E1C99599A6DEB978DADBB74484F73F57E02EE3816695DA44E033F994AAA53D17" ||
		strings.ToUpper(ManifestHashContent(files[ProjectServiceProviderPaths].Raw)) != "D19D1B5FF3BF061081B5FB4FDB8008CEAFE09D17782C34A3A74FFB6EEAD78023" {
		return "", errors.New("PROJECT_SERVICE_GRAPHIFY_SOURCE_CUT_CHANGED")
	}
	archive, err := zip.NewReader(bytes.NewReader(launcher.Raw), int64(len(launcher.Raw)))
	if err != nil {
		return "", errors.New("PROJECT_SERVICE_GRAPHIFY_LAUNCHER_ZIP_UNQUALIFIED")
	}
	var entry []byte
	for _, member := range archive.File {
		if member.Name != "__main__.py" {
			continue
		}
		if entry != nil {
			return "", errors.New("PROJECT_SERVICE_GRAPHIFY_ENTRY_AMBIGUOUS")
		}
		reader, openErr := member.Open()
		if openErr != nil {
			return "", openErr
		}
		raw, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return "", err
		}
		entry = raw
	}
	if !bytes.Contains(entry, []byte("from graphify.serve import _main")) {
		return "", errors.New("PROJECT_SERVICE_GRAPHIFY_MCP_ENTRY_UNQUALIFIED")
	}
	zipStart := bytes.Index(launcher.Raw, []byte{'P', 'K', 3, 4})
	if zipStart < 0 {
		return "", errors.New("PROJECT_SERVICE_GRAPHIFY_LAUNCHER_CONTEXT_UNAVAILABLE")
	}
	shebang := bytes.LastIndex(launcher.Raw[:zipStart], []byte("#!"))
	if shebang < 0 {
		return "", errors.New("PROJECT_SERVICE_GRAPHIFY_INTERPRETER_UNAVAILABLE")
	}
	line := strings.SplitN(string(launcher.Raw[shebang+2:zipStart]), string(byte(10)), 2)[0]
	selectedInterpreter := strings.Trim(strings.TrimSpace(line), string(byte(34)))
	if !filepath.IsAbs(selectedInterpreter) || !sameProjectServicePath(selectedInterpreter, interpreter.Observation.path) {
		return "", errors.New("PROJECT_SERVICE_GRAPHIFY_SELECTED_INTERPRETER_CHANGED")
	}
	venv := filepath.Dir(filepath.Dir(interpreter.Observation.path))
	if !sameProjectServicePath(files[ProjectServiceProviderVenv].Observation.path, filepath.Join(venv, "pyvenv.cfg")) ||
		!sameProjectServicePath(files[ProjectServiceProviderEntry].Observation.path, filepath.Join(venv, "Lib", "site-packages", "graphify", "serve.py")) ||
		!sameProjectServicePath(files[ProjectServiceProviderPaths].Observation.path, filepath.Join(venv, "Lib", "site-packages", "graphify", "paths.py")) ||
		!sameProjectServicePath(files[ProjectServiceProviderEntryPoints].Observation.path, filepath.Join(venv, "Lib", "site-packages", "graphifyy-0.9.78.dist-info", "entry_points.txt")) ||
		!sameProjectServicePath(files[ProjectServiceProviderMetadata].Observation.path, filepath.Join(venv, "Lib", "site-packages", "graphifyy-0.9.78.dist-info", "METADATA")) {
		return "", errors.New("PROJECT_SERVICE_GRAPHIFY_SELECTED_PACKAGE_CHANGED")
	}
	metadata := files[ProjectServiceProviderMetadata].Raw
	if projectServiceMetadataValue(metadata, "Name") != "graphifyy" || projectServiceMetadataValue(metadata, "Version") != "0.9.78" {
		return "", errors.New("PROJECT_SERVICE_GRAPHIFY_DEFINITION_UNSUPPORTED")
	}
	entryPoints := strings.ReplaceAll(string(files[ProjectServiceProviderEntryPoints].Raw), " ", "")
	if !strings.Contains(entryPoints, "graphify-mcp=graphify.serve:_main") {
		return "", errors.New("PROJECT_SERVICE_GRAPHIFY_PACKAGE_ENTRY_CHANGED")
	}
	graphFlag, positional := "", ""
	positionalOnly := false
	for index := 0; index < len(p.Arguments); index++ {
		arg := p.Arguments[index]
		switch {
		case arg == "--" && !positionalOnly:
			positionalOnly = true
		case arg == "--graph" && !positionalOnly:
			index++
			if index >= len(p.Arguments) || p.Arguments[index] == "" {
				return "", errors.New("PROJECT_SERVICE_GRAPHIFY_EXPLICIT_GRAPH_REQUIRED")
			}
			if strings.HasPrefix(p.Arguments[index], "-") {
				return "", errors.New("PROJECT_SERVICE_GRAPHIFY_OPTION_VALUE_QUALIFICATION_REQUIRED")
			}
			graphFlag = p.Arguments[index]
		case strings.HasPrefix(arg, "--graph=") && !positionalOnly:
			graphFlag = strings.TrimPrefix(arg, "--graph=")
			if graphFlag == "" {
				return "", errors.New("PROJECT_SERVICE_GRAPHIFY_EXPLICIT_GRAPH_REQUIRED")
			}
		case strings.HasPrefix(arg, "-") && arg != "-" && !positionalOnly:
			return "", errors.New("PROJECT_SERVICE_GRAPHIFY_OPTION_QUALIFICATION_REQUIRED")
		default:
			if arg == "" || positional != "" {
				return "", errors.New("PROJECT_SERVICE_GRAPHIFY_POSITIONAL_INVALID")
			}
			positional = arg
		}
	}
	if graphFlag != "" {
		return graphFlag, nil
	}
	if positional != "" {
		return positional, nil
	}
	return "", errors.New("PROJECT_SERVICE_GRAPHIFY_DEFAULT_QUALIFICATION_REQUIRED")
}
func projectServiceQualifiedPath(r *ProjectServiceLaunchPreparationRequest, p ProjectServiceExecutionProfile, operand string) (string, error) {
	if filepath.IsAbs(operand) {
		return filepath.Clean(operand), nil
	}
	// Drive-relative/rooted forms need additional original native drive context;
	// they cannot be converted into an ordinary CWD-relative file by Join.
	if filepath.VolumeName(operand) != "" || strings.HasPrefix(operand, "/") || strings.HasPrefix(operand, string(byte(92))) {
		return "", errors.New("PROJECT_SERVICE_NATIVE_PATH_CONTEXT_REQUIRED")
	}
	if r.sourceKind == ProjectServiceConfiguredStdio {
		cwd := r.launch.ConfiguredCWD()
		if cwd == nil || !filepath.IsAbs(*cwd) || !sameProjectServicePath(*cwd, p.WorkingDirectory) {
			return "", errors.New("PROJECT_SERVICE_RELATIVE_DATASET_CONTEXT_REQUIRED")
		}
	}
	manifest, err := config.ParseManifest(bytes.NewReader(r.manifest))
	if err != nil {
		return "", err
	}
	if len(manifest.Daemons) != 1 || !filepath.IsAbs(manifest.Daemons[0].Cwd) || !sameProjectServicePath(manifest.Daemons[0].Cwd, p.WorkingDirectory) {
		return "", errors.New("PROJECT_SERVICE_RELATIVE_MANAGED_CONTEXT_REQUIRED")
	}
	return filepath.Join(p.WorkingDirectory, operand), nil
}
func qualifyGraftProjectService(r *ProjectServiceLaunchPreparationRequest, p ProjectServiceExecutionProfile, prepared ProjectServicePreparedLaunch, files map[string]ProjectServiceProviderFileRead) (string, error) {
	if err := requireProjectServiceRoles(files, ProjectServiceProviderInterpreter, ProjectServiceProviderEntry, ProjectServiceProviderMetadata); err != nil {
		return "", err
	}
	interpreter, entry := files[ProjectServiceProviderInterpreter], files[ProjectServiceProviderEntry]
	if !sameProjectServicePath(p.CommandPath, interpreter.Observation.path) || len(p.Arguments) < 1 || p.Arguments[0] == "" || strings.HasPrefix(p.Arguments[0], "-") {
		return "", errors.New("PROJECT_SERVICE_GRAFT_DIRECT_MODULE_SELECTION_REQUIRED")
	}
	module, err := projectServiceQualifiedPath(r, p, p.Arguments[0])
	if err != nil {
		return "", err
	}
	if !sameProjectServicePath(module, entry.Observation.path) {
		return "", errors.New("PROJECT_SERVICE_GRAFT_DIRECT_MODULE_SELECTION_REQUIRED")
	}
	if err := projectServiceEmptyEnvironment(prepared, "NODE_OPTIONS", "NODE_PATH"); err != nil {
		return "", err
	}
	if !sameProjectServicePath(files[ProjectServiceProviderMetadata].Observation.path, filepath.Join(filepath.Dir(filepath.Dir(entry.Observation.path)), "package.json")) {
		return "", errors.New("PROJECT_SERVICE_GRAFT_PACKAGE_SELECTION_CHANGED")
	}
	var metadata struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if json.Unmarshal(files[ProjectServiceProviderMetadata].Raw, &metadata) != nil || metadata.Name != "@nanonets/graft" || metadata.Version != "0.18.0" {
		return "", errors.New("PROJECT_SERVICE_GRAFT_DEFINITION_UNSUPPORTED")
	}
	if strings.ToUpper(ManifestHashContent(entry.Raw)) != "F076683029CD82AFD4FD32817F017B71E28570B390ECE80B19C288BD5EB7CE9F" {
		return "", errors.New("PROJECT_SERVICE_GRAFT_SOURCE_CUT_CHANGED")
	}
	if len(p.Arguments) != 3 || p.Arguments[1] != "mcp" || p.Arguments[2] == "" || strings.HasPrefix(p.Arguments[2], "-") {
		return "", errors.New("PROJECT_SERVICE_GRAFT_EXPLICIT_MCP_DIRECTORY_REQUIRED")
	}
	return p.Arguments[2], nil
}
func qualifyProjectServiceRecipe(ctx context.Context, r *ProjectServiceLaunchPreparationRequest, p ProjectServiceExecutionProfile, prepared ProjectServicePreparedLaunch, input ProjectServiceProviderInputRead, reads []ProjectServiceProviderFileRead) (*projectServiceRecipe, error) {
	files, err := projectServiceProviderFiles(ctx, input, reads)
	if err != nil {
		return nil, err
	}
	var operand string
	switch r.service {
	case ProjectServiceGraphify:
		operand, err = qualifyGraphifyProjectService(p, prepared, files)
	case ProjectServiceGraft:
		operand, err = qualifyGraftProjectService(r, p, prepared, files)
	default:
		err = errors.New("PROJECT_SERVICE_KIND_INVALID")
	}
	if err != nil {
		return nil, err
	}
	if _, ok := decodeLowerProjectAdoptSHA256(p.EffectiveEnvironmentSHA256); !ok || p.CommandPath == "" || !filepath.IsAbs(p.WorkingDirectory) {
		return nil, errors.New("PROJECT_SERVICE_EXECUTION_PROFILE_INCOMPLETE")
	}
	dataset, err := projectServiceQualifiedPath(r, p, operand)
	if err != nil {
		return nil, err
	}
	manifest, err := config.ParseManifest(bytes.NewReader(r.manifest))
	if err != nil {
		return nil, err
	}
	if manifest.Kind != "global" || manifest.Transport != "stdio-bridge" || len(manifest.Daemons) != 1 {
		return nil, errors.New("PROJECT_SERVICE_MANIFEST_RECIPE_INVALID")
	}
	type fileIdentity struct {
		Role   string
		Path   string
		SHA256 string
	}
	identities := make([]fileIdentity, 0, len(files))
	for role, read := range files {
		identities = append(identities, fileIdentity{role, read.Observation.path, ManifestHashContent(read.Raw)})
	}
	sort.Slice(identities, func(i, j int) bool { return identities[i].Role < identities[j].Role })
	material, err := json.Marshal(struct {
		Domain      string
		Service     ProjectServiceKind
		Command     string
		Arguments   []string
		CWD         string
		Environment string
		Dataset     string
		Protocol    string
		Files       []fileIdentity
	}{"project-service-execution-profile-v1", r.service, p.CommandPath, slices.Clone(p.Arguments), p.WorkingDirectory, p.EffectiveEnvironmentSHA256, dataset, manifest.Daemons[0].MCPProtocolCompatibilityProfile, identities})
	if err != nil {
		return nil, err
	}
	return &projectServiceRecipe{service: r.service, dataset: dataset, profileSHA256: ManifestHashContent(material)}, nil
}
