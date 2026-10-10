// Package fixtures locates the captured release files under testdata/fixtures, wherever the
// calling test's package happens to sit in the tree.
package fixtures

import (
	"os"
	"path/filepath"
	"testing"
)

// Root returns the testdata/fixtures directory of the module the test is running in.
func Root(tb testing.TB) string {
	tb.Helper()
	dir, err := os.Getwd()
	if err != nil {
		tb.Fatalf("fixtures: getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			root := filepath.Join(dir, "testdata", "fixtures")
			if _, err := os.Stat(root); err != nil {
				tb.Fatalf("fixtures: %v", err)
			}
			return root
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			tb.Fatal("fixtures: no go.mod found above the test's working directory")
		}
		dir = parent
	}
}

// Path returns the absolute path of a fixture given its slash-separated path under Root.
func Path(tb testing.TB, rel string) string {
	tb.Helper()
	return filepath.Join(Root(tb), filepath.FromSlash(rel))
}

// Read returns a fixture's bytes, exactly as captured.
func Read(tb testing.TB, rel string) []byte {
	tb.Helper()
	b, err := os.ReadFile(Path(tb, rel))
	if err != nil {
		tb.Fatalf("fixtures: %v", err)
	}
	return b
}
