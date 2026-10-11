package engine

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/enls/gremloader/internal/source/github"
	"github.com/enls/gremloader/internal/ver"
)

// Action is what applying a plan item does to the install.
type Action string

// Actions. An item with no action has a Result saying why.
const (
	ActNone      Action = ""
	ActInstall   Action = "install"
	ActUpdate    Action = "update"
	ActDowngrade Action = "downgrade"
	ActReinstall Action = "reinstall"
)

// Result is one package's outcome, as printed and logged (plan P3-17). The values are stable.
type Result string

// Results.
const (
	ResultPending     Result = "pending" // the plan will act; not applied yet
	ResultInstalled   Result = "installed"
	ResultUpdated     Result = "updated"
	ResultUnchanged   Result = "unchanged"
	ResultHeld        Result = "held"
	ResultRejected    Result = "rejected"
	ResultCooldown    Result = "cooldown"
	ResultSkipped     Result = "skipped"
	ResultFailed      Result = "failed"
	ResultNeedsRepair Result = "needs-repair"
	ResultRolledBack  Result = "rolled-back"
	ResultRemoved     Result = "removed"
	ResultAdopted     Result = "adopted"
	ResultRepaired    Result = "repaired"
)

// resolveIn is everything the choice of version depends on. It is a plain value so the choice is a
// pure function, tested as a table.
type resolveIn struct {
	Releases []github.Release

	Channel         string   // "stable" | "prerelease"
	Rejected        []string // versions rolled back from (P3-2)
	CooldownDays    int
	CooldownUnknown string // "deny" | "allow"
	Now             time.Time
	IgnoreCooldown  bool // --now

	Pin  string // from the package file or the local override; "" none
	Want string // asked for explicitly (install pkg@ver, update --version); beats Pin

	Installed      string // "" = not installed
	InstallMissing bool   // a missing package is installed (apply, install), not skipped (update)
	Hold           bool
	IncludeHeld    bool
	Force          bool // reinstall the same version
	AllowDowngrade bool
}

// decision is the resolver's answer.
type decision struct {
	Release *github.Release // what to install; nil when nothing is to be done or it failed
	Newest  *github.Release // the newest release on the channel, for latest_seen and messages
	Action  Action
	Result  Result // set when Action is none
	Reason  string // for the plan output
	Err     error  // the package cannot be resolved (a pin that does not exist)
}

