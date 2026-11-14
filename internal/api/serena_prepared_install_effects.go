package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"runtime"
	"strings"

	"mcp-local-hub/internal/process"
)

type preparedSerenaCombinedClosure struct {
	receipts []process.ContainedStreamReceiptV1
	// Retain original private outcomes even when later interpretation fails.
	commands      []preparedSerenaCommandResult
	commandErrors []error
	receiptRaw    []byte
}

func (c *preparedSerenaCombinedClosure) Complete() bool {
	if c == nil || len(c.receipts) == 0 {
		return false
	}
	for _, r := range c.receipts {
		if !r.NativeSettlement.Complete() || !r.IOSettlement.Complete() {
			return false
		}
	}
	return true
}

// Installation is one effect of the package owner. uv receives exclusively
// this generation/attempt's directories; stdout/stderr remain bounded private
// receipt input. This function neither publishes nor mutates an intent.
func installPreparedSerena(ctx context.Context, inputs SerenaPreparationInputs, roots PreparedSerenaOwnedRoots, constraint *preparedSerenaBuildConstraint) (installed PreparedSerenaInstallation, err error) {
	if err = inputs.execution.networkPolicy.validateExecution(inputs.Environment, inputs.InstallArgs); err != nil {
		return installed, err
	}
	if inputs.execution.buildConstraint != (runtime.GOOS == "windows") || inputs.execution.buildConstraint && constraint == nil {
		return installed, errors.New("SERENA_BUILD_CONSTRAINT_POLICY_UNAVAILABLE")
	}
	for _, item := range inputs.Environment {
		key, _, _ := strings.Cut(item, "=")
		if strings.EqualFold(key, "UV_BUILD_CONSTRAINT") {
			return installed, errors.New("SERENA_BUILD_CONSTRAINT_ENV_OVERRIDE_REFUSED")
		}
	}
	for _, arg := range inputs.InstallArgs {
		if arg == "--build-constraint" || strings.HasPrefix(arg, "--build-constraint=") || strings.HasPrefix(arg, "-b") {
			return installed, errors.New("SERENA_BUILD_CONSTRAINT_ARG_OVERRIDE_REFUSED")
		}
	}
	r := inputs.Request
	if _, err = r.Identity(); err != nil {
		return installed, err
	}
	if !preparedSerenaFile(roots.Generation) || !preparedSerenaFile(roots.Attempt) {
		return installed, errors.New("SERENA_PREPARED_ROOTS_UNQUALIFIED")
	}
	if err = ValidatePreparedSerenaEnvironment(preparedSerenaEnvironmentMap(inputs.Environment)); err != nil {
		return installed, err
	}
	env := preparedSerenaEnvironmentWith(inputs.Environment, map[string]string{"UV_TOOL_DIR": filepath.Join(roots.Generation.Path, "tools"), "UV_TOOL_BIN_DIR": filepath.Join(roots.Generation.Path, "bin"), "UV_CACHE_DIR": filepath.Join(roots.Attempt.Path, "cache"), "TMP": roots.Attempt.Path, "TEMP": roots.Attempt.Path, "PYTHONDONTWRITEBYTECODE": "1", "UV_PYTHON_DOWNLOADS": "never"})
	work, cancel := context.WithDeadline(ctx, r.WorkDeadline)
	defer cancel()
	invoke := func(file PreparedSerenaFile, args []string) (preparedSerenaCommandResult, error) {
		return runPreparedSerenaInvocation(work, preparedSerenaInvocation{OperationID: r.OperationID, Executable: file, Args: args, Environment: env, WorkingDir: roots.Generation.Path, PolicySHA256: r.PolicySHA256, CleanupLimit: r.CleanupLimit, AbsoluteCleanupDeadline: r.AbsoluteCleanupDeadline, StdoutLimit: 64 << 20, StderrLimit: 8 << 20})
	}
	installArgs := append([]string(nil), inputs.InstallArgs...)
	if inputs.execution.buildConstraint {
		if err = beginPreparedSerenaBuildConstraintUse(work, constraint); err != nil {
			return installed, err
		}
		installArgs = append(installArgs, "--build-constraint", constraint.file.Path)
	}
	uv, installErr := invoke(r.UV, installArgs)
	if inputs.execution.buildConstraint {
		installErr = errors.Join(installErr, finishPreparedSerenaBuildConstraintUse(constraint, uv))
	}
	installed.Closure = uv.Closure
	combined := &preparedSerenaCombinedClosure{receipts: []process.ContainedStreamReceiptV1{uv.Receipt}, commands: []preparedSerenaCommandResult{uv}, commandErrors: []error{installErr}}
	installed.Closure.nativeEvidence = combined
	if installErr != nil || !uv.Closure.complete(r.OperationID) || !uv.Closure.RootExitKnown || uv.Closure.RootExitCode != 0 {
		return installed, errors.Join(installErr, errors.New("SERENA_PREPARED_INSTALL_REFUSED"))
	}
	// UV's installed-tool metadata, not a guessed executable basename, owns
	// the tool's declared entrypoint. The package leaf is the exact installed
	// distribution selected by the pinned source requirement.
	toolRoot := filepath.Join(roots.Generation.Path, "tools", r.Package)
	receiptPath := filepath.Join(toolRoot, "uv-receipt.toml")
	_, receiptRaw, readErr := qualifyPreparedSerenaFile(work, receiptPath, 1<<20)
	combined.receiptRaw = append([]byte(nil), receiptRaw...)
	if len(receiptRaw) != 0 {
		installed.UVReceiptSHA256 = preparedSerenaHash(receiptRaw)
	}
	if readErr != nil {
		return installed, readErr
	}
	receipt, entryPath, parseErr := parsePreparedSerenaUVReceipt(receiptRaw, r, roots.Generation.Path)
	if parseErr != nil {
		return installed, parseErr
	}
	if inputs.execution.buildConstraint {
		if err = validatePreparedSerenaBuildConstraintReceipt(receipt); err != nil {
			return installed, err
		}
	}
	environmentPython, _, err := qualifyPreparedSerenaFile(work, filepath.Join(toolRoot, "Scripts", "python.exe"), 64<<20)
	if err != nil {
		return installed, err
	}
	if err = revalidatePreparedSerenaFile(work, r.Launcher); err != nil {
		return installed, err
	}
	if environmentPython.SHA256 != r.Launcher.SHA256 {
		return installed, errors.New("SERENA_PREPARED_LAUNCHER_PROVENANCE_MISMATCH")
	}
	metadataCommand, metadataErr := invoke(environmentPython, []string{"-I", "-B", "-c", preparedSerenaMetadataCollector})
	combined.receipts = append(combined.receipts, metadataCommand.Receipt)
	combined.commands = append(combined.commands, metadataCommand)
	combined.commandErrors = append(combined.commandErrors, metadataErr)
	installed.Closure.HandlesClosed = combined.Complete()
	installed.Closure.IOClosed = combined.Complete()
	installed.Closure.MembersExited = combined.Complete()
	if !combined.Complete() {
		installed.Closure.Pending = append(installed.Closure.Pending, "metadata-command-settlement")
		return installed, errors.Join(metadataErr, errors.New("SERENA_PREPARED_METADATA_CUSTODY_PENDING"))
	}
	if metadataErr != nil || !metadataCommand.Closure.RootExitKnown || metadataCommand.Closure.RootExitCode != 0 {
		return installed, errors.Join(metadataErr, errors.New("SERENA_PREPARED_METADATA_REFUSED"))
	}
	metadata, resolution, parseErr := parsePreparedSerenaMetadata(metadataCommand.Stdout, r, toolRoot)
	if parseErr != nil {
		return installed, parseErr
	}
	if metadata.Executable != environmentPython.Path || metadata.BaseExecutable != r.Interpreter.Path {
		return installed, errors.New("SERENA_PREPARED_METADATA_INTERPRETER_MISMATCH")
	}
	// Re-read each observed distribution file through the same no-follow
	// identity/byte owner. Python's content digest is not a path authority.
	qualifiedMembers := map[string]PreparedSerenaFile{}
	for _, distribution := range metadata.Distributions {
		for _, file := range distribution.Files {
			actual, _, fileErr := qualifyPreparedSerenaFile(work, file.Path, 64<<20)
			if fileErr != nil || actual.SHA256 != file.SHA256 {
				return installed, errors.Join(fileErr, errors.New("SERENA_PREPARED_METADATA_FILE_DRIFT"))
			}
			qualifiedMembers[file.Path] = actual
		}
	}
	associations, associationErr := bindPreparedSerenaReceiptAssociations(receipt, metadata, r, toolRoot, "serena")
	if associationErr != nil {
		return installed, associationErr
	}
	identities := map[string]string{}
	var selected PreparedSerenaFile
	for _, association := range associations {
		path := filepath.FromSlash(association.Receipt.InstallPath)
		exported, _, exportErr := qualifyPreparedSerenaFile(work, path, 64<<20)
		if exportErr != nil {
			return installed, exportErr
		}
		if associationErr = bindPreparedSerenaExportIdentity(identities, association.Receipt.Name, qualifiedMembers[association.Member.Path], exported); associationErr != nil {
			return installed, associationErr
		}
		if path == entryPath {
			selected = exported
		}
	}
	if !preparedSerenaFile(selected) {
		return installed, errors.New("SERENA_PREPARED_EXPORT_SELECTED_MISSING")
	}
	installed.Entrypoint = selected
	installed.SourceSHA256 = preparedSerenaHash([]byte(r.SourceRequirement))
	installed.ResolutionSHA256 = resolution
	summary, encodeErr := json.Marshal(struct{ Install, Metadata, UVReceipt, Resolution, Entry string }{uv.Closure.ReceiptSHA256, metadataCommand.Closure.ReceiptSHA256, installed.UVReceiptSHA256, resolution, installed.Entrypoint.SHA256})
	if encodeErr != nil {
		return installed, encodeErr
	}
	installed.Closure.ReceiptSHA256 = preparedSerenaHash(summary)
	// Final full environment inventory and exact launcher-copy provenance are
	// supplied by the retained tree/input owners before qualification/Ready.
	return installed, nil
}
