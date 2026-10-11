// Package cli is the command-line front end. main only calls Run, so the whole command
// surface can be tested in-process with plain buffers and no os.Exit.
package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"

	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/version"
)

// IO is where a command reads and writes.
type IO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// say writes to the terminal. A failed write to stdout or stderr has nowhere useful to be
// reported, so it is deliberately dropped here, once, instead of at every call site.
func say(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

// globals are the flags every command takes (plan E.4).
type globals struct {
	file, home, root     string
	json, quiet, verbose bool
	yes                  bool
}

var globalNames = map[string]bool{"file": true, "home": true, "root": true, "json": true, "quiet": true, "verbose": true, "yes": true}

func (g *globals) define(fs *flag.FlagSet) {
	fs.StringVar(&g.file, "file", "", "the package file (default: $GREMLOADER_FILE, ./gremloader.toml, then the config folder)")
	fs.StringVar(&g.home, "home", "", "keep config, state and cache under this folder (like GREMLOADER_HOME)")
	fs.StringVar(&g.root, "root", "", "set the {root} variable of the package file")
	fs.BoolVar(&g.json, "json", false, "print JSON instead of text")
	fs.BoolVar(&g.quiet, "quiet", false, "print only results and errors")
	fs.BoolVar(&g.verbose, "verbose", false, "print more: notes, hook output")
	fs.BoolVar(&g.yes, "yes", false, "confirm the plan without asking (never overrides a safety flag)")
}

// call is one command being run.
type call struct {
	name string
	io   IO
	g    *globals
	d    deps
}

// usagef reports a wrong command line (exit 4).
func (c *call) usagef(format string, a ...any) int {
	msg := fmt.Sprintf(format, a...)
	say(c.io.Stderr, "gremloader %s: %s\nRun \"gremloader %s -h\" for help.\n", c.name, msg, c.name)
	if c.g.json {
		c.writeJSON(jsonOut{Command: c.name, ExitCode: exitcode.Usage, Error: msg})
	}
	return exitcode.Usage
}

// command is one entry of the command table. define registers the command's own flags on fs and
// returns the function that runs it with the positional arguments.
type command struct {
	name    string
	args    string // the positional arguments, for the usage line
	summary string
	define  func(fs *flag.FlagSet) func(c *call, args []string) int
}

// commands is filled in init: the help command lists the table, which would otherwise be an
// initialisation cycle.
var commands []*command

func init() {
	commands = []*command{
		{name: "list", summary: "List packages: installed, latest seen, status (no network)", define: defineList},
		{name: "check", args: "[package...]", summary: "Look for updates; exit 2 when there are some", define: defineCheck},
		{name: "plan", args: "[package...]", summary: "Show what apply would do, changing nothing", define: definePlan},
		{name: "apply", args: "[package...]", summary: "Install missing packages and update installed ones", define: defineApply},
		{name: "install", args: "<package>[@<version>]", summary: "Install one package", define: defineInstall},
		{name: "update", args: "[package...]", summary: "Update installed packages", define: defineUpdate},
		{name: "remove", args: "<package>", summary: "Uninstall a package (its own files only)", define: defineRemove},
		{name: "rollback", args: "<package>", summary: "Go back to the version before the last update", define: defineRollback},
		{name: "verify", args: "[package...]", summary: "Check installed files against what was installed (--remote: against the release)", define: defineVerify},
		{name: "adopt", args: "<package>", summary: "Record an existing folder as an installed package", define: defineAdopt},
		{name: "repair", summary: "Finish or undo an interrupted change (--forget <package>: give it up)", define: defineRepair},
		{name: "pin", args: "<package> <version>", summary: "Keep a package at one version", define: definePin},
		{name: "unpin", args: "<package>", summary: "Undo pin", define: defineUnpin},
		{name: "hold", args: "<package>", summary: "Leave a package out of automatic updates", define: defineHold},
		{name: "unhold", args: "<package>", summary: "Undo hold", define: defineUnhold},
		{name: "lock", args: "[package...]", summary: "Write gremloader.lock: the exact release of each package for each platform", define: defineLock},
		{name: "explain", args: "<package>", summary: "Show a package's effective settings and where each comes from", define: defineExplain},
		{name: "log", summary: "Show the event log", define: defineLog},
		{name: "self-update", summary: "Update GremLoader itself to its newest release (built by its own workflow, attested)", define: defineSelfUpdate},
		{name: "version", summary: "Print the version and build information", define: defineVersion},
		{name: "help", args: "[command]", summary: "Show this help, or a command's", define: defineHelp},
	}
}

func lookup(name string) *command {
	for _, c := range commands {
		if c.name == name {
			return c
		}
	}
	return nil
}

func usage() string {
	var b strings.Builder
	b.WriteString("GremLoader: install, update and log software packages.\n\nUsage:\n  gremloader <command> [arguments] [flags]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %-11s %s\n", c.name, c.summary)
	}
	b.WriteString("\nFlags may go anywhere on the line. Global flags:\n")
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	(&globals{}).define(fs)
	printFlags(&b, fs, func(n string) bool { return globalNames[n] })
	b.WriteString("\nRun \"gremloader <command> -h\" for a command's flags.\n")
	return b.String()
}