// resolve picks the version to install (plan E.3.1, steps 3 to 9; steps 1 and 2 happened in the
// source).
func resolve(in resolveIn) decision {
	all := sortReleases(in.Releases)

	var installed *ver.Version
	if in.Installed != "" {
		v, err := ver.Parse(in.Installed)
		if err != nil {
			return decision{Result: ResultFailed, Err: fmt.Errorf("installed version %q in state.json: %w", in.Installed, err)}
		}
		installed = &v
	}

	// 3. Channel.
	var onChannel []github.Release
	for _, r := range all {
		if in.Channel != "prerelease" && (r.Version.IsPrerelease() || (r.FlagKnown && r.Prerelease)) {
			continue
		}
		onChannel = append(onChannel, r)
	}
	var d decision
	if len(onChannel) > 0 {
		d.Newest = &onChannel[0]
	}

	// 7a. An explicit version (asked for now, or pinned) bypasses channel, cool-down and rejected,
	// and must exist.
	explicit, source := in.Want, "requested"
	if explicit == "" {
		explicit, source = in.Pin, "pinned"
	}
	var chosen *github.Release
	if explicit != "" {
		want, err := ver.ParseLoose(explicit)
		if err != nil {
			return decision{Newest: d.Newest, Result: ResultFailed, Err: fmt.Errorf("%s version %q: %w", source, explicit, err)}
		}
		i := slices.IndexFunc(all, func(r github.Release) bool { return r.Version.Compare(want) == 0 })
		if i < 0 {
			return decision{Newest: d.Newest, Result: ResultFailed,
				Err: fmt.Errorf("%s version %s is not among the releases found", source, want)}
		}
		chosen = &all[i]
	} else {
		// 5. Rejected. 6. Cool-down. 7b. Highest remaining.
		var blockedBy Result
		var blockedWhy string
		for i := range onChannel {
			r := &onChannel[i]
			if isRejected(r.Version, in.Rejected) {
				if blockedBy == "" {
					blockedBy, blockedWhy = ResultRejected, fmt.Sprintf("%s was rolled back from, so it is skipped", r.Version)
				}
				continue
			}
			if ok, why := cooledDown(r, in); !ok {
				if blockedBy == "" {
					blockedBy, blockedWhy = ResultCooldown, why
				}
				continue
			}
			chosen = r
			break
		}
		// blockedBy is set only when a release newer than chosen was passed over. It is the reason
		// to report when it is also newer than what is installed.
		newerBlocked := blockedBy != "" && (installed == nil || d.Newest.Version.Compare(*installed) > 0)
		switch {
		case chosen == nil && newerBlocked:
			d.Result, d.Reason = blockedBy, blockedWhy
			return d
		case chosen == nil && installed != nil:
			d.Result, d.Reason = ResultUnchanged, "no eligible release"
			return d
		case chosen == nil:
			d.Result, d.Reason = ResultSkipped, "no release on the "+channelName(in.Channel)+" channel"
			return d
		case newerBlocked && installed != nil && chosen.Version.Compare(*installed) <= 0:
			d.Result, d.Reason = blockedBy, blockedWhy // nothing newer than installed is eligible
		case newerBlocked:
			d.Reason = blockedWhy + "; the newest eligible release is used instead"
		}
	}

	// 8. Compare with what is installed.
	if installed == nil {
		if !in.InstallMissing {
			d.Result, d.Reason = ResultSkipped, "not installed"
			return d
		}
		d.Action = ActInstall
	} else {
		switch c := chosen.Version.Compare(*installed); {
		case c == 0:
			if !in.Force {
				if d.Result == "" {
					d.Result = ResultUnchanged
				}
				return d
			}
			d.Action = ActReinstall
		case c > 0:
			d.Action = ActUpdate
		case explicit == "":
			// The highest eligible release is older than what is installed (the installed one is
			// itself still cooling down, a newer one was withdrawn, or the installed one is outside
			// the channel): never go back on our own.
			if d.Result == "" {
				d.Result = ResultUnchanged
				if d.Newest == nil || d.Newest.Version.Compare(*installed) < 0 {
					d.Reason = fmt.Sprintf("installed %s is newer than every eligible release", *installed)
				}
			}
			return d
		case !in.AllowDowngrade:
			msg := fmt.Sprintf("%s %s is older than installed %s; a downgrade needs --allow-downgrade", source, chosen.Version, *installed)
			if in.Want != "" {
				// Asked for on the command line: not doing it is a failure (exit 1), so a script
				// does not read "skipped" as success. A pin is configuration and stays skipped:
				// otherwise every scheduled run would fail until it was changed.
				d.Result, d.Err = ResultFailed, errors.New(msg)
				return d
			}
			d.Result, d.Reason = ResultSkipped, msg
			return d
		default:
			d.Action = ActDowngrade
		}
	}

	// 9. Hold.
	if in.Hold && !in.IncludeHeld {
		d.Action, d.Result, d.Reason = ActNone, ResultHeld, fmt.Sprintf("%s available, held", chosen.Version)
		return d
	}
	d.Release = chosen
	d.Result = ""
	return d
}

// sortReleases returns the releases newest first: by version, and between equal versions (a tag
// and its build-metadata twin) the later publication first.
func sortReleases(rs []github.Release) []github.Release {
	out := slices.Clone(rs)
	slices.SortStableFunc(out, func(a, b github.Release) int {
		if c := b.Version.Compare(a.Version); c != 0 {
			return c
		}
		return b.Published.Compare(a.Published)
	})
	return out
}

func isRejected(v ver.Version, rejected []string) bool {
	for _, s := range rejected {
		if r, err := ver.Parse(s); err == nil && r.Compare(v) == 0 {
			return true
		}
	}
	return false
}

// cooledDown reports whether r has been published long enough. An unknown or future publication
// time counts as too new unless the package allows unknown ages.
func cooledDown(r *github.Release, in resolveIn) (bool, string) {
	if in.CooldownDays <= 0 || in.IgnoreCooldown {
		return true, ""
	}
	wait := time.Duration(in.CooldownDays) * 24 * time.Hour
	if r.Published.IsZero() || r.Published.After(in.Now) {
		if in.CooldownUnknown == "allow" {
			return true, ""
		}
		return false, fmt.Sprintf("%s has no known publication time, so its %d-day cool-down cannot be checked", r.Version, in.CooldownDays)
	}
	if in.Now.Sub(r.Published) >= wait {
		return true, ""
	}
	return false, fmt.Sprintf("%s is in its %d-day cool-down until %s", r.Version, in.CooldownDays,
		r.Published.Add(wait).UTC().Format("2006-01-02 15:04 UTC"))
}

func channelName(c string) string {
	if c == "" {
		return "stable"
	}
	return c
}
