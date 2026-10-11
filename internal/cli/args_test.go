package cli

import (
	"flag"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/exitcode"
)

func testFlags() *flag.FlagSet {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	(&globals{}).define(fs)
	fs.String("version", "", "a value flag")
	fs.Bool("force", false, "a bool flag")
	return fs
}

func TestSortArgs(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", "--"},
		{"photocraft --yes", "--yes -- photocraft"},
		{"--yes photocraft", "--yes -- photocraft"},
		{"a --force b --yes c", "--force --yes -- a b c"},
		{"photocraft --version 1.2.3", "--version 1.2.3 -- photocraft"},
		{"photocraft --version=1.2.3", "--version=1.2.3 -- photocraft"},
		{"photocraft -version 1.2.3 -yes", "-version 1.2.3 -yes -- photocraft"},
		{"--file f.toml --root C:/x list", "--file f.toml --root C:/x -- list"},
		{"a -- --yes b", "-- a --yes b"},
		{"--force=true a", "--force=true -- a"},
		{"a - b", "-- a - b"},
		{"--bogus a", "--bogus -- a"},                 // unknown: kept as a flag so Parse names it
		{"a --version", "--version -- a"},             // value missing: Parse reports it
		{"--version --yes a", "--version --yes -- a"}, // the flag package takes "--yes" as the value too
		{"--yes --yes a", "--yes --yes -- a"},
	} {
		var in []string
		if tc.in != "" {
			in = strings.Fields(tc.in)
		}
		got := strings.Join(sortArgs(testFlags(), in), " ")
		if got != tc.want {
			t.Errorf("sortArgs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The sorted line parses the way the user meant it.
func TestSortedArgsParse(t *testing.T) {
	fs := testFlags()
	if err := fs.Parse(sortArgs(fs, strings.Fields("update photocraft --yes --version 2.0.0 extra"))); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fs.Args(), []string{"update", "photocraft", "extra"}) {
		t.Errorf("args = %q", fs.Args())
	}
	if fs.Lookup("yes").Value.String() != "true" || fs.Lookup("version").Value.String() != "2.0.0" {
		t.Errorf("yes = %s, version = %s", fs.Lookup("yes").Value, fs.Lookup("version").Value)
	}
}

func TestSplitCommand(t *testing.T) {
	for _, tc := range []struct {
		in, name, rest string
		ok             bool
	}{
		{"", "", "", false},
		{"list", "list", "", true},
		{"--json list", "list", "--json", true},
		{"--file f.toml list --yes", "list", "--file f.toml --yes", true},
		{"--file=f.toml list", "list", "--file=f.toml", true},
		{"--root C:/x --home h plan a", "plan", "--root C:/x --home h a", true},
		{"--yes", "", "", false},
		{"-h", "help", "", true},
		{"--json --help", "help", "--json", true},
		{"--version", "version", "", true},
		{"--bogus list", "list", "--bogus", true},
	} {
		var in []string
		if tc.in != "" {
			in = strings.Fields(tc.in)
		}
		name, rest, ok := splitCommand(in)
		if name != tc.name || strings.Join(rest, " ") != tc.rest || ok != tc.ok {
			t.Errorf("splitCommand(%q) = %q, %q, %v; want %q, %q, %v", tc.in, name, strings.Join(rest, " "), ok, tc.name, tc.rest, tc.ok)
		}
	}
}

func TestFlagsAnywhereReachTheCommand(t *testing.T) {
	for _, line := range [][]string{
		{"--json", "version"},
		{"version", "--json"},
		{"version", "-json"},
	} {
		code, out, errOut := run(t, line...)
		if code != exitcode.OK || !strings.HasPrefix(out, "{") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", line, code, out, errOut)
		}
	}
}

func TestCommandHelp(t *testing.T) {
	for _, line := range [][]string{{"version", "-h"}, {"version", "--help"}, {"help", "version"}} {
		code, out, errOut := run(t, line...)
		if code != exitcode.OK || !strings.Contains(out, "gremloader version") || errOut != "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", line, code, out, errOut)
		}
	}
	code, _, errOut := run(t, "help", "frobnicate")
	if code != exitcode.Usage || !strings.Contains(errOut, `unknown command "frobnicate"`) {
		t.Errorf("help frobnicate: exit %d, stderr %q", code, errOut)
	}
}

func TestUsageListsEveryCommandAndGlobalFlag(t *testing.T) {
	u := usage()
	for _, c := range commands {
		if !strings.Contains(u, "  "+c.name+" ") {
			t.Errorf("usage does not list %q", c.name)
		}
	}
	for n := range globalNames {
		if !strings.Contains(u, "--"+n) {
			t.Errorf("usage does not list --%s", n)
		}
	}
}
