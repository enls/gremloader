package engine

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enls/gremloader/internal/testutil/archives"
)

// Decision 6 of the M5 bug check: a path that is a file in one version and a folder in the next (or
// the reverse) is replaced, and rollback brings the old one back.
func TestFileBecomesFolderBetweenVersions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		v1, v2 []archives.Entry
		want1  map[string]string
		want2  map[string]string
	}{
		{
			name:  "file to folder",
			v1:    []archives.Entry{archives.F("app.exe", "app v1"), archives.F("foo", "a file")},
			v2:    []archives.Entry{archives.F("app.exe", "app v2"), archives.F("foo/bar", "in a folder")},
			want1: map[string]string{"app.exe": "app v1", "foo": "a file"},
			want2: map[string]string{"app.exe": "app v2", "foo/": "", "foo/bar": "in a folder"},
		},
		{
			name:  "folder to file",
			v1:    []archives.Entry{archives.F("app.exe", "app v1"), archives.F("foo/bar", "in a folder"), archives.F("foo/sub/baz", "deeper")},
			v2:    []archives.Entry{archives.F("app.exe", "app v2"), archives.F("foo", "a file")},
			want1: map[string]string{"app.exe": "app v1", "foo/": "", "foo/bar": "in a folder", "foo/sub/": "", "foo/sub/baz": "deeper"},
			want2: map[string]string{"app.exe": "app v2", "foo": "a file"},
		},
		{ // on Windows "Foo" stands where "foo/" goes; on Linux it would simply go later
			name:  "file to folder, case differs",
			v1:    []archives.Entry{archives.F("app.exe", "app v1"), archives.F("Foo", "a file")},
			v2:    []archives.Entry{archives.F("app.exe", "app v2"), archives.F("foo/bar", "in a folder")},
			want1: map[string]string{"app.exe": "app v1", "Foo": "a file"},
			want2: map[string]string{"app.exe": "app v2", "foo/": "", "foo/bar": "in a folder"},
		},
		{
			name:  "folder to file, case differs",
			v1:    []archives.Entry{archives.F("app.exe", "app v1"), archives.F("Foo/bar", "in a folder")},
			v2:    []archives.Entry{archives.F("app.exe", "app v2"), archives.F("foo", "a file")},
			want1: map[string]string{"app.exe": "app v1", "Foo/": "", "Foo/bar": "in a folder"},
			want2: map[string]string{"app.exe": "app v2", "foo": "a file"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, appFile)
			r.publish("app", "1.0.0", 30, tc.v1...)
			if it := item(t, r.apply(r.engine(), Request{InstallMissing: true}), "app"); it.Result != ResultInstalled {
				t.Fatalf("install: %+v", it)
			}
			assertTree(t, r.dir(), tc.want1)
			r.publish("app", "2.0.0", 20, tc.v2...)
			if it := item(t, r.apply(r.engine(), Request{}), "app"); it.Result != ResultUpdated {
				t.Fatalf("update: %s (%v), folder:\n%s", it.Result, it.Err, show(tree(t, r.dir())))
			}
			assertTree(t, r.dir(), tc.want2)
			r.assertClean()
			for _, want := range []struct {
				version string
				tree    map[string]string
			}{{"1.0.0", tc.want1}, {"2.0.0", tc.want2}} {
				if it := r.engine().Rollback(context.Background(), "app"); it.Result != ResultRolledBack || it.To != want.version {
					t.Fatalf("rollback to %s = %+v (%v)", want.version, it, it.Err)
				}
				assertTree(t, r.dir(), want.tree)
				r.assertClean()
			}
			// The new folder is GremLoader's, though a file of the old version stood there.
			if it := r.engine().Remove(context.Background(), "app", RemoveOptions{}); it.Result != ResultRemoved {
				t.Fatalf("remove = %+v (%v)", it, it.Err)
			}
			assertTree(t, r.dir(), map[string]string{})
		})
	}
}

