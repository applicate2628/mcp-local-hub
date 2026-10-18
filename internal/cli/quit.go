package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/gui"

	"github.com/spf13/cobra"
)

func newQuitCmd() *cobra.Command {
	var stopDaemons bool
	command := &cobra.Command{
		Use:   "quit",
		Short: "Intentionally quit the GUI; keep it stopped until gui --resume",
		Long:  "Cooperatively quit the original native GUI generation. Intentional Quit survives automatic ticks and logon until mcphub gui --resume. This does not disable logon autorun. The GUI-owned supervisor retains its existing shutdown contract; adopted independent daemons are not broadly stopped unless --stop-daemons is supplied. HTTP acceptance alone is not settled success.",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runGUIQuit(ctx, command.OutOrStdout(), stopDaemons)
		},
	}
	command.Flags().BoolVar(&stopDaemons, "stop-daemons", false, "also stop all Hub-registered daemons through the existing stop API")
	return command
}

// runGUIQuit consumes the real caller/control budget. It never redirects,
// retries, terminates a PID, or substitutes an observed successor generation.
func runGUIQuit(ctx context.Context, out io.Writer, stopDaemons bool) (err error) {
	if ctx == nil {
		return errors.New("GUI_QUIT_CONTEXT_REQUIRED")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := gui.PidportPath()
	if err != nil {
		return err
	}
	absent, err := gui.WithAbsentGUIOwner(ctx, path, func() error {
		cause := "cli-quit"
		if stopDaemons {
			cause = "cli-quit-and-stop-all"
		}
		result, policyErr := api.NewAPI().RecordGUIStop(ctx, "", cause)
		if !result.Applied {
			return errors.Join(policyErr, errors.New("GUI_QUIT_POLICY_NOT_RECEIVED"))
		}
		if !result.NewlyStopped && stopDaemons {
			if !result.Control.GUIStopIncludesDaemons() {
				return errors.Join(policyErr, gui.ErrGUIQuitBreadthConflict)
			}
			return errors.Join(policyErr, gui.ErrGUIQuitDaemonOutcomeUnconfirmed)
		}
		var daemonErr error
		if result.NewlyStopped && result.Control.GUIStopIncludesDaemons() {
			daemonErr = stopGUIRequestedDaemons()
		}
		_, outputErr := fmt.Fprintln(out, "GUI already stopped; automatic startup remains stopped until mcphub gui --resume")
		return errors.Join(policyErr, daemonErr, outputErr)
	})
	if err != nil || absent {
		return err
	}
	record, identity, err := gui.AcquireGUIQuitObservation(ctx, path)
	defer func() { err = errors.Join(err, identity.Close()) }()
	if err != nil {
		return err
	}
	request := gui.GUIQuitRequest{ExpectedGeneration: record.Generation, StopDaemons: stopDaemons}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/gui/quit", record.Port)
	message, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	message.Header.Set("Content-Type", "application/json")
	message.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", record.Port))
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("GUI_QUIT_REDIRECT_REFUSED") }}
	defer client.CloseIdleConnections()
	response, err := client.Do(message)
	if err != nil {
		return fmt.Errorf("GUI_QUIT_ACCEPTANCE_UNCONFIRMED: %w", err)
	}
	var reply struct {
		gui.GUIQuitResult
		Code  string `json:"code,omitempty"`
		Error string `json:"error,omitempty"`
	}
	decoder := json.NewDecoder(response.Body)
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(&reply)
	if decodeErr == nil {
		var trailing any
		if tailErr := decoder.Decode(&trailing); tailErr != io.EOF {
			decodeErr = errors.Join(tailErr, errors.New("GUI_QUIT_INVALID_RESPONSE"))
		}
	}
	closeErr := response.Body.Close()
	var remoteErr error
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusMethodNotAllowed {
		remoteErr = errors.New("GUI_QUIT_UNSUPPORTED_INCUMBENT")
	} else if response.StatusCode != http.StatusAccepted {
		remoteErr = fmt.Errorf("GUI_QUIT_REMOTE_FAILURE: HTTP %d", response.StatusCode)
	}
	err = errors.Join(remoteErr, decodeErr, closeErr)
	if !reply.Accepted || reply.Generation != record.Generation {
		return errors.Join(err, errors.New("GUI_QUIT_ACCEPTANCE_UNCONFIRMED"))
	}
	_, outputErr := fmt.Fprintf(out, "GUI Quit accepted for generation %s; awaiting original shutdown\n", record.Generation)
	err = errors.Join(err, outputErr)
	// Even a partial/error response retains accepted intent and must receive
	// actual original settlement. A malformed response never becomes success.
	exitCode, waitErr := identity.Wait(ctx)
	err = errors.Join(err, waitErr)
	if waitErr != nil {
		return errors.Join(err, gui.ErrGUIQuitSettlementUnconfirmed)
	}
	if exitCode != 0 {
		err = errors.Join(err, fmt.Errorf("GUI_QUIT_NATIVE_EXIT: %d", exitCode))
	}
	settleErr := gui.VerifyGUIQuitSettlement(path, record)
	err = errors.Join(err, settleErr)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "GUI quit and original native generation settled; resume with mcphub gui --resume")
	return err
}
