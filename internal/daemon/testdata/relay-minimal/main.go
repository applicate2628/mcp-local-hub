// Isolated measurement control, not an installed product command.
package main

import (
	"context"
	"flag"
	"fmt"
	"mcp-local-hub/internal/daemon"
	"os"
	"os/signal"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "relay" {
		args = args[1:]
	}
	flags := flag.NewFlagSet("rx-relay-min", flag.ContinueOnError)
	url := flags.String("url", "", "owned loopback fixture endpoint")
	if err := flags.Parse(args); err != nil || *url == "" || flags.NArg() != 0 {
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	relay := daemon.HTTPToStdioRelay{URL: *url, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	if err := relay.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
