package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/enls/gremloader/internal/archive"
	"github.com/enls/gremloader/internal/atomicfile"
	"github.com/enls/gremloader/internal/checksum"
	"github.com/enls/gremloader/internal/download"
	"github.com/enls/gremloader/internal/events"
	"github.com/enls/gremloader/internal/pkgfile"
	"github.com/enls/gremloader/internal/redact"
)

// quarantineCap is the largest failed download kept for inspection. Larger ones are described in
// info.json but not kept: a quarantine that can fill the disk would be a problem of its own.
var quarantineCap int64 = 512 << 20

// staged is a verified download unpacked in staging.
type staged struct {
	got    download.Result
	levels []string
	ar     *archive.Result
	tree   string
}

// stage runs steps 1 to 3: download, verify, extract. A transient corruption is the common cause of
// a hash mismatch, so a mismatch is downloaded once more before it is final (plan E.3.4); so is an
// archive that turns out damaged when nothing verified its hash (when something did, the same
// bytes would come again). A final mismatch keeps the bytes in quarantine.
func (e *Engine) stage(ctx context.Context, it *Item, pkg *pkgfile.Package, work string, o ApplyOptions) (*staged, error) {
	for attempt := 1; ; attempt++ {
		st, err := e.stageOnce(ctx, it, pkg, work, o)
		if err == nil {
			return st, nil
		}
		var ve *VerifyError
		mismatch := errors.As(err, &ve) && ve.received != ""
		corrupt := st != nil && len(st.levels) == 0 && isCorrupt(err)
		if attempt == 1 && (mismatch || corrupt) && ctx.Err() == nil {
			e.emit(events.Event{Level: events.Warn, Event: "download.retry", Package: it.Package, Version: it.To,
				URL: it.Asset.URL, Msg: err.Error()})
			if rerr := os.RemoveAll(work); rerr != nil {
				return nil, rerr
			}
			if rerr := os.MkdirAll(work, 0o700); rerr != nil {
				return nil, rerr
			}
			continue
		}
		if mismatch {
			ve.Msg += "\n" + e.quarantine(it, filepath.Join(work, "asset"), ve)
		}
		return nil, err
	}
}

// stageOnce is one attempt. On an extraction failure it still returns what it learned (the levels
// that verified the download), so stage can tell whether trying again could help.
func (e *Engine) stageOnce(ctx context.Context, it *Item, pkg *pkgfile.Package, work string, o ApplyOptions) (*staged, error) {
	// 1. Download, hashing while streaming.
	if err := e.at("download"); err != nil {
		return nil, err
	}
	assetPath := filepath.Join(work, "asset")
	got, err := e.fetch(ctx, it, it.Asset.URL, assetPath, pkg.Limits.MaxDownload)
	if err != nil {
		return nil, err
	}
	var sums *checksum.File
	if it.Checksums != nil {
		p := filepath.Join(work, "checksums")
		if _, err := e.fetch(ctx, it, it.Checksums.URL, p, maxChecksumFile); err != nil {
			return nil, fmt.Errorf("checksum file %s: %w", it.Checksums.Name, err)
		}
		if sums, err = parseChecksums(p); err != nil {
			return nil, &VerifyError{Package: it.Package, Msg: fmt.Sprintf("checksum file %s: %v", it.Checksums.Name, err)}
		}
	}

	// 2. Verify, before anything is unpacked.
	if err := e.at("verify"); err != nil {
		return nil, err
	}
	levels, err := e.verify(it, pkg, got, sums)
	if err != nil {
		var ve *VerifyError
		if !errors.As(err, &ve) || ve.received == "" || !e.accepted(it, o, got) {
			return nil, err
		}
		levels = []string{"hash-override"}
	}

	// 3. Extract into staging.
	if err := e.at("extract"); err != nil {
		return nil, err
	}
	tree := filepath.Join(work, "tree")
	ar, err := archive.Extract(ctx, assetPath, tree, archive.Options{Limits: pkg.Limits.ArchiveLimits(), Strip: pkg.Strip})
	if err != nil {
		return &staged{got: got, levels: levels}, fmt.Errorf("unpacking %s: %w", it.Asset.Name, err)
	}
	return &staged{got: got, levels: levels, ar: ar, tree: tree}, nil
}

