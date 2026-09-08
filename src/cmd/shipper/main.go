package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/cli"
	"github.com/QuesmaOrg/quesma-shipper/internal/platform"
)

// reportPanic is the defer's body, separated so a test can hand it a value. The stack goes to
// stderr and no further, being the one diagnostic that can carry payload-derived strings; the fact
// of the crash is persisted, so it outlives the terminal and rides the next heartbeat that ships.
func reportPanic(errOut io.Writer, args []string, r any) {
	fmt.Fprintf(errOut, "panic: %v\n\n%s", r, debug.Stack())
	app.RecordPanic(verbOf(args), r)
}

// verbOf names the verb for a persisted panic. The first non-flag argument, so `shipper -q sync`
// still reads as sync; "shipper" alone when there is none.
func verbOf(args []string) string {
	for _, a := range args[1:] {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return "shipper"
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			reportPanic(os.Stderr, os.Args, r)
			os.Exit(2)
		}
	}()
	if err := platform.ApplyMaxInFlightBytesFromEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", app.Name, err)
		os.Exit(1)
	}
	root := cli.Root(app.NewBuild(), os.Stdout, os.Stderr)
	err := root.ExecuteContext(context.Background())
	if code, show := cli.ExitCode(err); code != 0 {
		if show {
			fmt.Fprintln(os.Stderr, cli.ErrorLine(err, os.Stderr))
		}
		if code == 2 {
			fmt.Fprintln(os.Stderr, cli.HelpPointer(os.Stderr))
		}
		os.Exit(code)
	}
}
