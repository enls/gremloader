// Package archives builds zip and tar.gz archives in memory for tests, including deliberately
// malformed ones. archive/zip and archive/tar will happily *write* names such as "../x" or
// "C:\evil", which is exactly what the extractor's tests need.
//
// The builders are deterministic: same entries, same bytes.
package archives

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

// Kind is the type of an archive entry.
type Kind int

const (
	File Kind = iota
	Dir
	Symlink
	Hardlink
)

// Entry is one archive member.
type Entry struct {
	Name string
	Data []byte
	Mode fs.FileMode // permission bits; 0 means 0644 for files, 0755 for directories
	Kind Kind
	Link string // target of a Symlink or Hardlink
}

// F is shorthand for a regular file entry.
func F(name, data string) Entry { return Entry{Name: name, Data: []byte(data)} }

// X is shorthand for an executable file entry.
func X(name, data string) Entry { return Entry{Name: name, Data: []byte(data), Mode: 0o755} }

// D is shorthand for a directory entry.
func D(name string) Entry { return Entry{Name: name, Kind: Dir} }

// L is shorthand for a symlink entry.
func L(name, target string) Entry { return Entry{Name: name, Kind: Symlink, Link: target} }

// H is shorthand for a hard link entry.
func H(name, target string) Entry { return Entry{Name: name, Kind: Hardlink, Link: target} }

var stamp = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func (e Entry) perm() fs.FileMode {
	switch {
	case e.Mode != 0:
		return e.Mode
	case e.Kind == Dir:
		return 0o755
	}
	return 0o644
}

// Zip builds a zip archive. A Symlink is stored the way Info-ZIP does: a unix-mode symlink
// whose content is the target. Hardlinks do not exist in zip and are stored as symlinks.
func Zip(entries ...Entry) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.Name, Method: zip.Deflate, Modified: stamp}
		data := e.Data
		switch e.Kind {
		case Dir:
			if !strings.HasSuffix(h.Name, "/") {
				h.Name += "/"
			}
			h.Method = zip.Store
			h.SetMode(fs.ModeDir | e.perm())
			data = nil
		case Symlink, Hardlink:
			h.SetMode(fs.ModeSymlink | 0o777)
			data = []byte(e.Link)
		default:
			h.SetMode(e.perm())
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			panic(fmt.Sprintf("archives.Zip: %v", err))
		}
		if _, err := w.Write(data); err != nil {
			panic(fmt.Sprintf("archives.Zip: %v", err))
		}
	}
	if err := zw.Close(); err != nil {
		panic(fmt.Sprintf("archives.Zip: %v", err))
	}
	return buf.Bytes()
}

// Tar builds an uncompressed tar archive.
func Tar(entries ...Entry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.Name, ModTime: stamp, Mode: int64(e.perm())}
		switch e.Kind {
		case Dir:
			if !strings.HasSuffix(h.Name, "/") {
				h.Name += "/"
			}
			h.Typeflag = tar.TypeDir
		case Symlink:
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.Link
		case Hardlink:
			h.Typeflag, h.Linkname = tar.TypeLink, e.Link
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(e.Data))
		}
		if err := tw.WriteHeader(h); err != nil {
			panic(fmt.Sprintf("archives.Tar: %v", err))
		}
		if e.Kind == File {
			if _, err := tw.Write(e.Data); err != nil {
				panic(fmt.Sprintf("archives.Tar: %v", err))
			}
		}
	}
	if err := tw.Close(); err != nil {
		panic(fmt.Sprintf("archives.Tar: %v", err))
	}
	return buf.Bytes()
}

// TarGz builds a gzip-compressed tar archive with a fixed gzip header (no timestamp).
func TarGz(entries ...Entry) []byte {
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		panic(err)
	}
	if _, err := gz.Write(Tar(entries...)); err != nil {
		panic(err)
	}
	if err := gz.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// fakeExe is not a real executable, only something that is big enough to be worth hashing.
func fakeExe(name string, size int) string {
	return "MZ-fake-" + name + "-" + strings.Repeat("0123456789abcdef", size/16+1)[:size]
}

// CraftWindowsZip has the shape of a real Craft Windows portable release, as observed on
// soundcraft v0.4.0: exactly seven files, all under one top-level folder named after the zip,
// "/" separators, no directory entries, no unix modes. File contents are synthetic.
func CraftWindowsZip(app, version string) []byte {
	top := fmt.Sprintf("%s-%s-windows-x64-portable/", app, version)
	var es []Entry
	for _, n := range []string{"ATTRIBUTION.md", "LICENSE-APACHE", "LICENSE-MIT", "NOTICE", "README.md"} {
		es = append(es, F(top+n, n+" for "+app+" "+version+"\n"))
	}
	es = append(es,
		F(top+app+"-cli.exe", fakeExe(app+"-cli", 700)),
		F(top+app+".exe", fakeExe(app, 2000)),
	)
	// Real Craft zips carry no unix mode bits (the .exe is not marked executable).
	return zipNoModes(es)
}

func zipNoModes(entries []Entry) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.Name, Method: zip.Deflate, Modified: stamp})
		if err != nil {
			panic(err)
		}
		if _, err := w.Write(e.Data); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// CraftLinuxTarGz has the shape of a real Craft Linux release, as observed on soundcraft v0.4.0
// and cadcraft v0.4.0: one top-level folder, an install prefix inside it (bin/ and share/), explicit
// directory entries, no symlinks or hard links. File contents are synthetic.
func CraftLinuxTarGz(app, version, arch string) []byte {
	top := fmt.Sprintf("%s-%s-linux-%s/", app, version, arch)
	id := "ai.storyteller." + app
	return TarGz(
		D(top),
		D(top+"bin"),
		X(top+"bin/"+app+"-cli", fakeExe(app+"-cli", 500)),
		X(top+"bin/"+app, fakeExe(app, 1500)),
		D(top+"share"),
		D(top+"share/doc"),
		D(top+"share/doc/"+app),
		F(top+"share/doc/"+app+"/LICENSE-APACHE", "apache\n"),
		F(top+"share/doc/"+app+"/LICENSE-MIT", "mit\n"),
		F(top+"share/doc/"+app+"/NOTICE", "notice\n"),
		F(top+"share/doc/"+app+"/README.md", "readme\n"),
		D(top+"share/applications"),
		F(top+"share/applications/"+id+".desktop", "[Desktop Entry]\nName="+app+"\nExec="+app+" %F\n"),
		D(top+"share/icons"),
		D(top+"share/icons/hicolor"),
		D(top+"share/icons/hicolor/64x64"),
		D(top+"share/icons/hicolor/64x64/apps"),
		F(top+"share/icons/hicolor/64x64/apps/"+id+".png", "\x89PNG-fake"),
	)
}
