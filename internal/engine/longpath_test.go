package engine

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/testutil/archives"
)

func TestLongPath(t *testing.T) {
	dir := `C:\` + strings.Repeat("d", 97)                       // 100 characters
	file := func(n int) string { return strings.Repeat("f", n) } // dir + `\` + n = 101 + n
	for _, tc := range []struct {
		name  string
		goos  string
		files []string
		dirs  []string
		want  string // the rel path reported, "" none
		n     int
	}{
		{"short", "windows", []string{"a.exe", "sub/b.dll"}, []string{"sub"}, "", 0},
		{"file at the limit", "windows", []string{file(158)}, nil, "", 0},            // 259
		{"file over the limit", "windows", []string{file(159)}, nil, file(159), 260}, // 260
		{"folder at its limit", "windows", nil, []string{file(146)}, "", 0},          // 247
		{"folder over its limit", "windows", nil, []string{file(147)}, file(147), 248},
		{"longest wins", "windows", []string{file(160), "x/" + file(170)}, []string{file(150)}, "x/" + file(170), 273},
		{"not Windows", "linux", []string{file(300)}, []string{file(300)}, "", 0},
		// UTF-16 units, not bytes: U+1F600 is 4 bytes and 2 units; é is 2 bytes and 1 unit.
		{"counted in UTF-16", "windows", []string{strings.Repeat("é", 158)}, nil, "", 0},
		{"astral counts two", "windows", []string{strings.Repeat("\U0001F600", 80)}, nil, strings.Repeat("\U0001F600", 80), 261},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, n, ok := longPath(tc.goos, dir, tc.files, tc.dirs)
			if ok != (tc.want != "") || n != tc.n {
				t.Fatalf("longPath = %q, %d, %v; want %q, %d", p, n, ok, tc.want, tc.n)
			}
			if ok && p != filepath.Join(dir, filepath.FromSlash(tc.want)) {
				t.Errorf("path = %q", p)
			}
		})
	}
}

// longFile is a package file whose install folder is long enough that a 150-character file name
// in it goes over MAX_PATH, while the archive stays under the extractor's 200-byte path limit.
const longFile = `schema_version = 1

[package.app]
repo      = "o/app"
asset     = "app-{version}.zip"
checksums = "SHA256SUMS.txt"
dir       = "{root}/` + "Long Folder Name For The Path Limit Test Of GremLoader On Windows Machines" + `"
`

func TestLongPathIsNotedOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the warning is for Windows; longPath itself is tested on every OS")
	}
	r := newRig(t, longFile)
	deep := "sub/" + strings.Repeat("n", 140) + ".txt"
	r.publish("app", "1.0.0", 30, archives.F("app.exe", "1"), archives.F(deep, "deep"))
	e := r.engine()
	if it := item(t, r.plan(e, Request{InstallMissing: true}), "app"); len(it.Dir)+1+len(deep) <= maxWindowsFile {
		t.Fatalf("the test path is not long enough: %d", len(it.Dir)+1+len(deep))
	}
	it := item(t, r.apply(e, Request{InstallMissing: true}), "app")
	if it.Result != ResultInstalled {
		t.Fatalf("install = %+v (%v): a long path must never block", it, it.Err)
	}
	if !strings.Contains(strings.Join(it.Notes, "\n"), "MAX_PATH") {
		t.Errorf("install notes = %q", it.Notes)
	}
	found := false
	for _, ev := range r.events() {
		found = found || (ev.Event == "apply.step" && ev.Step == "long-path")
	}
	if !found {
		t.Error("no long-path event")
	}

	// The next plan warns from the installed file list, before any download.
	r.publish("app", "2.0.0", 20, archives.F("app.exe", "2"))
	if it := item(t, r.plan(r.engine(), Request{}), "app"); !strings.Contains(strings.Join(it.Notes, "\n"), "MAX_PATH") {
		t.Errorf("plan notes = %q", it.Notes)
	}
}
