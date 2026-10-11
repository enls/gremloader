package engine

import (
	"errors"
	"fmt"
	"slices"

	"github.com/enls/gremloader/internal/checksum"
	"github.com/enls/gremloader/internal/download"
	"github.com/enls/gremloader/internal/events"
	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/redact"
)

// trusted is one statement of what the asset's SHA-256 must be.
type trusted struct {
	source string // for messages: "SHA256SUMS.txt line 3", "GitHub's digest", "gremloader.lock"
	level  string // the verification level it satisfies: checksum or digest
	hash   string
}

// expectations gathers every trusted hash for it's asset (plan E.3.4): the checksum file entry,
// GitHub's digest when the release came through the API, or the lock's hash under --locked.
func expectations(it *Item, pkg *pkgfile.Package, sums *checksum.File) ([]trusted, error) {
	if it.Locked {
		return []trusted{{source: "gremloader.lock", level: "checksum", hash: it.LockHash}}, nil
	}
	var out []trusted
	if sums != nil {
		var e checksum.Entry
		var err error
		if it.Sidecar {
			var warn string
			e, warn, err = sums.Sidecar(it.Asset.Name)
			if warn != "" {
				it.notef("%s", warn)
			}
		} else {
			e, err = sums.Lookup(it.Asset.Name)
		}
		switch {
		case err == nil:
			out = append(out, trusted{source: fmt.Sprintf("%s line %d", it.Checksums.Name, e.Line), level: "checksum", hash: e.Hash})
		case errors.Is(err, checksum.ErrNotListed) && !slices.Contains(pkg.VerifyRequire, "checksum"):
			it.notef("%s does not list %s", it.Checksums.Name, it.Asset.Name)
		default:
			return nil, &VerifyError{Package: it.Package, Msg: fmt.Sprintf("%s: %v", it.Checksums.Name, err)}
		}
	}
	if it.Asset.SHA256 != "" {
		out = append(out, trusted{source: "GitHub's digest", level: "digest", hash: it.Asset.SHA256})
	}
	return out, nil
}

// verify checks the downloaded asset against every trusted hash. All must agree with the bytes
// received; a required level with no source is a failure too. It returns the levels that passed.
func (e *Engine) verify(it *Item, pkg *pkgfile.Package, got download.Result, sums *checksum.File) ([]string, error) {
	exp, err := expectations(it, pkg, sums)
	if err != nil {
		return nil, err
	}
	var levels []string
	for _, t := range exp {
		if !slices.Contains(levels, t.level) {
			levels = append(levels, t.level)
		}
	}
	if !it.Locked {
		for _, req := range pkg.VerifyRequire {
			if !slices.Contains(levels, req) {
				return nil, &VerifyError{Package: it.Package, Msg: fmt.Sprintf("verify.require lists %s, and nothing could check it", req)}
			}
		}
	}
	if it.Locked && it.LockSize > 0 && got.Size != it.LockSize {
		return nil, e.mismatch(it, exp, got, fmt.Sprintf("the lock says %d bytes, %d were received", it.LockSize, got.Size))
	}
	for _, t := range exp {
		if !checksum.Equal(t.hash, got.SHA256) {
			return nil, e.mismatch(it, exp, got, "")
		}
	}
	e.emit(events.Event{Level: events.Info, Event: "verify.ok", Package: it.Package, Version: it.To, Result: fmt.Sprint(levels)})
	return levels, nil
}

// mismatch words a hash mismatch without claiming malice (E.3.4): what was expected and from where,
// what arrived, and the usual causes.
func (e *Engine) mismatch(it *Item, exp []trusted, got download.Result, extra string) error {
	msg := fmt.Sprintf("%s does not match what it was checked against.\n", it.Asset.Name)
	for _, t := range exp {
		mark := "matches"
		if !checksum.Equal(t.hash, got.SHA256) {
			mark = "DIFFERS"
		}
		msg += fmt.Sprintf("  expected %s (%s): %s\n", t.hash, t.source, mark)
	}
	msg += fmt.Sprintf("  received %s, %d bytes, from %s\n", got.SHA256, got.Size, redact.URL(it.Asset.URL))
	if extra != "" {
		msg += "  " + extra + "\n"
	}
	msg += "Likely causes: a corrupt or truncated download, a caching proxy or CDN, a publisher who did not " +
		"regenerate the checksum file, or tampering. Nothing was installed."
	e.emit(events.Event{Level: events.Error, Event: "verify.fail", Package: it.Package, Version: it.To,
		URL: it.Asset.URL, Code: "hash-mismatch", Msg: msg})
	return &VerifyError{Package: it.Package, Msg: msg, received: got.SHA256}
}
