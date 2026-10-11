package cli

import (
	"flag"
	"strings"
)

// The standard flag package stops at the first argument that is not a flag, so
// `gremloader update photocraft --yes` would take "--yes" for a package name (plan P2-20). These
// helpers put every flag ahead of the positional arguments first, so flags may go anywhere.

// globalValueFlags are the global flags that take a value; splitCommand must skip their values
// to find the command name.
var globalValueFlags = map[string]bool{"file": true, "home": true, "root": true}

// splitCommand finds the command name: the first argument that is neither a flag nor the value
// of a global flag. A help or version flag before any command is the command. The other
// arguments, flags included, are returned in their order.
func splitCommand(args []string) (name string, rest []string, ok bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			if i+1 < len(args) {
				return args[i+1], append(append([]string{}, args[:i]...), args[i+2:]...), true
			}
			return "", nil, false
		case a == "-h" || a == "--help" || a == "-help":
			return "help", append(append([]string{}, args[:i]...), args[i+1:]...), true
		case a == "--version" || a == "-version":
			return "version", append(append([]string{}, args[:i]...), args[i+1:]...), true
		case isFlag(a):
			if n, hasValue := flagName(a); globalValueFlags[n] && !hasValue {
				i++ // its value
			}
		default:
			return a, append(append([]string{}, args[:i]...), args[i+1:]...), true
		}
	}
	return "", nil, false
}

// sortArgs returns args with every flag (and its value) first, then "--", then the positional
// arguments in their order. fs says which flags take a value. An unknown flag is kept as a flag
// so that fs.Parse reports it by name. Everything after "--" is positional.
func sortArgs(fs *flag.FlagSet, args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !isFlag(a) {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		n, hasValue := flagName(a)
		if f := fs.Lookup(n); f != nil && !hasValue && !isBoolFlag(f) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(append(flags, "--"), pos...)
}

// isFlag: "-x", "--x", "--x=v". A lone "-" is an argument (it usually means stdin).
func isFlag(a string) bool { return len(a) > 1 && a[0] == '-' }

// flagName returns the flag's name without dashes and whether the value is attached ("--x=v").
func flagName(a string) (name string, hasValue bool) {
	name = strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-")
	if i := strings.IndexByte(name, '='); i >= 0 {
		return name[:i], true
	}
	return name, false
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}
