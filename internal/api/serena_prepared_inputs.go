package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"mcp-local-hub/internal/api/binary_discovery"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/config"
)

// Admission is assembled before the caller's intent exclusion. The original
// declaration/ref/overlay owner supplies provenance before secret resolution;
// the private build execution values never enter record/package identity.
// Network/index inputs are explicit. Discovery never downloads an interpreter.
type SerenaPreparationAdmission struct {
	OperationID, GenerationID string
	Environment               []string
	EnvironmentSource         SerenaPreparationEnvironmentSource
	InstallSettings           []string
}
type SerenaPreparationInputs struct {
	Request                                           PreparedSerenaRequest
	Environment                                       []string `json:"-"`
	InstallArgs                                       []string
	CleanupDeadline                                   time.Time
	Policy                                            SerenaPreparationPolicyV2
	EnvironmentProjectionSHA256                       string
	execution                                         SerenaPreparationEnvironment
	uvVersionProbe, pythonFindProbe, interpreterProbe *preparedSerenaCommandResult
	qualifiedUV, qualifiedInterpreter                 PreparedSerenaFile
	source                                            SerenaPreparationEnvironmentSource
}

func preparedSerenaEnvironmentMap(values []string) map[string]string {
	result := map[string]string{}
	for _, item := range values {
		key, value, ok := strings.Cut(item, "=")
		if ok && key != "" {
			result[strings.ToUpper(key)] = value
		}
	}
	return result
}
func preparedSerenaEnvironmentWith(values []string, overrides map[string]string) []string {
	mapValues := preparedSerenaEnvironmentMap(values)
	for key, value := range overrides {
		mapValues[strings.ToUpper(key)] = value
	}
	keys := make([]string, 0, len(mapValues))
	for key := range mapValues {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+mapValues[key])
	}
	return result
}

