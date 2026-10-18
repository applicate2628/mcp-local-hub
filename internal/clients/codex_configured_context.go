package clients

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

type codexConfiguredContext struct {
	executable                                string
	command                                   *exec.Cmd
	environment                               []string
	global, project                           []byte
	auth                                      []byte
	files                                     []codexConfiguredFile
	globalEntry, projectEntry                 map[string]any
	routeSHA, contextSHA, layerSHA, policySHA string
	replayContextSHA                          string
}

func (c codexConfiguredContext) equal(other codexConfiguredContext) bool {
	if c.routeSHA != other.routeSHA || c.contextSHA != other.contextSHA || c.layerSHA != other.layerSHA || c.policySHA != other.policySHA || len(c.files) != len(other.files) {
		return false
	}
	for index := range c.files {
		left, right := c.files[index], other.files[index]
		if left.layer.Present != right.layer.Present || (left.layer.Present && !os.SameFile(left.info, right.info)) || !left.globalBinding.equal(right.globalBinding) {
			return false
		}
	}
	return true
}

func (c codexConfiguredContext) nonrefreshing(clock time.Time, budget time.Duration) error {
	return codexConfiguredAuthNonrefreshing(c.auth, clock, budget)
}

func captureCodexConfiguredContext(ctx context.Context, root, globalPath, projectPath, logical string, readIO codexConfiguredReadIO) (out codexConfiguredContext, resultErr error) {
	if err := ctx.Err(); err != nil {
		return out, err
	}
	out.environment = append([]string(nil), readIO.environ()...)
	env, userRoot, systemRoot, err := codexConfiguredHomeContext(out.environment, readIO.knownFolders)
	if err != nil {
		return out, err
	}
	if !strings.EqualFold(filepath.Clean(globalPath), filepath.Join(filepath.Clean(userRoot), ".codex", "config.toml")) {
		return out, errors.New("native home and global owner disagree")
	}
	if SecureVerifyParentDir == nil {
		return out, errors.New("native context parent verification unavailable")
	}
	if present, err := SecureVerifyParentDir(ctx, root); err != nil || !present {
		return out, errors.Join(err, ErrCodexProjectSourceRootInvalid)
	}
	global, err := readCodexConfiguredGlobalFile(globalPath, 4<<20, readIO.sourceClose)
	out.files = append(out.files, global)
	if err != nil || !global.layer.Present {
		return out, errors.Join(err, errors.New("native global source absent"))
	}
	project, err := readCodexConfiguredFile(projectPath, 4<<20, readIO.sourceClose)
	out.files = append(out.files, project)
	if err != nil || !project.layer.Present || os.SameFile(global.info, project.info) {
		return out, errors.Join(err, ErrProjectClientInvalid)
	}
	out.global, out.project = global.bytes, project.bytes
	globalDocument, err := decodeCodexTOMLBytes(global.bytes)
	if err != nil {
		return out, err
	}
	projectDocument, err := decodeCodexTOMLBytes(project.bytes)
	if err != nil {
		return out, err
	}
	for _, document := range []map[string]any{globalDocument, projectDocument} {
		for _, key := range []string{"profile", "profiles", "project_root_markers", "cli_auth_credentials_store", "network_proxy"} {
			if _, present := document[key]; present {
				return out, errors.New("native configuration selects unsupported contributor")
			}
		}
	}
	if _, present := projectDocument["projects"]; present {
		return out, errors.New("project config supplies trust overrides")
	}
	projects, ok := globalDocument["projects"].(map[string]any)
	if !ok {
		return out, errors.New("native project trust table unavailable")
	}
	trusted := false
	for spelling, value := range projects {
		if CanonicalProjectKey(spelling) != CanonicalProjectKey(root) {
			continue
		}
		fields, ok := value.(map[string]any)
		if !ok || fields["trust_level"] != "trusted" {
			return out, errors.New("native equivalent project trust is not trusted")
		}
		trusted = true
	}
	if !trusted {
		return out, errors.New("native selected project not trusted")
	}
	globalEntry, present, err := codexConfiguredRawEntry(globalDocument, logical)
	out.globalEntry = cloneCodexRawEntry(globalEntry)
	if err != nil || !present {
		return out, errors.Join(err, errors.New("native logical global source absent"))
	}
	if _, _, err := codexTransportOfEntry(globalEntry, logical); err != nil {
		return out, err
	}
	for key := range globalEntry {
		switch key {
		case "command", "args", "env", "env_vars", "cwd", "url", "bearer_token_env_var", "http_headers", "env_http_headers", "enabled", "enabled_tools", "disabled_tools", "startup_timeout_sec", "tool_timeout_sec":
		default:
			return out, errors.New("native omitted raw policy is unsupported")
		}
	}
	projectEntry, present, err := codexConfiguredRawEntry(projectDocument, logical)
	out.projectEntry = cloneCodexRawEntry(projectEntry)
	if err != nil {
		return out, err
	}
	if present {
		for key, value := range projectEntry {
			switch key {
			case "enabled":
				if _, ok := value.(bool); !ok {
					return out, errors.New("native project enabled type invalid")
				}
			case "args":
				if _, err := providerStringSlice(value); err != nil || value == nil {
					if err == nil {
						err = errors.New("native project args is null")
					}
					return out, err
				}
			default:
				return out, errors.New("native project policy contributor unsupported")
			}
		}
	}
	// The native loader composes values. This records full raw inputs without
	// implementing TOML merge/precedence or inventing omitted native policy.
	out.policySHA, err = providerFingerprint(struct {
		Global  map[string]any `json:"global"`
		Project map[string]any `json:"project"`
	}{globalEntry, projectEntry})
	if err != nil {
		return out, err
	}
	markers, err := codexConfiguredRootMarkers(ctx, root)
	out.files = append(out.files, markers...)
	if err != nil {
		return out, err
	}
	home := filepath.Dir(globalPath)
	conditional := []codexConfiguredFile{}
	for _, path := range []string{filepath.Join(systemRoot, "OpenAI", "Codex", "config.toml"), filepath.Join(systemRoot, "OpenAI", "Codex", "requirements.toml"), filepath.Join(home, "managed_config.toml"), filepath.Join(home, "cloud-config-bundle-cache.json")} {
		file, err := readCodexConfiguredFile(path, 4<<20, readIO.sourceClose)
		out.files = append(out.files, file)
		if err != nil || file.layer.Present {
			return out, errors.Join(err, errors.New("native additional layer unsupported"))
		}
		conditional = append(conditional, file)
	}
	auth, err := readCodexConfiguredFile(filepath.Join(home, "auth.json"), 1<<20, readIO.sourceClose)
	out.files = append(out.files, auth)
	if err != nil || !auth.layer.Present {
		return out, errors.Join(err, errors.New("native file authentication unavailable"))
	}
	out.auth = auth.bytes
	conditional = append(conditional, auth)
	out.executable, err = readIO.lookup("codex")
	if err != nil {
		return out, err
	}
	command, err := resolveCodexProviderCommandWithLookup(out.executable, readIO.lookup, codexNativeMCPGet, logical, readIO.shimClose)
	if err != nil {
		return out, err
	}
	command.Dir, command.Env = root, append([]string(nil), out.environment...)
	routeFiles, definition, err := qualifyCodexConfiguredRoute(out.executable, command.Path, command.Args)
	out.files = append(out.files, routeFiles...)
	if err != nil {
		return out, err
	}
	out.command = command
	out.routeSHA, err = codexConfiguredLayerDigest(routeFiles)
	if err != nil {
		return out, err
	}
	// The native embedded defaults are a real contributor. Their inspected
	// source bytes/URI and the actual observed package/image route supply the
	// definition correlation; this is not a disk-path or byte-build attestation.
	defaultsIdentity := codexAttachmentSHA([]byte(definition.defaultsURI + "|" + out.routeSHA))
	defaultsContent := codexAttachmentSHA([]byte(codexConfiguredPackagedDefaults))
	defaults := codexConfiguredFile{layer: codexConfiguredLayer{
		PathSHA256: codexAttachmentSHA([]byte(definition.defaultsURI)), Present: true,
		IdentitySHA256: &defaultsIdentity, ContentSHA256: &defaultsContent,
	}}
	out.layerSHA, err = codexConfiguredLayerDigest([]codexConfiguredFile{global, project, defaults})
	if err != nil {
		return out, err
	}
	out.files = append([]codexConfiguredFile{global, project}, conditional...)
	out.files = append(out.files, markers...)
	out.files = append(out.files, routeFiles...)
	observations, err := codexConfiguredLayerDigest(out.files)
	if err != nil {
		return out, err
	}
	contextInputs := struct {
		Definition      string `json:"definition"`
		ProducerVersion string `json:"producer_version"`
		Root            string `json:"root_sha256"`
		Environment     string `json:"environment_sha256"`
		Observations    string `json:"observations_sha256"`
	}{definition.commit, definition.version, codexAttachmentSHA([]byte(CanonicalProjectKey(root))), codexConfiguredEnvironmentFingerprint(env), observations}
	out.contextSHA, err = providerFingerprint(contextInputs)
	if err != nil {
		return out, err
	}
	// Only the mutable regular global file's observation is excluded. All
	// definition/root/environment inputs and every other original contributor
	// use the same fingerprint structure, ordering and digest owner above.
	replayObservations, err := codexConfiguredLayerDigest(out.files[1:])
	if err != nil {
		return out, err
	}
	contextInputs.Observations = replayObservations
	out.replayContextSHA, err = providerFingerprint(contextInputs)
	return out, err
}