// printFlags lists the flags of fs that want selects, sorted by name.
func printFlags(w io.Writer, fs *flag.FlagSet, want func(name string) bool) {
	var lines []string
	fs.VisitAll(func(f *flag.Flag) {
		if !want(f.Name) {
			return
		}
		kind, text := flag.UnquoteUsage(f)
		if isBoolFlag(f) {
			kind = ""
		}
		head := "--" + f.Name
		if kind != "" {
			head += " " + kind
		}
		lines = append(lines, fmt.Sprintf("  %-24s %s\n", head, text))
	})
	slices.Sort(lines)
	for _, l := range lines {
		say(w, "%s", l)
	}
}

func commandHelp(w io.Writer, c *command, fs *flag.FlagSet) {
	say(w, "Usage:\n  gremloader %s", c.name)
	if c.args != "" {
		say(w, " %s", c.args)
	}
	say(w, " [flags]\n\n%s.\n", c.summary)
	var b bytes.Buffer
	printFlags(&b, fs, func(n string) bool { return !globalNames[n] })
	if b.Len() > 0 {
		say(w, "\nFlags:\n%s", b.String())
	}
	say(w, "\nGlobal flags: see \"gremloader help\".\n")
}

// Run executes one command line (args excludes the program name) and returns the exit code.
func Run(args []string, io IO) int {
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	ctx, stop := interruptible(sig, io.Stderr, os.Exit)
	defer stop()
	d := systemDeps()
	d.ctx = ctx
	return runWith(args, io, d)
}

func runWith(args []string, io IO, d deps) int {
	name, rest, ok := splitCommand(args)
	if !ok {
		say(io.Stderr, "%s", usage())
		return noCommand(io, d, args, "", "no command given")
	}
	cmd := lookup(name)
	if cmd == nil {
		say(io.Stderr, "gremloader: unknown command %q\n\n%s", name, usage())
		return noCommand(io, d, args, name, fmt.Sprintf("unknown command %q", name))
	}

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	var parseErr bytes.Buffer
	fs.SetOutput(&parseErr)
	fs.Usage = func() {} // help is printed below, to stdout when asked for
	g := &globals{}
	g.define(fs)
	runCmd := cmd.define(fs)
	if err := fs.Parse(sortArgs(fs, rest)); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			commandHelp(io.Stdout, cmd, fs)
			return exitcode.OK
		}
		// The parser stops at the first bad flag, which may come before --json: look for it on
		// the line so that the error is still one JSON object.
		g.json = g.json || wantsJSON(rest)
		return (&call{name: name, io: io, g: g, d: d}).usagef("%v", err)
	}
	return runCmd(&call{name: name, io: io, g: g, d: d}, fs.Args())
}

// noCommand ends a line that names no command it can run: the usage is already on stderr; with
// --json anywhere on the line, stdout still gets its one object.
func noCommand(io IO, d deps, args []string, name, msg string) int {
	if wantsJSON(args) {
		(&call{name: name, io: io, g: &globals{json: true}, d: d}).writeJSON(jsonOut{Command: name, ExitCode: exitcode.Usage, Error: msg})
	}
	return exitcode.Usage
}

func defineHelp(*flag.FlagSet) func(*call, []string) int {
	return func(c *call, args []string) int {
		switch len(args) {
		case 0:
			say(c.io.Stdout, "%s", usage())
			return exitcode.OK
		case 1:
			cmd := lookup(args[0])
			if cmd == nil {
				return c.usagef("unknown command %q", args[0])
			}
			fs := flag.NewFlagSet(cmd.name, flag.ContinueOnError)
			(&globals{}).define(fs)
			cmd.define(fs)
			commandHelp(c.io.Stdout, cmd, fs)
			return exitcode.OK
		default:
			return c.usagef("one command at a time")
		}
	}
}

func defineVersion(*flag.FlagSet) func(*call, []string) int {
	return func(c *call, args []string) int {
		if len(args) != 0 {
			return c.usagef("unexpected argument %q", args[0])
		}
		info := version.Get()
		if c.g.json {
			enc := json.NewEncoder(c.io.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(info); err != nil {
				say(c.io.Stderr, "gremloader version: %v\n", err)
				return exitcode.Failed
			}
			return exitcode.OK
		}
		say(c.io.Stdout, "%s\n", info)
		return exitcode.OK
	}
}
