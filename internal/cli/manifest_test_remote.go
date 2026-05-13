package cli

import (
	"context"
	"fmt"
	"time"

	"mcp-local-hub/internal/api"

	"github.com/spf13/cobra"
)

func newManifestTestRemoteCmd() *cobra.Command {
	var timeoutSec int
	c := &cobra.Command{
		Use:   "test-remote <name>",
		Short: "Send a one-shot MCP initialize to a remote-http manifest's URL",
		Long: `Smoke-check a remote-http manifest without installing it.

Loads the manifest, expands ${secret:KEY} placeholders in url and
headers, then POSTs an MCP 'initialize' JSON-RPC request to the
expanded URL with the expanded headers. Prints the upstream's
protocolVersion + serverInfo on success, or surfaces the error.

No client config writes, no scheduler tasks, no backups — purely a
network smoke check. Use this to confirm reachability and credentials
BEFORE running 'mcphub install --server <name>'.

Manifest must have transport: remote-http. Other transports are
rejected with a clear error.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), time.Duration(timeoutSec)*time.Second)
			defer cancel()
			a := api.NewAPI()
			res, err := a.ManifestTestRemote(ctx, args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "✓ %s reachable\n", args[0])
			if res.ProtocolVersion != "" {
				fmt.Fprintf(out, "  protocolVersion: %s\n", res.ProtocolVersion)
			}
			if res.ServerName != "" || res.ServerVersion != "" {
				fmt.Fprintf(out, "  serverInfo:      %s %s\n", res.ServerName, res.ServerVersion)
			}
			if len(res.Capabilities) > 0 {
				keys := make([]string, 0, len(res.Capabilities))
				for k := range res.Capabilities {
					keys = append(keys, k)
				}
				fmt.Fprintf(out, "  capabilities:    %v\n", keys)
			}
			return nil
		},
	}
	c.Flags().IntVar(&timeoutSec, "timeout", 20, "request timeout in seconds")
	return c
}