func codexConfiguredHomeContext(environment []string, knownFolders func() (string, string, error)) (map[string]string, string, string, error) {
	userRoot, systemRoot, err := knownFolders()
	if err != nil || userRoot == "" || systemRoot == "" || !filepath.IsAbs(userRoot) || !filepath.IsAbs(systemRoot) {
		return nil, "", "", errors.Join(err, errors.New("native Windows folder context unavailable"))
	}
	env := make(map[string]string)
	for _, row := range environment {
		key, value, ok := strings.Cut(row, "=")
		key = strings.ToUpper(key)
		if !ok || key == "" {
			return nil, "", "", errors.New("native environment key invalid")
		}
		if _, duplicate := env[key]; duplicate {
			return nil, "", "", errors.New("native environment duplicate key")
		}
		env[key] = value
	}
	for _, key := range []string{"HOME", "CODEX_HOME", "CODEX_ACCESS_TOKEN", "CODEX_API_KEY", "OPENAI_API_KEY", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE", "OPENAI_WORKLOAD_IDENTITY_CONTEXT"} {
		if _, present := env[key]; present {
			return nil, "", "", errors.New("native environment selects unsupported contributor")
		}
	}
	if !strings.EqualFold(filepath.Clean(env["USERPROFILE"]), filepath.Clean(userRoot)) {
		return nil, "", "", errors.New("native home and environment disagree")
	}
	return env, userRoot, systemRoot, nil
}

