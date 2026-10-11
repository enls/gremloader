// Package exitcode defines the process exit codes. They are part of the command-line contract:
// scripts and the scheduler branch on them, so they are stable and never reused.
package exitcode

const (
	// OK means the command did what was asked.
	OK = 0
	// Failed is a runtime failure: something was attempted and did not work.
	Failed = 1
	// UpdatesAvailable is returned by check-style commands when nothing failed but there is
	// something to update. It is not an error.
	UpdatesAvailable = 2
	// Locked means another GremLoader run holds the lock.
	Locked = 3
	// Usage means the command line was wrong; nothing was attempted.
	Usage = 4
	// VerifyFailed means a download did not match what it was checked against (a hash mismatch,
	// two sources that disagree, a required check that could not be made). Nothing was installed.
	VerifyFailed = 5
	// NeedsRepair means a change was interrupted or could not be undone; `gremloader repair` is
	// needed before that package is touched again.
	NeedsRepair = 6
	// Offline means nothing could be resolved because the network or GitHub's rate limit stood in
	// the way.
	Offline = 7
)

// Worst picks the code to exit with when a run had several outcomes: 6 > 5 > 7 > 1, and 2 only
// when nothing failed. 3 and 4 end a run before any package is touched and never meet the others.
func Worst(codes ...int) int {
	rank := map[int]int{NeedsRepair: 5, VerifyFailed: 4, Offline: 3, Failed: 2, UpdatesAvailable: 1}
	out := OK
	for _, c := range codes {
		if rank[c] > rank[out] {
			out = c
		}
	}
	return out
}
