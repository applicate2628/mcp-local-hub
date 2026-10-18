package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"mcp-local-hub/internal/process"
)

type preparedSerenaCombinedClosure struct {
	receipts []process.ContainedStreamReceiptV1
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
func installPreparedSerena(ctx context.Context, inputs SerenaPreparationInputs, roots PreparedSerenaOwnedRoots) (installed PreparedSerenaInstallation, err error) {
	if err = inputs.execution.networkPolicy.validateExecution(inputs.Environment, inputs.InstallArgs); err != nil {
		return installed, err
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
	uv, installErr := invoke(r.UV, inputs.InstallArgs)
	installed.Closure = uv.Closure
	if installErr != nil || !uv.Closure.complete(r.OperationID) || !uv.Closure.RootExitKnown || uv.Closure.RootExitCode != 0 {
		return installed, errors.Join(installErr, errors.New("SERENA_PREPARED_INSTALL_REFUSED"))
	}
	combined := &preparedSerenaCombinedClosure{receipts: []process.ContainedStreamReceiptV1{uv.Receipt}}
	installed.Closure.nativeEvidence = combined
	// UV's installed-tool metadata, not a guessed executable basename, owns
	// the tool's declared entrypoint. The package leaf is the exact installed
	// distribution selected by the pinned source requirement.
	toolRoot := filepath.Join(roots.Generation.Path, "tools", r.Package)
	receiptPath := filepath.Join(toolRoot, "uv-receipt.toml")
	_, receiptRaw, readErr := qualifyPreparedSerenaFile(work, receiptPath, 1<<20)
	if readErr != nil {
		return installed, readErr
	}
	_, entryPath, parseErr := parsePreparedSerenaUVReceipt(receiptRaw, r, roots.Generation.Path)
	if parseErr != nil {
		return installed, parseErr
	}
	installed.UVReceiptSHA256 = preparedSerenaHash(receiptRaw)
	installed.Entrypoint, _, err = qualifyPreparedSerenaFile(work, entryPath, 64<<20)
	if err != nil {
		return installed, err
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
	installed.Closure.HandlesClosed = combined.Complete()
	installed.Closure.IOClosed = combined.Complete()
	installed.Closure.MembersExited = combined.Complete()
	if metadataErr != nil || !metadataCommand.Closure.RootExitKnown || metadataCommand.Closure.RootExitCode != 0 || !combined.Complete() {
		return installed, errors.Join(metadataErr, errors.New("SERENA_PREPARED_METADATA_CUSTODY_PENDING"))
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
	for _, distribution := range metadata.Distributions {
		for _, file := range distribution.Files {
			actual, _, fileErr := qualifyPreparedSerenaFile(work, file.Path, 64<<20)
			if fileErr != nil || actual.SHA256 != file.SHA256 {
				return installed, errors.Join(fileErr, errors.New("SERENA_PREPARED_METADATA_FILE_DRIFT"))
			}
		}
	}
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
