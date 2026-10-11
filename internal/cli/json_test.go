package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/testutil/archives"
)

// With --json every command prints exactly one JSON object on stdout, whose command and exit code
// are the run's, in success, failure and usage errors alike (plan E.4: a versioned schema).
func TestEveryCommandPrintsOneJSONObject(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	r.publish("2.0.0", 5, archives.F("app.exe", "2"))

	for _, line := range [][]string{
		{"list"},
		{"check"},
		{"plan"},
		{"install", "app@1.0.0", "--yes"},
		{"update", "--yes"},
		{"rollback", "app", "--yes"},
		{"verify"},
		{"verify", "--remote"},
		{"explain", "app"},
		{"log", "--pkg", "app"},
		{"pin", "app", "1.0.0"},
		{"unpin", "app"},
		{"hold", "app"},
		{"unhold", "app"},
		{"lock"},
		{"repair"},
		{"apply", "--yes"},
		{"adopt", "app"}, // fails: already installed
		{"remove", "app", "--yes"},
		{"repair", "--forget", "app", "--yes"}, // fails: nothing to forget
		{"install"},                            // usage error
		{"install", "app", "--bogus"},          // usage error from the flag parser
		{"check", "nope"},                      // unknown package
		{"apply"},                              // no terminal, no --yes
	} {
		code, out, errOut := r.run(append(line, "--json")...)
		dec := json.NewDecoder(strings.NewReader(out))
		var res jsonResult
		if err := dec.Decode(&res); err != nil {
			t.Errorf("%q: stdout is not JSON (%v):\n%s\nstderr: %s", line, err, out, errOut)
			continue
		}
		if dec.More() {
			t.Errorf("%q: more than one JSON value on stdout:\n%s", line, out)
		}
		if res.SchemaVersion != jsonSchema || res.Command != line[0] || res.ExitCode != code {
			t.Errorf("%q: schema %d command %q exit %d, process exit %d", line, res.SchemaVersion, res.Command, res.ExitCode, code)
		}
		if code != exitcode.OK && code != exitcode.UpdatesAvailable && res.Error == "" && len(res.Items) == 0 {
			t.Errorf("%q: exit %d with neither an error nor items", line, code)
		}
	}
}

// --json never mixes human text into stdout, also when a plan is waiting for confirmation.
func TestJSONStdoutIsOnlyJSON(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("1.0.0", 10, archives.F("app.exe", "1"))
	r.tty, r.stdin = true, "y\n"
	code, out, _ := r.run("install", "app", "--json")
	if code != exitcode.OK || !bytes.HasPrefix(bytes.TrimSpace([]byte(out)), []byte("{")) || strings.Count(out, `"schema_version"`) != 1 {
		t.Errorf("exit %d, stdout:\n%s", code, out)
	}
}