func BuildSerenaPreparationInputs(ctx context.Context, manifest *config.ServerManifest, rawManifestSHA string, admission SerenaPreparationAdmission, override *SerenaPreparationPolicyV2) (result SerenaPreparationInputs, err error) {
	defer func() {
		if err != nil {
			err = &serenaPreparationInputsError{cause: err, inputs: result}
		}
	}()
	policy, err := ResolveSerenaPreparationPolicy(override)
	if err != nil {
		return result, err
	}
	if ctx == nil || manifest == nil || manifest.Name != SerenaServerName || !preparedSerenaDigest(rawManifestSHA) || !preparedSerenaID(admission.OperationID) || !preparedSerenaID(admission.GenerationID) || admission.EnvironmentSource == nil {
		return result, errors.New("SERENA_PREPARATION_INPUT_INVALID")
	}
	end := time.Now().Add(policy.WorkLimit)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(end) {
		end = deadline
	}
	work, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	ctx = work
	result = SerenaPreparationInputs{Policy: policy, CleanupDeadline: end.Add(policy.CleanupLimit)}
	namespace, rootObservation, rootErr := ResolveDaemonStateDirObserved(work)
	if rootErr != nil {
		return result, rootErr
	}
	if rootErr = rootObservation.Validate(work, namespace); rootErr != nil {
		return result, rootErr
	}
	if len(manifest.BaseArgs) < 3 || manifest.BaseArgs[0] != "--from" || manifest.BaseArgs[2] != "serena" {
		return result, errors.New("SERENA_PREPARED_SOURCE_PREFIX_INVALID")
	}
	projection, execution, err := BuildSerenaBuildEnvironmentProjection(ctx, admission.EnvironmentSource, policy)
	if err != nil {
		return result, err
	}
	projectionSHA, err := projection.SHA256()
	if err != nil {
		return result, err
	}
	admission.Environment = append([]string(nil), execution.values...)
	settings, err := preparedSerenaInstallSettings(admission.InstallSettings, execution.networkPolicy)
	if err != nil {
		return result, err
	}
	source := manifest.BaseArgs[1]
	at := strings.LastIndex(source, "@")
	if at < 0 {
		return result, errors.New("SERENA_PREPARED_SOURCE_INVALID")
	}
	policySHA, err := policy.IdentitySHA256()
	if err != nil {
		return result, err
	}
	result = SerenaPreparationInputs{Policy: policy, CleanupDeadline: end.Add(policy.CleanupLimit), Environment: append([]string(nil), admission.Environment...), EnvironmentProjectionSHA256: projectionSHA, execution: execution, source: admission.EnvironmentSource}
	publisherSource, sourceErr := buildinfo.CurrentPublisherSource()
	if sourceErr != nil {
		return result, sourceErr
	}
	if sourceErr = publisherSource.Validate(publisherSource.Epoch()); sourceErr != nil {
		return result, sourceErr
	}
	metadataRequestRaw, metadataErr := json.Marshal(struct{ OperationID, GenerationID, RawManifestSHA256, PolicySHA256, ProjectionSHA256 string }{admission.OperationID, admission.GenerationID, rawManifestSHA, policySHA, projectionSHA})
	if metadataErr != nil {
		return result, metadataErr
	}
	if metadataErr = preflightSerenaPreparationIntentMetadata(work, namespace, admission.OperationID, preparedSerenaHash(metadataRequestRaw), publisherSource.Epoch()); metadataErr != nil {
		return result, metadataErr
	}
	// Search the qualified build PATH before installation hints. Presence is
	// only a candidate, followed by anchored hash identity and original-owner
	// version receiving below.
	uvPath, err := discoverPreparedSerenaUV(work, execution, binary_discovery.DefaultHints())
	if err != nil {
		return result, err
	}
	uv, _, err := qualifyPreparedSerenaFile(work, uvPath, 64<<20)
	if err != nil {
		return result, err
	}
	result.qualifiedUV = uv
	// Probe stores are an acquired attempt capability, not ambient UV cache.
	probeKey := preparedSerenaHash([]byte(rawManifestSHA + policySHA))
	probe, acquireErr := AcquireSerenaOwnedTree(work, SerenaOwnedTreeRequest{Namespace: namespace, PackageIdentitySHA256: probeKey, OperationID: admission.OperationID, GenerationID: admission.GenerationID, Kind: "attempt", Limits: policy.OwnedTreeLimits, AbsoluteCleanupDeadline: result.CleanupDeadline})
	if acquireErr != nil {
		if probe != nil {
			_, closeErr := probe.Close()
			acquireErr = errors.Join(acquireErr, closeErr)
		}
		return result, acquireErr
	}
	probePath, workingErr := probe.WorkingDirectory(work)
	if workingErr != nil {
		_, closeErr := probe.Close()
		return result, errors.Join(workingErr, closeErr)
	}
	env := preparedSerenaEnvironmentWith(admission.Environment, map[string]string{"UV_CACHE_DIR": filepath.Join(probePath, "cache"), "TMP": probePath, "TEMP": probePath, "PYTHONDONTWRITEBYTECODE": "1", "UV_PYTHON_DOWNLOADS": "never"})
	var lastClosure PreparedSerenaExecutionClosure
	defer func() {
		cleanup, cancel := context.WithDeadline(context.WithoutCancel(ctx), result.CleanupDeadline)
		defer cancel()
		if lastClosure.complete(admission.OperationID) {
			receipt, cleanupErr := probe.RemoveOwned(cleanup, true, lastClosure)
			err = errors.Join(err, cleanupErr)
			if !receipt.Complete() {
				err = errors.Join(err, errors.New("SERENA_PREPARATION_PROBE_CLEANUP_PENDING"))
			}
		} else {
			err = errors.Join(err, errors.New("SERENA_PREPARATION_PROBE_CUSTODY_PENDING"))
		}
		_, closeErr := probe.Close()
		err = errors.Join(err, closeErr)
	}()
	run := func(args []string) (preparedSerenaCommandResult, error) {
		v, e := runPreparedSerenaInvocation(work, preparedSerenaInvocation{OperationID: admission.OperationID, Executable: uv, Args: args, Environment: env, WorkingDir: probePath, PolicySHA256: policySHA, CleanupLimit: policy.CleanupLimit, AbsoluteCleanupDeadline: result.CleanupDeadline, StdoutLimit: 1 << 20, StderrLimit: 1 << 20})
		lastClosure = v.Closure
		return v, e
	}
	version, err := run([]string{"--version"})
	result.uvVersionProbe = &version
	if versionErr := validateSerenaUVProbe(version, admission.OperationID, policy, err); versionErr != nil {
		return result, versionErr
	}
	found, err := run(policy.pythonFindArgs())
	result.pythonFindProbe = &found
	if err != nil || !found.Closure.complete(admission.OperationID) || !found.Closure.RootExitKnown || found.Closure.RootExitCode != 0 {
		return result, errors.Join(err, errors.New("SERENA_RUNTIME_INSTALL_NEEDED"))
	}
	path := strings.TrimSpace(string(found.Stdout))
	if strings.ContainsAny(path, "\r\n\x00") || !filepath.IsAbs(path) {
		return result, errors.New("SERENA_PREPARATION_INTERPRETER_OUTPUT_INVALID")
	}
	interpreter, _, err := qualifyPreparedSerenaFile(work, filepath.Clean(path), 64<<20)
	if err != nil {
		return result, err
	}
	result.qualifiedInterpreter = interpreter
	qualified, err := runPreparedSerenaInvocation(work, preparedSerenaInvocation{OperationID: admission.OperationID, Executable: interpreter, Args: []string{"-I", "-B", "-c", `import json,platform,sys,sysconfig;print(json.dumps({'version':platform.python_version(),'version_info':list(sys.version_info[:3]),'platform':sys.platform,'machine':platform.machine(),'soabi':sysconfig.get_config_var('SOABI') or '', 'stdlib':sysconfig.get_path('stdlib')},sort_keys=True,separators=(',',':')))`}, Environment: env, WorkingDir: probePath, PolicySHA256: policySHA, CleanupLimit: policy.CleanupLimit, AbsoluteCleanupDeadline: result.CleanupDeadline, StdoutLimit: 1 << 20, StderrLimit: 1 << 20})
	lastClosure = qualified.Closure
	result.interpreterProbe = &qualified
	abi, abiErr := decodeSerenaInterpreterABI(qualified.Stdout)
	if err != nil || !lastClosure.complete(admission.OperationID) || !lastClosure.RootExitKnown || lastClosure.RootExitCode != 0 || abiErr != nil {
		return result, errors.Join(err, abiErr, errors.New("SERENA_PREPARATION_INTERPRETER_ABI_INVALID"))
	}
	if err = policy.validatePythonVersion(abi.VersionInfo); err != nil {
		return result, err
	}
	if !filepath.IsAbs(abi.Stdlib) || filepath.Clean(abi.Stdlib) != abi.Stdlib {
		return result, errors.New("SERENA_PREPARATION_STDLIB_LOCATOR_INVALID")
	}
	var launcher PreparedSerenaFile
	for _, candidate := range []string{filepath.Join(abi.Stdlib, "venv", "scripts", "nt", "python.exe"), filepath.Join(abi.Stdlib, "venv", "scripts", "nt", "venvlauncher.exe"), filepath.Join(filepath.Dir(interpreter.Path), "venvlauncher.exe"), interpreter.Path} {
		actual, _, qualifyErr := qualifyPreparedSerenaFile(work, candidate, 64<<20)
		if qualifyErr != nil {
			if preparedSerenaQualifiedFileMissing(qualifyErr) {
				continue
			}
			return result, qualifyErr
		}
		launcher = actual
		break
	}
	if !preparedSerenaFile(launcher) {
		return result, errors.New("SERENA_PREPARATION_LAUNCHER_UNQUALIFIED")
	}
	buildRaw, _ := json.Marshal(struct {
		Settings                    []string
		AllowNetwork                bool
		EnvironmentProjectionSHA256 string
	}{settings, !execution.networkPolicy.offline, projectionSHA})
	result.Request = PreparedSerenaRequest{OperationID: admission.OperationID, GenerationID: admission.GenerationID, Namespace: namespace, ManifestSHA256: rawManifestSHA, SourceRequirement: source, SourceCommit: source[at+1:], Package: "serena-agent", UV: uv, Interpreter: interpreter, PlatformABISHA256: preparedSerenaHash(qualified.Stdout), PolicySHA256: policySHA, BuildSettingsSHA256: preparedSerenaHash(buildRaw), WorkLimit: policy.WorkLimit, CleanupLimit: policy.CleanupLimit}
	result.Request.WorkDeadline = end
	result.Request.OwnedTreeLimits = policy.OwnedTreeLimits
	result.Request.PackageManifestProjectionSHA256 = projection.PackageManifestProjectionSHA256
	result.Request.BuildEnvironmentSHA256 = projectionSHA
	result.Request.Launcher = launcher
	result.Request.AbsoluteCleanupDeadline = result.CleanupDeadline
	result.Request.preparationContext = &serenaPreparationContext{policy: policy, source: result.source, request: result.Request, uvProbe: result.uvVersionProbe, abiProbe: result.interpreterProbe}
	if _, err = result.Request.Identity(); err != nil {
		return result, err
	}
	result.InstallArgs = append([]string{"tool", "install", "--python", interpreter.Path, "--no-python-downloads", "--no-config", "--from", source}, settings...)
	result.InstallArgs = append(result.InstallArgs, result.Request.Package)
	result.Environment = preparedSerenaEnvironmentWith(admission.Environment, map[string]string{"PYTHONDONTWRITEBYTECODE": "1", "UV_PYTHON_DOWNLOADS": "never"})
	if err = execution.networkPolicy.validateExecution(result.Environment, result.InstallArgs); err != nil {
		return result, err
	}
	return result, nil
}

