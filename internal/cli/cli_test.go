package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/exitcode"
	"github.com/enls/gremloader/internal/version"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = Run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersion(t *testing.T) {
	code, out, errOut := run(t, "version")
	if code != exitcode.OK {
		t.Fatalf("exit = %d, want %d (stderr %q)", code, exitcode.OK, errOut)
	}
	if !strings.HasPrefix(out, "gremloader ") || !strings.HasSuffix(out, "\n") {
		t.Errorf("stdout = %q", out)
	}
	if errOut != "" {
		t.Errorf("stderr = %q, want empty", errOut)
	}
}

func TestVersionDashDashVersion(t *testing.T) {
	code, out, _ := run(t, "--version")
	if code != exitcode.OK || !strings.HasPrefix(out, "gremloader ") {
		t.Errorf("exit = %d, stdout = %q", code, out)
	}
}

func TestVersionJSON(t *testing.T) {
	code, out, errOut := run(t, "version", "-json")
	if code != exitcode.OK {
		t.Fatalf("exit = %d (stderr %q)", code, errOut)
	}
	var got version.Info
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got.Version == "" || got.GoVersion == "" {
		t.Errorf("decoded info incomplete: %+v", got)
	}
}

func TestVersionRejectsExtraArguments(t *testing.T) {
	code, out, errOut := run(t, "version", "now")
	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if out != "" || !strings.Contains(errOut, `unexpected argument "now"`) {
		t.Errorf("stdout = %q, stderr = %q", out, errOut)
	}
}

func TestVersionUnknownFlag(t *testing.T) {
	code, _, errOut := run(t, "version", "-bogus")
	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if !strings.Contains(errOut, "bogus") {
		t.Errorf("stderr does not name the bad flag: %q", errOut)
	}
}

func TestNoArgsIsUsageError(t *testing.T) {
	code, out, errOut := run(t)
	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if out != "" || !strings.Contains(errOut, "Usage:") {
		t.Errorf("stdout = %q, stderr = %q", out, errOut)
	}
}

func TestUnknownCommand(t *testing.T) {
	code, out, errOut := run(t, "frobnicate")
	if code != exitcode.Usage {
		t.Errorf("exit = %d, want %d", code, exitcode.Usage)
	}
	if out != "" || !strings.Contains(errOut, `unknown command "frobnicate"`) {
		t.Errorf("stdout = %q, stderr = %q", out, errOut)
	}
}

func TestHelp(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		code, out, errOut := run(t, arg)
		if code != exitcode.OK || !strings.Contains(out, "Usage:") || errOut != "" {
			t.Errorf("%s: exit = %d, stdout = %q, stderr = %q", arg, code, out, errOut)
		}
	}
}
