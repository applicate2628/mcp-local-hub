package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/process"
)

// A preparation invocation receives the already composed closed environment
// and qualified file from the single input owner. It never resolves PATH,
// merges secrets, chooses a version or renews a work/cleanup budget.
type preparedSerenaInvocation struct {
	OperationID              string
	Executable               PreparedSerenaFile
	Args                     []string
	Environment              []string
	WorkingDir               string
	PolicySHA256             string
	CleanupLimit             time.Duration
	AbsoluteCleanupDeadline  time.Time
	StdoutLimit, StderrLimit int64
}
type preparedSerenaCommandResult struct {
	Closure PreparedSerenaExecutionClosure
	Receipt process.ContainedStreamReceiptV1
	// Private in-memory data for the bounded receipt/metadata parser only.
	Stdout, Stderr []byte
}

func runPreparedSerenaInvocation(ctx context.Context, r preparedSerenaInvocation) (result preparedSerenaCommandResult, err error) {
	if !preparedSerenaID(r.OperationID) || !preparedSerenaFile(r.Executable) || len(r.Environment) == 0 || !filepath.IsAbs(r.WorkingDir) || !preparedSerenaDigest(r.PolicySHA256) || r.StdoutLimit <= 0 || r.StderrLimit <= 0 {
		return result, errors.New("SERENA_PREPARED_INVOCATION_INVALID")
	}
	if err = revalidatePreparedSerenaFile(ctx, r.Executable); err != nil {
		return result, err
	}
	cmd := &exec.Cmd{Path: r.Executable.Path, Args: append([]string{r.Executable.Path}, r.Args...), Env: append([]string(nil), r.Environment...), Dir: r.WorkingDir}
	var stdout, stderr bytes.Buffer
	result.Receipt, err = process.RunContainedStreamObserved(ctx, cmd, process.ContainedStreamOptions{CleanupTimeout: r.CleanupLimit, AbsoluteCleanupDeadline: r.AbsoluteCleanupDeadline, StdoutByteLimit: r.StdoutLimit, StderrByteLimit: r.StderrLimit, Stderr: &stderr}, func(reader io.Reader) error { _, e := io.Copy(&stdout, reader); return e })
	// Do not access a borrowed sink while its worker remains unjoined.
	if result.Receipt.IOSettlement.Stdout.WorkerComplete {
		result.Stdout = append([]byte(nil), stdout.Bytes()...)
	}
	if result.Receipt.IOSettlement.Stderr.WorkerComplete {
		result.Stderr = append([]byte(nil), stderr.Bytes()...)
	}
	v := result.Receipt.NativeSettlement.Snapshot()
	result.Closure = PreparedSerenaExecutionClosure{nativeEvidence: result.Receipt.NativeSettlement, OriginalOperationID: r.OperationID, RootExitKnown: result.Receipt.RootWaitKnown, RootExitCode: result.Receipt.RootExitCode, MembersExited: v.MembersExited, HandlesClosed: v.HandlesClosed, IOClosed: result.Receipt.IOSettlement.Complete(), NoChildKnown: v.NoChildKnown, NoChildReleased: v.NoChildKnown && v.HandlesClosed}
	if !result.Receipt.NativeSettlement.Complete() {
		result.Closure.Pending = append(result.Closure.Pending, "original-native-settlement")
	}
	if !result.Receipt.IOSettlement.Complete() {
		result.Closure.Pending = append(result.Closure.Pending, "original-io-settlement")
	}
	// The safe projection binds source/input and actual outcomes without
	// exposing stream content, environment values or authority-bearing handles.
	summary := struct {
		OperationID, ExecutableSHA256, PolicySHA256, StdoutSHA256, StderrSHA256 string
		Stage                                                                   process.ContainedRunStage
		RootExitKnown                                                           bool
		RootExitCode                                                            int
		TerminalAction                                                          string
		NativeComplete, IOComplete                                              bool
	}{r.OperationID, r.Executable.SHA256, r.PolicySHA256, preparedSerenaHash(result.Stdout), preparedSerenaHash(result.Stderr), result.Receipt.Stage, result.Receipt.RootWaitKnown, result.Receipt.RootExitCode, result.Receipt.TerminalAction, result.Receipt.NativeSettlement.Complete(), result.Receipt.IOSettlement.Complete()}
	raw, encodeErr := json.Marshal(summary)
	if encodeErr == nil {
		result.Closure.ReceiptSHA256 = preparedSerenaHash(raw)
	}
	return result, errors.Join(err, encodeErr)
}
