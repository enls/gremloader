package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/testutil/archives"
)

// Decision 1 of the M5 bug check: the files an --overlay install overwrites are kept (as its
// journal in previous/<pkg>/) only until the next update or remove, and nothing said so.
func TestOverlayWarnsHowLongItKeepsWhatItOverwrites(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 10, archives.F("app.exe", "new"))
	if err := os.MkdirAll(r.dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir(), "app.exe"), []byte("someone else's"), 0o600); err != nil {
		t.Fatal(err)
	}
	it := item(t, r.apply(r.engine(), Request{InstallMissing: true, Overlay: true}), "app")
	if it.Result != ResultInstalled || !strings.Contains(strings.Join(it.Notes, "\n"), "kept only until the next update or remove") {
		t.Fatalf("install = %+v", it)
	}

	rm := r.engine().Remove(context.Background(), "app", RemoveOptions{})
	if rm.Result != ResultRemoved {
		t.Fatalf("remove = %+v (%v)", rm, rm.Err)
	}
	if notes := strings.Join(rm.Notes, "\n"); !strings.Contains(notes, "--overlay install overwrote") {
		t.Errorf("remove notes = %q", notes)
	}
	if _, err := os.Stat(r.paths.Previous("app")); err == nil {
		t.Error("the overlay record outlived the remove")
	}
}

// A remove with nothing of an overlay to discard says nothing about one.
func TestPlainRemoveSaysNothingAboutAnOverlay(t *testing.T) {
	r := atV2(t) // previous/ holds the update's rollback record, pointed to by state
	rm := r.engine().Remove(context.Background(), "app", RemoveOptions{})
	if rm.Result != ResultRemoved || strings.Contains(strings.Join(rm.Notes, "\n"), "overlay") {
		t.Errorf("remove = %+v", rm)
	}
}