func codexConfiguredEnvironmentFingerprint(environment map[string]string) string {
	// Environment stays private, including values; only its full equality binding
	// enters the source context. Encoding/json orders these string map keys.
	encoded, _ := json.Marshal(environment)
	return codexAttachmentSHA(encoded)
}

func codexConfiguredRawEntry(document map[string]any, logical string) (map[string]any, bool, error) {
	container, exists := document["mcp_servers"]
	if !exists {
		return nil, false, nil
	}
	servers, ok := container.(map[string]any)
	if !ok {
		return nil, false, ErrCodexLayerParseFailed
	}
	value, exists := servers[logical]
	if !exists {
		return nil, false, nil
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return nil, false, ErrCodexLayerParseFailed
	}
	return cloneCodexRawEntry(entry), true, nil
}

func codexConfiguredRootMarkers(ctx context.Context, root string) ([]codexConfiguredFile, error) {
	var observations []codexConfiguredFile
	for directory := root; ; directory = filepath.Dir(directory) {
		marker := filepath.Join(directory, ".git")
		info, err := os.Lstat(marker)
		if err == nil {
			if directory != root || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, errors.New("native marker selection is unsupported")
			}
			if present, err := SecureVerifyParentDir(ctx, marker); err != nil || !present {
				return nil, errors.Join(err, ErrCodexProjectSourceRootInvalid)
			}
			head, err := readCodexConfiguredFile(filepath.Join(marker, "HEAD"), 64<<10)
			observations = append(observations, head)
			if err != nil || !head.layer.Present {
				return observations, errors.Join(err, errors.New("native root marker HEAD unavailable"))
			}
			return observations, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		observations = append(observations, codexConfiguredFile{layer: codexConfiguredLayer{PathSHA256: codexAttachmentSHA([]byte(CanonicalProjectKey(marker)))}})
		if filepath.Dir(directory) == directory {
			return observations, nil
		}
	}
}

