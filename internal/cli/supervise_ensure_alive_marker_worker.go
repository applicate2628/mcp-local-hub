package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

const (
	guiOwnerUnknownConfirmationWriteBudget      = 5 * time.Second
	guiOwnerUnknownConfirmationWorkerVersion    = 1
	guiOwnerUnknownConfirmationWorkerMessageMax = 4 * 1024
	guiOwnerUnknownConfirmationWorkerStderrMax  = 16 * 1024
	guiOwnerUnknownConfirmationFutureTolerance  = time.Second
)

type guiOwnerUnknownConfirmationWorkerFailure string

const (
	guiOwnerUnknownConfirmationFailureTimeout           guiOwnerUnknownConfirmationWorkerFailure = "timeout"
	guiOwnerUnknownConfirmationFailureContainment       guiOwnerUnknownConfirmationWorkerFailure = "containment_failed"
	guiOwnerUnknownConfirmationFailureExecution         guiOwnerUnknownConfirmationWorkerFailure = "execution_failed"
	guiOwnerUnknownConfirmationFailureInvalidInvocation guiOwnerUnknownConfirmationWorkerFailure = "invalid_invocation"
	guiOwnerUnknownConfirmationFailureProtocol          guiOwnerUnknownConfirmationWorkerFailure = "protocol_invalid"
	guiOwnerUnknownConfirmationFailureStateWrite        guiOwnerUnknownConfirmationWorkerFailure = "state_write_failed"
)

type guiOwnerUnknownConfirmationWorkerRequest struct {
	Version    int       `json:"version"`
	StateDir   string    `json:"state_dir"`
	ObservedAt time.Time `json:"observed_at"`
}

type guiOwnerUnknownConfirmationWorkerResult struct {
	Version int    `json:"version"`
	Outcome string `json:"outcome"`
	Failure string `json:"failure,omitempty"`
}

type guiOwnerUnknownConfirmationWorkerError struct {
	failure guiOwnerUnknownConfirmationWorkerFailure
	cause   error
	pid     int
	stdout  process.StrictRunCapture
	stderr  process.StrictRunCapture
}

func (e *guiOwnerUnknownConfirmationWorkerError) Error() string {
	return "GUI owner confirmation marker worker " + string(e.failure)
}

func (e *guiOwnerUnknownConfirmationWorkerError) Unwrap() error { return e.cause }

func newGUIOwnerUnknownConfirmationMarkerWorkerCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "gui-owner-unknown-confirmation-worker",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGUIOwnerUnknownConfirmationMarkerWorker(cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

func runGUIOwnerUnknownConfirmationMarkerWorker(in io.Reader, out io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(in, guiOwnerUnknownConfirmationWorkerMessageMax+1))
	if err != nil {
		return fmt.Errorf("read confirmation marker request: %w", err)
	}
	if len(raw) > guiOwnerUnknownConfirmationWorkerMessageMax {
		return writeGUIOwnerUnknownConfirmationWorkerResult(out, guiOwnerUnknownConfirmationWorkerResult{
			Version: guiOwnerUnknownConfirmationWorkerVersion, Outcome: "rejected", Failure: string(guiOwnerUnknownConfirmationFailureProtocol),
		})
	}
	var request guiOwnerUnknownConfirmationWorkerRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return writeGUIOwnerUnknownConfirmationWorkerResult(out, guiOwnerUnknownConfirmationWorkerResult{
			Version: guiOwnerUnknownConfirmationWorkerVersion, Outcome: "rejected", Failure: string(guiOwnerUnknownConfirmationFailureProtocol),
		})
	}
	if err := ensureGUIOwnerUnknownConfirmationWorkerEOF(decoder); err != nil {
		return writeGUIOwnerUnknownConfirmationWorkerResult(out, guiOwnerUnknownConfirmationWorkerResult{
			Version: guiOwnerUnknownConfirmationWorkerVersion, Outcome: "rejected", Failure: string(guiOwnerUnknownConfirmationFailureProtocol),
		})
	}
	result := guiOwnerUnknownConfirmationWorkerResult{Version: guiOwnerUnknownConfirmationWorkerVersion}
	cleanStateDir := filepath.Clean(request.StateDir)
	now := time.Now().UTC()
	if request.Version != guiOwnerUnknownConfirmationWorkerVersion || strings.TrimSpace(request.StateDir) != request.StateDir ||
		request.StateDir == "" || !filepath.IsAbs(request.StateDir) || cleanStateDir != request.StateDir ||
		request.ObservedAt.IsZero() || request.ObservedAt.Location() != time.UTC || request.ObservedAt.After(now.Add(guiOwnerUnknownConfirmationFutureTolerance)) {
		result.Outcome, result.Failure = "rejected", string(guiOwnerUnknownConfirmationFailureProtocol)
		return writeGUIOwnerUnknownConfirmationWorkerResult(out, result)
	}
	markerPath := filepath.Join(cleanStateDir, guiOwnerUnknownConfirmationFileLeaf)
	if err := api.WriteStateFileBytesAtomic(markerPath, []byte(request.ObservedAt.Format(time.RFC3339Nano))); err != nil {
		result.Outcome, result.Failure = "failed", string(guiOwnerUnknownConfirmationFailureStateWrite)
		return writeGUIOwnerUnknownConfirmationWorkerResult(out, result)
	}
	result.Outcome = "written"
	return writeGUIOwnerUnknownConfirmationWorkerResult(out, result)
}