// accepted reports whether the user's --accept-sha256 lets these bytes through: same package, same
// version, and exactly the hash received. The override is logged with its own code.
func (e *Engine) accepted(it *Item, o ApplyOptions, got download.Result) bool {
	a := o.Accept
	if a == nil || a.Package != it.Package || a.Version != it.To || !checksum.Equal(a.SHA256, got.SHA256) {
		return false
	}
	e.emit(events.Event{Level: events.Warn, Event: "verify.ok", Package: it.Package, Version: it.To, URL: it.Asset.URL,
		Code: "hash-override", Msg: "accepted " + got.SHA256 + " by --accept-sha256 although it differs from the trusted hash"})
	it.notef("installed with --accept-sha256 %s: the hash differs from the trusted one", got.SHA256)
	return true
}

// isCorrupt reports an archive that could not be read: a truncated stream or a damaged entry
// (the two forms the extractor uses, plan E.9).
func isCorrupt(err error) bool {
	var re *archive.RejectError
	return errors.Is(err, archive.ErrCorrupt) || (errors.As(err, &re) && re.Rule == archive.RuleCorrupt)
}

// quarantineInfo is info.json beside a kept download.
type quarantineInfo struct {
	Package  string `json:"package"`
	Version  string `json:"version"`
	Asset    string `json:"asset"`
	URL      string `json:"url"`
	Received string `json:"received_sha256"`
	Size     int64  `json:"size"`
	Kept     bool   `json:"kept"`
	At       string `json:"at"`
	Message  string `json:"message"`
}

func (e *Engine) quarantineDir(name string) string {
	return filepath.Join(e.cfg.Paths.State, "quarantine", name)
}

// clearQuarantine drops a kept download once its version is installed (accepted, or verified on a
// later try): the question it asked is answered.
func (e *Engine) clearQuarantine(name, version string) {
	dir := e.quarantineDir(name)
	b, err := os.ReadFile(filepath.Join(dir, "info.json")) //nolint:gosec // G304: under the state dir
	if err != nil {
		return
	}
	var qi quarantineInfo
	if json.Unmarshal(b, &qi) == nil && qi.Version == version {
		_ = os.RemoveAll(dir)
	}
}

// quarantine keeps a download that failed verification in state/quarantine/<pkg>/ for inspection:
// never extracted, replaced by the package's next failure, and only up to quarantineCap. It returns
// a sentence for the error message.
func (e *Engine) quarantine(it *Item, asset string, ve *VerifyError) string {
	dir := e.quarantineDir(it.Package)
	if err := os.RemoveAll(dir); err != nil {
		return "The download could not be kept: " + err.Error()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "The download could not be kept: " + err.Error()
	}
	fi, err := os.Stat(asset)
	if err != nil {
		return "The download could not be kept: " + err.Error()
	}
	info := quarantineInfo{
		Package: it.Package, Version: it.To, Asset: it.Asset.Name, URL: redact.URL(it.Asset.URL),
		Received: ve.received, Size: fi.Size(), At: e.now().Format("2006-01-02T15:04:05Z"), Message: ve.Msg,
	}
	if fi.Size() <= quarantineCap {
		if err := rename(asset, filepath.Join(dir, "download")); err == nil {
			info.Kept = true
		}
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err == nil {
		err = atomicfile.Write(filepath.Join(dir, "info.json"), append(b, '\n'), 0o600)
	}
	if err != nil {
		return "The download could not be described: " + err.Error()
	}
	if !info.Kept {
		return fmt.Sprintf("The download (%d MiB) is too large to keep; what is known about it is in %s.", fi.Size()>>20, dir)
	}
	return "The download is kept, never unpacked, in " + dir + " for inspection. If you have checked the hash " +
		"received elsewhere (the release page, a second download, the author), install it with " +
		fmt.Sprintf("`gremloader install %s@%s --accept-sha256 %s`.", it.Package, it.To, ve.received)
}
