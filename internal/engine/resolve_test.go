package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/enls/gremloader/internal/source/github"
	"github.com/enls/gremloader/internal/ver"
)

var now = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// rel is a release published daysAgo days before now (negative: in the future; 0 with unknown:
// no publication time).
func rel(v string, daysAgo int) github.Release {
	pv, err := ver.Parse(v)
	if err != nil {
		panic(err)
	}
	return github.Release{Tag: "v" + v, Version: pv, Published: now.Add(-time.Duration(daysAgo) * 24 * time.Hour)}
}

func unknownAge(r github.Release) github.Release { r.Published = time.Time{}; return r }

// flagged is a release GitHub marks as a prerelease although its version looks stable.
func flagged(r github.Release) github.Release { r.Prerelease, r.FlagKnown = true, true; return r }

func TestResolve(t *testing.T) {
	std := []github.Release{rel("0.5.0", 60), rel("0.6.0", 30), rel("0.7.0-rc.1", 5), rel("0.6.1", 2)}

	for _, c := range []struct {
		name       string
		in         resolveIn
		want       string // version chosen, "" for none
		action     Action
		result     Result
		reasonHas  string
		wantErr    string
		wantNewest string
		noReason   bool
	}{
		// Plain cases.
		{name: "install the newest stable", in: resolveIn{Releases: std, InstallMissing: true},
			want: "0.6.1", action: ActInstall, wantNewest: "0.6.1"},
		{name: "update", in: resolveIn{Releases: std, Installed: "0.5.0"}, want: "0.6.1", action: ActUpdate},
		{name: "up to date", in: resolveIn{Releases: std, Installed: "0.6.1"}, result: ResultUnchanged},
		{name: "force reinstalls", in: resolveIn{Releases: std, Installed: "0.6.1", Force: true},
			want: "0.6.1", action: ActReinstall},
		{name: "missing package is skipped by update", in: resolveIn{Releases: std},
			result: ResultSkipped, reasonHas: "not installed"},
		{name: "no releases", in: resolveIn{InstallMissing: true}, result: ResultSkipped, reasonHas: "no release"},
		{name: "installed and nothing listed", in: resolveIn{Installed: "0.5.0"}, result: ResultUnchanged},

		// Channel.
		{name: "prerelease channel takes the rc", in: resolveIn{Releases: std, Channel: "prerelease", Installed: "0.6.1"},
			want: "0.7.0-rc.1", action: ActUpdate, wantNewest: "0.7.0-rc.1"},
		{name: "GitHub's prerelease flag counts on stable", in: resolveIn{
			Releases: []github.Release{rel("1.0.0", 9), flagged(rel("1.1.0", 1))}, InstallMissing: true},
			want: "1.0.0", action: ActInstall, wantNewest: "1.0.0"},
		{name: "installed prerelease is newer than every stable", in: resolveIn{Releases: std, Installed: "0.7.0-rc.1"},
			result: ResultUnchanged, reasonHas: "newer than every eligible"},

		// Rejected (P3-2).
		{name: "rolled-back version is skipped", in: resolveIn{Releases: std, Installed: "0.6.0", Rejected: []string{"0.6.1"}},
			result: ResultRejected, reasonHas: "0.6.1"},
		{name: "a newer release clears a rejection", in: resolveIn{
			Releases: append(std, rel("0.6.2", 1)), Installed: "0.6.0", Rejected: []string{"0.6.1"}},
			want: "0.6.2", action: ActUpdate},
		{name: "rejected newest falls back to an older eligible one", in: resolveIn{
			Releases: std, Installed: "0.5.0", Rejected: []string{"0.6.1"}},
			want: "0.6.0", action: ActUpdate, reasonHas: "rolled back"},
		{name: "explicit version overrides rejected", in: resolveIn{
			Releases: std, Installed: "0.6.0", Rejected: []string{"0.6.1"}, Want: "0.6.1"},
			want: "0.6.1", action: ActUpdate},

		// Cool-down.
		{name: "too new", in: resolveIn{Releases: std, Installed: "0.6.0", CooldownDays: 7, Now: now},
			result: ResultCooldown, reasonHas: "cool-down until 2026-10-15"},
		{name: "older eligible version is used", in: resolveIn{Releases: std, Installed: "0.5.0", CooldownDays: 7, Now: now},
			want: "0.6.0", action: ActUpdate, reasonHas: "newest eligible"},
		{name: "exactly old enough", in: resolveIn{Releases: std, Installed: "0.6.0", CooldownDays: 2, Now: now},
			want: "0.6.1", action: ActUpdate},
		{name: "--now ignores it", in: resolveIn{Releases: std, Installed: "0.6.0", CooldownDays: 7, Now: now, IgnoreCooldown: true},
			want: "0.6.1", action: ActUpdate},
		{name: "unknown age is denied", in: resolveIn{
			Releases: []github.Release{unknownAge(rel("1.0.0", 0))}, CooldownDays: 1, Now: now, InstallMissing: true},
			result: ResultCooldown, reasonHas: "no known publication time"},
		{name: "unknown age allowed", in: resolveIn{
			Releases: []github.Release{unknownAge(rel("1.0.0", 0))}, CooldownDays: 1, Now: now, InstallMissing: true, CooldownUnknown: "allow"},
			want: "1.0.0", action: ActInstall},
		{name: "future publication is denied", in: resolveIn{
			Releases: []github.Release{rel("1.0.0", -1)}, CooldownDays: 1, Now: now, InstallMissing: true},
			result: ResultCooldown},
		{name: "not installed, everything cooling", in: resolveIn{Releases: std, CooldownDays: 365, Now: now, InstallMissing: true},
			result: ResultCooldown},
		{name: "installed newest still in its cool-down is up to date", in: resolveIn{
			Releases: std, Installed: "0.6.1", CooldownDays: 7, Now: now},
			result: ResultUnchanged, noReason: true},
		{name: "future publication counts as unknown, so allow lets it through", in: resolveIn{
			Releases: []github.Release{rel("1.0.0", -1)}, CooldownDays: 1, Now: now, InstallMissing: true, CooldownUnknown: "allow"},
			want: "1.0.0", action: ActInstall},
		{name: "force reinstall while a newer one cools down", in: resolveIn{
			Releases: std, Installed: "0.6.0", CooldownDays: 7, Now: now, Force: true},
			want: "0.6.0", action: ActReinstall},

		// Pin and explicit versions (P3-1).
		{name: "pin holds back", in: resolveIn{Releases: std, Installed: "0.5.0", Pin: "0.6.0"}, want: "0.6.0", action: ActUpdate},
		{name: "pin bypasses channel and cool-down", in: resolveIn{
			Releases: std, Installed: "0.6.1", Pin: "0.7.0-rc.1", CooldownDays: 30, Now: now},
			want: "0.7.0-rc.1", action: ActUpdate},
		{name: "pin accepts the loose form", in: resolveIn{Releases: std, Installed: "0.5.0", Pin: "v0.6"}, want: "0.6.0", action: ActUpdate},
		{name: "pin that does not exist", in: resolveIn{Releases: std, Installed: "0.5.0", Pin: "0.9.0"},
			result: ResultFailed, wantErr: "pinned version 0.9.0 is not among"},
		{name: "explicit beats pin", in: resolveIn{Releases: std, Installed: "0.5.0", Pin: "0.6.0", Want: "0.6.1"},
			want: "0.6.1", action: ActUpdate},
		{name: "pin below installed is not a silent downgrade", in: resolveIn{Releases: std, Installed: "0.6.1", Pin: "0.5.0"},
			result: ResultSkipped, reasonHas: "--allow-downgrade"},
		{name: "downgrade when allowed", in: resolveIn{Releases: std, Installed: "0.6.1", Want: "0.5.0", AllowDowngrade: true},
			want: "0.5.0", action: ActDowngrade},
		{name: "explicit version of a missing package", in: resolveIn{Releases: std, Want: "0.5.0", InstallMissing: true},
			want: "0.5.0", action: ActInstall},

		// Hold.
		{name: "held", in: resolveIn{Releases: std, Installed: "0.5.0", Hold: true}, result: ResultHeld, reasonHas: "0.6.1 available, held"},
		{name: "--include-held", in: resolveIn{Releases: std, Installed: "0.5.0", Hold: true, IncludeHeld: true},
			want: "0.6.1", action: ActUpdate},
		{name: "held and up to date is just up to date", in: resolveIn{Releases: std, Installed: "0.6.1", Hold: true},
			result: ResultUnchanged},

		// Damaged input.
		{name: "bad installed version", in: resolveIn{Releases: std, Installed: "not-a-version"},
			result: ResultFailed, wantErr: "state.json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if c.in.Now.IsZero() {
				c.in.Now = now
			}
			d := resolve(c.in)
			got := ""
			if d.Release != nil {
				got = d.Release.Version.String()
			}
			if got != c.want || d.Action != c.action || d.Result != c.result {
				t.Errorf("got %q action=%q result=%q reason=%q err=%v; want %q action=%q result=%q",
					got, d.Action, d.Result, d.Reason, d.Err, c.want, c.action, c.result)
			}
			if c.noReason && d.Reason != "" {
				t.Errorf("reason = %q, want none", d.Reason)
			}
			if c.reasonHas != "" && !strings.Contains(d.Reason, c.reasonHas) {
				t.Errorf("reason = %q, want it to mention %q", d.Reason, c.reasonHas)
			}
			if (c.wantErr == "") != (d.Err == nil) || (d.Err != nil && !strings.Contains(d.Err.Error(), c.wantErr)) {
				t.Errorf("err = %v, want %q", d.Err, c.wantErr)
			}
			if c.wantNewest != "" && (d.Newest == nil || d.Newest.Version.String() != c.wantNewest) {
				t.Errorf("newest = %v, want %s", d.Newest, c.wantNewest)
			}
		})
	}
}

// Equal versions (v1.0.0 and v1.0.0+build2 are the same precedence) prefer the later publication.
func TestSortPrefersTheLaterOfEqualVersions(t *testing.T) {
	a := rel("1.0.0", 10)
	b := rel("1.0.0+build2", 1)
	b.Tag = "v1.0.0+build2"
	got := sortReleases([]github.Release{a, b, rel("0.9.0", 20)})
	if got[0].Tag != "v1.0.0+build2" || got[2].Version.String() != "0.9.0" {
		t.Errorf("order = %v, %v, %v", got[0].Tag, got[1].Tag, got[2].Tag)
	}
}
