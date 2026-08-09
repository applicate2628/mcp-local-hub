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

	"github.com/gofrs/flock"
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
	guiOwnerUnknownConfirmationFailureStateRead         guiOwnerUnknownConfirmationWorkerFailure = "state_read_failed"
	guiOwnerUnknownConfirmationFailureStateWrite        guiOwnerUnknownConfirmationWorkerFailure = "state_write_failed"
)

const (
	guiOwnerUnknownConfirmationOperationWrite   = "write"
	guiOwnerUnknownConfirmationOperationConsume = "consume_elapsed"
)

type guiOwnerUnknownConfirmationWorkerRequest struct {
	Version    int       `json:"version"`
	Operation  string    `json:"operation,omitempty"`
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
	operation := request.Operation
	if operation == "" {
		operation = guiOwnerUnknownConfirmationOperationWrite
	}
	if operation == guiOwnerUnknownConfirmationOperationConsume {
		result.Outcome, result.Failure = consumeGUIOwnerUnknownConfirmationMarkerLockHeld(markerPath, request.ObservedAt)
		return writeGUIOwnerUnknownConfirmationWorkerResult(out, result)
	}
	if operation != guiOwnerUnknownConfirmationOperationWrite {
		result.Outcome, result.Failure = "rejected", string(guiOwnerUnknownConfirmationFailureProtocol)
		return writeGUIOwnerUnknownConfirmationWorkerResult(out, result)
	}
	if err := api.WriteStateFileBytesAtomic(markerPath, []byte(request.ObservedAt.Format(time.RFC3339Nano))); err != nil {
		result.Outcome, result.Failure = "failed", string(guiOwnerUnknownConfirmationFailureStateWrite)
		return writeGUIOwnerUnknownConfirmationWorkerResult(out, result)
	}
	result.Outcome = "written"
	return writeGUIOwnerUnknownConfirmationWorkerResult(out, result)
}

// consumeGUIOwnerUnknownConfirmationMarkerLockHeld is the cross-process
// authorization point for escalation. The elapsed check and removal share the
// marker's canonical writer flock, so two overlapping ensure-alive processes
// cannot both consume one confirmation window.
func consumeGUIOwnerUnknownConfirmationMarkerLockHeld(markerPath string, observedAt time.Time) (outcome, failure string) {
	lk := flock.New(markerPath + ".lock")
	if err := lk.Lock(); err != nil {
		_ = lk.Close()
		return "failed", string(guiOwnerUnknownConfirmationFailureStateRead)
	}
	defer func() {
		if err := lk.Close(); err != nil {
			outcome, failure = "failed", string(guiOwnerUnknownConfirmationFailureStateWrite)
		}
	}()
	raw, err := api.ReadStateFileInodeAnchored(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		return "not_consumed", ""
	}
	if err != nil {
		return "failed", string(guiOwnerUnknownConfirmationFailureStateRead)
	}
	firstObserved, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		return "failed", string(guiOwnerUnknownConfirmationFailureStateRead)
	}
	age := observedAt.UTC().Sub(firstObserved.UTC())
	if age < guiOwnerUnknownConfirmationWindow {
		return "not_consumed", ""
	}
	if err := os.Remove(markerPath); errors.Is(err, os.ErrNotExist) {
		return "not_consumed", ""
	} else if err != nil {
		return "failed", string(guiOwnerUnknownConfirmationFailureStateWrite)
	}
	return "consumed", ""
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
	workerResult, err := runGUIOwnerUnknownConfirmationMarkerOperation(ctx, stateDir, observedAt, guiOwnerUnknownConfirmationOperationWrite)
	if err != nil {
		return err
	}
	if workerResult.Outcome != "written" || workerResult.Failure != "" {
		return &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: errors.New("confirmation marker result contradicts write protocol")}
	}
	return nil
}

func consumeGUIOwnerUnknownConfirmationMarker(ctx context.Context, stateDir string, observedAt time.Time) (bool, error) {
	workerResult, err := runGUIOwnerUnknownConfirmationMarkerOperation(ctx, stateDir, observedAt, guiOwnerUnknownConfirmationOperationConsume)
	if err != nil {
		return false, err
	}
	switch workerResult.Outcome {
	case "consumed":
		return true, nil
	case "not_consumed":
		return false, nil
	default:
		return false, &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: errors.New("confirmation marker result contradicts consume protocol")}
	}
}