// Decision 6, the refusals: what stands in the way and was not installed by the old version is
// not GremLoader's to delete. The update fails, naming it, and leaves the old version exactly.
func TestKindChangeBlockedByTheUsersFiles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		v1, v2  []archives.Entry
		user    map[string]string // added by the user after the install
		blocker string            // the path the error names
	}{
		{
			name:    "a file of the user's where a folder goes",
			v1:      []archives.Entry{archives.F("app.exe", "app v1")},
			v2:      []archives.Entry{archives.F("app.exe", "app v2"), archives.F("foo/bar", "in a folder")},
			user:    map[string]string{"foo": "the user's"},
			blocker: "foo",
		},
		{
			name:    "a file of the user's in the old folder where a file goes",
			v1:      []archives.Entry{archives.F("app.exe", "app v1"), archives.F("foo/bar", "in a folder")},
			v2:      []archives.Entry{archives.F("app.exe", "app v2"), archives.F("foo", "a file")},
			user:    map[string]string{"foo/settings.ini": "the user's"},
			blocker: "foo",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, appFile)
			r.publish("app", "1.0.0", 30, tc.v1...)
			if it := item(t, r.apply(r.engine(), Request{InstallMissing: true}), "app"); it.Result != ResultInstalled {
				t.Fatalf("install: %+v", it)
			}
			for p, c := range tc.user {
				if err := os.WriteFile(filepath.Join(r.dir(), filepath.FromSlash(p)), []byte(c), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := tree(t, r.dir())
			r.publish("app", "2.0.0", 20, tc.v2...)
			it := item(t, r.apply(r.engine(), Request{}), "app")
			if it.Result != ResultFailed || it.Err == nil || !strings.Contains(it.Err.Error(), filepath.Join(r.dir(), tc.blocker)+":") {
				t.Fatalf("update = %s (%v), want a failure naming %s", it.Result, it.Err, tc.blocker)
			}
			assertTree(t, r.dir(), before)
			r.assertClean()
		})
	}
}

// Decision 6 under failures and crashes: an update and a rollback across a kind change end exactly
// before or exactly after, at every fault point, both ways round.
func TestKindChangeUnderFailuresAndCrashes(t *testing.T) {
	file := map[string]string{"foo": "a file"}
	folder := map[string]string{"foo/": "", "foo/bar": "in a folder", "foo/sub/": "", "foo/sub/baz": "deeper"}
	entries := func(m map[string]string) []archives.Entry {
		var out []archives.Entry
		for p, c := range m {
			if !strings.HasSuffix(p, "/") {
				out = append(out, archives.F(p, c))
			}
		}
		return out
	}
	update := func(e *Engine) (*Item, error) {
		p, err := e.Plan(context.Background(), Request{})
		if err != nil {
			return nil, err
		}
		err = e.Apply(context.Background(), p, ApplyOptions{})
		return p.Items[0], err
	}
	rollback := func(e *Engine) (*Item, error) { return e.rollback(context.Background(), "app") }
	for _, tc := range []struct {
		name   string
		v1, v2 map[string]string
	}{{"file to folder", file, folder}, {"folder to file", folder, file}} {
		t.Run(tc.name, func(t *testing.T) {
			v1, v2 := maps.Clone(tc.v1), maps.Clone(tc.v2)
			v1["app.exe"], v2["app.exe"] = "app v1", "app v2"
			atV1 := func(t *testing.T) *rig {
				r := newRig(t, appFile)
				r.publish("app", "1.0.0", 30, entries(v1)...)
				if it := item(t, r.apply(r.engine(), Request{InstallMissing: true}), "app"); it.Result != ResultInstalled {
					t.Fatalf("install: %+v", it)
				}
				r.publish("app", "2.0.0", 20, entries(v2)...)
				return r
			}
			atV2 := func(t *testing.T) *rig {
				r := atV1(t)
				if it := item(t, r.apply(r.engine(), Request{}), "app"); it.Result != ResultUpdated {
					t.Fatalf("update: %+v (%v)", it, it.Err)
				}
				return r
			}
			t.Run("update", func(t *testing.T) { sweep(t, atV1, update, v1, v2, nil) })
			t.Run("rollback", func(t *testing.T) { sweep(t, atV2, rollback, v2, v1, nil) })
		})
	}
}