func writeGUIOwnerUnknownConfirmationWorkerResult(out io.Writer, result guiOwnerUnknownConfirmationWorkerResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(raw) > guiOwnerUnknownConfirmationWorkerMessageMax {
		return errors.New("confirmation marker result exceeds bound")
	}
	_, err = out.Write(raw)
	return err
}

func ensureGUIOwnerUnknownConfirmationWorkerEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple confirmation marker JSON values")
}

func runGUIOwnerUnknownConfirmationMarker(ctx context.Context, stateDir string, observedAt time.Time) error {
	payload, err := json.Marshal(guiOwnerUnknownConfirmationWorkerRequest{
		Version: guiOwnerUnknownConfirmationWorkerVersion, StateDir: stateDir, ObservedAt: observedAt.UTC(),
	})
	if err != nil || len(payload) > guiOwnerUnknownConfirmationWorkerMessageMax {
		return &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: err}
	}
	exe, err := os.Executable()
	if err != nil {
		return &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureExecution, cause: err}
	}
	cmd := exec.Command(exe, "gui-owner-unknown-confirmation-worker")
	result, runErr := process.RunStrictlyContained(ctx, process.StrictRunInvocation{
		Command: cmd,
		Input:   payload, InputLimit: guiOwnerUnknownConfirmationWorkerMessageMax,
		StdoutLimit: guiOwnerUnknownConfirmationWorkerMessageMax, StderrLimit: guiOwnerUnknownConfirmationWorkerStderrMax,
	})
	if runErr != nil {
		return newGUIOwnerUnknownConfirmationWorkerRunError(cmd, result, runErr)
	}
	return interpretGUIOwnerUnknownConfirmationWorkerResult(result)
}

func newGUIOwnerUnknownConfirmationWorkerRunError(cmd *exec.Cmd, result process.StrictRunResult, runErr error) error {
	failure := guiOwnerUnknownConfirmationFailureExecution
	var strictErr *process.StrictRunError
	if errors.As(runErr, &strictErr) {
		switch strictErr.Kind {
		case process.StrictRunTimeout:
			failure = guiOwnerUnknownConfirmationFailureTimeout
		case process.StrictRunContainmentFailed:
			failure = guiOwnerUnknownConfirmationFailureContainment
		case process.StrictRunInvalidInvocation:
			failure = guiOwnerUnknownConfirmationFailureInvalidInvocation
		}
	}
	pid := 0
	if cmd != nil && cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	return &guiOwnerUnknownConfirmationWorkerError{failure: failure, cause: runErr, pid: pid, stdout: result.Stdout, stderr: result.Stderr}
}

func interpretGUIOwnerUnknownConfirmationWorkerResult(result process.StrictRunResult) error {
	if result.Stdout.Truncated {
		return &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: errors.New("confirmation marker result exceeds bound"), stdout: result.Stdout, stderr: result.Stderr}
	}
	var workerResult guiOwnerUnknownConfirmationWorkerResult
	decoder := json.NewDecoder(bytes.NewReader(result.Stdout.Prefix))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&workerResult); err != nil {
		return &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: err, stdout: result.Stdout, stderr: result.Stderr}
	}
	if err := ensureGUIOwnerUnknownConfirmationWorkerEOF(decoder); err != nil {
		return &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: err, stdout: result.Stdout, stderr: result.Stderr}
	}
	if workerResult.Version != guiOwnerUnknownConfirmationWorkerVersion {
		return &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: errors.New("confirmation marker result version mismatch"), stdout: result.Stdout, stderr: result.Stderr}
	}
	if workerResult.Outcome == "written" && workerResult.Failure == "" {
		return nil
	}
	if workerResult.Outcome == "failed" && workerResult.Failure == string(guiOwnerUnknownConfirmationFailureStateWrite) {
		return &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureStateWrite, stdout: result.Stdout, stderr: result.Stderr}
	}
	return &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: errors.New("confirmation marker result contradicts protocol"), stdout: result.Stdout, stderr: result.Stderr}
}

func writeGUIOwnerUnknownConfirmationMarkerContained(markerPath string, at time.Time) error {
	cleanPath := filepath.Clean(markerPath)
	stateDir := filepath.Dir(cleanPath)
	if markerPath == "" || !filepath.IsAbs(markerPath) || cleanPath != markerPath || filepath.Base(cleanPath) != guiOwnerUnknownConfirmationFileLeaf {
		return &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureInvalidInvocation, cause: errors.New("confirmation marker path is not the canonical leaf")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), guiOwnerUnknownConfirmationWriteBudget)
	defer cancel()
	return runGUIOwnerUnknownConfirmationMarker(ctx, stateDir, at)
}

func guiOwnerUnknownConfirmationFailureBody(err error, fields map[string]any) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	failure := guiOwnerUnknownConfirmationFailureStateWrite
	var workerErr *guiOwnerUnknownConfirmationWorkerError
	if errors.As(err, &workerErr) {
		failure = workerErr.failure
		fields["stdout_bytes"] = workerErr.stdout.Bytes
		fields["stdout_truncated"] = workerErr.stdout.Truncated
		fields["stderr_bytes"] = workerErr.stderr.Bytes
		fields["stderr_truncated"] = workerErr.stderr.Truncated
	}
	fields["failure_id"] = string(failure)
	fields["error"] = err.Error()
	return fields
}
