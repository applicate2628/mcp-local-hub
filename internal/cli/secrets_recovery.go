package cli

import (
	"encoding/json"
	"errors"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

const (
	recoveryUsageInvalid    = "recovery_usage_invalid: invalid secrets recovery command or arguments; use --help."
	recoveryAttemptRequired = "recovery_attempt_id_required: inspect requires --attempt-id."
	recoveryInspectFailed   = "recovery_inspect_failed: unable to inspect recovery evidence; evidence or input is unavailable or invalid."
	recoveryListFailed      = "recovery_list_failed: unable to list recovery evidence; evidence is unavailable or invalid."
	recoveryOutputFailed    = "recovery_output_failed: unable to write recovery JSON output."
)

// recoveryPresentationError keeps operator-facing diagnostics fixed while
// preserving the original cause for internal error inspection.
type recoveryPresentationError struct {
	message string
	cause   error
}

func (e *recoveryPresentationError) Error() string { return e.message }
func (e *recoveryPresentationError) Unwrap() error { return e.cause }

type secretsRecoveryReader interface {
	ListSecretsInitRecovery() ([]api.SecretsInitRecoverySummary, error)
	ReadSecretsInitRecovery(string) (api.SecretsInitRecoveryReport, error)
}

func newSecretsRecoveryCmd() *cobra.Command {
	return newSecretsRecoveryCmdWithReader(&api.API{})
}

func newSecretsRecoveryCmdWithReader(reader secretsRecoveryReader) *cobra.Command {
	noArgs := func(cmd *cobra.Command, args []string) error {
		if err := cobra.NoArgs(cmd, args); err != nil {
			return &recoveryPresentationError{recoveryUsageInvalid, err}
		}
		return nil
	}
	root := &cobra.Command{
		Use:   "recovery",
		Short: "Inspect historical secrets initialization evidence (read-only)",
		Long:  "Read historical initialization evidence only. Reports do not inspect current targets or authorize recovery actions.",
		Args:  noArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &recoveryPresentationError{recoveryUsageInvalid, err}
	})
	list := &cobra.Command{
		Use:   "list",
		Short: "List historical attempt identifiers and states",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := reader.ListSecretsInitRecovery()
			if err != nil {
				return &recoveryPresentationError{recoveryListFailed, err}
			}
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
				return &recoveryPresentationError{recoveryOutputFailed, err}
			}
			return nil
		},
	}
	var attemptID string
	inspect := &cobra.Command{
		Use:   "inspect --attempt-id <id>",
		Short: "Inspect one historical attempt without inspecting current targets",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !cmd.Flags().Changed("attempt-id") {
				return &recoveryPresentationError{recoveryAttemptRequired, errors.New("required attempt-id flag is absent")}
			}
			result, err := reader.ReadSecretsInitRecovery(attemptID)
			if err != nil {
				return &recoveryPresentationError{recoveryInspectFailed, err}
			}
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
				return &recoveryPresentationError{recoveryOutputFailed, err}
			}
			return nil
		},
	}
	inspect.Flags().StringVar(&attemptID, "attempt-id", "", "historical attempt identifier")
	root.AddCommand(list, inspect)
	return root
}