// Image bytes are hashed while the same bounded no-reparse reader owns the
// handle. They are not allocated/captured as a configuration or public log.
func observeCodexConfiguredImage(path string) (out codexConfiguredFile, resultErr error) {
	out.layer.PathSHA256 = codexAttachmentSHA([]byte(CanonicalProjectKey(path)))
	defer func() { out.ownerError = resultErr }()
	var identity, content string
	_, present, err := readCodexConfigNoReparse(path, codexConfigReadIO{
		read: func(file *os.File) (value []byte, readErr error) {
			out.reader, out.readEntered = file, true
			defer func() { out.readReturned, out.readError = true, readErr }()
			var err error
			out.info, err = file.Stat()
			if err != nil || out.info.Size() <= 0 || out.info.Size() > 1<<30 {
				return nil, errors.Join(err, errors.New("native image size unsupported"))
			}
			identity, err = codexConfiguredFileIdentity(file)
			if err != nil {
				return nil, err
			}
			out.objectIdentity = &CodexConfiguredObjectIdentityV1{Scheme: codexConfiguredIdentityScheme, Value: identity}
			digest := sha256.New()
			count, err := io.Copy(digest, io.LimitReader(file, (1<<30)+1))
			after, statErr := file.Stat()
			if err != nil || statErr != nil || count != out.info.Size() || after.Size() != out.info.Size() || after.ModTime() != out.info.ModTime() {
				return nil, errors.Join(err, statErr, ErrProjectClientPlanStale)
			}
			content = fmt.Sprintf("%x", digest.Sum(nil))
			return nil, nil
		}, close: func(file *os.File) error {
			out.reader, out.closeAttempted = file, true
			out.closeError = file.Close()
			out.closeReturned = true
			return out.closeError
		},
	})
	if err != nil || !present {
		return out, errors.Join(err, errors.New("native image unavailable"))
	}
	identitySHA := codexAttachmentSHA([]byte(identity))
	out.layer.Present, out.layer.IdentitySHA256, out.layer.ContentSHA256 = true, &identitySHA, &content
	return out, nil
}

