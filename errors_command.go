package main

// `freecad-mcp errors`: the recent failed tool calls, from the error log the
// MCP server appends to (internal/mcpserver/errorlog.go).

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sairaph/mcp-wizard/command"

	"github.com/sairaph/freecad-mcp/internal/mcpserver"
)

const defaultErrorsShown = 50

func registerErrorsCommand(r *command.Registry) {
	r.Register(command.Handler{
		Name:        "errors",
		Description: "Show the most recent failed tool calls from the error log",
		Usage:       "errors [--last N]",
		Run: func(_ context.Context, args []string) int {
			return runErrors(args, os.Stdout, os.Stderr)
		},
	})
}

// runErrors prints the last N entries of the error log, oldest first.
func runErrors(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("errors", flag.ContinueOnError)
	fs.SetOutput(stderr)
	last := fs.Int("last", defaultErrorsShown, "how many of the most recent entries to show")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "  errors: unexpected argument %q; the only option is --last\n", fs.Arg(0))
		return 2
	}
	if *last < 1 {
		fmt.Fprintln(stderr, "  errors: --last must be at least 1")
		return 2
	}
	lines, err := mcpserver.RecentErrors(*last)
	if err != nil {
		fmt.Fprintf(stderr, "  errors: could not read the error log: %v\n", err)
		return 1
	}
	if len(lines) == 0 {
		path, _ := mcpserver.ErrorLogPath()
		fmt.Fprintf(stdout, "  No failed tool calls are logged yet (%s).\n", path)
		return 0
	}
	for _, line := range lines {
		fmt.Fprintln(stdout, line)
	}
	return 0
}
