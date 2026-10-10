// Package cli is the command-line front end. main only calls Run, so the whole command
// surface can be tested in-process with plain buffers and no os.Exit.
package cli

import (
	"encoding/json"
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

// Run executes one command line (args excludes the program name) and returns the exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitcode.Usage
	}

	switch args[0] {
	case "version", "--version":
		return runVersion(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitcode.OK
	default:
		fmt.Fprintf(stderr, "gremloader: unknown command %q\n\n%s", args[0], usage)
		return exitcode.Usage
	}
}

func runVersion(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the build information as JSON")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitcode.OK
		}
		return exitcode.Usage
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "gremloader version: unexpected argument %q\n", fs.Arg(0))
		return exitcode.Usage
	}

	info := version.Get()
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(info); err != nil {
			fmt.Fprintf(stderr, "gremloader version: %v\n", err)
			return exitcode.Failed
		}
		return exitcode.OK
	}
	fmt.Fprintln(stdout, info)
	return exitcode.OK
}