func runGUIOwnerUnknownConfirmationMarkerOperation(ctx context.Context, stateDir string, observedAt time.Time, operation string) (guiOwnerUnknownConfirmationWorkerResult, error) {
	payload, err := json.Marshal(guiOwnerUnknownConfirmationWorkerRequest{
		Version: guiOwnerUnknownConfirmationWorkerVersion, Operation: operation, StateDir: stateDir, ObservedAt: observedAt.UTC(),
	})
	if err != nil || len(payload) > guiOwnerUnknownConfirmationWorkerMessageMax {
		return guiOwnerUnknownConfirmationWorkerResult{}, &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: err}
	}
	exe, err := os.Executable()
	if err != nil {
		return guiOwnerUnknownConfirmationWorkerResult{}, &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureExecution, cause: err}
	}
	cmd := exec.Command(exe, "gui-owner-unknown-confirmation-worker")
	result, runErr := process.RunStrictlyContained(ctx, process.StrictRunInvocation{
		Command: cmd,
		Input:   payload, InputLimit: guiOwnerUnknownConfirmationWorkerMessageMax,
		StdoutLimit: guiOwnerUnknownConfirmationWorkerMessageMax, StderrLimit: guiOwnerUnknownConfirmationWorkerStderrMax,
	})
	if runErr != nil {
		return guiOwnerUnknownConfirmationWorkerResult{}, newGUIOwnerUnknownConfirmationWorkerRunError(cmd, result, runErr)
	}
	return decodeGUIOwnerUnknownConfirmationWorkerResult(result)
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
	workerResult, err := decodeGUIOwnerUnknownConfirmationWorkerResult(result)
	if err != nil {
		return err
	}
	if workerResult.Outcome == "written" && workerResult.Failure == "" {
		return nil
	}
	return &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: errors.New("confirmation marker result contradicts protocol"), stdout: result.Stdout, stderr: result.Stderr}
}

func decodeGUIOwnerUnknownConfirmationWorkerResult(result process.StrictRunResult) (guiOwnerUnknownConfirmationWorkerResult, error) {
	if result.Stdout.Truncated {
		return guiOwnerUnknownConfirmationWorkerResult{}, &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: errors.New("confirmation marker result exceeds bound"), stdout: result.Stdout, stderr: result.Stderr}
	}
	var workerResult guiOwnerUnknownConfirmationWorkerResult
	decoder := json.NewDecoder(bytes.NewReader(result.Stdout.Prefix))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&workerResult); err != nil {
		return workerResult, &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: err, stdout: result.Stdout, stderr: result.Stderr}
	}
	if err := ensureGUIOwnerUnknownConfirmationWorkerEOF(decoder); err != nil {
		return workerResult, &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: err, stdout: result.Stdout, stderr: result.Stderr}
	}
	if workerResult.Version != guiOwnerUnknownConfirmationWorkerVersion {
		return workerResult, &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: errors.New("confirmation marker result version mismatch"), stdout: result.Stdout, stderr: result.Stderr}
	}
	if workerResult.Failure == "" && (workerResult.Outcome == "written" || workerResult.Outcome == "consumed" || workerResult.Outcome == "not_consumed") {
		return workerResult, nil
	}
	if workerResult.Outcome == "failed" && (workerResult.Failure == string(guiOwnerUnknownConfirmationFailureStateWrite) || workerResult.Failure == string(guiOwnerUnknownConfirmationFailureStateRead)) {
		return workerResult, &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationWorkerFailure(workerResult.Failure), stdout: result.Stdout, stderr: result.Stderr}
	}
	return workerResult, &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureProtocol, cause: errors.New("confirmation marker result contradicts protocol"), stdout: result.Stdout, stderr: result.Stderr}
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

func consumeGUIOwnerUnknownConfirmationMarkerContained(markerPath string, at time.Time) (bool, error) {
	cleanPath := filepath.Clean(markerPath)
	stateDir := filepath.Dir(cleanPath)
	if markerPath == "" || !filepath.IsAbs(markerPath) || cleanPath != markerPath || filepath.Base(cleanPath) != guiOwnerUnknownConfirmationFileLeaf {
		return false, &guiOwnerUnknownConfirmationWorkerError{failure: guiOwnerUnknownConfirmationFailureInvalidInvocation, cause: errors.New("confirmation marker path is not the canonical leaf")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), guiOwnerUnknownConfirmationWriteBudget)
	defer cancel()
	return consumeGUIOwnerUnknownConfirmationMarker(ctx, stateDir, at)
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