func codexConfiguredPackage(path, version string, native bool) (codexConfiguredFile, error) {
	file, err := readCodexConfiguredFile(path, 1<<20)
	if err != nil || !file.layer.Present {
		return file, errors.Join(err, errors.New("native package unavailable"))
	}
	object, err := codexConfiguredObject(file.bytes)
	if err != nil {
		return file, err
	}
	var name, actualVersion string
	if json.Unmarshal(object["name"], &name) != nil || json.Unmarshal(object["version"], &actualVersion) != nil || name != "@openai/codex" || (version != "" && actualVersion != version) {
		return file, errors.New("native package definition version unsupported")
	}
	if !native {
		if _, err := codexConfiguredProducerDefinition(actualVersion); err != nil {
			return file, err
		}
	}
	if native {
		var operatingSystems, cpus []string
		if json.Unmarshal(object["os"], &operatingSystems) != nil || json.Unmarshal(object["cpu"], &cpus) != nil || !reflect.DeepEqual(operatingSystems, []string{"win32"}) || !reflect.DeepEqual(cpus, []string{"x64"}) {
			return file, errors.New("native package platform unsupported")
		}
	} else {
		bins, err := codexConfiguredObject(object["bin"])
		var bin string
		if err != nil || json.Unmarshal(bins["codex"], &bin) != nil || bin != "bin/codex.js" {
			return file, errors.New("native package entrypoint unsupported")
		}
		optional, err := codexConfiguredObject(object["optionalDependencies"])
		var dependency string
		if err != nil || json.Unmarshal(optional["@openai/codex-win32-x64"], &dependency) != nil || dependency != "npm:@openai/codex@"+actualVersion+"-win32-x64" {
			return file, errors.New("native package dependency association invalid")
		}
	}
	return file, nil
}

func qualifyCodexConfiguredRoute(resolved, executable string, args []string) (files []codexConfiguredFile, definition codexConfiguredDefinition, resultErr error) {
	if !filepath.IsAbs(resolved) || !filepath.IsAbs(executable) {
		return nil, definition, errors.New("native route must be absolute")
	}
	var nativePath, packageRoot string
	if strings.EqualFold(filepath.Ext(resolved), ".cmd") {
		if len(args) != 6 {
			return nil, definition, errors.New("native npm route argument shape unsupported")
		}
		packageRoot = filepath.Dir(filepath.Dir(args[1]))
		// This is the actual stock npm optional-package relationship; unknown
		// resolution/install layouts refuse rather than execute a guessed path.
		nativePath = filepath.Join(packageRoot, "node_modules", "@openai", "codex-win32-x64", "vendor", "x86_64-pc-windows-msvc", "bin", "codex.exe")
	} else {
		nativePath = resolved
		nativeRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(nativePath))))
		packageRoot = filepath.Dir(filepath.Dir(filepath.Dir(nativeRoot)))
		if !strings.EqualFold(nativePath, filepath.Join(packageRoot, "node_modules", "@openai", "codex-win32-x64", "vendor", "x86_64-pc-windows-msvc", "bin", "codex.exe")) {
			return nil, definition, errors.New("native executable package association unavailable")
		}
	}
	main, err := codexConfiguredPackage(filepath.Join(packageRoot, "package.json"), "", false)
	files = append(files, main)
	if err != nil {
		return files, definition, err
	}
	mainObject, err := codexConfiguredObject(main.bytes)
	if err != nil {
		return files, definition, err
	}
	var version string
	if json.Unmarshal(mainObject["version"], &version) != nil {
		return files, definition, errors.New("native package version unavailable")
	}
	definition, err = codexConfiguredProducerDefinition(version)
	if err != nil {
		return files, definition, err
	}
	nativeRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(nativePath))))
	platform, err := codexConfiguredPackage(filepath.Join(nativeRoot, "package.json"), definition.version+"-win32-x64", true)
	files = append(files, platform)
	if err != nil {
		return files, definition, err
	}
	image, err := observeCodexConfiguredImage(nativePath)
	files = append(files, image)
	if err != nil {
		return files, definition, err
	}
	entry, err := readCodexConfiguredFile(filepath.Join(packageRoot, "bin", "codex.js"), 1<<20)
	files = append(files, entry)
	if err != nil || !entry.layer.Present {
		return files, definition, errors.Join(err, errors.New("native package entrypoint absent"))
	}
	if !strings.EqualFold(resolved, nativePath) {
		shim, err := readCodexConfiguredFile(resolved, 64<<10)
		files = append(files, shim)
		if err != nil {
			return files, definition, err
		}
		node, err := observeCodexConfiguredImage(executable)
		files = append(files, node)
		if err != nil {
			return files, definition, err
		}
	}
	return files, definition, nil
}