// execution is issued by BuildSerenaBuildEnvironmentProjection after bootstrap
// qualification. Its PATH owns discovery; the ambient PATH is not a fallback.
func discoverPreparedSerenaUV(ctx context.Context, execution SerenaPreparationEnvironment, fallbackHints []string) (string, error) {
	path := preparedSerenaEnvironmentMap(execution.values)["PATH"]
	names := []string{"uv"}
	if runtime.GOOS == "windows" {
		names = []string{"uv.exe", "uv"}
	}
	for _, directory := range filepath.SplitList(path) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// Qualified PATH entries are literal directories. Discover expands
		// environment references in hints, which would change a literal '$'.
		for _, name := range names {
			candidate := filepath.Join(directory, name)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				return candidate, nil
			}
		}
	}
	discovered, err := binary_discovery.Discover(ctx, []string{"uv"}, fallbackHints)
	if err != nil {
		return "", err
	}
	uvPath := discovered["uv"]
	if uvPath == "" {
		return "", errors.Join(&exec.Error{Name: "uv", Err: exec.ErrNotFound}, errors.New("SERENA_RUNTIME_INSTALL_NEEDED"))
	}
	return filepath.Abs(uvPath)
}

func preparedSerenaInstallSettings(settings []string, network *serenaPackageNetworkPolicy) ([]string, error) {
	if network == nil || network.source == nil || network.source.issuer == nil || !network.decisionDefined {
		return nil, errors.New("SERENA_PACKAGE_NETWORK_POLICY_UNAVAILABLE")
	}
	// These are existing explicit uv build/index settings, not arbitrary CLI
	// flags capable of changing the final generation or downloading Python.
	allowed := map[string]bool{"--index": true, "--default-index": true, "--constraint": true, "--override": true, "--build-constraint": true, "--config-setting": true}
	result := append([]string(nil), settings...)
	for i := 0; i < len(settings); i += 2 {
		if !allowed[settings[i]] || i+1 >= len(settings) || settings[i+1] == "" || strings.ContainsAny(settings[i+1], "\x00\r\n") {
			return nil, errors.New("SERENA_PREPARATION_INSTALL_SETTING_INVALID")
		}
		if settings[i] == "--index" || settings[i] == "--default-index" {
			endpoint := settings[i+1]
			prefix := ""
			if settings[i] == "--index" {
				if name, value, named := strings.Cut(endpoint, "="); named {
					prefix = name + "="
					endpoint = value
				}
			}
			normalized, err := serenaBuildIndexURL(endpoint)
			if err != nil {
				return nil, err
			}
			result[i+1] = prefix + normalized
		} else {
			return nil, errors.New("SERENA_PREPARATION_BUILD_ARTIFACT_SOURCE_UNQUALIFIED")
		}
	}
	if network.offline {
		result = append(result, "--offline")
	}
	return result, nil
}

