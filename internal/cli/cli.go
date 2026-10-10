// Package cli is the command-line front end. main only calls Run, so the whole command
// surface can be tested in-process with plain buffers and no os.Exit.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/version"
)

const usage = `GremLoader: install, update and log software packages.

Usage:
  gremloader <command> [flags]

Commands:
  version    Print the version and build information
  help       Show this help

Run "gremloader <command> -h" for a command's flags.
`

// say writes to the terminal. A failed write to stdout or stderr has nowhere useful to be
// reported, so it is deliberately dropped here, once, instead of at every call site.
func say(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

// Run executes one command line (args excludes the program name) and returns the exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		say(stderr, "%s", usage)
		return exitcode.Usage
	}

	switch args[0] {
	case "version", "--version":
		return runVersion(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		say(stdout, "%s", usage)
		return exitcode.OK
	default:
		say(stderr, "gremloader: unknown command %q\n\n%s", args[0], usage)
		return exitcode.Usage
	}
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the build information as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitcode.OK
		}
		return exitcode.Usage
	}
	if fs.NArg() != 0 {
		say(stderr, "gremloader version: unexpected argument %q\n", fs.Arg(0))
		return exitcode.Usage
	}

	info := version.Get()
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info); err != nil {
			say(stderr, "gremloader version: %v\n", err)
			return exitcode.Failed
		}
		return exitcode.OK
	}
	say(stdout, "%s\n", info)
	return exitcode.OK
}
