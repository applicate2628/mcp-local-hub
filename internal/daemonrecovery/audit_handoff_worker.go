package daemonrecovery

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

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

const (
	CommittedAuditHandoffWorkerCommand = "daemon-recovery-audit-handoff-worker"
	committedAuditHandoffProtocol      = 1
	committedAuditHandoffMessageMax    = 64 << 10
	committedAuditHandoffStderrMax     = 16 << 10
)

type committedAuditHandoffRequest struct {
	Version  int    `json:"version"`
	StateDir string `json:"state_dir"`
	Prepared []byte `json:"prepared"`
	Replay   bool   `json:"replay"`
}

type committedAuditHandoffResult struct {
	Version int    `json:"version"`
	Outcome string `json:"outcome"`
}

type committedAuditHandoffPersist func(context.Context, string, api.PreparedSupervisorEvent, bool) error

func persistCommittedAuditHandoffBounded(ctx context.Context, stateDir string, prepared api.PreparedSupervisorEvent, replay bool) error {
	raw, err := api.PreparedSupervisorEventBytes(prepared)
	if err != nil {
		return fmt.Errorf("encode committed audit handoff: %w", err)
	}
	payload, err := json.Marshal(committedAuditHandoffRequest{Version: committedAuditHandoffProtocol, StateDir: stateDir, Prepared: raw, Replay: replay})
	if err != nil {
		return fmt.Errorf("encode bounded committed audit handoff request: %w", err)
	}
	if len(payload) > committedAuditHandoffMessageMax {
		return errors.New("bounded committed audit handoff request exceeds limit")
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve committed audit handoff worker: %w", err)
	}
	run, err := process.RunStrictlyContained(ctx, process.StrictRunInvocation{
		Command:     exec.Command(executable, CommittedAuditHandoffWorkerCommand),
		Input:       payload,
		InputLimit:  committedAuditHandoffMessageMax,
		StdoutLimit: committedAuditHandoffMessageMax,
		StderrLimit: committedAuditHandoffStderrMax,
	})
	if err != nil {
		return fmt.Errorf("run committed audit handoff worker: %w", err)
	}
	if run.Stdout.Truncated {
		return errors.New("committed audit handoff worker result exceeds bound")
	}
	var result committedAuditHandoffResult
	decoder := json.NewDecoder(bytes.NewReader(run.Stdout.Prefix))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return fmt.Errorf("decode committed audit handoff worker result: %w", err)
	}
	if err := ensureCommittedAuditHandoffEOF(decoder); err != nil {
		return err
	}
	if result.Version != committedAuditHandoffProtocol || result.Outcome != "durable" {
		return errors.New("committed audit handoff worker did not prove durability")
	}
	return nil
}

func persistCommittedAuditHandoffDirect(_ context.Context, stateDir string, prepared api.PreparedSupervisorEvent, replay bool) error {
	logger, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		return err
	}
	defer func() { _ = logger.Close() }()
	if err := logger.PersistPending(prepared); err != nil {
		return err
	}
	if replay {
		_ = logger.TryReplayPending()
	}
	return nil
}

// RunCommittedAuditHandoffWorker is the bounded same-binary leaf. The parent
// owns containment and deadline; this child alone owns the potentially blocking
// pending-carrier filesystem operation.
func RunCommittedAuditHandoffWorker(in io.Reader, out io.Writer) error {
	limited := io.LimitReader(in, committedAuditHandoffMessageMax+1)
	payload, err := io.ReadAll(limited)
	if err != nil || len(payload) > committedAuditHandoffMessageMax {
		return errors.New("invalid committed audit handoff request")
	}
	var request committedAuditHandoffRequest
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return errors.New("invalid committed audit handoff request")
	}
	if err := ensureCommittedAuditHandoffEOF(decoder); err != nil || request.Version != committedAuditHandoffProtocol || !filepath.IsAbs(request.StateDir) {
		return errors.New("invalid committed audit handoff request")
	}
	prepared, err := api.PreparedSupervisorEventFromBytes(request.Prepared)
	if err != nil {
		return errors.New("invalid committed audit handoff record")
	}
	outcome := "durable"
	if err := persistCommittedAuditHandoffDirect(context.Background(), request.StateDir, prepared, request.Replay); err != nil {
		outcome = "failed"
	}
	return json.NewEncoder(out).Encode(committedAuditHandoffResult{Version: committedAuditHandoffProtocol, Outcome: outcome})
}

func ensureCommittedAuditHandoffEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("committed audit handoff protocol has trailing data")
	}
	return nil
}