// Original partial execution results remain private data and preserve joined causes.
type serenaPreparationInputsError struct {
	cause  error
	inputs SerenaPreparationInputs
}

func (e *serenaPreparationInputsError) Error() string { return e.cause.Error() }
func (e *serenaPreparationInputsError) Unwrap() error { return e.cause }

func validateSerenaUVProbe(v preparedSerenaCommandResult, id string, p SerenaPreparationPolicyV2, runErr error) error {
	if runErr != nil || !v.Closure.complete(id) || !v.Closure.RootExitKnown || v.Closure.RootExitCode != 0 {
		return errors.Join(runErr, errors.New("SERENA_PREPARATION_UV_VERSION_MISMATCH"))
	}
	if e := p.validateUVVersion(v.Stdout); e != nil {
		return errors.Join(e, errors.New("SERENA_PREPARATION_UV_VERSION_MISMATCH"))
	}
	return nil
}

type serenaInterpreterABI struct {
	Version     string `json:"version"`
	VersionInfo []int  `json:"version_info"`
	Platform    string `json:"platform"`
	Machine     string `json:"machine"`
	SOABI       string `json:"soabi"`
	Stdlib      string `json:"stdlib"`
}

var serenaPythonVersion = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)(?:(?:a|b|rc)[0-9]+)?(?:\+[A-Za-z0-9.-]+)?$`)

func decodeSerenaInterpreterABI(raw []byte) (serenaInterpreterABI, error) {
	var abi serenaInterpreterABI
	if !utf8.Valid(raw) {
		return abi, errors.New("SERENA_PREPARATION_INTERPRETER_ABI_ENCODING_INVALID")
	}
	if e := rejectSerenaJSONDuplicates(raw); e != nil {
		return abi, e
	}
	if e := requireSerenaOperationFields(raw, []string{"version", "version_info", "platform", "machine", "soabi", "stdlib"}); e != nil {
		return abi, e
	}
	// JSON null leaves a primitive integer at zero. Preserve element presence
	// at this boundary before decoding the original numeric ABI representation.
	var tuple struct {
		VersionInfo []*int `json:"version_info"`
	}
	if err := json.Unmarshal(raw, &tuple); err != nil {
		return abi, err
	}
	for _, component := range tuple.VersionInfo {
		if component == nil {
			return abi, errors.New("SERENA_PREPARATION_INTERPRETER_ABI_INVALID")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if e := decoder.Decode(&abi); e != nil {
		return abi, e
	}
	match := serenaPythonVersion.FindStringSubmatch(abi.Version)
	if match == nil || len(abi.VersionInfo) != 3 || abi.Platform != "win32" || abi.Machine == "" || abi.SOABI == "" || !filepath.IsAbs(abi.Stdlib) || filepath.Clean(abi.Stdlib) != abi.Stdlib {
		return abi, errors.New("SERENA_PREPARATION_INTERPRETER_ABI_INVALID")
	}
	numeric, e := serenaNumericVersion(match[1], 3)
	if e != nil || numeric != [3]int{abi.VersionInfo[0], abi.VersionInfo[1], abi.VersionInfo[2]} {
		return abi, errors.Join(e, errors.New("SERENA_PREPARATION_INTERPRETER_ABI_VERSION_CONTRADICTORY"))
	}
	return abi, nil
}
