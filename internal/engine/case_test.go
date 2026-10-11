package engine

import (
	"context"
	"testing"

	"github.com/enls/gremloader/internal/testutil/archives"
)

// Bug check of M5 (sub-agent finding 1): a file whose name changes only in case between versions.
// On a case-insensitive file system the new file replaces the old one in place; removing "the old
// name" afterwards must not delete it.
func TestCaseOnlyRenameBetweenVersions(t *testing.T) {
	r := newRig(t, appFile)
	r.publish("app", "1.0.0", 30, archives.F("app.exe", "app v1"), archives.F("Readme.txt", "old readme"), archives.F("Logs/old.log", "x"))
	if it := item(t, r.apply(r.engine(), Request{InstallMissing: true}), "app"); it.Result != ResultInstalled {
		t.Fatalf("install: %+v", it)
	}
	r.publish("app", "2.0.0", 20, archives.F("app.exe", "app v2"), archives.F("README.txt", "new readme"), archives.D("logs/"))
	it := item(t, r.apply(r.engine(), Request{}), "app")
	got := tree(t, r.dir())
	t.Logf("result %s (%v), folder:\n%s", it.Result, it.Err, show(got))
	switch it.Result {
	case ResultUpdated:
		_, lower := got["logs/"]
		_, upper := got["Logs/"]
		if !lower && !upper {
			t.Errorf("the empty folder the new version ships was removed:\n%s", show(got))
		}
		readme := got["README.txt"] + got["Readme.txt"]
		if readme != "new readme" || got["app.exe"] != "app v2" {
			t.Fatalf("updated, but the folder holds:\n%s", show(got))
		}
	case ResultFailed:
		if got["Readme.txt"] != "old readme" || got["app.exe"] != "app v1" {
			t.Fatalf("failed, but the folder holds:\n%s", show(got))
		}
	default:
		t.Fatalf("item = %+v", it)
	}
	r.assertClean()

	// Back and forth again: the same names meet the other way round.
	for _, want := range []struct{ version, readme, app string }{
		{"1.0.0", "old readme", "app v1"}, {"2.0.0", "new readme", "app v2"},
	} {
		if it := r.engine().Rollback(context.Background(), "app"); it.Result != ResultRolledBack || it.To != want.version {
			t.Fatalf("rollback to %s = %+v", want.version, it)
		}
		got := tree(t, r.dir())
		if got["README.txt"]+got["Readme.txt"] != want.readme || got["app.exe"] != want.app {
			t.Fatalf("at %s the folder holds:\n%s", want.version, show(got))
		}
		r.assertClean()
	}
}
