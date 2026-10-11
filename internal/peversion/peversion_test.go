package peversion

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

func TestOnlyWindowsIsSupported(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("implemented here")
	}
	if _, err := ProductVersion("/bin/sh"); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

// A system DLL always carries a version resource. Its exact value changes with every Windows
// update, so only the shape is checked.
func TestSystemFileHasAProductVersion(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows only")
	}
	dll := filepath.Join(os.Getenv("SystemRoot"), "System32", "kernel32.dll")
	v, err := ProductVersion(dll)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+`).MatchString(v) {
		t.Errorf("ProductVersion(kernel32.dll) = %q", v)
	}
}

// Go does not embed a version resource, so the test binary itself has none.
func TestFileWithoutAVersionResource(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows only")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if v, err := ProductVersion(self); !errors.Is(err, ErrNoVersion) {
		t.Errorf("ProductVersion(test binary) = %q, %v; want ErrNoVersion", v, err)
	}
	text := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(text, []byte("not a program"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := ProductVersion(text); !errors.Is(err, ErrNoVersion) {
		t.Errorf("ProductVersion(text file) = %q, %v; want ErrNoVersion", v, err)
	}
}

func TestMissingFile(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows only")
	}
	_, err := ProductVersion(filepath.Join(t.TempDir(), "absent.exe"))
	if err == nil || errors.Is(err, ErrNoVersion) {
		t.Errorf("err = %v, want a not-found error, not ErrNoVersion", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Logf("note: err = %v does not match fs.ErrNotExist", err)
	}
}
